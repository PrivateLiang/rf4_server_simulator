package gameserver

// L1 协议接入：AI 私聊请求的入队与 AI 回复的投递。
// 关键边界：只有 source_kind=human 的消息能进 ai_task；AI 回复只写 mail/推送，
// 绝不回流，从结构上杜绝 Agent ↔ Agent 循环。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/proto"
	"rf4server/internal/store"
)

const (
	aiTaskTTL          = 30 * time.Second
	aiMaxMessageRunes  = 500
	aiMaxDeliveryBatch = 16
)

// receiveAIChat 处理 19/1 发给某个 AI 联系人的私聊。立即 ACK，不等待模型。
func (s *Server) receiveAIChat(ctx *Ctx, contactUID int64, agentKey, text string) ([]byte, bool) {
	if !ctx.ExpectResponse {
		return nil, true
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return []byte{0}, true
	}
	if r := []rune(text); len(r) > aiMaxMessageRunes {
		text = string(r[:aiMaxMessageRunes])
	}
	deadline := float64(time.Now().Add(aiTaskTTL).Unix())
	// 急停期间新任务不触发：立即 ACK，不入队、不合并到旧任务。
	// 恢复后玩家再发一句才会重新入队（停前意图不重放）。
	cctx, active := s.bindAIIntent(ctx.Sess.pushCtx(), s.DB)
	if !active {
		return []byte{0}, true
	}
	// 连发消息合并成一次请求：既省 token，也避免 AI 对半句话逐条回复。
	merged, mergeErr := s.DB.AIMergePendingDM(cctx, ctx.Sess.Userid,
		agentKey, text, deadline, aiMaxMessageRunes)
	if mergeErr != nil {
		s.log.Warn("AI 消息合并失败", "uid", ctx.Sess.Userid, "agent", agentKey, "err", mergeErr)
	}
	if merged {
		return []byte{0}, true
	}
	_, err := s.DB.AIEnqueue(cctx, &store.AITask{
		Kind:       "dm",
		Scope:      "uid",
		ScopeKey:   fmt.Sprint(ctx.Sess.Userid),
		UID:        ctx.Sess.Userid,
		Agent:      agentKey,
		ContactUID: contactUID,
		SourceKind: "human",
		Text:       text,
		Loc:        s.currentLevel(ctx.Sess.Userid),
		DeadlineTS: deadline,
	})
	if err != nil {
		s.log.Warn("AI 任务入队失败", "uid", ctx.Sess.Userid, "agent", agentKey, "err", err)
	}
	return []byte{0}, true
}

// aiDeliveryBarrierForTest 仅测试用：在频道最终推送与私聊事务边界复核急停前
// 调用，让用例能精确把急停插在「筛选完成」与「真正发送」之间。生产恒为 nil。
var aiDeliveryBarrierForTest func()
var aiMailNotifyBarrierForTest func()

func (s *Server) aiEmergencyNow(ctx context.Context, db *store.DB) aiconfig.EmergencyDecision {
	if db == nil {
		return aiconfig.EvaluateEmergency(aiconfig.EmergencyReading{Err: errors.New("gs: 数据库未连接")})
	}
	return aiconfig.EvaluateEmergency(db.AIEmergencyState(ctx))
}

func (s *Server) bindAIIntent(ctx context.Context, db *store.DB) (context.Context, bool) {
	d := s.aiEmergencyNow(ctx, db)
	if epoch, bound := store.AIIntentEpoch(ctx); bound {
		active, _ := d.IntentActive(epoch)
		return ctx, active
	}
	blocked, _ := d.RecordBlocked()
	return store.AIIntentContext(ctx, d.Epoch), !blocked
}

func (s *Server) aiBlockedNow(ctx context.Context, db *store.DB) bool {
	_, active := s.bindAIIntent(ctx, db)
	return !active
}

// pollAIResults consumes stale results and sends at most one channel reply per poll.
func (s *Server) pollAIResults(ctx context.Context, db *store.DB) {
	if db == nil {
		return
	}
	channelSent := false
	seen := make(map[int64]bool)
	for attempted := 0; attempted < aiMaxDeliveryBatch; {
		batch, err := db.AIPendingResults(ctx, aiMaxDeliveryBatch)
		if err != nil {
			s.log.Warn("AI 结果读取失败", "err", err)
			return
		}
		progress := false
		for _, r := range batch {
			if seen[r.ID] || attempted >= aiMaxDeliveryBatch {
				continue
			}
			dec := s.aiEmergencyNow(ctx, db)
			if dec.Fault == aiconfig.FaultReadError {
				return
			}
			active, _ := dec.IntentActive(r.IntentEpoch)
			if active && r.Kind == "channel" && channelSent {
				continue
			}
			seen[r.ID], progress = true, true
			if !active {
				if err := db.AIDropResult(ctx, r.ID); err != nil {
					s.log.Warn("AI 结果丢弃失败", "id", r.ID, "err", err)
				}
				continue
			}
			attempted++
			ok, stale, err := s.deliverAIResult(ctx, db, r)
			if err != nil {
				s.log.Warn("AI 回复待重试", "id", r.ID, "err", err)
				continue
			}
			switch {
			case stale:
				err = db.AIDropResult(ctx, r.ID)
			case r.Kind == "channel" || !ok:
				err = db.AIMarkDelivered(ctx, r.ID, ok)
			}
			if err != nil {
				s.log.Warn("AI 结果标记失败", "id", r.ID, "err", err)
			}
			channelSent = channelSent || (r.Kind == "channel" && ok)
		}
		if !progress {
			return
		}
	}
}

// deliverAIResult 把回复写进 mail 表，并在 ACK 之后通知在线会话。
// 返回值第二项表示「在最终发送边界观察到急停或原意图失效，本条不得投递」，
// 由调用方落终态。
func (s *Server) deliverAIResult(ctx context.Context, db *store.DB, r store.AIResult) (ok, stale bool, err error) {
	if r.ReplyText == "" || r.ContactUID == 0 {
		return false, false, nil
	}
	if r.Kind == "channel" {
		return s.deliverAIChannel(ctx, db, r)
	}
	if r.UID == 0 {
		return false, false, nil
	}
	if aiDeliveryBarrierForTest != nil {
		aiDeliveryBarrierForTest()
	}
	var mailID int64
	tx, err := db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// The conditional write acquires SQLite write ownership before validation.
	res, err := tx.ExecContext(ctx, `UPDATE ai_result SET delivered_ts=?, reply_text=''
		WHERE id=? AND task_id=? AND delivered_ts IS NULL`, float64(time.Now().Unix()), r.ID, r.TaskID)
	if err != nil {
		return false, false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, false, err
	}
	reading := store.ReadAIEmergency(ctx, tx)
	if reading.Err != nil {
		return false, false, reading.Err
	}
	epoch, err := store.AITaskEpoch(ctx, tx, r.TaskID)
	if err != nil {
		return false, false, err
	}
	if active, _ := aiconfig.EvaluateEmergency(reading).IntentActive(epoch); !active || epoch != r.IntentEpoch {
		return false, true, nil
	}
	mailID, err = insertContactReply(ctx, tx, r.UID, r.ContactUID, r.ReplyText)
	if err != nil {
		return false, false, err
	}
	if err := tx.Commit(); err != nil {
		return false, false, err
	}

	// The mail is already committed. Only the optional online notification is gated.
	mail, err := db.MailGet(ctx, r.UID, mailID)
	if err != nil || mail.Read != 0 {
		return true, false, nil
	}
	body := proto.Args524(buildMail11062(mail))
	for _, sess := range s.sessionsForUID(r.UID) {
		if aiMailNotifyBarrierForTest != nil {
			aiMailNotifyBarrierForTest()
		}
		if active, _ := s.aiEmergencyNow(ctx, db).IntentActive(r.IntentEpoch); !active {
			break
		}
		sess.Push(19, 10, body)
	}
	return true, false, nil
}
