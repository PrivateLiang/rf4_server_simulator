package store

// AI 接入的队列与配置仓储。复用主库事务，不触碰只读静态库。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rogpeppe/go-internal/lockedfile"

	"rf4server/internal/aiconfig"
)

// AITask 是一次待处理的对话请求。只有 source_kind=human 才允许入队，
// 从结构上杜绝 Agent 回复回流触发新的 Agent 请求。
type AITask struct {
	ID           int64
	Kind         string
	Scope        string
	ScopeKey     string
	UID          int64
	Agent        string
	ContactUID   int64
	SourceKind   string
	Text         string
	State        string
	DeadlineTS   float64
	Attempt      int
	Participants string  // roundtable：JSON 数组，参与 Agent 的 key
	Loc          string  // 入队时的地图 key，用于注入「当前地图鱼情」
	CreatedTS    float64 // 入队时间（Unix 秒）
	IntentEpoch  int64
}

// AIResult 是一条待投递给客户端的回复。
type AIResult struct {
	ID          int64
	TaskID      int64
	Status      string
	ReplyText   string
	ContactUID  int64
	UID         int64
	Kind        string  // dm=私聊 mail；channel=公共频道 24/27
	ScopeKey    string  // channel 时为频道名
	CreatedTS   float64 // 结果生成时间（秒）
	IntentEpoch int64   // 由父任务绑定；0 表示来源不可证。
}

// AIAgent 是 Agent 注册表的一行。
type AIAgent struct {
	Key                string
	Display            string
	Role               string
	ContactUID         int64
	Prompt             string
	ToolsJSON          string
	ThinkingEnabled    bool
	ThinkingEffort     string
	Enabled            bool
	Joined             bool   // true 才会出现在游戏社交列表并接收 19/1 私聊
	ActorEnabled       bool   // true 才维护「在钓鱼」的角色状态
	ReplyMode          string // social=可主动插话，mention=只有被点名才说，silent=只看不说
	Talkativeness      int    // 0-100，越高越容易主动接话
	FishingTalkEnabled bool   // true 才允许「AI 自己上鱼后主动报一句」
	// Generation / Revision 来自 ai_runtime_control（缺行按 1/0）：
	// 后台入队时带上它们，旧页面提交的过期代次会被 GS 拒绝。
	Generation int64
	Revision   int64
}

func nowTS() float64 { return float64(time.Now().Unix()) }

// AIEnqueue 写入一条真人发起的对话任务。
//
// 这是保留的公共入口：真人私聊与明确直接询答按「直接对话」绑定证据
// （Direct=true，无事件类别）。主动事件的入口必须用 AIEnqueueTrusted
// 显式给出类别与逐角色能力，否则消费端无法证明类别（T-108 §7）。
func (d *DB) AIEnqueue(ctx context.Context, t *AITask) (int64, error) {
	if t == nil || t.SourceKind != "human" {
		return 0, errors.New("ai: only human messages can be enqueued")
	}
	ev := AITaskEvidence{FormatVersion: AITaskEvidenceFormatV1, Direct: true}
	return d.aiEnqueueTask(ctx, *t, &ev)
}

func (d *DB) aiEnqueueTask(ctx context.Context, t AITask, ev *AITaskEvidence) (int64, error) {
	ctx = aiCaptureIntent(ctx, d)
	epoch, _ := AIIntentEpoch(ctx)
	if t.Participants == "" {
		t.Participants = "[]"
	}
	var id int64
	err := d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		if err := aiRequireIntent(ctx, tx.SQL(), epoch); err != nil {
			return err
		}
		bound, binding, err := bindAITaskEvidence(ctx, tx.SQL(), t, ev)
		if err != nil {
			return err
		}
		res, err := tx.SQL().ExecContext(ctx, `
			INSERT INTO ai_task(kind, scope, scope_key, uid, agent, contact_uid,
				source_kind, text, state, deadline_ts, attempt, created_ts, participants, loc, intent_epoch)
			VALUES(?,?,?,?,?,?,?,?, 'pending',?,0,?,?,?,?)`,
			t.Kind, t.Scope, t.ScopeKey, t.UID, t.Agent, t.ContactUID,
			t.SourceKind, t.Text, t.DeadlineTS, nowTS(), t.Participants, t.Loc, epoch)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		if err != nil {
			return err
		}
		if bound {
			if _, err := tx.SQL().ExecContext(ctx, `
				INSERT INTO ai_task_evidence(task_id, binding_json) VALUES(?,?)`,
				id, binding); err != nil {
				return fmt.Errorf("ai: 任务证据写入失败: %w", err)
			}
		}
		return nil
	})
	return id, err
}

// AIEnqueueTrusted 是受信事件入口：显式带事件类别、直接对话标识、发起者与
// 有界参与者列表，在同一事务里与 ai_task 一起落任务证据。
//
// 参与者可以只给 Key（以及该角色这一次被要求的能力）；角色代次与能力修订
// 由本事务即读即写，调用方给的值只作一致性断言，不一致就拒绝入队。
func (d *DB) AIEnqueueTrusted(ctx context.Context, t *AITask, ev AITaskEvidence) (int64, error) {
	if t == nil {
		return 0, errors.New("ai: 缺少任务")
	}
	if t.SourceKind != "human" && t.SourceKind != "system" {
		return 0, errors.New("ai: 未知任务来源")
	}
	return d.aiEnqueueTask(ctx, *t, &ev)
}

// AIMergePendingDM 把同一玩家刚发的下一条私聊并入尚未处理的 DM 任务，
// 避免连发几句就产生多次模型请求。只有 pending 任务可合并；已开始处理的
// 消息会正常排到后面。
func (d *DB) AIMergePendingDM(ctx context.Context, uid int64, agent, text string, deadline float64, maxRunes int) (bool, error) {
	if strings.TrimSpace(text) == "" {
		return false, nil
	}
	if maxRunes <= 0 {
		maxRunes = 500
	}
	ctx = aiCaptureIntent(ctx, d)
	epoch, _ := AIIntentEpoch(ctx)
	var merged bool
	err := d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		if err := aiRequireIntent(ctx, tx.SQL(), epoch); err != nil {
			return err
		}
		res, err := tx.SQL().ExecContext(ctx, `
			UPDATE ai_task SET text=text||char(10)||?, deadline_ts=?
			WHERE id=(
				SELECT id FROM ai_task
				WHERE kind='dm' AND uid=? AND agent=? AND state='pending' AND intent_epoch=?
				ORDER BY id DESC LIMIT 1
			)
			AND length(text)+length(?)+1<=?`,
			text, deadline, uid, agent, epoch, text, maxRunes)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		merged = n > 0
		return err
	})
	return merged, err
}

// AIPopTask 事务内取最旧一条 pending 任务并标记 processing。
//
// 返回的 AITask.Attempt 是「本次认领之后」的尝试号（同一事务回读，不是认领前
// 的旧值）：写结果、结束任务、追加圆桌发言都必须带它。任务被回收后重新认领
// 会推进这个号，于是旧尝试的迟到写入按「不再活跃」拒绝（T-108 §9.2 E4）。
func (d *DB) AIPopTask(ctx context.Context) (*AITask, bool, error) {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, `
		SELECT id, kind, scope, scope_key, uid, agent, contact_uid,
		       source_kind, text, state, deadline_ts, attempt, participants, loc, created_ts, COALESCE(CASE WHEN typeof(intent_epoch)='integer' THEN intent_epoch END,0)
		FROM ai_task WHERE state='pending' ORDER BY id LIMIT 1`)
	var t AITask
	if err := row.Scan(&t.ID, &t.Kind, &t.Scope, &t.ScopeKey, &t.UID, &t.Agent,
		&t.ContactUID, &t.SourceKind, &t.Text, &t.State, &t.DeadlineTS, &t.Attempt,
		&t.Participants, &t.Loc, &t.CreatedTS, &t.IntentEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE ai_task SET state='processing', attempt=attempt+1 WHERE id=? AND state='pending'`, t.ID)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 并发的另一个 worker 抢先认领了同一行：本次没有任务。
		return nil, false, nil
	}
	// 同一事务回读本次认领后的尝试号，避免结构体里还是旧值。
	if err := tx.QueryRowContext(ctx,
		`SELECT state, attempt FROM ai_task WHERE id=?`, t.ID).Scan(&t.State, &t.Attempt); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &t, true, nil
}

// ErrAITaskNotActive 表示任务已被过期清理、已被回收重领或已被其他 worker 处理，
// 当前这次尝试不再活跃，不能再写结果或改状态。
var ErrAITaskNotActive = errors.New("ai: task is not active")

// aiProcessingAttempt 读任务当前 processing 尝试号。
//
// 只给保留入口用（测试与内部按「当前尝试」绑定的包装）；生产写路径必须显式
// 传 AIPopTask 返回的尝试号，迟到写入才会因 attempt 不等而被拒。
func (d *DB) aiProcessingAttempt(ctx context.Context, taskID int64) (int, error) {
	var attempt int
	err := d.SQL.QueryRowContext(ctx,
		`SELECT attempt FROM ai_task WHERE id=? AND state='processing'`, taskID).Scan(&attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrAITaskNotActive
	}
	if err != nil {
		return 0, err
	}
	return attempt, nil
}

// AIComplete 写结果并把任务置为终态（保留入口：内部按当前 processing 尝试绑定）。
//
// 只接受 processing 状态：任务若已过期/被清理/已被回收重领，迟到的模型输出
// 直接丢弃，避免隔天补发。
// 同时清空 user 原文：聊天内容只留在内存会话与游戏邮件里，不在队列表长期堆积。
//
// 不写表达证据：调用方没有证明「实际用了哪一版四维」，这条结果按未确认处理。
func (d *DB) AIComplete(ctx context.Context, taskID int64, status, reply, rawUsage, errText string) error {
	attempt, err := d.aiProcessingAttempt(ctx, taskID)
	if err != nil {
		return err
	}
	return d.AICompleteWithExpression(ctx, taskID, attempt, status, reply, rawUsage, errText, nil)
}

// AICompleteWithExpression 是 BR 的写结果入口：attempt 必须是本次认领后的
// 尝试号（AIPopTask 返回值）；expr 非 nil 时，与 ai_result 同事务写一行
// ai_result_expression，记录本句实际使用的角色代次与四维修订。
//
// 只有真正用同一份已校验配置产生正常非空表达结果时才允许带 expr；
// 失败、空正文、未配四维的结果一律不带，避免把「未确认」写成「已生效」。
func (d *DB) AICompleteWithExpression(ctx context.Context, taskID int64, attempt int,
	status, reply, rawUsage, errText string, expr *AIResultExpression) error {
	if err := validateAIResultExpression(status, reply, expr); err != nil {
		return err
	}
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	epoch, err := AITaskEpoch(ctx, tx, taskID)
	if err != nil {
		return err
	}
	reason, err := aiCheckIntent(ctx, tx, epoch)
	if err != nil {
		return err
	}
	if reason != "" {
		status, reply, rawUsage, errText = "fallback", "", "", reason
		expr = nil
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE ai_task SET state=?, text='' WHERE id=? AND state='processing' AND attempt=?`,
		status, taskID, attempt)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAITaskNotActive
	}
	var delivered any
	if reply == "" {
		delivered = -1
	}
	ins, err := tx.ExecContext(ctx, `
		INSERT INTO ai_result(task_id, status, reply_text, raw_usage, err, created_ts, delivered_ts)
		VALUES(?,?,?,?,?,?,?)`, taskID, status, reply, rawUsage, errText, nowTS(), delivered)
	if err != nil {
		return fmt.Errorf("ai result: %w", err)
	}
	if expr != nil {
		resultID, err := ins.LastInsertId()
		if err != nil {
			return err
		}
		if err := insertAIResultExpression(ctx, tx, resultID, *expr); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var aiMemoryDeliveryReclaim sync.Mutex

// aiDeliveryReclaimLock 串行化同一主库的频道发帧和任务回收。GS 与 BR 可在
// 不同进程；锁文件由操作系统持锁，进程退出时自动释放。持锁期间可照常
// 使用 SQLite，包括 Session.Push 为 call_id 保存的 KV。
func (d *DB) aiDeliveryReclaimLock(ctx context.Context) (func(), error) {
	// 从 SQLite 取得实际主库路径，兼容 URI 打开方式与内存库。
	var path string
	if err := d.SQL.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&path); err != nil {
		return nil, fmt.Errorf("ai delivery: read database path: %w", err)
	}
	var unlock func()
	if path == "" {
		aiMemoryDeliveryReclaim.Lock()
		unlock = aiMemoryDeliveryReclaim.Unlock
	} else {
		path, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("ai delivery: resolve database path: %w", err)
		}
		unlock, err = lockedfile.MutexAt(path + ".ai-delivery.lock").Lock()
		if err != nil {
			return nil, fmt.Errorf("ai delivery: acquire reclaim lock: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// AIExpireTasks 清理超时任务并回收异常退出留下的 processing 任务。
//   - pending 且已过 deadline：标记 expired，不再处理；
//   - processing 且超过 deadline+grace 且只跑过一次：退回 pending，允许重试一次；
//   - processing 且已重试过：标记 fallback，避免无限重跑。
func (d *DB) AIExpireTasks(ctx context.Context, now, grace float64) (expired, recovered int64, err error) {
	unlock, err := d.aiDeliveryReclaimLock(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer unlock()
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	reading := ReadAIEmergency(ctx, tx)
	if reading.Err != nil {
		return 0, 0, reading.Err
	}
	dec := aiconfig.EvaluateEmergency(reading)
	blocked, reason := dec.RecordBlocked()
	if !blocked {
		reason = aiconfig.IntentQueuedBeforeResume
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ai_result(task_id,status,reply_text,raw_usage,err,created_ts,delivered_ts)
		SELECT id,'fallback','','',CASE WHEN ?=0 AND (typeof(intent_epoch)<>'integer' OR intent_epoch<=0) THEN ? ELSE ? END,?,-1
		FROM ai_task WHERE state IN ('pending','processing')
		  AND (? OR typeof(intent_epoch)<>'integer' OR intent_epoch<>?)`,
		blocked, aiconfig.IntentOriginUnknown, reason, now, blocked, dec.Epoch); err != nil {
		return 0, 0, err
	}
	stale, err := tx.ExecContext(ctx, `UPDATE ai_task SET state='fallback', text=''
		WHERE state IN ('pending','processing') AND (? OR typeof(intent_epoch)<>'integer' OR intent_epoch<>?)`,
		blocked, dec.Epoch)
	if err != nil {
		return 0, 0, err
	}
	expired, _ = stale.RowsAffected()
	res, err := tx.ExecContext(ctx, `
		UPDATE ai_task SET state='expired', text=''
		WHERE state='pending' AND deadline_ts>0 AND deadline_ts<?`, now)
	if err != nil {
		return 0, 0, err
	}
	timedOut, _ := res.RowsAffected()
	expired += timedOut
	// 回收 processing 任务之前，先在同一事务里把它未投递的旧结果置终态：
	// 旧尝试已经作废，那些结果不允许被 AIPendingResults 迟到取走、更不允许补发。
	// 只处理「清正文 + 终态标记」，有父行历史与已投递结果都不动。
	if _, err := tx.ExecContext(ctx, `
		UPDATE ai_result SET delivered_ts=-1, reply_text='',
		       err=CASE WHEN err='' THEN ? ELSE err END
		WHERE delivered_ts IS NULL AND task_id IN (
			SELECT id FROM ai_task
			WHERE state='processing' AND deadline_ts>0 AND deadline_ts<? AND attempt<2)`,
		AIEvidenceReasonAttemptReclaimed, now-grace); err != nil {
		return 0, 0, err
	}
	res, err = tx.ExecContext(ctx, `
		UPDATE ai_task SET state='pending'
		WHERE state='processing' AND deadline_ts>0 AND deadline_ts<? AND attempt<2`, now-grace)
	if err != nil {
		return 0, 0, err
	}
	recovered, _ = res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `
		UPDATE ai_task SET state='fallback', text=''
		WHERE state='processing' AND deadline_ts>0 AND deadline_ts<? AND attempt>=2`, now-grace); err != nil {
		return 0, 0, err
	}
	return expired, recovered, tx.Commit()
}

// AIExpireToolJobs 把长时间停在 processing 的工具作业标记为超时，
// 防止 gameserver 异常退出后作业永远占着队列。
func (d *DB) AIExpireToolJobs(ctx context.Context, before float64) (int64, error) {
	var count int64
	err := d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		reading := ReadAIEmergency(ctx, tx.SQL())
		if reading.Err != nil {
			return reading.Err
		}
		dec := aiconfig.EvaluateEmergency(reading)
		blocked, reason := dec.RecordBlocked()
		if !blocked {
			reason = aiconfig.IntentQueuedBeforeResume
		}
		stale, err := tx.SQL().ExecContext(ctx, `UPDATE ai_tool_job AS j
			SET state='done', args='', result_text='', done_ts=?,
			    terminal_reason=CASE WHEN ?=0 AND NOT EXISTS (SELECT 1 FROM ai_task t
			         WHERE t.id=j.task_id AND t.state='processing' AND typeof(t.intent_epoch)='integer' AND t.intent_epoch>0)
			         THEN ? ELSE ? END
			WHERE state IN ('pending','processing') AND (? OR NOT EXISTS
				(SELECT 1 FROM ai_task t WHERE t.id=j.task_id AND t.state='processing' AND t.intent_epoch=?))`,
			nowTS(), blocked, aiconfig.IntentOriginUnknown, reason, blocked, dec.Epoch)
		if err != nil {
			return err
		}
		count, _ = stale.RowsAffected()
		res, err := tx.SQL().ExecContext(ctx, `UPDATE ai_tool_job
			SET state='done', args='', result_text='（查询超时）', done_ts=?, terminal_reason='expired'
			WHERE state='processing' AND created_ts<?`, nowTS(), before)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		count += n
		return err
	})
	return count, err
}

// AIPrune 清理已投递/已终结的历史记录，避免 ai_* 表无限增长。
// 聊天原文在任务结束时已清空；这里只删元数据与迟到的空结果。
//
// 两张从属凭据表在同一事务里随父行回收：先删 ai_result 的表达证据，
// 再删 ai_result；父任务证据在 ai_task 之前删，任何一步失败都整体回滚，
// 不会留下没有父行的孤儿凭据。
//
// 回退窗口（DR-108 / O1）：旧版程序不认识这两张附表，清理父行时不会跟着删，
// 因此这里额外做一次「无父行孤儿」清扫——只删没有父行的凭据行，有父行的历史
// （包括旧版本保留的终态结果与已投递结果）一行不动。
func (d *DB) AIPrune(ctx context.Context, before float64) error {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ai_tool_job WHERE state='done' AND created_ts<?`, before); err != nil {
		return err
	}
	// 回退窗口孤儿：旧版 AIPrune 只删 ai_task / ai_result，两张附表会留下没有
	// 父行的行。这里只按「父行不存在」回收，不碰任何有父行的证据。
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM ai_task_evidence WHERE NOT EXISTS (
			SELECT 1 FROM ai_task WHERE ai_task.id=ai_task_evidence.task_id)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM ai_result_expression WHERE NOT EXISTS (
			SELECT 1 FROM ai_result WHERE ai_result.id=ai_result_expression.result_id)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM ai_result_expression WHERE result_id IN (
			SELECT id FROM ai_result WHERE delivered_ts IS NOT NULL AND created_ts<?)`, before); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ai_result WHERE delivered_ts IS NOT NULL AND created_ts<?`, before); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM ai_task_evidence WHERE task_id IN (
			SELECT id FROM ai_task
			WHERE state IN ('done','fallback','expired') AND created_ts<?
			  AND NOT EXISTS (SELECT 1 FROM ai_result WHERE task_id=ai_task.id)
			  AND NOT EXISTS (SELECT 1 FROM ai_tool_job WHERE task_id=ai_task.id AND state<>'done')
			  AND NOT EXISTS (SELECT 1 FROM ai_runtime_command WHERE parent_task_id=ai_task.id AND status='pending'))`,
		before); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM ai_task
		WHERE state IN ('done','fallback','expired') AND created_ts<?
		  AND NOT EXISTS (SELECT 1 FROM ai_result WHERE task_id=ai_task.id)
		  AND NOT EXISTS (SELECT 1 FROM ai_tool_job WHERE task_id=ai_task.id AND state<>'done')
		  AND NOT EXISTS (SELECT 1 FROM ai_runtime_command WHERE parent_task_id=ai_task.id AND status='pending')`, before); err != nil {
		return err
	}
	return tx.Commit()
}

// AIPendingResults 取尚未投递的结果，联表带回收件人与联系人 UID。
func (d *DB) AIPendingResults(ctx context.Context, limit int) ([]AIResult, error) {
	if limit <= 0 {
		limit = 16
	}
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT r.id, r.task_id, r.status, r.reply_text,
		       COALESCE(NULLIF(r.contact_uid,0), t.contact_uid, 0),
		       COALESCE(t.uid,0),
		       CASE WHEN t.kind='roundtable' THEN 'channel' ELSE COALESCE(t.kind,'dm') END,
		       COALESCE(t.scope_key,''), COALESCE(r.created_ts,0), COALESCE(CASE WHEN typeof(t.intent_epoch)='integer' THEN t.intent_epoch END,0)
		FROM ai_result r LEFT JOIN ai_task t ON t.id=r.task_id
		WHERE r.delivered_ts IS NULL AND r.reply_text <> ''
		ORDER BY r.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIResult
	for rows.Next() {
		var r AIResult
		if err := rows.Scan(&r.ID, &r.TaskID, &r.Status, &r.ReplyText,
			&r.ContactUID, &r.UID, &r.Kind, &r.ScopeKey, &r.CreatedTS, &r.IntentEpoch); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AIMarkDelivered 标记结果已投递（或投递失败）。
// 投递成功后清空回复正文：DM 已写进游戏邮件，频道已在客户端展示，
// 不再让聊天内容长期留在 AI 队列里。
//
// 成功标记与失败标记都只在 delivered_ts IS NULL（仍待投递）时生效：
// 结果一旦被回收（AIExpireTasks 写 attempt_reclaimed）或被丢弃，那一行已有终态，
// 迟到的成功标记不得把它翻成「已投递」，否则「旧尝试不补发」的终态会被
// 一次并发投递悄悄抹掉（T-108 §9.2 E4 的发前复核与标记一致性）。
func (d *DB) AIMarkDelivered(ctx context.Context, resultID int64, ok bool) error {
	state := float64(0)
	if !ok {
		state = -1
	}
	if ok {
		_, err := d.SQL.ExecContext(ctx,
			`UPDATE ai_result SET delivered_ts=?, reply_text='' WHERE id=? AND delivered_ts IS NULL`,
			nowTS(), resultID)
		return err
	}
	_, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_result SET delivered_ts=? WHERE id=? AND delivered_ts IS NULL`, state, resultID)
	return err
}

// AIDropResult 丢弃一条待投递结果：标记终态并清空正文。
// 急停期间的「不投递」用它，恢复后也不会把停前生成的内容补发出去。
func (d *DB) AIDropResult(ctx context.Context, resultID int64) error {
	_, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_result SET delivered_ts=-1, reply_text='' WHERE id=? AND delivered_ts IS NULL`,
		resultID)
	return err
}

// AIDropResultWithReason 在丢弃时一并记下原因，供回执逐条核对
// 「任务、结果、原因」。已终结的结果保持幂等，不会覆盖原有原因。
func (d *DB) AIDropResultWithReason(ctx context.Context, resultID int64, reason string) error {
	_, err := d.SQL.ExecContext(ctx, `
		UPDATE ai_result SET delivered_ts=-1, reply_text='',
		       err=CASE WHEN err='' THEN ? ELSE err END
		WHERE id=? AND delivered_ts IS NULL`, reason, resultID)
	return err
}

// AIAgentByKey 取一个 Agent 配置（含运行态代次，供工具环境注入）。
func (d *DB) AIAgentByKey(ctx context.Context, key string) (*AIAgent, error) {
	row := d.SQL.QueryRowContext(ctx, `
		SELECT a.key, a.display, a.role, a.contact_uid, a.prompt, a.tools_json,
		       a.thinking_enabled, a.thinking_effort, a.enabled, a.joined, a.actor_enabled,
		       COALESCE(a.reply_mode,'social'), COALESCE(a.talkativeness,50), COALESCE(a.fishing_talk_enabled,0),
		       COALESCE(c.generation,1), COALESCE(c.revision,0)
		FROM ai_agent a
		LEFT JOIN ai_runtime_control c ON c.agent_key=a.key
		WHERE a.key=? AND a.deleted_ts=0`, key)
	var a AIAgent
	var thinking, enabled, joined, actor, fishTalk int
	if err := row.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt, &a.ToolsJSON,
		&thinking, &a.ThinkingEffort, &enabled, &joined, &actor, &a.ReplyMode, &a.Talkativeness,
		&fishTalk, &a.Generation, &a.Revision); err != nil {
		return nil, err
	}
	a.ThinkingEnabled = thinking != 0
	a.Enabled = enabled != 0
	a.Joined = joined != 0
	a.ActorEnabled = actor != 0
	a.FishingTalkEnabled = fishTalk != 0
	return &a, nil
}

// AIListAgents 返回全部已启用的 Agent（后台用，含未接入的），按联系人 UID 降序。
// generation/revision 来自运行态 control，供后台把「看到的状态」作为入队期望值。
func (d *DB) AIListAgents(ctx context.Context) ([]AIAgent, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT a.key, a.display, a.role, a.contact_uid, a.prompt, a.tools_json,
		       a.thinking_enabled, a.thinking_effort, a.enabled, a.joined, a.actor_enabled,
		       COALESCE(a.reply_mode,'social'), COALESCE(a.talkativeness,50), COALESCE(a.fishing_talk_enabled,0),
		       COALESCE(c.generation,1), COALESCE(c.revision,0)
		FROM ai_agent a
		LEFT JOIN ai_runtime_control c ON c.agent_key=a.key
		WHERE a.enabled=1 AND a.deleted_ts=0 ORDER BY a.contact_uid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIAgent
	for rows.Next() {
		var a AIAgent
		var thinking, enabled, joined, actor, fishTalk int
		if err := rows.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt, &a.ToolsJSON,
			&thinking, &a.ThinkingEffort, &enabled, &joined, &actor, &a.ReplyMode, &a.Talkativeness,
			&fishTalk, &a.Generation, &a.Revision); err != nil {
			return nil, err
		}
		a.ThinkingEnabled = thinking != 0
		a.Enabled = enabled != 0
		a.Joined = joined != 0
		a.ActorEnabled = actor != 0
		a.FishingTalkEnabled = fishTalk != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// AIJoinedAgents 只返回已接入游戏的 Agent（社交成员列表用）。
func (d *DB) AIJoinedAgents(ctx context.Context) ([]AIAgent, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT key, display, role, contact_uid, prompt, tools_json,
		       thinking_enabled, thinking_effort, enabled, joined, actor_enabled,
		       COALESCE(reply_mode,'social'), COALESCE(talkativeness,50), COALESCE(fishing_talk_enabled,0)
		FROM ai_agent WHERE enabled=1 AND joined=1 AND deleted_ts=0 ORDER BY contact_uid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIAgent
	for rows.Next() {
		var a AIAgent
		var thinking, enabled, joined, actor, fishTalk int
		if err := rows.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt, &a.ToolsJSON,
			&thinking, &a.ThinkingEffort, &enabled, &joined, &actor, &a.ReplyMode, &a.Talkativeness, &fishTalk); err != nil {
			return nil, err
		}
		a.ThinkingEnabled = thinking != 0
		a.Enabled = enabled != 0
		a.Joined = joined != 0
		a.ActorEnabled = actor != 0
		a.FishingTalkEnabled = fishTalk != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// AIAgentByContactUID 按联系人 UID 找已接入的 Agent（19/1 分流与资料卡用）。
func (d *DB) AIAgentByContactUID(ctx context.Context, uid int64) (*AIAgent, error) {
	row := d.SQL.QueryRowContext(ctx, `
		SELECT key, display, role, contact_uid, prompt, tools_json,
		       thinking_enabled, thinking_effort, enabled, joined, actor_enabled,
		       COALESCE(reply_mode,'social'), COALESCE(talkativeness,50), COALESCE(fishing_talk_enabled,0)
		FROM ai_agent WHERE contact_uid=? AND enabled=1 AND joined=1 AND deleted_ts=0`, uid)
	var a AIAgent
	var thinking, enabled, joined, actor, fishTalk int
	if err := row.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt, &a.ToolsJSON,
		&thinking, &a.ThinkingEffort, &enabled, &joined, &actor, &a.ReplyMode, &a.Talkativeness, &fishTalk); err != nil {
		return nil, err
	}
	a.ThinkingEnabled = thinking != 0
	a.Enabled = enabled != 0
	a.Joined = joined != 0
	a.ActorEnabled = actor != 0
	a.FishingTalkEnabled = fishTalk != 0
	return &a, nil
}

// AINextContactUID 从 2147483640 向下找一个没被占用的 AI 联系人 UID。
func (d *DB) AINextContactUID(ctx context.Context) (int64, error) {
	used := map[int64]bool{}
	rows, err := d.SQL.QueryContext(ctx, `SELECT contact_uid FROM ai_agent`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var u int64
		if rows.Scan(&u) == nil {
			used[u] = true
		}
	}
	_ = rows.Close()
	for u := int64(2147483640); u > aiContactUIDMin; u-- {
		if !used[u] {
			return u, nil
		}
	}
	return 0, errors.New("ai: 没有可用的联系人 UID")
}

// ErrAIAgentExists 表示这个 key 已经有一个在用的 Agent。
var ErrAIAgentExists = errors.New("ai: agent key 已存在")

// ErrAIContactUIDUsed 表示指定的联系人 UID 已经被占用。
var ErrAIContactUIDUsed = errors.New("ai: contact_uid 已被占用")

// ErrAIContactUIDInvalid 表示联系人 UID 不在虚拟联系人专用段内。
// 允许管理员改 UID，但不能写进真实玩家号段，否则 19/1 私聊会被错误分流。
var ErrAIContactUIDInvalid = errors.New("ai: contact_uid 必须在 2147480000-2147483645")

const (
	aiContactUIDMin int64 = 2147480000
	aiContactUIDMax int64 = 2147483645
)

func validAIContactUID(uid int64) bool { return uid >= aiContactUIDMin && uid <= aiContactUIDMax }

// aiContactUIDUsed 判断联系人 UID 是否已被别的 Agent（含逻辑删除的）占用。
func (d *DB) aiContactUIDUsed(ctx context.Context, uid, exceptID int64) (bool, error) {
	var n int
	if err := d.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ai_agent WHERE contact_uid=? AND id<>?`, uid, exceptID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// AICreateAgent 后台新建 Agent，默认不接入游戏。
// 如果这个 key 之前被逻辑删除过，则恢复原记录并复用联系人 UID：
// 私聊对象与历史因此不会断，也不会把旧 UID 分配给别的新角色。
func (d *DB) AICreateAgent(ctx context.Context, a AIAgent, toolsJSON string) (int64, bool, error) {
	thinking := 0
	if a.ThinkingEnabled {
		thinking = 1
	}
	mode := a.ReplyMode
	if mode != "mention" && mode != "silent" {
		mode = "social"
	}
	talk := a.Talkativeness
	if talk < 0 || talk > 100 {
		talk = 50
	}

	var id, oldUID int64
	var deletedTS float64
	err := d.SQL.QueryRowContext(ctx,
		`SELECT id, contact_uid, deleted_ts FROM ai_agent WHERE key=?`, a.Key).
		Scan(&id, &oldUID, &deletedTS)
	switch {
	case err == nil && deletedTS == 0:
		return 0, false, ErrAIAgentExists
	case err == nil:
		// 恢复：保留联系人 UID、战绩与历史；角色配置按这次填的覆盖。
		uid := oldUID
		if a.ContactUID > 0 {
			if !validAIContactUID(a.ContactUID) {
				return 0, false, ErrAIContactUIDInvalid
			}
			used, checkErr := d.aiContactUIDUsed(ctx, a.ContactUID, id)
			if checkErr != nil {
				return 0, false, checkErr
			}
			if used {
				return 0, false, ErrAIContactUIDUsed
			}
			uid = a.ContactUID
		}
		_, upErr := d.SQL.ExecContext(ctx, `
			UPDATE ai_agent SET display=?, role=?, contact_uid=?, prompt=?, tools_json=?,
			  thinking_enabled=?, thinking_effort=?, enabled=1, joined=0, actor_enabled=0,
			  reply_mode=?, talkativeness=?, deleted_ts=0
			WHERE id=?`,
			a.Display, a.Role, uid, a.Prompt, toolsJSON,
			thinking, a.ThinkingEffort, mode, talk, id)
		if upErr != nil {
			return 0, false, upErr
		}
		// 恢复身份：control 行保留单调代次，只解除墓碑；禁止删除后代次归零。
		if _, err := d.SQL.ExecContext(ctx, `
			INSERT OR IGNORE INTO ai_runtime_control(agent_key, generation, revision, tombstoned)
			VALUES(?,1,0,0)`, a.Key); err != nil {
			return 0, false, err
		}
		if _, err := d.SQL.ExecContext(ctx,
			`UPDATE ai_runtime_control SET tombstoned=0 WHERE agent_key=?`, a.Key); err != nil {
			return 0, false, err
		}
		return uid, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, err
	}

	uid := a.ContactUID
	if uid == 0 {
		uid, err = d.AINextContactUID(ctx)
		if err != nil {
			return 0, false, err
		}
	} else {
		if !validAIContactUID(uid) {
			return 0, false, ErrAIContactUIDInvalid
		}
		used, checkErr := d.aiContactUIDUsed(ctx, uid, 0)
		if checkErr != nil {
			return 0, false, checkErr
		}
		if used {
			return 0, false, ErrAIContactUIDUsed
		}
	}
	_, err = d.SQL.ExecContext(ctx, `
		INSERT INTO ai_agent(key, display, role, contact_uid, prompt, tools_json,
		                     thinking_enabled, thinking_effort, enabled, joined,
		                     reply_mode, talkativeness, deleted_ts)
		VALUES(?,?,?,?,?,?,?,?,1,0,?,?,0)`,
		a.Key, a.Display, a.Role, uid, a.Prompt, toolsJSON,
		thinking, a.ThinkingEffort, mode, talk)
	if err != nil {
		return 0, false, err
	}
	// 新 Agent 首次接管：generation=1、revision=0；已存在则不动（幂等）。
	if _, err := d.SQL.ExecContext(ctx, `
		INSERT OR IGNORE INTO ai_runtime_control(agent_key, generation, revision, tombstoned)
		VALUES(?,1,0,0)`, a.Key); err != nil {
		return 0, false, err
	}
	return uid, false, nil
}

// AISetAgentBehavior 设置频道里的说话方式与活跃度。
func (d *DB) AISetAgentBehavior(ctx context.Context, key, mode string, talk int) error {
	if mode != "social" && mode != "mention" && mode != "silent" {
		return errors.New("ai: 未知的 reply_mode")
	}
	if talk < 0 {
		talk = 0
	}
	if talk > 100 {
		talk = 100
	}
	_, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_agent SET reply_mode=?, talkativeness=? WHERE key=? AND deleted_ts=0`, mode, talk, key)
	return err
}

// AISetAgentFishingTalk 设置角色是否允许「AI 自己上鱼后主动报一句」。
// 全局范围只决定在哪能见到它，这个开关决定它能不能报鱼。
func (d *DB) AISetAgentFishingTalk(ctx context.Context, key string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_agent SET fishing_talk_enabled=? WHERE key=? AND deleted_ts=0`, v, key)
	return err
}

// AISetAgentJoined 接入/移出游戏；接入时可同时改联系人 UID。
func (d *DB) AISetAgentJoined(ctx context.Context, key string, joined bool, contactUID int64) error {
	j := 0
	if joined {
		j = 1
	}
	if contactUID != 0 && !validAIContactUID(contactUID) {
		return ErrAIContactUIDInvalid
	}
	if contactUID > 0 {
		_, err := d.SQL.ExecContext(ctx,
			`UPDATE ai_agent SET joined=?, contact_uid=? WHERE key=? AND deleted_ts=0`, j, contactUID, key)
		return err
	}
	_, err := d.SQL.ExecContext(ctx, `UPDATE ai_agent SET joined=? WHERE key=? AND deleted_ts=0`, j, key)
	return err
}

// AIUpdateAgent 改角色、提示词与展示名。
func (d *DB) AIUpdateAgent(ctx context.Context, key, display, role, prompt, toolsJSON string) error {
	_, err := d.SQL.ExecContext(ctx, `
		UPDATE ai_agent SET display=?, role=?, prompt=?, tools_json=? WHERE key=? AND deleted_ts=0`,
		display, role, prompt, toolsJSON, key)
	return err
}

// AIDeleteAgent 逻辑删除：删的是账号，不删聊天对象与历史。
// 保留联系人 UID、ai_result 与 ai_memory，之后用同一个 key 新建可恢复；
// 同时清掉运行态、频道订阅与尚未投递的旧消息，避免污染新角色。
// 生产路径由 GS 在运行态事务里调用 aiSoftDeleteAgent（同一事务还要 generation++、
// tombstone）；这个 DB 方法只保留给既有存储层测试与无事务场景。
func (d *DB) AIDeleteAgent(ctx context.Context, key string) error {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := aiSoftDeleteAgent(ctx, tx, key); err != nil {
		return err
	}
	return tx.Commit()
}

// aiSoftDeleteAgent 是逻辑删除 + 运行态级联的唯一实现；调用方负责事务与
// control 的 generation/tombstone 推进。返回 false 表示此前已删除（幂等）。
func aiSoftDeleteAgent(ctx context.Context, q AIQueryer, key string) (bool, error) {
	var agentID, uid int64
	if err := q.QueryRowContext(ctx,
		`SELECT id, contact_uid FROM ai_agent WHERE key=? AND deleted_ts=0`, key).
		Scan(&agentID, &uid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // 已经删过：保持幂等
		}
		return false, err
	}
	if _, err := q.ExecContext(ctx,
		`UPDATE ai_agent SET deleted_ts=?, enabled=0, joined=0, actor_enabled=0 WHERE id=?`,
		nowTS(), agentID); err != nil {
		return false, err
	}
	if err := deleteAIActor(ctx, q, key); err != nil {
		return false, err
	}
	if _, err := q.ExecContext(ctx, `UPDATE ai_channel_agent SET enabled=0 WHERE agent_id=?`, agentID); err != nil {
		return false, err
	}
	if _, err := q.ExecContext(ctx,
		`UPDATE ai_task SET state='fallback' WHERE agent=? AND state IN ('pending','processing')`,
		key); err != nil {
		return false, err
	}
	if uid > 0 {
		// 旧回复不再投递（delivered_ts=-1 表示作废），但记录本身保留。
		if _, err := q.ExecContext(ctx,
			`UPDATE ai_result SET delivered_ts=-1 WHERE delivered_ts IS NULL AND contact_uid=?`,
			uid); err != nil {
			return false, err
		}
	}
	return true, nil
}

// AIAddChannelResult 为圆桌追加一条频道发言（保留入口：内部按当前 processing
// 尝试绑定）。任务已过期/已被回收重领时，迟到的发言不再入库。
//
// 不写表达证据：调用方没有证明本句实际用了哪一版四维。
func (d *DB) AIAddChannelResult(ctx context.Context, taskID, contactUID int64, text string) error {
	attempt, err := d.aiProcessingAttempt(ctx, taskID)
	if err != nil {
		return err
	}
	return d.AIAddChannelResultWithExpression(ctx, taskID, attempt, contactUID, text, nil)
}

// AIAddChannelResultWithExpression 是圆桌的写发言入口：attempt 必须是本次
// 认领后的尝试号；expr 非 nil 时与 ai_result 同事务写一行 ai_result_expression，
// 按本句实际 contact_uid 对应的发言者核对（不是按任务参与者顺序推断）。
func (d *DB) AIAddChannelResultWithExpression(ctx context.Context, taskID int64, attempt int,
	contactUID int64, text string, expr *AIResultExpression) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if err := validateAIResultExpression("done", text, expr); err != nil {
		return err
	}
	return d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		epoch, err := AITaskEpoch(ctx, tx.SQL(), taskID)
		if err != nil {
			return err
		}
		if err := aiRequireIntent(ctx, tx.SQL(), epoch); err != nil {
			return err
		}
		res, err := tx.SQL().ExecContext(ctx, `
			INSERT INTO ai_result(task_id,status,reply_text,raw_usage,err,created_ts,contact_uid)
			SELECT ?,'done',?,'','',?,?
			WHERE EXISTS (SELECT 1 FROM ai_task WHERE id=? AND state='processing' AND attempt=?)`,
			taskID, text, nowTS(), contactUID, taskID, attempt)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrAITaskNotActive
		}
		if expr != nil {
			resultID, err := res.LastInsertId()
			if err != nil {
				return err
			}
			if err := insertAIResultExpression(ctx, tx.SQL(), resultID, *expr); err != nil {
				return err
			}
		}
		return nil
	})
}

// AIFinishTask 只改任务状态，不额外插结果（圆桌已逐条写过结果）。
// 保留入口：内部按当前 processing 尝试绑定，只更新本次尝试的 processing 任务。
func (d *DB) AIFinishTask(ctx context.Context, id int64, state string) error {
	attempt, err := d.aiProcessingAttempt(ctx, id)
	if err != nil {
		return err
	}
	return d.AIFinishTaskWithReason(ctx, id, attempt, state, "")
}

// AIFinishTaskWithReason 与 AIFinishTask 相同，但按本次认领的 attempt 限定：
// 迟到的结束（回收后由旧尝试调用）不会改掉新尝试的任务状态，只会返回
// ErrAITaskNotActive。任务没有可投递结果时额外落一行带原因的空结果，
// 回执因此能列出「任务、结果、原因」。
func (d *DB) AIFinishTaskWithReason(ctx context.Context, id int64, attempt int, state, reason string) error {
	return d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		epoch, err := AITaskEpoch(ctx, tx.SQL(), id)
		if err != nil {
			return err
		}
		intentReason, err := aiCheckIntent(ctx, tx.SQL(), epoch)
		if err != nil {
			return err
		}
		if intentReason != "" {
			state = "fallback"
			if reason == "" {
				reason = intentReason
			}
		}
		res, err := tx.SQL().ExecContext(ctx,
			`UPDATE ai_task SET state=?, text='' WHERE id=? AND state='processing' AND attempt=?`,
			state, id, attempt)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrAITaskNotActive
		}
		if reason == "" {
			return nil
		}
		_, err = tx.SQL().ExecContext(ctx, `INSERT INTO ai_result
			(task_id,status,reply_text,raw_usage,err,created_ts,delivered_ts)
			SELECT ?,'fallback','','',?,?,-1
			WHERE NOT EXISTS (SELECT 1 FROM ai_result WHERE task_id=?)`,
			id, reason, nowTS(), id)
		return err
	})
}

// AIEnqueueSystemRoundtable 只允许系统创建圆桌任务（AI 上鱼后的庆祝圆桌）。
// 普通入队仍然只接受 source_kind=human，Agent 输出无法借此回流。
//
// ev 是这条事件的受信证据（类别 + 逐角色能力）；nil 表示调用方没有给出类别，
// 消费端会按「类别不可证」终止，不再退回「只要还持有某个事件能力」。
func (d *DB) AIEnqueueSystemRoundtable(ctx context.Context, channel string, uid int64, text string,
	participants []string, ev *AITaskEvidence) (int64, error) {
	if len(participants) < 2 {
		return 0, errors.New("ai: 圆桌至少需要 2 个参与者")
	}
	parts, err := json.Marshal(participants)
	if err != nil {
		return 0, err
	}
	return d.aiEnqueueTask(ctx, AITask{
		Kind: "roundtable", Scope: "channel", ScopeKey: channel, UID: uid,
		SourceKind: "system", Text: text, Participants: string(parts), DeadlineTS: nowTS() + 180,
	}, ev)
}

// AIMemory 是一条结构化战绩记忆（星/蓝），不占对话历史。
type AIMemory struct {
	ID        int64
	Kind      string
	FishKey   string
	FishName  string
	WeightG   int64
	Loc       string
	CreatedTS float64
}

// AIAddMemory 写入一条战绩。
func (d *DB) AIAddMemory(ctx context.Context, agentKey, kind, fishKey, fishName string, weightG int64, loc string) error {
	return addAIMemory(ctx, d.SQL, agentKey, kind, fishKey, fishName, weightG, loc)
}

// addAIMemory 是战绩写入的唯一 SQL；GS 在上鱼事务里直接调用。
func addAIMemory(ctx context.Context, q AIQueryer, agentKey, kind, fishKey, fishName string, weightG int64, loc string) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO ai_memory(agent_key,kind,fish_key,fish_name,weight_g,loc,created_ts)
		VALUES(?,?,?,?,?,?,?)`,
		agentKey, kind, fishKey, fishName, weightG, loc, nowTS())
	return err
}

// AIRecentMemories 最近 N 条。
func (d *DB) AIRecentMemories(ctx context.Context, agentKey string, limit int) ([]AIMemory, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.aiMemories(ctx, `
		SELECT id,kind,fish_key,fish_name,weight_g,loc,created_ts
		FROM ai_memory WHERE agent_key=? ORDER BY id DESC LIMIT ?`, agentKey, limit)
}

// AIBestMemories 最佳 N 条：蓝冠优先，其次按重量。
func (d *DB) AIBestMemories(ctx context.Context, agentKey string, limit int) ([]AIMemory, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.aiMemories(ctx, `
		SELECT id,kind,fish_key,fish_name,weight_g,loc,created_ts
		FROM ai_memory WHERE agent_key=?
		ORDER BY CASE WHEN kind='super' THEN 0 ELSE 1 END, weight_g DESC, id DESC LIMIT ?`, agentKey, limit)
}

func (d *DB) aiMemories(ctx context.Context, query string, args ...any) ([]AIMemory, error) {
	rows, err := d.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIMemory
	for rows.Next() {
		var m AIMemory
		if err := rows.Scan(&m.ID, &m.Kind, &m.FishKey, &m.FishName, &m.WeightG, &m.Loc, &m.CreatedTS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AIPruneMemories 只保留最近 keep 条。
func (d *DB) AIPruneMemories(ctx context.Context, agentKey string, keep int) error {
	return pruneAIMemories(ctx, d.SQL, agentKey, keep)
}

func pruneAIMemories(ctx context.Context, q AIQueryer, agentKey string, keep int) error {
	if keep <= 0 {
		keep = 50
	}
	_, err := q.ExecContext(ctx, `
		DELETE FROM ai_memory WHERE agent_key=? AND id NOT IN (
			SELECT id FROM ai_memory WHERE agent_key=? ORDER BY id DESC LIMIT ?
		)`, agentKey, agentKey, keep)
	return err
}

// AIClearMemories 清空某 Agent 的全部战绩。
func (d *DB) AIClearMemories(ctx context.Context, agentKey string) error {
	return clearAIMemories(ctx, d.SQL, agentKey)
}

func clearAIMemories(ctx context.Context, q AIQueryer, agentKey string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM ai_memory WHERE agent_key=?`, agentKey)
	return err
}

// AIChannelInfo 是后台频道下拉的一项。
type AIChannelInfo struct {
	Name      string
	Label     string
	Count     int
	Agents    []string
	AgentKeys []string
}

// AIMapInfo 是后台地图级订阅下拉的一项。
type AIMapInfo struct {
	MapKey    string
	Label     string
	Count     int
	Agents    []string
	AgentKeys []string
}

// AIUpsertChannel 记录客户端实际用过的频道名（切频道时调用）。
func (d *DB) AIUpsertChannel(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	_, err := d.SQL.ExecContext(ctx, `INSERT OR IGNORE INTO ai_channel(name) VALUES(?)`, name)
	return err
}

// AIListChannels 返回频道 + 显示名 + 已接入 Agent 数/名单。
// 频道来源 = ai_channel ∪ ai_channel_agent，兼容历史数据。
func (d *DB) AIListChannels(ctx context.Context) ([]AIChannelInfo, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT ch.name, COALESCE(c.label,''), ag.key, ag.display
		FROM (SELECT name FROM ai_channel UNION SELECT channel FROM ai_channel_agent) ch
		LEFT JOIN ai_channel c ON c.name=ch.name
		LEFT JOIN ai_channel_agent a ON a.channel=ch.name AND a.enabled=1
		LEFT JOIN ai_agent ag ON ag.id=a.agent_id AND ag.enabled=1 AND ag.deleted_ts=0
		ORDER BY ch.name, a.agent_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	order := make([]string, 0, 8)
	byName := map[string]*AIChannelInfo{}
	for rows.Next() {
		var name, label string
		var key, display sql.NullString
		if err := rows.Scan(&name, &label, &key, &display); err != nil {
			return nil, err
		}
		info, ok := byName[name]
		if !ok {
			info = &AIChannelInfo{Name: name, Label: label}
			byName[name] = info
			order = append(order, name)
		}
		if key.Valid {
			info.AgentKeys = append(info.AgentKeys, key.String)
		}
		if display.Valid && display.String != "" {
			info.Agents = append(info.Agents, display.String)
			info.Count++
		}
	}
	out := make([]AIChannelInfo, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, rows.Err()
}

// AISetChannelLabel 设置频道显示名；空字符串表示恢复显示原始 name。
func (d *DB) AISetChannelLabel(ctx context.Context, name, label string) error {
	_, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_channel(name, label) VALUES(?,?)
		ON CONFLICT(name) DO UPDATE SET label=excluded.label`, name, label)
	return err
}

// AIChannelCooldownMS 取频道内 Agent 的最小冷却（毫秒）。
func (d *DB) AIChannelCooldownMS(ctx context.Context, channel string) int {
	var v int
	if err := d.SQL.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(cooldown_ms),8000) FROM ai_channel_agent
		WHERE channel=? AND enabled=1`, channel).Scan(&v); err != nil || v <= 0 {
		return 8000
	}
	return v
}

// ChannelMapKey 把频道名解析成它所属的地图 key。
//
// 官服频道名形如 nav<地图序号>_<图内区域序号>：nav 前缀的数字就是该地图
// level_0NN_xxx 的 NN。实测 13 张有 nav 数组的图全部命中，无需映射表。
// 补零不统一（nav1 与 nav03 都存在），所以按数字解析而不是按固定宽度切串。
//
// 解析不出地图时返回 ("", false)：调用方降级为「精确订阅 ∪ 全局」，
// 不报错、不静默丢弃。私人频道（玩家自建，名字不遵循 nav 命名）也走这条路。
func (d *DB) ChannelMapKey(ctx context.Context, channel string) (string, bool) {
	channel = strings.TrimSpace(channel)
	if !strings.HasPrefix(channel, "nav") {
		return "", false
	}
	rest := channel[len("nav"):]
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return "", false
	}
	num, err := strconv.Atoi(rest[:i])
	if err != nil || num <= 0 {
		return "", false
	}
	// level_%03d_ 前缀在地图清单里唯一，loc_alias 的 alias 是 level key。
	// 注意 LIKE 里的下划线是单字符通配符，要用 ESCAPE 转义，否则 level_001_
	// 会匹配不到 level_001_lake（缺 % 时）或误匹配别的 key。
	var key string
	err = d.SQL.QueryRowContext(ctx, `
		SELECT alias FROM loc_alias
		WHERE alias LIKE ? ESCAPE '\' ORDER BY alias LIMIT 1`,
		"level\\_"+fmt.Sprintf("%03d", num)+"\\_%").Scan(&key)
	if err != nil || key == "" {
		return "", false
	}
	return key, true
}

// aiChannelAgentRows 是三源并集查询共用的列清单。
const aiChannelAgentRows = `a.key, a.display, COALESCE(a.role,''), a.contact_uid, a.prompt,
	       COALESCE(a.tools_json,'[]'), COALESCE(a.thinking_enabled,0),
	       COALESCE(a.thinking_effort,''), COALESCE(a.enabled,1), COALESCE(a.joined,0),
	       COALESCE(a.actor_enabled,0), COALESCE(a.reply_mode,'social'), COALESCE(a.talkativeness,50), COALESCE(a.fishing_talk_enabled,0)`

// AIMapKeyGlobal 是全局范围的 map_key：在这个范围里的 Agent 在任何频道都能发言。
// 它是「逐个添加」出来的 —— 加进这个范围的才常驻，不加就还是只在订阅的地图/频道出现。
const AIMapKeyGlobal = "*"

// AIChannelAgents 返回该频道可发言的全部 Agent。
//
// 可见性 = 地图级订阅 ∪ 精确频道订阅 ∪ 全局范围订阅。
// 官服一张图有十几个到上百个区域频道，只按精确频道订阅时玩家在图内切区域
// 就会失聪，所以地图级订阅是主路径，精确订阅保留为高级用法。
func (d *DB) AIChannelAgents(ctx context.Context, channel string) ([]AIAgent, error) {
	mapKey, hasMap := d.ChannelMapKey(ctx, channel)
	// 无地图时把 map_key 参数置空串：ai_map_agent 里不会有空 map_key，
	// 因此地图级来源自然为空，只剩精确订阅与全局范围（私人频道走的就是这条路）。
	mapArg := ""
	if hasMap {
		mapArg = mapKey
	}

	rows, err := d.SQL.QueryContext(ctx, `
		SELECT `+aiChannelAgentRows+`
		FROM (
			SELECT a.id, a.key, c.priority FROM ai_channel_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.channel=? AND c.enabled=1
			UNION
			SELECT a.id, a.key, c.priority FROM ai_map_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.map_key=? AND c.enabled=1
			UNION
			SELECT a.id, a.key, c.priority FROM ai_map_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.map_key=? AND c.enabled=1
		) src
		JOIN ai_agent a ON a.id=src.id AND a.enabled=1 AND a.deleted_ts=0
		ORDER BY src.priority, a.id`, channel, mapArg, AIMapKeyGlobal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIAgent
	for rows.Next() {
		var a AIAgent
		var te, en, joined, actor, fishTalk int
		if err := rows.Scan(&a.Key, &a.Display, &a.Role, &a.ContactUID, &a.Prompt,
			&a.ToolsJSON, &te, &a.ThinkingEffort, &en, &joined, &actor,
			&a.ReplyMode, &a.Talkativeness, &fishTalk); err != nil {
			return nil, err
		}
		a.ThinkingEnabled = te != 0
		a.Enabled = en != 0
		a.Joined = joined != 0
		a.ActorEnabled = actor != 0
		a.FishingTalkEnabled = fishTalk != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AIHasNewerHumanTask 判断圆桌期间是否有新的真人消息（人类优先，立即中止）。
func (d *DB) AIHasNewerHumanTask(ctx context.Context, afterID int64) (bool, error) {
	var n int
	err := d.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ai_task
		WHERE id>? AND state IN ('pending','processing') AND source_kind='human'`, afterID).Scan(&n)
	return n > 0, err
}

// AISetting 读一个全局开关；缺失时返回 def。
func (d *DB) AISetting(ctx context.Context, key, def string) string {
	var v string
	if err := d.SQL.QueryRowContext(ctx, `SELECT value FROM ai_setting WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (d *DB) AIWebSearchSettings(ctx context.Context) (aiconfig.WebSearchSettings, error) {
	var cfg aiconfig.WebSearchSettings
	var enabled string
	err := d.SQL.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT value FROM ai_setting WHERE key=?),''),
		COALESCE((SELECT value FROM ai_setting WHERE key=?),''),
		COALESCE((SELECT value FROM ai_setting WHERE key=?),'')`,
		aiconfig.KeyWebSearchEnabled, aiconfig.KeySearchMode, aiconfig.KeySearchEndpoint).
		Scan(&enabled, &cfg.Mode, &cfg.Endpoint)
	cfg.Enabled = enabled == "1"
	return cfg, err
}

// AIRecordNativeSearch keeps the latest confirmation across concurrent tasks.
func (d *DB) AIRecordNativeSearch(ctx context.Context, evidence aiconfig.NativeSearchEvidence) error {
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = d.SQL.ExecContext(ctx, `INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts
		WHERE CASE WHEN json_valid(ai_setting.value) THEN
			COALESCE(json_extract(ai_setting.value,'$.revision'),0) < ? OR
			(json_extract(ai_setting.value,'$.revision') = ? AND COALESCE(json_extract(ai_setting.value,'$.confirmed_at'),0) <= ?)
		ELSE 1 END`, aiconfig.KeyNativeSearchEvidence, string(raw), nowTS(),
		evidence.Revision, evidence.Revision, evidence.ConfirmedAt)
	return err
}

// aiEmergencySelect may be extended with additional columns from the same snapshot.
const aiEmergencySelect = `SELECT
	COALESCE((SELECT value FROM ai_setting WHERE key='emergency_stop'),''),
	COALESCE((SELECT value FROM ai_setting WHERE key='emergency_stop_lifted_ts'),''),
	COALESCE((SELECT value FROM ai_setting WHERE key='emergency_epoch_record'),'')`

func ReadAIEmergency(ctx context.Context, q AIQueryer) aiconfig.EmergencyReading {
	var r aiconfig.EmergencyReading
	var raw string
	r.Err = q.QueryRowContext(ctx, aiEmergencySelect).Scan(&r.StopRaw, &r.LiftedRaw, &raw)
	parseAIEmergencyEpoch(raw, &r)
	return r
}

func parseAIEmergencyEpoch(raw string, r *aiconfig.EmergencyReading) {
	var record struct {
		Version int64 `json:"version"`
		Epoch   int64 `json:"epoch"`
	}
	if json.Unmarshal([]byte(raw), &record) == nil {
		r.Version, r.Epoch = record.Version, record.Epoch
	}
}

func (d *DB) AIEmergencyState(ctx context.Context) aiconfig.EmergencyReading {
	return ReadAIEmergency(ctx, d.SQL)
}

// AIGateConfiguration validates the emergency, revision and exact reference in one read snapshot.
func (d *DB) AIGateConfiguration(ctx context.Context) (aiconfig.EmergencyReading, AIConfigSnapshot, error) {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return aiconfig.EmergencyReading{}, AIConfigSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	r := ReadAIEmergency(ctx, tx)
	if r.Err != nil {
		return r, AIConfigSnapshot{}, r.Err
	}
	snap, err := aiConfigSnapshotTx(ctx, tx)
	return r, snap, err
}

func (d *DB) AISearchGate(ctx context.Context) (aiconfig.EmergencyReading, bool, error) {
	var r aiconfig.EmergencyReading
	var epochRaw string
	var enabled bool
	err := d.SQL.QueryRowContext(ctx, aiEmergencySelect+`,
		COALESCE((SELECT value FROM ai_setting WHERE key='web_search_enabled'),'0')='1'`).
		Scan(&r.StopRaw, &r.LiftedRaw, &epochRaw, &enabled)
	parseAIEmergencyEpoch(epochRaw, &r)
	return r, enabled, err
}

func readAIToolGate(ctx context.Context, q AIQueryer, jobID int64) (aiconfig.EmergencyReading, int64, error) {
	var r aiconfig.EmergencyReading
	var raw string
	var epoch int64
	err := q.QueryRowContext(ctx, aiEmergencySelect+`,
		COALESCE((SELECT t.intent_epoch FROM ai_tool_job j JOIN ai_task t ON t.id=j.task_id
			WHERE j.id=? AND j.state='processing' AND t.state='processing'
			AND typeof(t.intent_epoch)='integer'),0)`, jobID).
		Scan(&r.StopRaw, &r.LiftedRaw, &raw, &epoch)
	parseAIEmergencyEpoch(raw, &r)
	return r, epoch, err
}

type aiIntentEpochKey struct{}

// AIIntentContext carries a trusted binding from a server entry point.
func AIIntentContext(ctx context.Context, epoch int64) context.Context {
	return context.WithValue(ctx, aiIntentEpochKey{}, epoch)
}

func AIIntentEpoch(ctx context.Context) (int64, bool) {
	epoch, bound := ctx.Value(aiIntentEpochKey{}).(int64)
	return epoch, bound
}

func aiCaptureIntent(ctx context.Context, d *DB) context.Context {
	if _, bound := AIIntentEpoch(ctx); bound {
		return ctx
	}
	dec := aiconfig.EvaluateEmergency(d.AIEmergencyState(ctx))
	epoch := int64(0)
	if blocked, _ := dec.RecordBlocked(); !blocked {
		epoch = dec.Epoch
	}
	return AIIntentContext(ctx, epoch)
}

var ErrAIIntentInactive = errors.New("ai: 原意图已失效")

func aiCheckIntent(ctx context.Context, q AIQueryer, epoch int64) (string, error) {
	r := ReadAIEmergency(ctx, q)
	if r.Err != nil {
		return "", r.Err
	}
	_, reason := aiconfig.EvaluateEmergency(r).IntentActive(epoch)
	return reason, nil
}

func aiRequireIntent(ctx context.Context, q AIQueryer, epoch int64) error {
	reason, err := aiCheckIntent(ctx, q, epoch)
	if err != nil {
		return err
	}
	if reason != "" {
		return fmt.Errorf("%w: %s", ErrAIIntentInactive, reason)
	}
	return nil
}

func AITaskEpoch(ctx context.Context, q AIQueryer, id int64) (int64, error) {
	var epoch int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(
		(SELECT intent_epoch FROM ai_task WHERE id=? AND typeof(intent_epoch)='integer'), 0)`, id).Scan(&epoch)
	return epoch, err
}

func (d *DB) AISetSetting(ctx context.Context, key, value string) error {
	return d.AISetSettings(ctx, map[string]string{key: value})
}

func (d *DB) AISetSettings(ctx context.Context, kv map[string]string) error {
	return d.aiSetSettingsTx(ctx, kv, nil, false, "", nil)
}

var ErrAIEmergencyConflict = errors.New("ai: 急停状态已更改，请刷新")

// AISetAdminSettings is the authenticated settings writer. Switch transitions
// compare the epoch under the write lock, then increment and audit atomically.
func (d *DB) AISetAdminSettings(ctx context.Context, kv map[string]string, seen *int64,
	actor string, rec *aiconfig.ConfigRecord) error {
	return d.aiSetSettingsTx(ctx, kv, seen, true, actor, rec)
}

func (d *DB) aiSetSettingsTx(ctx context.Context, kv map[string]string, seen *int64,
	requireSeen bool, actor string, rec *aiconfig.ConfigRecord) error {
	if len(kv) == 0 && rec == nil {
		return nil
	}
	return d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		q := tx.SQL()
		now := nowTS()
		for k, v := range kv {
			if k == aiconfig.EmergencyEpochKey || k == aiconfig.KeyEmergencyEpochRecord {
				return ErrAIEmergencyConflict
			}
			if k == aiconfig.KeyEmergencyStop {
				r := ReadAIEmergency(ctx, q)
				if r.Err != nil {
					return r.Err
				}
				v = strings.TrimSpace(v)
				if v == "" {
					v = aiconfig.DefaultEmergencyStop
				}
				prev := r.StopRaw
				if prev == "" {
					prev = aiconfig.DefaultEmergencyStop
				}
				if prev == v {
					continue
				}
				if r.Version != 1 || r.Epoch <= 0 || r.Epoch == math.MaxInt64 {
					return ErrAIEmergencyConflict
				}
				if requireSeen && (seen == nil || *seen != r.Epoch) {
					return ErrAIEmergencyConflict
				}
				next := fmt.Sprintf(`{"version":1,"epoch":%d}`, r.Epoch+1)
				if err := aiWriteSetting(ctx, q, aiconfig.KeyEmergencyEpochRecord, next, now); err != nil {
					return err
				}
				if v == aiconfig.DefaultEmergencyStop {
					if err := aiWriteSetting(ctx, q, aiconfig.KeyEmergencyStopLiftedTS, strconv.FormatFloat(now, 'f', 6, 64), now); err != nil {
						return err
					}
				}
				if requireSeen {
					if err := writeAuditLog(ctx, q, actor, "ai_emergency_stop", "global",
						fmt.Sprintf("stop=%s epoch=%d", v, r.Epoch+1)); err != nil {
						return err
					}
				}
			}
			if err := aiWriteSetting(ctx, q, k, v, now); err != nil {
				return err
			}
		}
		if rec != nil {
			raw, err := aiconfig.MarshalConfigRecord(*rec)
			if err != nil {
				return err
			}
			key := fmt.Sprintf("%s%d:%d", aiconfig.KeySearchRecordPrefix, rec.Revision, time.Now().UnixNano())
			_, err = q.ExecContext(ctx, `INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)`, key, raw, now)
			return err
		}
		return nil
	})
}

func aiWriteSetting(ctx context.Context, q AIQueryer, key, value string, ts float64) error {
	_, err := q.ExecContext(ctx, `INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts`, key, value, ts)
	return err
}

// ErrAIModelRowNotFound 表示后台选中的模型行不存在（并发删除或旧库缺行）。
// 调用方必须把这个错误报给用户，不能把「一行都没改」当成保存成功。
var ErrAIModelRowNotFound = errors.New("ai: 模型行不存在")

// AISaveProviderDestination 在一个事务里更新厂商地址/模型并刷新外发目的地快照。
//
// 两步必须同事务：任何一步失败都不能留下「新目的地 + 旧快照」的混合状态。
// 该函数不写 consent：授权只能由独立确认/撤销接口写入；目的地变化后，
// 旧授权在读取时因范围不匹配而失效（aiconfig.EvaluateConsent）。
// baseURL/modelID 为空表示不修改该列；destJSON 总是写入（授权范围以快照为准）。
//
// 模型行必须用后台实际选中的 ai_model.id 精确定位（T-016）：
// 同一 provider 可能有历史种子遗留的空行，按 provider_id 批量改会撞
// UNIQUE(provider_id, model_id)。modelRowID 为 0 或行已不存在时返回
// ErrAIModelRowNotFound，不静默跳过。
func (d *DB) AISaveProviderDestination(ctx context.Context, providerID, modelRowID int64, baseURL, modelID, destJSON string) error {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if strings.TrimSpace(baseURL) != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE ai_provider SET base_url=? WHERE id=?`, strings.TrimSpace(baseURL), providerID); err != nil {
			return err
		}
	}
	if strings.TrimSpace(modelID) != "" {
		if modelRowID <= 0 {
			return ErrAIModelRowNotFound
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE ai_model SET model_id=? WHERE id=? AND provider_id=?`,
			strings.TrimSpace(modelID), modelRowID, providerID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrAIModelRowNotFound
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts`,
		aiconfig.KeyEgressDestination, destJSON, nowTS()); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- 配置发布（DEV-114 A / T-102） ----
//
// AD 是唯一配置写者：新 Key 先以不可变密文对象落到外部版本目录，再由这里在
// 一个 BEGIN IMMEDIATE 事务里重验活动修订/确切行 ID/凭据引用，然后同时更新
// 厂商与模型行、活动指针、自动记录、目的地兼容快照与审计。只有提交才生效：
// 任一步失败整笔回滚，读者继续用上一套完整配置。
//
// 读者（Runner / 后台复测 / 状态回读）只按 KeyConfigActive 的同一份快照取
// 地址、模型、修订与凭据引用；本包不解释密文，解密在 internal/aibridge。

var (
	// ErrAIConfigRevisionConflict 表单携带的修订已过期：应刷新后重试。
	ErrAIConfigRevisionConflict = errors.New("ai: 配置修订冲突")
	// ErrAIConfigHistoryConflict 存在发布历史但活动指针缺失/矛盾：锁定，不回退旧文件。
	ErrAIConfigHistoryConflict = errors.New("ai: 配置发布历史与活动指针矛盾")
	// ErrAIConfigSnapshotInvalid 活动指针损坏：锁定。
	ErrAIConfigSnapshotInvalid = errors.New("ai: 活动配置记录无效")
	// ErrAIConfigCredentialRequired 既没有活动凭据引用也没有新 Key。
	ErrAIConfigCredentialRequired = errors.New("ai: 缺少可用凭据")
	// ErrAIConfigSourceChanged 准备时选定的行/来源已被别的写者改掉。
	ErrAIConfigSourceChanged = errors.New("ai: 厂商配置在发布前已变化")
	// ErrAIConfigNoProvider 没有任何启用的厂商/模型行。
	ErrAIConfigNoProvider = errors.New("ai: 没有启用的厂商")
)

// AIConfigSnapshot 是发布前在同一读事务内选定的一套完整配置（不含明文密钥）。
// Revision=0 且 Legacy=true 表示旧安装：尚无活动修订，兼容只读旧一套。
type AIConfigSnapshot struct {
	Revision      int64
	Legacy        bool
	ProviderID    int64
	ProviderName  string
	Protocol      string
	AuthScheme    string
	BaseURL       string
	ConfigJSON    string
	ModelRowID    int64
	ModelID       string
	CredentialRef string
	// Source 是活动指针记录的发布来源（admin_save / migration）；旧安装为空。
	Source string
}

// AIPublishRequest 是一次发布请求。CredentialRef 由调用方在事务外先创建好；
// 这里只做行 ID/修订/来源复核与同事务发布。
type AIPublishRequest struct {
	// ExpectedRevision >0 时要求与当前活动修订完全一致；0 表示旧调用者，
	// 按现有字段串行合并（但仍复核来源，不套用过期的空 Key）。
	ExpectedRevision int64
	// PriorRevision / PriorBaseURL 是准备阶段读到的值，用于并发下判断
	// 「留空 Key 是否仍在同源」。
	PriorRevision int64
	PriorBaseURL  string
	// ProviderID / ModelRowID 是准备阶段选定的确切行。
	ProviderID int64
	ModelRowID int64

	BaseURL string
	ModelID string

	CredentialRef     string
	CredentialChanged bool
	CredentialMasked  string

	Source string // admin_save | migration
	Actor  string
	// Purpose/Contents 是这次发布覆盖的用途与内容类别（自动记录用）。
	Purpose  []string
	Contents []string

	SearchEndpoint string
	SearchChanged  bool

	Now int64
}

// AIConfigPublication 是发布成功后的结果。
type AIConfigPublication struct {
	Revision int64
	Active   aiconfig.ActiveConfig
}

// aiSettingRaw 读一个 ai_setting 原值；缺失 found=false。
// 与 AISetting 不同，它把真实 DB 错误返回给调用方：发布/升级判定不能把
// 「读失败」当成「未初始化」。
func aiSettingRaw(ctx context.Context, q AIQueryer, key string) (value string, found bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT value FROM ai_setting WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// AIConfigActiveRaw 读活动指针原值（含是否存在的真实错误）。
func (d *DB) AIConfigActiveRaw(ctx context.Context) (string, bool, error) {
	return aiSettingRaw(ctx, d.SQL, aiconfig.KeyConfigActive)
}

// AIConfigRecordMax 返回已存在的模型发布记录的最大修订；没有任何记录时 ok=false。
func (d *DB) AIConfigRecordMax(ctx context.Context) (int64, bool, error) {
	rows, err := d.SQL.QueryContext(ctx,
		`SELECT key FROM ai_setting WHERE key LIKE ?`, aiconfig.KeyConfigRecordPrefix+"%")
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var max int64
	found := false
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return 0, false, err
		}
		rev, err := strconv.ParseInt(strings.TrimPrefix(key, aiconfig.KeyConfigRecordPrefix), 10, 64)
		if err != nil {
			// 记录键损坏同样按矛盾处理，由调用方锁定。
			return 0, true, ErrAIConfigHistoryConflict
		}
		if rev > max {
			max = rev
		}
		found = true
	}
	return max, found, rows.Err()
}

// AIConfigRecord 读指定修订的自动记录。
func (d *DB) AIConfigRecord(ctx context.Context, revision int64) (aiconfig.ConfigRecord, bool, error) {
	raw, found, err := aiSettingRaw(ctx, d.SQL, aiconfig.ConfigRecordKey(revision))
	if err != nil || !found {
		return aiconfig.ConfigRecord{}, false, err
	}
	rec, err := aiconfig.ParseConfigRecord(raw)
	if err != nil {
		return aiconfig.ConfigRecord{}, false, err
	}
	return rec, true, nil
}

// AIConfigRecords 按修订倒序列出最近的模型发布记录与搜索去向记录。
func (d *DB) AIConfigRecords(ctx context.Context, limit int) ([]aiconfig.ConfigRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT key, value FROM ai_setting
		WHERE key LIKE ? OR key LIKE ?
		ORDER BY key DESC LIMIT ?`,
		aiconfig.KeyConfigRecordPrefix+"%", aiconfig.KeySearchRecordPrefix+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]aiconfig.ConfigRecord, 0, limit)
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, err
		}
		rec, err := aiconfig.ParseConfigRecord(raw)
		if err != nil {
			continue // 坏记录不阻塞读取；活动指针的完整性由快照校验负责
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// AIConnectionState binds the status view, active configuration and retest to one snapshot.
func (d *DB) AIConnectionState(ctx context.Context) (AIConfigSnapshot, aiconfig.EmergencyReading, aiconfig.RetestStatus, error) {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return AIConfigSnapshot{}, aiconfig.EmergencyReading{Err: err}, aiconfig.RetestStatus{}, err
	}
	defer func() { _ = tx.Rollback() }()
	em := ReadAIEmergency(ctx, tx)
	retest, _, err := aiLatestRetest(ctx, tx)
	if err != nil {
		return AIConfigSnapshot{}, em, aiconfig.RetestStatus{}, err
	}
	snap, err := aiConfigSnapshotTx(ctx, tx)
	return snap, em, retest, err
}

func aiLatestRetest(ctx context.Context, q AIQueryer) (aiconfig.RetestStatus, bool, error) {
	raw, found, err := aiSettingRaw(ctx, q, aiconfig.KeyConfigRetest)
	if err != nil || !found {
		return aiconfig.RetestStatus{}, false, err
	}
	st, ok := aiconfig.ParseRetest(raw)
	return st, ok, nil
}

// AIConfigSnapshot 在一个读事务里选定当前一套配置：
// 有活动指针就只信指针（并校验对应记录存在）；没有指针时只有「从无发布历史」
// 才回退旧安装行，历史与指针矛盾一律返回 ErrAIConfigHistoryConflict。
func (d *DB) AIConfigSnapshot(ctx context.Context) (AIConfigSnapshot, error) {
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return AIConfigSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	return aiConfigSnapshotTx(ctx, tx)
}

func aiConfigSnapshotTx(ctx context.Context, q AIQueryer) (AIConfigSnapshot, error) {
	raw, found, err := aiSettingRaw(ctx, q, aiconfig.KeyConfigActive)
	if err != nil {
		return AIConfigSnapshot{}, err
	}
	if found {
		active, ok := aiconfig.ParseActive(raw)
		if !ok {
			return AIConfigSnapshot{}, ErrAIConfigSnapshotInvalid
		}
		var recExists int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_setting WHERE key=?`,
			aiconfig.ConfigRecordKey(active.Revision)).Scan(&recExists); err != nil {
			return AIConfigSnapshot{}, err
		}
		if recExists == 0 {
			return AIConfigSnapshot{}, ErrAIConfigHistoryConflict
		}
		return AIConfigSnapshot{
			Revision: active.Revision, ProviderID: active.ProviderID,
			ProviderName: active.ProviderName, Protocol: active.Protocol,
			AuthScheme: active.AuthScheme, BaseURL: active.BaseURL,
			ConfigJSON: active.ConfigJSON, ModelRowID: active.ModelRowID,
			ModelID: active.ModelID, CredentialRef: active.CredentialRef,
			Source: active.Source,
		}, nil
	}
	// 没有活动指针：只要有历史记录就是矛盾，不能当旧安装。
	maxRev, hasRecords, err := aiConfigRecordMaxTx(ctx, q)
	if err != nil {
		return AIConfigSnapshot{}, err
	}
	if hasRecords {
		return AIConfigSnapshot{}, fmt.Errorf("%w: 存在记录 %d 但无活动指针", ErrAIConfigHistoryConflict, maxRev)
	}
	snap, err := aiLegacySnapshotTx(ctx, q)
	if err != nil {
		return AIConfigSnapshot{}, err
	}
	snap.Legacy = true
	return snap, nil
}

func aiConfigRecordMaxTx(ctx context.Context, q AIQueryer) (int64, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT key FROM ai_setting WHERE key LIKE ?`,
		aiconfig.KeyConfigRecordPrefix+"%")
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var max int64
	found := false
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return 0, false, err
		}
		rev, perr := strconv.ParseInt(strings.TrimPrefix(key, aiconfig.KeyConfigRecordPrefix), 10, 64)
		if perr != nil {
			return 0, true, ErrAIConfigHistoryConflict
		}
		if rev > max {
			max = rev
		}
		found = true
	}
	return max, found, rows.Err()
}

// aiLegacySnapshotTx 读取旧安装的一套：第一条启用的厂商 + 其第一条启用模型。
// 凭据引用为空，由调用方按「外部凭据文件优先、旧库密文后备」解析。
func aiLegacySnapshotTx(ctx context.Context, q AIQueryer) (AIConfigSnapshot, error) {
	var snap AIConfigSnapshot
	err := q.QueryRowContext(ctx, `
		SELECT p.id, p.name, p.protocol, p.auth_scheme, p.base_url, p.config_json,
		       COALESCE(m.id,0), COALESCE(m.model_id,'')
		FROM ai_provider p
		LEFT JOIN ai_model m ON m.provider_id=p.id AND m.enabled=1
		WHERE p.enabled=1 ORDER BY p.id, m.id LIMIT 1`).
		Scan(&snap.ProviderID, &snap.ProviderName, &snap.Protocol, &snap.AuthScheme,
			&snap.BaseURL, &snap.ConfigJSON, &snap.ModelRowID, &snap.ModelID)
	if errors.Is(err, sql.ErrNoRows) {
		return AIConfigSnapshot{}, ErrAIConfigNoProvider
	}
	if err != nil {
		return AIConfigSnapshot{}, err
	}
	return snap, nil
}

// AIPublishConfig 在一个 BEGIN IMMEDIATE 事务里发布一套配置。
// 事务内重验：活动修订、确切 provider/model 行、凭据引用与来源。
// aiPublishBarrier 仅测试用：发布事务提交前后的进程中断屏障（nil = 生产无操作）。
// 供 §2.2「写 SQL 未提交 / 已提交未回 HTTP」两处终止写进程的验收用例使用。
var aiPublishBarrier func(stage string)

// SetAIPublishBarrierForTest 仅测试用：设置发布屏障，返回恢复函数。
func SetAIPublishBarrierForTest(fn func(stage string)) (restore func()) {
	old := aiPublishBarrier
	aiPublishBarrier = fn
	return func() { aiPublishBarrier = old }
}

func aiPublishStage(stage string) {
	if aiPublishBarrier != nil {
		aiPublishBarrier(stage)
	}
}

func (d *DB) AIPublishConfig(ctx context.Context, req AIPublishRequest) (AIConfigPublication, error) {
	if d == nil || d.SQL == nil {
		return AIConfigPublication{}, errors.New("ai config: 数据库未连接")
	}
	now := req.Now
	if now <= 0 {
		now = int64(nowTS())
	}
	conn, err := d.SQL.Conn(ctx)
	if err != nil {
		return AIConfigPublication{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return AIConfigPublication{}, fmt.Errorf("ai config: 开启发布事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	raw, found, err := aiSettingRaw(ctx, conn, aiconfig.KeyConfigActive)
	if err != nil {
		return AIConfigPublication{}, err
	}
	var active aiconfig.ActiveConfig
	curRev := int64(0)
	if found {
		var ok bool
		active, ok = aiconfig.ParseActive(raw)
		if !ok {
			return AIConfigPublication{}, ErrAIConfigSnapshotInvalid
		}
		curRev = active.Revision
		var recRows int
		if rerr := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_setting WHERE key=?`,
			aiconfig.ConfigRecordKey(curRev)).Scan(&recRows); rerr != nil {
			return AIConfigPublication{}, rerr
		} else if recRows == 0 {
			return AIConfigPublication{}, ErrAIConfigHistoryConflict
		}
	} else {
		if _, has, herr := aiConfigRecordMaxTx(ctx, conn); herr != nil {
			return AIConfigPublication{}, herr
		} else if has {
			return AIConfigPublication{}, ErrAIConfigHistoryConflict
		}
	}
	if req.ExpectedRevision > 0 && req.ExpectedRevision != curRev {
		return AIConfigPublication{}, fmt.Errorf("%w: 表单 %d，当前 %d",
			ErrAIConfigRevisionConflict, req.ExpectedRevision, curRev)
	}

	// 事务内重读确切行：准备阶段选中的 provider/model 行必须仍然存在且一致。
	var curProvider, curModelRow int64
	var curName, curProtocol, curAuth, curBase, curConfig, curModel string
	if err := conn.QueryRowContext(ctx, `
		SELECT p.id, p.name, p.protocol, p.auth_scheme, p.base_url, p.config_json,
		       COALESCE(m.id,0), COALESCE(m.model_id,'')
		FROM ai_provider p
		LEFT JOIN ai_model m ON m.provider_id=p.id AND m.enabled=1
		WHERE p.enabled=1 ORDER BY p.id, m.id LIMIT 1`).
		Scan(&curProvider, &curName, &curProtocol, &curAuth, &curBase, &curConfig,
			&curModelRow, &curModel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AIConfigPublication{}, ErrAIConfigNoProvider
		}
		return AIConfigPublication{}, err
	}
	if req.ProviderID > 0 && req.ProviderID != curProvider {
		return AIConfigPublication{}, fmt.Errorf("%w: 厂商行 %d → %d",
			ErrAIConfigSourceChanged, req.ProviderID, curProvider)
	}
	if req.ModelRowID > 0 && req.ModelRowID != curModelRow {
		return AIConfigPublication{}, fmt.Errorf("%w: 模型行 %d → %d",
			ErrAIConfigSourceChanged, req.ModelRowID, curModelRow)
	}

	targetBase := strings.TrimSpace(req.BaseURL)
	if targetBase == "" {
		targetBase = curBase
	}
	targetModel := strings.TrimSpace(req.ModelID)
	if targetModel == "" {
		targetModel = curModel
	}
	if targetBase == "" || targetModel == "" {
		return AIConfigPublication{}, ErrAIConfigNoProvider
	}
	// 留空 Key：只能沿用当前凭据，且必须与准备阶段仍同源；否则整笔拒绝。
	credRef := strings.TrimSpace(req.CredentialRef)
	if credRef == "" {
		if curRev == 0 || strings.TrimSpace(active.CredentialRef) == "" {
			return AIConfigPublication{}, ErrAIConfigCredentialRequired
		}
		if req.PriorBaseURL != "" && !aiconfig.SameDestinationOrigin(req.PriorBaseURL, curBase) {
			return AIConfigPublication{}, fmt.Errorf("%w: 来源已从 %s 变化",
				ErrAIConfigSourceChanged, aiconfig.DestinationHost(req.PriorBaseURL))
		}
		credRef = active.CredentialRef
	}
	// 旧调用者省略修订且字段与活动套完全一致：按无变更处理，不写第二条记录。
	sameFields := curRev > 0 && targetBase == active.BaseURL && targetModel == active.ModelID &&
		credRef == active.CredentialRef
	if req.ExpectedRevision == 0 && sameFields {
		if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
			return AIConfigPublication{}, err
		}
		committed = true
		return AIConfigPublication{Revision: curRev, Active: active}, nil
	}

	newRev := curRev + 1
	source := strings.TrimSpace(req.Source)
	if source == "" {
		if curRev == 0 {
			source = "migration"
		} else {
			source = "admin_save"
		}
	}
	destHost := aiconfig.DestinationHost(targetBase)
	destDisplay := aiconfig.RedactURLForDisplay(targetBase)
	dest := aiconfig.ConsentDestination{BaseURL: targetBase, ModelID: targetModel}
	destJSON, err := aiconfig.MarshalDestination(dest)
	if err != nil {
		return AIConfigPublication{}, err
	}
	pub := aiconfig.ActiveConfig{
		Schema: aiconfig.ConfigSchemaVersion, Revision: newRev,
		ProviderID: curProvider, ProviderName: curName, Protocol: curProtocol,
		AuthScheme: curAuth, BaseURL: targetBase, ModelRowID: curModelRow,
		ModelID: targetModel, ConfigJSON: curConfig, CredentialRef: credRef,
		Source: source, Actor: strings.TrimSpace(req.Actor), TS: now,
	}
	pubJSON, err := aiconfig.MarshalActive(pub)
	if err != nil {
		return AIConfigPublication{}, err
	}
	rec := aiconfig.ConfigRecord{
		Schema: aiconfig.ConfigSchemaVersion, Revision: newRev, Kind: "model",
		Actor: strings.TrimSpace(req.Actor), Source: source, TS: now,
		Purpose: append([]string(nil), req.Purpose...), Contents: append([]string(nil), req.Contents...),
		DestinationHost: destHost, Destination: destDisplay, Model: targetModel,
		CredentialChanged: req.CredentialChanged,
	}
	recJSON, err := aiconfig.MarshalConfigRecord(rec)
	if err != nil {
		return AIConfigPublication{}, err
	}

	// 1) 厂商地址行：必须正好命中一行。
	res, err := conn.ExecContext(ctx, `UPDATE ai_provider SET base_url=? WHERE id=?`, targetBase, curProvider)
	if err != nil {
		return AIConfigPublication{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return AIConfigPublication{}, fmt.Errorf("%w: 厂商行更新影响 %d 行", ErrAIConfigSourceChanged, n)
	}
	// 2) 模型行：按确切 id + provider_id 更新，影响行数必须为 1。
	res, err = conn.ExecContext(ctx, `UPDATE ai_model SET model_id=? WHERE id=? AND provider_id=?`,
		targetModel, curModelRow, curProvider)
	if err != nil {
		return AIConfigPublication{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return AIConfigPublication{}, ErrAIModelRowNotFound
	}
	// 3) 活动指针（唯一写入点）。
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts`,
		aiconfig.KeyConfigActive, pubJSON, float64(now)); err != nil {
		return AIConfigPublication{}, err
	}
	// 4) 自动记录：同一修订只允许一条，冲突即整笔失败。
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)`,
		aiconfig.ConfigRecordKey(newRev), recJSON, float64(now)); err != nil {
		return AIConfigPublication{}, fmt.Errorf("ai config: 写入发布记录: %w", err)
	}
	// 5) 目的地兼容快照（旧读者可读；不再作为放行条件）。
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts`,
		aiconfig.KeyEgressDestination, destJSON, float64(now)); err != nil {
		return AIConfigPublication{}, err
	}
	// 6) 搜索去向：单独记一行，不与模型凭据共用；同事务写设置本身。
	if req.SearchChanged {
		searchHost := aiconfig.DestinationHost(req.SearchEndpoint)
		searchRec := rec
		searchRec.Kind = "search"
		searchRec.DestinationHost = searchHost
		searchRec.Destination = ""
		searchRec.SearchHost = searchHost
		searchRec.SearchEndpoint = aiconfig.RedactURLForDisplay(req.SearchEndpoint)
		searchRec.Model = ""
		searchRec.CredentialChanged = false
		searchJSON, merr := aiconfig.MarshalConfigRecord(searchRec)
		if merr != nil {
			return AIConfigPublication{}, merr
		}
		key := fmt.Sprintf("%s%d:%d", aiconfig.KeySearchRecordPrefix, newRev, now)
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)`,
			key, searchJSON, float64(now)); err != nil {
			return AIConfigPublication{}, fmt.Errorf("ai config: 写入搜索记录: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts`,
			aiconfig.KeySearchEndpoint, req.SearchEndpoint, float64(now)); err != nil {
			return AIConfigPublication{}, fmt.Errorf("ai config: 写入搜索端点: %w", err)
		}
	}
	// 7) 审计：在同一个事务里写，失败整笔回滚（不把「审计没写」当成成功）。
	detail := fmt.Sprintf("source=%s revision=%d dest=%s model=%s credential_changed=%t purpose=%s",
		source, newRev, destHost, targetModel, req.CredentialChanged, strings.Join(req.Purpose, ","))
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO audit_log(admin, action, target, detail, ts) VALUES(?,?,?,?,?)`,
		strings.TrimSpace(req.Actor), "ai_config", strconv.FormatInt(curProvider, 10), detail,
		float64(now)); err != nil {
		return AIConfigPublication{}, fmt.Errorf("ai config: 写入配置审计: %w", err)
	}
	if req.CredentialChanged {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO audit_log(admin, action, target, detail, ts) VALUES(?,?,?,?,?)`,
			strings.TrimSpace(req.Actor), "ai_key", strconv.FormatInt(curProvider, 10),
			strings.TrimSpace(req.CredentialMasked), float64(now)); err != nil {
			return AIConfigPublication{}, fmt.Errorf("ai config: 写入密钥审计: %w", err)
		}
	}
	aiPublishStage("before_commit")
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return AIConfigPublication{}, fmt.Errorf("ai config: 提交发布: %w", err)
	}
	committed = true
	aiPublishStage("after_commit")
	return AIConfigPublication{Revision: newRev, Active: pub}, nil
}

var ErrAIRetestStale = errors.New("ai: 复测已失效，请重新复测")

// AIBeginRetest reserves the newest attempt together with its active configuration.
func (d *DB) AIBeginRetest(ctx context.Context, st aiconfig.RetestStatus) (AIConfigSnapshot, aiconfig.RetestStatus, error) {
	var snap AIConfigSnapshot
	err := d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		q := tx.SQL()
		if err := aiRequireIntent(ctx, q, st.Epoch); err != nil {
			return err
		}
		var err error
		snap, err = aiConfigSnapshotTx(ctx, q)
		if err != nil {
			return err
		}
		st.Revision, st.Pending = snap.Revision, true
		st.Host, st.Model = aiconfig.DestinationHost(snap.BaseURL), snap.ModelID
		return aiWriteRetest(ctx, q, st)
	})
	return snap, st, err
}

// AIValidateRetest is the final gate before a request is released.
func (d *DB) AIValidateRetest(ctx context.Context, st aiconfig.RetestStatus, ref string) error {
	snap, r, latest, err := d.AIConnectionState(ctx)
	if err != nil {
		return err
	}
	if active, _ := aiconfig.EvaluateEmergency(r).IntentActive(st.Epoch); !active {
		return ErrAIIntentInactive
	}
	return aiMatchRetest(snap, latest, st, ref)
}

// AIRecordRetest completes the attempt under the same lock as settings and publication.
func (d *DB) AIRecordRetest(ctx context.Context, st aiconfig.RetestStatus, ref string) error {
	return d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		q := tx.SQL()
		if err := aiRequireIntent(ctx, q, st.Epoch); err != nil {
			return err
		}
		snap, err := aiConfigSnapshotTx(ctx, q)
		if err != nil {
			return err
		}
		latest, _, err := aiLatestRetest(ctx, q)
		if err != nil {
			return err
		}
		if err := aiMatchRetest(snap, latest, st, ref); err != nil {
			return err
		}
		st.Pending = false
		return aiWriteRetest(ctx, q, st)
	})
}

func aiMatchRetest(snap AIConfigSnapshot, latest, st aiconfig.RetestStatus, ref string) error {
	if snap.Revision != st.Revision || snap.CredentialRef != ref || !latest.Pending ||
		latest.Attempt != st.Attempt || latest.Revision != st.Revision || latest.Epoch != st.Epoch {
		return ErrAIRetestStale
	}
	return nil
}

func aiWriteRetest(ctx context.Context, q AIQueryer, st aiconfig.RetestStatus) error {
	raw, err := aiconfig.MarshalRetest(st)
	if err != nil {
		return err
	}
	return aiWriteSetting(ctx, q, aiconfig.KeyConfigRetest, raw, nowTS())
}

// AIQueueStats 是后台状态卡用的队列积压快照。
type AIQueueStats struct {
	Pending        int
	Processing     int
	ToolPending    int
	ToolProcessing int
}

// AIQueueStats 读取当前队列积压。
func (d *DB) AIQueueStats(ctx context.Context) (AIQueueStats, error) {
	var s AIQueueStats
	err := d.SQL.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM ai_task WHERE state='pending'),
		  (SELECT COUNT(*) FROM ai_task WHERE state='processing'),
		  (SELECT COUNT(*) FROM ai_tool_job WHERE state='pending'),
		  (SELECT COUNT(*) FROM ai_tool_job WHERE state='processing')`).
		Scan(&s.Pending, &s.Processing, &s.ToolPending, &s.ToolProcessing)
	return s, err
}

// ErrAISecretWriteRetired 表示旧库凭据写入路径已停用（T-012）：
// 新凭据只写外部受保护凭据文件，不再写共享数据库。
var ErrAISecretWriteRetired = errors.New("store: ai_secret 写入已停用，新凭据写入外部凭据文件")

// AISecret 只读读取厂商密钥的密文。明文不落库。
func (d *DB) AISecret(ctx context.Context, providerID int64) (ciphertext, nonce []byte, err error) {
	err = d.SQL.QueryRowContext(ctx,
		`SELECT ciphertext, nonce FROM ai_secret WHERE provider_id=?`, providerID).
		Scan(&ciphertext, &nonce)
	return
}

// AISaveSecret 已停用：保留函数让任何残留调用显式失败，而不是静默把新凭据写回库。
func (d *DB) AISaveSecret(context.Context, int64, []byte, []byte, string) error {
	return ErrAISecretWriteRetired
}

// AISecretRow 是旧库 ai_secret 的一行（只读导出用，不含明文）。
type AISecretRow struct {
	ProviderID int64
	Provider   string
	Ciphertext []byte
	Nonce      []byte
	Masked     string
	UpdatedTS  float64
}

// AIListSecrets 只读列出旧库全部密文及其厂商名，供一次性导出到外部凭据文件。
// 不修改任何行；旧密文在导出后仍保留在库里。
func (d *DB) AIListSecrets(ctx context.Context) ([]AISecretRow, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT s.provider_id, p.name, s.ciphertext, s.nonce, s.masked, s.updated_ts
		FROM ai_secret s JOIN ai_provider p ON p.id=s.provider_id
		ORDER BY s.provider_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AISecretRow
	for rows.Next() {
		var r AISecretRow
		if err := rows.Scan(&r.ProviderID, &r.Provider, &r.Ciphertext, &r.Nonce, &r.Masked, &r.UpdatedTS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AISecretByName 只读读取指定厂商的旧库密文（兼容路径）。found=false 表示没有该行。
func (d *DB) AISecretByName(ctx context.Context, name string) (ciphertext, nonce []byte, found bool, err error) {
	err = d.SQL.QueryRowContext(ctx, `
		SELECT s.ciphertext, s.nonce FROM ai_secret s
		JOIN ai_provider p ON p.id=s.provider_id WHERE p.name=?`, name).
		Scan(&ciphertext, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	return ciphertext, nonce, true, nil
}

// AISetChannelAgent 把 Agent 拉进或移出频道（对应 @邀请 / @移出 指令与后台勾选）。
// 生产调用点是 GS 的运行态事务（setChannelAgent）与公聊立即应用；AD 只入队。
func (d *DB) AISetChannelAgent(ctx context.Context, channel, agentKey string, enable bool) error {
	return setChannelAgent(ctx, d.SQL, channel, agentKey, enable)
}

func setChannelAgent(ctx context.Context, q AIQueryer, channel, agentKey string, enable bool) error {
	if !enable {
		_, err := q.ExecContext(ctx, `
			DELETE FROM ai_channel_agent
			WHERE channel=? AND agent_id=(SELECT id FROM ai_agent WHERE key=?)`, channel, agentKey)
		return err
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO ai_channel_agent(channel,agent_id,enabled,priority,cooldown_ms,added_by,added_ts)
		SELECT ?, id, 1, 100, 8000, 'chat', ? FROM ai_agent WHERE key=?
		ON CONFLICT(channel,agent_id) DO UPDATE SET enabled=1
		WHERE ai_channel_agent.enabled=0`, channel, nowTS(), agentKey)
	return err
}

// AISetMapAgent 把 Agent 拉进或移出一个范围（后台范围级订阅勾选）。
// 订阅一张地图 = 该图所有区域频道都能发言，是官服频道体系下的主路径；
// 订阅全局范围（map_key='*'）= 该 Agent 在任何频道都能发言。
func (d *DB) AISetMapAgent(ctx context.Context, mapKey, agentKey string, enable bool) error {
	mapKey = strings.TrimSpace(mapKey)
	if mapKey == "" || agentKey == "" {
		return nil
	}
	if !enable {
		_, err := d.SQL.ExecContext(ctx, `
			DELETE FROM ai_map_agent
			WHERE map_key=? AND agent_id=(SELECT id FROM ai_agent WHERE key=?)`, mapKey, agentKey)
		return err
	}
	_, err := d.SQL.ExecContext(ctx, `
		INSERT OR IGNORE INTO ai_map_agent(map_key,agent_id,enabled,priority,cooldown_ms,added_by,added_ts)
		SELECT ?, id, 1, 100, 8000, 'admin', ? FROM ai_agent WHERE key=?`, mapKey, nowTS(), agentKey)
	return err
}

// AIListMaps 返回全部地图 key + 中文名 + 已订阅的 Agent 名单。
// 地图来源 = loc_alias 里 level_ 前缀的行（即地图清单），不依赖玩家是否到过。
// 首项是全局范围（map_key='*'）：加进这里的助手在任何频道都能发言。
func (d *DB) AIListMaps(ctx context.Context) ([]AIMapInfo, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT m.alias, COALESCE(m.loc,''), ag.key, ag.display
		FROM (
			SELECT 0 AS ord, ? AS alias, '全局（任何频道）' AS loc
			UNION
			SELECT 1, alias, loc FROM loc_alias WHERE alias LIKE ? ESCAPE '\'
		) m
		LEFT JOIN ai_map_agent c ON c.map_key=m.alias AND c.enabled=1
		LEFT JOIN ai_agent ag ON ag.id=c.agent_id AND ag.enabled=1 AND ag.deleted_ts=0
		ORDER BY m.ord, m.alias, c.agent_id`, AIMapKeyGlobal, `level\_%`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	order := make([]string, 0, 8)
	byKey := map[string]*AIMapInfo{}
	for rows.Next() {
		var key, label string
		var agentKey, display sql.NullString
		if err := rows.Scan(&key, &label, &agentKey, &display); err != nil {
			return nil, err
		}
		info, ok := byKey[key]
		if !ok {
			info = &AIMapInfo{MapKey: key, Label: label}
			byKey[key] = info
			order = append(order, key)
		}
		if agentKey.Valid {
			info.AgentKeys = append(info.AgentKeys, agentKey.String)
		}
		if display.Valid && display.String != "" {
			info.Agents = append(info.Agents, display.String)
			info.Count++
		}
	}
	out := make([]AIMapInfo, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out, rows.Err()
}

// AIMapAgents 返回订阅了某地图的 Agent key 列表（后台展示用）。
func (d *DB) AIMapAgents(ctx context.Context, mapKey string) ([]string, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT a.key FROM ai_map_agent c
		JOIN ai_agent a ON a.id=c.agent_id AND a.enabled=1 AND a.deleted_ts=0
		WHERE c.map_key=? AND c.enabled=1 ORDER BY c.priority, a.id`, mapKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AIChannelSubscribed 返回该频道可发言的 Agent key 列表（后台展示、钓鱼选人）。
// 与 AIChannelAgents 同一套可见性口径：精确频道 ∪ 地图级 ∪ 全局范围。
func (d *DB) AIChannelSubscribed(ctx context.Context, channel string) ([]string, error) {
	return d.aiChannelScopeKeys(ctx, d.SQL, channel, false)
}

// AIEventScopeKeys 是主动事件的唯一范围口径（T-108 §9.2 E2 / v6 §3.1）：
// AIChannelSubscribed 的精确 ∪ 地图 ∪ 全局，再 ∧ enabled ∧ 未删除 ∧ joined。
//
// 只多一条「已接入」：事件是频道里的实际发言，未接进游戏的角色没有联系人身份，
// 不能出现在频道里。DM 与明确直接询答不经过这个函数，也不受它限制。
func (d *DB) AIEventScopeKeys(ctx context.Context, channel string) ([]string, error) {
	return d.aiChannelScopeKeys(ctx, d.SQL, channel, true)
}

// AIEventScopeHas 报告某角色此刻是否仍在频道范围内（事件复核用）。
//
// 读失败按「不可证」返回 false：范围证明不了就不放行，调用方据此记 out_of_scope。
func (d *DB) AIEventScopeHas(ctx context.Context, channel, agentKey string) (bool, error) {
	if channel == "" || agentKey == "" {
		return false, nil
	}
	keys, err := d.AIEventScopeKeys(ctx, channel)
	if err != nil {
		return false, err
	}
	for _, k := range keys {
		if k == agentKey {
			return true, nil
		}
	}
	return false, nil
}

// aiChannelScopeKeys 是上面两个入口共用的同一条查询：范围口径只有这一处。
// requireJoined=true 时额外要求 joined=1（主动事件用）；后台展示与钓鱼选人不要。
func (d *DB) aiChannelScopeKeys(ctx context.Context, q AIQueryer, channel string, requireJoined bool) ([]string, error) {
	mapKey, hasMap := d.ChannelMapKey(ctx, channel)
	mapArg := ""
	if hasMap {
		mapArg = mapKey
	}
	joinedClause := ""
	if requireJoined {
		joinedClause = " AND a.joined=1"
	}
	rows, err := q.QueryContext(ctx, `
		SELECT a.key FROM (
			SELECT a.id, a.key, c.priority FROM ai_channel_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.channel=? AND c.enabled=1
			UNION
			SELECT a.id, a.key, c.priority FROM ai_map_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.map_key=? AND c.enabled=1
			UNION
			SELECT a.id, a.key, c.priority FROM ai_map_agent c
				JOIN ai_agent a ON a.id=c.agent_id
				WHERE c.map_key=? AND c.enabled=1
		) src
		JOIN ai_agent a ON a.id=src.id AND a.enabled=1 AND a.deleted_ts=0`+joinedClause+`
		ORDER BY src.priority, a.id`, channel, mapArg, AIMapKeyGlobal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AIToolJob 是交给 gameserver 执行的只读工具请求。
type AIToolJob struct {
	ID     int64
	TaskID int64
	Tool   string
	Args   string
	UID    int64
	// Agent 来自父任务，执行前按当前能力复核（撤权即时生效）。
	Agent string
}

// AIEnqueueToolJob 写入一条 fish_query 工具请求。
func (d *DB) AIEnqueueToolJob(ctx context.Context, taskID int64, tool, args string, uid int64) (int64, error) {
	var id int64
	err := d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		var epoch int64
		if err := tx.SQL().QueryRowContext(ctx, `SELECT COALESCE(intent_epoch,0)
			FROM ai_task WHERE id=? AND state='processing'`, taskID).Scan(&epoch); err != nil {
			return err
		}
		if bound, ok := AIIntentEpoch(ctx); ok && bound != epoch {
			return ErrAIIntentInactive
		}
		if err := aiRequireIntent(ctx, tx.SQL(), epoch); err != nil {
			return err
		}
		res, err := tx.SQL().ExecContext(ctx, `
			INSERT INTO ai_tool_job(task_id,tool,args,uid,state,created_ts)
			VALUES(?,?,?,?,'pending',?)`, taskID, tool, args, uid, nowTS())
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	return id, err
}

// AIToolJobActive checks the live parent and emergency state together.
func (d *DB) AIToolJobActive(ctx context.Context, id int64) (bool, error) {
	r, epoch, err := readAIToolGate(ctx, d.SQL, id)
	if err != nil {
		return false, err
	}
	active, _ := aiconfig.EvaluateEmergency(r).IntentActive(epoch)
	return active, nil
}

// AIPendingToolJobs 取待处理工具请求并标记 processing。
func (d *DB) AIPendingToolJobs(ctx context.Context, limit int) ([]AIToolJob, error) {
	if limit <= 0 {
		limit = 8
	}
	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT j.id, j.task_id, j.tool, j.args, j.uid, COALESCE(t.agent,'')
		FROM ai_tool_job j LEFT JOIN ai_task t ON t.id=j.task_id
		WHERE j.state='pending' ORDER BY j.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []AIToolJob
	for rows.Next() {
		var j AIToolJob
		if err := rows.Scan(&j.ID, &j.TaskID, &j.Tool, &j.Args, &j.UID, &j.Agent); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, j)
	}
	_ = rows.Close()
	for _, j := range out {
		if _, err := tx.ExecContext(ctx, `UPDATE ai_tool_job SET state='processing' WHERE id=?`, j.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// AICompleteToolJob 写回工具结果。
func (d *DB) AICompleteToolJob(ctx context.Context, id int64, result string) error {
	return d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		r, epoch, err := readAIToolGate(ctx, tx.SQL(), id)
		if err != nil {
			return err
		}
		_, reason := aiconfig.EvaluateEmergency(r).IntentActive(epoch)
		if reason != "" {
			result = ""
		}
		_, err = tx.SQL().ExecContext(ctx, `UPDATE ai_tool_job
			SET state='done', args='', result_text=?, done_ts=?, terminal_reason=?
			WHERE id=? AND state='processing'`, result, nowTS(), reason, id)
		return err
	})
}

// AIGetToolResult 供 aibridge 轮询结果；未完成时 ok=false。
func (d *DB) AIGetToolResult(ctx context.Context, id int64) (string, bool, error) {
	var state, result string
	err := d.SQL.QueryRowContext(ctx,
		`SELECT state, result_text FROM ai_tool_job WHERE id=?`, id).Scan(&state, &result)
	if err != nil {
		return "", false, err
	}
	return result, state == "done", nil
}

// ---- 性格四维：每 Agent 一条 AI 设置值（T-103） ----
//
// 真源是 ai_setting 里按 agent_key 的一条 JSON：schema 版本、修订、所属代次
// 与四维取值。不新增表、不新增官方表字段；派生行为与派生表达都不落库，
// 保存路径只有 AISavePersonality 一处。
//
// 为什么带代次：Agent 逻辑删除后同名 key 可以恢复（沿用联系人与历史），
// 但删除前的四维不能自动跟过去。读取时比对 ai_runtime_control 的当前代次，
// 旧代次按「已停用」处理；恢复后要么显式保存一次，要么由 AISavePersonality
// 重写当前代次。

// aiPersonalityPrefix 是每 Agent 性格配置在 ai_setting 里的键前缀。
// 带冒号，与全局开关的键空间不重叠。
const aiPersonalityPrefix = "ai_personality:"

// AIPersonalityInactive* 是性格配置当前不得加载的两个原因。
const (
	AIPersonalityInactiveDeleted    = "deleted"    // Agent 已逻辑删除或已建墓碑
	AIPersonalityInactiveGeneration = "generation" // 配置属旧代次，恢复后需重新确认
)

var (
	// ErrAIPersonalityInvalid 表示显式四维非法；保存拒绝，不静默钳制。
	ErrAIPersonalityInvalid = errors.New("ai: 性格配置非法")
	// ErrAIPersonalityConflict 表示 base_revision 与库中当前修订不一致。
	ErrAIPersonalityConflict = errors.New("ai: 性格配置修订冲突，请重新读取后再保存")
	// ErrAIPersonalityInactive 表示 Agent 已删除，没有可保存的性格配置。
	ErrAIPersonalityInactive = errors.New("ai: 性格配置的 Agent 已删除")
)

// aiPersonalityKey 返回某 Agent 性格配置在 ai_setting 里的键。
func aiPersonalityKey(agentKey string) string { return aiPersonalityPrefix + agentKey }

// AIPersonality 是一个 Agent 性格配置的读取结果。
//
// Present=false 表示缺值（中性默认，可派生）；NeedsRepair=true 表示显式非法
// （原值保留、不参与派生）；Inactive 非空表示已停用（不参与派生）。
type AIPersonality struct {
	AgentKey          string
	Present           bool
	Config            aiconfig.PersonalityConfig
	Raw               string
	NeedsRepair       bool
	RepairReason      string
	Inactive          string
	CurrentGeneration int64
}

// Derivable 报告这条配置能否参与派生。
//
// 非法值或已停用时代码不得补默认值，也不得产出新的派生决定或派生表达；
// 缺值（Present=false）不属于这两类，按中性默认参与。
func (p AIPersonality) Derivable() bool {
	return !p.NeedsRepair && p.Inactive == ""
}

// AIGetPersonality 读一个 Agent 的四维配置，并与运行态当前代次比对。
//
// 三件事按一次读完成：Agent 是否还在、配置属哪一代次、存值是否合法。
func (d *DB) AIGetPersonality(ctx context.Context, agentKey string) (AIPersonality, error) {
	out := AIPersonality{AgentKey: agentKey}
	var deletedTS float64
	var generation, tombstoned int64
	err := d.SQL.QueryRowContext(ctx, `
		SELECT a.deleted_ts, COALESCE(c.generation,1), COALESCE(c.tombstoned,0)
		FROM ai_agent a
		LEFT JOIN ai_runtime_control c ON c.agent_key=a.key
		WHERE a.key=?`, agentKey).Scan(&deletedTS, &generation, &tombstoned)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		out.Inactive = AIPersonalityInactiveDeleted
	case err != nil:
		return AIPersonality{}, err
	default:
		out.CurrentGeneration = generation
		if deletedTS > 0 || tombstoned != 0 {
			out.Inactive = AIPersonalityInactiveDeleted
		}
	}

	var raw string
	err = d.SQL.QueryRowContext(ctx,
		`SELECT value FROM ai_setting WHERE key=?`, aiPersonalityKey(agentKey)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		out.Config = aiconfig.DefaultPersonality()
		return out, nil
	}
	if err != nil {
		return AIPersonality{}, err
	}
	out.Present = true
	out.Raw = raw
	cfg, _, parseErr := aiconfig.ParsePersonality(raw)
	out.Config = cfg
	if parseErr != nil {
		out.NeedsRepair = true
		out.RepairReason = parseErr.Error()
		return out, nil
	}
	if out.Inactive == "" && cfg.Generation != generation {
		out.Inactive = AIPersonalityInactiveGeneration
	}
	return out, nil
}

// AISavePersonality 在单个事务里比较 base_revision 并写入新修订。
//
// 语义：
//   - schema_version 必须显式等于当前版本：未知版本直接拒绝，不静默改成当前版本后落库；
//   - 四维按 0..100 校验，显式非法直接报错，不钳制后落库；
//   - base_revision 与当前修订不同 → ErrAIPersonalityConflict，库里一个字都不改，
//     失败重试不会覆盖另一个管理员刚保存的值；
//   - 「当前修订」与读侧可提交修订用同一个口径（aiconfig.PersonalityRevisionOf）：
//     坏记录只要修订号还能读出，就必须用读到的那个修订来修，不能要求 base 0；
//   - Agent 已删除或已建墓碑 → ErrAIPersonalityInactive；
//   - 记录总是绑定当前代次：同名 key 恢复后旧代次配置不生效，保存一次即重新确认。
//
// 返回写入后的修订号。事务内只走 tx，不碰 d.SQL（单连接池会自锁）。
func (d *DB) AISavePersonality(ctx context.Context, agentKey string, baseRevision int64,
	values aiconfig.PersonalityConfig) (int64, error) {
	if values.SchemaVersion != aiconfig.PersonalitySchemaVersion {
		return 0, fmt.Errorf("%w: 未知 schema_version=%d（当前支持 %d）",
			ErrAIPersonalityInvalid, values.SchemaVersion, aiconfig.PersonalitySchemaVersion)
	}
	next := values
	next.Revision = 0
	next.Generation = 0
	if err := next.Validate(); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrAIPersonalityInvalid, err)
	}

	tx, err := d.SQL.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var deletedTS float64
	var generation, tombstoned int64
	err = tx.QueryRowContext(ctx, `
		SELECT a.deleted_ts, COALESCE(c.generation,1), COALESCE(c.tombstoned,0)
		FROM ai_agent a
		LEFT JOIN ai_runtime_control c ON c.agent_key=a.key
		WHERE a.key=?`, agentKey).Scan(&deletedTS, &generation, &tombstoned)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %s", ErrAIPersonalityInactive, agentKey)
	}
	if err != nil {
		return 0, err
	}
	if deletedTS > 0 || tombstoned != 0 {
		return 0, fmt.Errorf("%w: %s", ErrAIPersonalityInactive, agentKey)
	}

	var current int64
	var raw string
	err = tx.QueryRowContext(ctx,
		`SELECT value FROM ai_setting WHERE key=?`, aiPersonalityKey(agentKey)).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 没有记录：当前修订按 0 计。
	case err != nil:
		return 0, err
	default:
		// 修复路径：坏记录只要修订号还能读出，就用它比较；读不出才按 0。
		// 这与 AIGetPersonality 报给管理员的「可提交修订」是同一个函数。
		current, _ = aiconfig.PersonalityRevisionOf(raw)
	}
	if baseRevision != current {
		return 0, fmt.Errorf("%w: 当前修订 %d，提交 %d", ErrAIPersonalityConflict, current, baseRevision)
	}

	next.Revision = current + 1
	next.Generation = generation
	blob, err := aiconfig.MarshalPersonality(next)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrAIPersonalityInvalid, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts`,
		aiPersonalityKey(agentKey), blob, nowTS()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return next.Revision, nil
}

// ---- 角色能力：每 Agent 一条 AI 设置值（T-104 / DEV-111 A） ----
//
// 真源是 ai_setting 里按 agent_key 的一条 JSON：能力目录版本、修订、代次与
// 能力 ID 集合。不新增表、不新增官方表字段。AD 是配置唯一写者，GS 是运行态
// 唯一写者；旧 tools_json 只在没有新记录时降级为只读子集，永远不授予写或事件。

const aiCapabilityPrefix = "ai_capability:"

var (
	// ErrAICapabilityInvalid 表示显式能力集合非法（未知 ID、未知版本）。
	ErrAICapabilityInvalid = errors.New("ai: 能力配置非法")
	// ErrAICapabilityConflict 表示 base_revision 与库中当前修订不一致。
	ErrAICapabilityConflict = errors.New("ai: 能力配置修订冲突，请重新读取后再保存")
	// ErrAICapabilityInactive 表示 Agent 已删除，没有可保存的能力配置。
	ErrAICapabilityInactive = errors.New("ai: 能力配置的 Agent 已删除")
)

// AICapabilityKey 返回某 Agent 能力配置在 ai_setting 里的键。
func AICapabilityKey(agentKey string) string { return aiCapabilityPrefix + agentKey }

// AICapabilityView 是管理侧读取结果：既有投影后的状态，也有原始修订。
//
// Present=false 表示没有新记录（可能是缺配置，也可能是旧 tools_json 降级）；
// Revision 是保存时必须提交的 base_revision（坏记录也要能按读到的修订修复）。
// Generation/ActorEnabled 是同一份快照里的运行事实，供可用性判定使用。
type AICapabilityView struct {
	AgentKey     string
	Present      bool
	Revision     int64
	Raw          string
	State        aiconfig.CapabilityState
	Generation   int64
	ActorEnabled bool
}

// AIGetCapabilities 读一个角色的能力配置与投影。
func (d *DB) AIGetCapabilities(ctx context.Context, agentKey string) (AICapabilityView, error) {
	return aiGetCapabilities(ctx, d.SQL, agentKey)
}

// AIReadCapabilityState 是 GS 写事务内的读入口：只返回投影，不暴露原始值。
func AIReadCapabilityState(ctx context.Context, q AIQueryer, agentKey string) (aiconfig.CapabilityState, error) {
	v, err := aiGetCapabilities(ctx, q, agentKey)
	if err != nil {
		return aiconfig.CapabilityState{}, err
	}
	return v.State, nil
}

func aiGetCapabilities(ctx context.Context, q AIQueryer, agentKey string) (AICapabilityView, error) {
	out := AICapabilityView{AgentKey: agentKey}
	var deletedTS float64
	var toolsJSON string
	var actor int
	var generation, tombstoned int64
	err := q.QueryRowContext(ctx, `
		SELECT a.deleted_ts, COALESCE(a.tools_json,'[]'), COALESCE(a.actor_enabled,0),
		       COALESCE(c.generation,1), COALESCE(c.tombstoned,0)
		FROM ai_agent a
		LEFT JOIN ai_runtime_control c ON c.agent_key=a.key
		WHERE a.key=?`, agentKey).Scan(&deletedTS, &toolsJSON, &actor, &generation, &tombstoned)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		out.State = aiconfig.CapabilityState{
			AgentKey: agentKey, State: aiconfig.CapabilityStateInactive, Reason: "deleted",
		}
		return out, nil
	case err != nil:
		return AICapabilityView{}, err
	}
	out.Generation = generation
	out.ActorEnabled = actor != 0
	if deletedTS > 0 || tombstoned != 0 {
		out.State = aiconfig.CapabilityState{
			AgentKey: agentKey, State: aiconfig.CapabilityStateInactive, Reason: "deleted",
		}
		return out, nil
	}

	var raw string
	err = q.QueryRowContext(ctx,
		`SELECT value FROM ai_setting WHERE key=?`, AICapabilityKey(agentKey)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		out.State = aiconfig.ResolveCapabilityState(agentKey, nil, false, nil, toolsJSON, generation)
		return out, nil
	}
	if err != nil {
		return AICapabilityView{}, err
	}
	out.Present = true
	out.Raw = raw
	if rev, ok := aiconfig.CapabilityRevisionOf(raw); ok {
		out.Revision = rev
	}
	rec, parseErr := aiconfig.ParseCapabilitySet(raw)
	if parseErr != nil {
		out.State = aiconfig.ResolveCapabilityState(agentKey, nil, true, parseErr, toolsJSON, generation)
		return out, nil
	}
	out.State = aiconfig.ResolveCapabilityState(agentKey, &rec, true, nil, toolsJSON, generation)
	// 能力保存自己撤销「虚拟垂钓」时，GS 的 actor_set_enabled=false 会推进一次
	// 代次。若不解释这次推进，保存后的攻略能力会按旧代次失效——这正是 R-138
	// 复验发现的缺陷。只有「当前代次恰好是记录代次 +1」且「记录写入之后应用的
	// 最后一条能力运行命令」与集合意图一致时才解释；外部禁用、重复推进、
	// 旧后台写入都不会被解释成生效。
	if out.State.State == aiconfig.CapabilityStateInactive &&
		aiCapabilityGenerationExplained(ctx, q, agentKey, &rec, generation) {
		effective := aiconfig.ResolveCapabilityState(agentKey, &rec, true, nil, toolsJSON, rec.Generation)
		effective.Generation = generation
		out.State = effective
	}
	return out, nil
}

// aiCapabilityGenerationExplained 判断一次代次推进是否由能力保存自己的
// actor_set_enabled 命令造成。只读命令表，不写任何状态。
//
// GS 已经对 cap1- 命令复核能力修订：只有「当前修订」的补派能应用，因此
// 造成这次 +1 的禁用命令必然属于记录修订。用修订前缀而不是时间戳判断，
// 避免设备时钟回拨/测试固定时钟把一次已应用的自致推进误判成外部禁用。
func aiCapabilityGenerationExplained(ctx context.Context, q AIQueryer, agentKey string,
	rec *aiconfig.CapabilitySet, currentGeneration int64) bool {
	if rec == nil || rec.Generation+1 != currentGeneration {
		return false
	}
	desired := aiconfig.RequiresActorSimulation(rec.Capabilities)
	prefix := aiconfig.CapabilityRuntimeRequestPrefix(agentKey, rec.Revision)
	rows, err := q.QueryContext(ctx, `
		SELECT payload FROM ai_runtime_command
		WHERE agent_key=? AND kind=? AND source=? AND request_id LIKE ? AND status=?
		ORDER BY id DESC LIMIT 16`,
		agentKey, AIRuntimeKindActorSetEnabled, AIRuntimeSourceAdmin,
		prefix+"%", AIRuntimeStatusApplied)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if rows.Scan(&payload) != nil {
			return false
		}
		enabled, err := AIRuntimePayloadActorSetEnabled(payload)
		if err != nil {
			continue
		}
		return enabled == desired
	}
	return false
}

// AICapabilityRuntimeCommand 是一条与能力保存关联的 GS 运行命令的脱敏视图。
type AICapabilityRuntimeCommand struct {
	ID                 int64
	RequestID          string
	Status             string
	Reason             string
	Enabled            bool
	Revision           int64
	ExpectedGeneration int64
	CreatedTS          float64
	AppliedTS          float64
	ResultRevision     int64
}

// 能力运行可用性。只有 available 表示「GS 运行事实已经等于已保存意图」。
const (
	AICapabilityAvailabilityAvailable   = "available"   // 运行事实与已保存意图一致
	AICapabilityAvailabilityPending     = "pending"     // 已入队，等待 GS 应用
	AICapabilityAvailabilityFailed      = "failed"      // 最近一次补派被拒绝/过期/未补派
	AICapabilityAvailabilityUnavailable = "unavailable" // 配置本身未生效或角色不存在
)

// AICapabilityRuntimeStatus 是「已保存的能力集合」与 GS 运行事实的差量。
//
// 目录里只有「虚拟垂钓」直接对应 actor_set_enabled；其余能力不因保存而
// 改变运行态。GS 的运行标志只作已应用事实，不能反过来被解释成授权。
type AICapabilityRuntimeStatus struct {
	AgentKey       string
	Revision       int64
	Generation     int64
	State          string
	Required       bool
	Desired        bool
	Actual         bool
	Availability   string
	Reason         string
	CommandID      int64
	RequestID      string
	Status         string
	AppliedTS      float64
	ResultRevision int64
	Commands       []AICapabilityRuntimeCommand

	// sameRevisionPending 表示当前修订已有一条待应用命令；只供保存路径
	// 判断是否仍需补派，不序列化给页面。
	sameRevisionPending bool
}

// AICapabilitySaveResult 是一次能力保存的完整回交：状态、修订与运行差量。
type AICapabilitySaveResult struct {
	State        aiconfig.CapabilityState
	Revision     int64
	Changed      bool
	Runtime      AICapabilityRuntimeStatus
	RuntimeError string
}

// AISaveCapabilities 在一个事务里比较 base_revision 并写入新修订。
//
// 语义：
//   - 能力 ID 必须全部属于目录，按目录顺序保存；未知 ID 直接拒绝；
//   - base_revision 与当前修订不同 → ErrAICapabilityConflict，库里一个字都不改；
//   - 显式空集合合法，表示全部拒绝；缺记录不写就等于缺配置；
//   - 同一集合在当前修订上重复保存是幂等的：修订不跳号，也不重复补派命令；
//   - 记录绑定当前代次：同名 key 恢复后旧配置不生效，保存一次即重新确认；
//   - readOnlyTools 用于导入旧只读子集。普通保存传 nil，保留原有收窄。
func (d *DB) AISaveCapabilities(ctx context.Context, agentKey string, baseRevision int64,
	capabilities, readOnlyTools []string) (aiconfig.CapabilityState, error) {
	res, err := d.AISaveCapabilitiesWithRuntime(ctx, agentKey, baseRevision, capabilities, readOnlyTools)
	return res.State, err
}

// AISaveCapabilitiesWithRuntime 与 AISaveCapabilities 相同，并返回所需 GS
// 运行变更的命令 ID 与可用性。配置保存与运行变更不是一件事：配置先落库，
// 运行变更按已保存的修订补派；补派失败只体现在可用性里，下一次保存会重试。
func (d *DB) AISaveCapabilitiesWithRuntime(ctx context.Context, agentKey string, baseRevision int64,
	capabilities, readOnlyTools []string) (AICapabilitySaveResult, error) {
	caps, err := aiconfig.NormalizeCapabilities(capabilities)
	if err != nil {
		return AICapabilitySaveResult{}, fmt.Errorf("%w: %v", ErrAICapabilityInvalid, err)
	}
	// 显式空集合要能区分于「没传」：Normalize 对空入参返回空切片。
	if capabilities == nil {
		caps = nil
	}
	tools, err := aiconfig.NormalizeReadOnlyTools(readOnlyTools)
	if err != nil {
		return AICapabilitySaveResult{}, fmt.Errorf("%w: %v", ErrAICapabilityInvalid, err)
	}

	var next aiconfig.CapabilitySet
	changed := false
	manageActor := false
	err = d.AIWithRuntimeTx(ctx, func(tx *AIRuntimeTx) error {
		q := tx.SQL()
		var deletedTS float64
		var legacyToolsJSON string
		var generation, tombstoned int64
		err := q.QueryRowContext(ctx, `
			SELECT a.deleted_ts, COALESCE(c.generation,1), COALESCE(c.tombstoned,0), COALESCE(a.tools_json,'[]')
			FROM ai_agent a
			LEFT JOIN ai_runtime_control c ON c.agent_key=a.key
			WHERE a.key=?`, agentKey).Scan(&deletedTS, &generation, &tombstoned, &legacyToolsJSON)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrAICapabilityInactive, agentKey)
		}
		if err != nil {
			return err
		}
		if deletedTS > 0 || tombstoned != 0 {
			return fmt.Errorf("%w: %s", ErrAICapabilityInactive, agentKey)
		}

		var current int64
		var raw string
		present := false
		err = q.QueryRowContext(ctx,
			`SELECT value FROM ai_setting WHERE key=?`, AICapabilityKey(agentKey)).Scan(&raw)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// 没有记录：当前修订按 0 计。
		case err != nil:
			return err
		default:
			present = true
			current, _ = aiconfig.CapabilityRevisionOf(raw)
		}
		if baseRevision != current {
			return fmt.Errorf("%w: 当前修订 %d，提交 %d", ErrAICapabilityConflict, current, baseRevision)
		}

		// 相同集合不推进修订：重复保存只刷新代次与时间，不制造空版本。
		prevSet, prevOK := aiconfig.CapabilitySet{}, false
		hasGuide := slices.Contains(caps, aiconfig.CapabilityGuideHelp)
		keepGuide := hasGuide && readOnlyTools == nil
		provenGuide := len(tools) > 0
		if keepGuide {
			if legacy := aiconfig.LegacyReadTools(legacyToolsJSON); len(legacy) > 0 {
				tools = legacy
				provenGuide = true
			}
		}
		changed = true
		if present {
			if rec, parseErr := aiconfig.ParseCapabilitySet(raw); parseErr == nil {
				prevSet, prevOK = rec, true
				if slices.Contains(rec.Capabilities, aiconfig.CapabilityGuideHelp) {
					provenGuide = true
					if keepGuide {
						tools = rec.ReadOnlyTools
					}
				}
				sameCaps := slicesEqual(rec.Capabilities, caps)
				sameTools := slicesEqual(rec.ReadOnlyTools, tools)
				changed = !(sameCaps && sameTools)
			}
		}
		if hasGuide && !provenGuide {
			return fmt.Errorf("%w: 旧攻略授权仅用于兼容历史配置，请选择独立资料能力", ErrAICapabilityInvalid)
		}
		nextRevision := current
		if changed {
			nextRevision = current + 1
		}

		// 「虚拟垂钓」是唯一直接改 GS 模拟开关的能力。只有本集合或历史集合
		// 曾经声明过它（或本 Agent 已有能力运行命令）时才接管这个开关；
		// 否则保存「攻略」等能力不能把旧后台打开的角色模拟顺手关掉。
		var capCmds int
		if err := q.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM ai_runtime_command
			WHERE agent_key=? AND kind=? AND source=? AND request_id LIKE ?`,
			agentKey, AIRuntimeKindActorSetEnabled, AIRuntimeSourceAdmin,
			aiconfig.CapabilityRuntimeRequestTag()+"%").Scan(&capCmds); err != nil {
			return err
		}
		manageActor = (prevOK && aiconfig.RequiresActorSimulation(prevSet.Capabilities)) ||
			aiconfig.RequiresActorSimulation(caps) || capCmds > 0

		next = aiconfig.CapabilitySet{
			SchemaVersion: aiconfig.CapabilitySchemaVersion,
			Revision:      nextRevision,
			Generation:    generation,
			Capabilities:  caps,
			ReadOnlyTools: tools,
			UpdatedTS:     nowTS(),
		}
		blob, err := aiconfig.MarshalCapabilitySet(next)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrAICapabilityInvalid, err)
		}
		_, err = q.ExecContext(ctx, `
			INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_ts=excluded.updated_ts`,
			AICapabilityKey(agentKey), blob, next.UpdatedTS)
		return err
	})
	if err != nil {
		return AICapabilitySaveResult{}, err
	}

	state := aiconfig.ResolveCapabilityState(agentKey, &next, true, nil, "", next.Generation)
	out := AICapabilitySaveResult{State: state, Revision: next.Revision, Changed: changed}
	rt, rtErr := d.reconcileCapabilityActor(ctx, agentKey, state, manageActor)
	if rtErr != nil {
		rt.Availability = AICapabilityAvailabilityFailed
		rt.Reason = "runtime_read_failed"
		out.RuntimeError = rtErr.Error()
	}
	out.Runtime = rt
	return out, nil
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AICapabilityRuntimeStatus 只读地计算一次保存所需的运行变更是否已应用。
// 它绝不入队、不改运行态；页面刷新与保存回执共用同一口径。
func (d *DB) AICapabilityRuntimeStatus(ctx context.Context, agentKey string,
	state aiconfig.CapabilityState) (AICapabilityRuntimeStatus, error) {
	return d.aiCapabilityRuntimeStatus(ctx, agentKey, state, false)
}

func (d *DB) aiCapabilityRuntimeStatus(ctx context.Context, agentKey string,
	state aiconfig.CapabilityState, manageHint bool) (AICapabilityRuntimeStatus, error) {
	out := AICapabilityRuntimeStatus{
		AgentKey: agentKey, Revision: state.Revision, State: state.State,
	}
	var actor int
	var generation int64
	err := d.SQL.QueryRowContext(ctx, `
		SELECT COALESCE(a.actor_enabled,0), COALESCE(c.generation,1)
		FROM ai_agent a
		LEFT JOIN ai_runtime_control c ON c.agent_key=a.key
		WHERE a.key=? AND a.deleted_ts=0`, agentKey).Scan(&actor, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		out.Availability = AICapabilityAvailabilityUnavailable
		out.Reason = "agent_missing"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Generation = generation
	out.Actual = actor != 0
	if state.State != aiconfig.CapabilityStateConfigured &&
		state.State != aiconfig.CapabilityStateEmpty {
		out.Availability = AICapabilityAvailabilityUnavailable
		out.Reason = state.State
		if out.Reason == "" {
			out.Reason = "unknown"
		}
		return out, nil
	}
	if state.Legacy {
		out.Availability = AICapabilityAvailabilityUnavailable
		out.Reason = "legacy"
		return out, nil
	}
	out.Desired = aiconfig.RequiresActorSimulation(state.Capabilities)

	cmds, err := d.aiCapabilityRuntimeCommands(ctx, agentKey)
	if err != nil {
		return out, err
	}
	managed := manageHint || out.Desired || len(cmds) > 0
	if !managed {
		// 这份集合从不声明虚拟垂钓，也没有任何能力运行命令：GS 模拟开关
		// 不属于本集合的管理范围，不能因为保存攻略就把它当成需要修正的差量。
		out.Availability = AICapabilityAvailabilityAvailable
		return out, nil
	}
	prefix := aiconfig.CapabilityRuntimeRequestPrefix(agentKey, state.Revision)
	var latestSameRev, pendingSameRev *AIRuntimeCommand
	for _, c := range cmds {
		if len(out.Commands) < 8 {
			out.Commands = append(out.Commands, aiCapabilityRuntimeCommandOf(c))
		}
		if !strings.HasPrefix(c.RequestID, prefix) {
			continue
		}
		if latestSameRev == nil {
			latestSameRev = c
		}
		if c.Status == AIRuntimeStatusPending && pendingSameRev == nil {
			pendingSameRev = c
		}
	}
	out.sameRevisionPending = pendingSameRev != nil
	out.Required = out.Actual != out.Desired
	switch {
	case !out.Required:
		out.Availability = AICapabilityAvailabilityAvailable
		out.Reason = ""
	case out.sameRevisionPending:
		out.Availability = AICapabilityAvailabilityPending
	case latestSameRev != nil && (latestSameRev.Status == AIRuntimeStatusRejected ||
		latestSameRev.Status == AIRuntimeStatusExpired):
		out.Availability = AICapabilityAvailabilityFailed
		out.Reason = latestSameRev.Reason
		if out.Reason == "" {
			out.Reason = "unknown"
		}
	case latestSameRev != nil && latestSameRev.Status == AIRuntimeStatusApplied:
		out.Availability = AICapabilityAvailabilityFailed
		out.Reason = "applied_state_mismatch"
	default:
		out.Availability = AICapabilityAvailabilityFailed
		out.Reason = "not_dispatched"
	}
	if latestSameRev != nil {
		out.CommandID = latestSameRev.ID
		out.RequestID = latestSameRev.RequestID
		out.Status = latestSameRev.Status
		out.AppliedTS = latestSameRev.AppliedTS
		out.ResultRevision = latestSameRev.ResultRevision
		if out.Reason == "" && latestSameRev.Reason != "" {
			out.Reason = latestSameRev.Reason
		}
	}
	return out, nil
}

// reconcileCapabilityActor 按已保存的修订补派所需的 actor_set_enabled。
//
// manage=false 表示这份集合并未接管 GS 模拟开关（历史与管理事实都没有
// 声明过虚拟垂钓），此时不制造任何运行命令。
// 只有「当前修订」的待应用命令会被视为已在路上；旧修订的待应用命令会被
// GS 的能力修订复核拒绝，不能代替本次补派。命令 ID 携带修订与 attempt，
// 同一个请求重试不产生第二行，失败后按当前修订重新补派。
func (d *DB) reconcileCapabilityActor(ctx context.Context, agentKey string,
	state aiconfig.CapabilityState, manage bool) (AICapabilityRuntimeStatus, error) {
	rt, err := d.aiCapabilityRuntimeStatus(ctx, agentKey, state, manage)
	if err != nil {
		return rt, err
	}
	if !manage || rt.Availability == AICapabilityAvailabilityUnavailable ||
		!rt.Required || rt.sameRevisionPending {
		return rt, nil
	}

	base := aiconfig.CapabilityRuntimeRequestID(agentKey, state.Revision, rt.Desired, 1)
	var used int
	if err := d.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ai_runtime_command
		WHERE agent_key=? AND kind=? AND source=? AND (request_id=? OR request_id LIKE ?)`,
		agentKey, AIRuntimeKindActorSetEnabled, AIRuntimeSourceAdmin,
		base, base+"-a%").Scan(&used); err != nil {
		return rt, err
	}
	requestID := aiconfig.CapabilityRuntimeRequestID(agentKey, state.Revision, rt.Desired, used+1)

	expectedGeneration := int64(0)
	if !rt.Desired {
		// 禁用会推进代次，必须带上入队时看到的代次；重复禁用按旧代次被拒，
		// 不会把记录代次甩开两代。启用是绝对意图，不约束代次。
		ctl, err := d.AIGetRuntimeControl(ctx, agentKey)
		if err != nil {
			return rt, err
		}
		expectedGeneration = ctl.Generation
	}
	payload, err := json.Marshal(AIRuntimeActorSetEnabledPayload{Enabled: rt.Desired})
	if err != nil {
		return rt, err
	}
	_, _, err = d.AIEnqueueRuntimeCommand(ctx, AIRuntimeEnqueueRequest{
		RequestID:          requestID,
		Source:             AIRuntimeSourceAdmin,
		Kind:               AIRuntimeKindActorSetEnabled,
		AgentKey:           agentKey,
		ExpectedGeneration: expectedGeneration,
		ExpectedRevision:   state.Revision,
		Payload:            payload,
		TTL:                10 * time.Minute,
	})
	if err != nil {
		rt.Availability = AICapabilityAvailabilityFailed
		rt.Reason = aiCapabilityDispatchReason(err)
		return rt, nil
	}
	return d.AICapabilityRuntimeStatus(ctx, agentKey, state)
}

func aiCapabilityDispatchReason(err error) string {
	switch {
	case errors.Is(err, ErrAIRuntimeQueueFull):
		return "queue_full"
	case errors.Is(err, ErrAIRuntimeCommandConflict):
		return "request_conflict"
	case errors.Is(err, ErrAIIntentInactive):
		return "intent_inactive"
	default:
		return "enqueue_failed"
	}
}

// aiCapabilityRuntimeCommands 返回某 Agent 全部能力保存触发的运行命令，新的在前。
func (d *DB) aiCapabilityRuntimeCommands(ctx context.Context, agentKey string) ([]*AIRuntimeCommand, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT `+aiRuntimeCommandColumns+` FROM ai_runtime_command
		WHERE agent_key=? AND kind=? AND source=? AND request_id LIKE ?
		ORDER BY id DESC LIMIT 64`,
		agentKey, AIRuntimeKindActorSetEnabled, AIRuntimeSourceAdmin,
		aiconfig.CapabilityRuntimeRequestTag()+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AIRuntimeCommand
	for rows.Next() {
		c, err := scanAIRuntimeCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func aiCapabilityRuntimeCommandOf(c *AIRuntimeCommand) AICapabilityRuntimeCommand {
	enabled, _ := AIRuntimePayloadActorSetEnabled(c.Payload)
	return AICapabilityRuntimeCommand{
		ID: c.ID, RequestID: c.RequestID, Status: c.Status, Reason: c.Reason,
		Enabled: enabled, Revision: c.ExpectedRevision,
		ExpectedGeneration: c.ExpectedGeneration, CreatedTS: c.CreatedTS,
		AppliedTS: c.AppliedTS, ResultRevision: c.ResultRevision,
	}
}

// AIEnqueueSystemChannel 只允许系统创建「某个 Agent 在频道里接一句」的任务，
// 用于玩家自己上星/上蓝时让 AI 反应一下。Agent 的输出仍然只进 ai_result，
// 不会回流成新任务。
//
// ev 是这条事件的受信证据（类别 + 该角色能力）；nil 表示调用方没有给出类别，
// 消费端会按「类别不可证」终止。
func (d *DB) AIEnqueueSystemChannel(ctx context.Context, channel string, uid int64,
	text, agentKey string, contactUID int64, ev *AITaskEvidence) (int64, error) {
	if channel == "" || agentKey == "" {
		return 0, errors.New("ai: 频道或 Agent 为空")
	}
	return d.aiEnqueueTask(ctx, AITask{
		Kind: "channel", Scope: "channel", ScopeKey: channel, UID: uid,
		Agent: agentKey, ContactUID: contactUID, SourceKind: "system", Text: text, DeadlineTS: nowTS() + 30,
	}, ev)
}

// ---------------------------------------------------------------------------
// T-108 / DEV-121 A：任务选择证据与结果表达证据
// ---------------------------------------------------------------------------
//
// 两张从属表只回答两个问题：
//   1. ai_task_evidence：这条任务为什么被允许入队——受信入口类别（或直接对话）、
//      发起者、有界参与者，以及每位参与者在入队事务里读到的角色代次与所需能力的修订；
//   2. ai_result_expression：这一句表达实际用了哪一版四维——只有 BR 真正把同一份
//      已校验配置编译进提示词、并产生正常非空结果时才写。
//
// 两个写者不变：GS 只写任务侧，BR 只写结果侧；AD 只读投影。正文、模型自报、
// 完成计数都没有授权或生效证明力，因此都不进这两张表。

const (
	// AITaskEvidenceFormatV1 是 binding_json 的载荷格式版。
	AITaskEvidenceFormatV1 = 1
	// AITaskEvidenceMaxParticipants 是有界参与者上限（与圆桌最大发言者一致）。
	// 与 aiconfig 共用同一份数值：氛围档最高 10 人，留到 12 的余量。
	AITaskEvidenceMaxParticipants = aiconfig.RoundtableMaxParticipants
)

// 证据判定的原因码。字面量集中在这里，GS/BR/回执引用同一份，避免各自发明。
const (
	AIEvidenceReasonMatched = "matched"
	// AIEvidenceReasonOutOfScope：当前频道订阅范围（精确 ∪ 地图 ∪ 全局，且
	// 已接入、未删除）已不含该角色。撤订阅、改到别的图、撤地图级只留全局都
	// 走同一口径；DM 与明确直接询答不受范围限制（T-108 §9.2 E2）。
	AIEvidenceReasonOutOfScope = "out_of_scope"
	// AIEvidenceReasonAttemptReclaimed：旧尝试对未投递结果的终态原因。
	// 任务被回收重领后，上一尝试的结果不允许再被投递（T-108 §9.2 E4）。
	AIEvidenceReasonAttemptReclaimed = "attempt_reclaimed"
	// AIEvidenceReasonResultNotPending：发前复核发现这条结果已不是待投递状态
	// （另一次投递、回收或丢弃已先落终态），本条不得再生成新帧。
	AIEvidenceReasonResultNotPending = "result_not_pending"
	// AIEvidenceReasonSourceUnavailable：没有允许的读取路径时，事实来源显式
	// 记为不可得（E5 的 event_snapshot 固定这一项）。
	AIEvidenceReasonSourceUnavailable = "source_unavailable"
	AITaskEvidenceReasonMissing       = "task_evidence_missing"
	AITaskEvidenceReasonUnreadable    = "task_evidence_unreadable"
	AITaskEvidenceReasonUnknownCat    = "event_category_unknown"
	AITaskEvidenceReasonNoParticipant = "participant_not_bound"
	AITaskEvidenceReasonGeneration    = "generation_changed"
	AITaskEvidenceReasonCapability    = "event_capability_revoked"
	AITaskEvidenceReasonRevision      = "capability_revision_changed"
	AITaskEvidenceReasonInitiator     = "initiator_revoked"
	AITaskEvidenceReasonNoExpression  = "br_expression_missing"
	AITaskEvidenceReasonNoGSPersona   = "gs_personality_revision_missing"
	AITaskEvidenceReasonPersonaDiff   = "personality_revision_mismatch"
	AITaskEvidenceReasonTaskMissing   = "task_not_found"
	// 以下三个只用于「按角色当前版本」的只读投影（T-107 P4 的消费方）。
	AIAgentEvidenceReasonAgentMissing  = "agent_not_found"
	AIAgentEvidenceReasonAgentInactive = "agent_inactive"
	AIAgentEvidenceReasonResultVoided  = "result_voided"
)

// AITaskEvidenceParticipant 是一名参与者在入队事务里被绑定的事实。
//
// Capability 是本次事件规则要求该角色具备的事件能力（例如 reply_star、
// join_roundtable、broadcast_catch）；CapRevision 是入队事务里读到的该能力
// 配置修订。GSPersonalityRevision 只在「该角色真的用四维做过这次选择」时
// 才有值，取自同一个 PersonalityConfig，不是事后补写的。
type AITaskEvidenceParticipant struct {
	Key                   string `json:"key"`
	Capability            string `json:"capability,omitempty"`
	Generation            int64  `json:"generation"`
	CapRevision           int64  `json:"cap_revision"`
	GSPersonalityRevision *int64 `json:"gs_personality_revision,omitempty"`
}

// AITaskEvidence 是 ai_task_evidence.binding_json 的 v1 载荷。
type AITaskEvidence struct {
	FormatVersion int                         `json:"format_version"`
	Category      string                      `json:"category,omitempty"`
	Direct        bool                        `json:"direct_dialog,omitempty"`
	Initiator     string                      `json:"initiator,omitempty"`
	Participants  []AITaskEvidenceParticipant `json:"participants"`
}

// AIResultExpression 是一句表达实际使用的版本凭据（ai_result_expression 一行）。
//
// BRPersonalityRevision 必须 > 0：0 表示「没有已保存且已生效的修订」，
// 那种情况下中性默认照常回答，但不写这一行，两端同版结论保持「未确认」。
type AIResultExpression struct {
	AgentKey              string
	Generation            int64
	BRPersonalityRevision int64
}

// validateAIResultExpression 守住「只有正常非空表达才带凭据」这条边界。
func validateAIResultExpression(status, reply string, expr *AIResultExpression) error {
	if expr == nil {
		return nil
	}
	if status != "done" || strings.TrimSpace(reply) == "" {
		return errors.New("ai: 只有正常完成且有正文的结果才能写表达凭据")
	}
	if strings.TrimSpace(expr.AgentKey) == "" {
		return errors.New("ai: 表达凭据缺角色")
	}
	if expr.Generation <= 0 {
		return fmt.Errorf("ai: 表达凭据缺角色代次（%d）", expr.Generation)
	}
	if expr.BRPersonalityRevision <= 0 {
		return fmt.Errorf("ai: 表达凭据缺四维修订（%d）", expr.BRPersonalityRevision)
	}
	return nil
}

// insertAIResultExpression 在既有结果事务里写一行表达凭据。
func insertAIResultExpression(ctx context.Context, q AIQueryer, resultID int64, expr AIResultExpression) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO ai_result_expression(result_id, agent_key, generation, br_personality_revision)
		VALUES(?,?,?,?)`, resultID, expr.AgentKey, expr.Generation, expr.BRPersonalityRevision)
	if err != nil {
		return fmt.Errorf("ai: 表达凭据写入失败: %w", err)
	}
	return nil
}

// aiPersonalityRevisionOf 在给定连接/事务上读某角色已保存的四维修订。
// present=false 表示没有存值（中性默认），不是错误。
func aiPersonalityRevisionOf(ctx context.Context, q AIQueryer, agentKey string) (int64, bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM ai_setting WHERE key=?`, aiPersonalityKey(agentKey)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	rev, ok := aiconfig.PersonalityRevisionOf(raw)
	if !ok {
		return 0, false, nil
	}
	return rev, true, nil
}

// bindAITaskEvidence 在入队事务里把证据补全并校验，返回是否要落库与序列化后的载荷。
//
// 三条硬约束：
//   - 载荷里的参与者必须与任务行自己的参与者/收件人一致（不一致就拒绝入队）；
//   - 角色代次与能力修订以本事务读到的值为准，调用方给的值只作一致性断言；
//   - 事件入口的每位参与者都必须给出本次被要求的事件能力，且此刻确实具备。
func bindAITaskEvidence(ctx context.Context, q AIQueryer, t AITask, ev *AITaskEvidence) (bool, string, error) {
	if ev == nil {
		return false, "", nil
	}
	if ev.FormatVersion != AITaskEvidenceFormatV1 {
		return false, "", fmt.Errorf("ai: 未知证据格式版 %d", ev.FormatVersion)
	}
	taskKeys, err := aiTaskEvidenceTaskKeys(t)
	if err != nil {
		return false, "", err
	}
	if ev.Direct && ev.Category != "" {
		return false, "", errors.New("ai: 直接对话不得携带事件类别")
	}
	if !ev.Direct {
		if ev.Category == "" {
			return false, "", errors.New("ai: 事件任务缺受信类别")
		}
		if _, ok := aiconfig.CapabilityForEvent(ev.Category); !ok {
			return false, "", fmt.Errorf("ai: 未知事件类别 %q", ev.Category)
		}
	}
	bound := AITaskEvidence{
		FormatVersion: AITaskEvidenceFormatV1,
		Category:      ev.Category,
		Direct:        ev.Direct,
		Initiator:     strings.TrimSpace(ev.Initiator),
		Participants:  make([]AITaskEvidenceParticipant, 0, len(ev.Participants)),
	}
	// 直接对话可以不带参与者（任务行没有可核对的收件人时，不编造一条）。
	if len(ev.Participants) == 0 {
		if !ev.Direct {
			return false, "", errors.New("ai: 事件任务必须给出参与者")
		}
		for _, key := range taskKeys {
			bound.Participants = append(bound.Participants, AITaskEvidenceParticipant{Key: key})
		}
	} else {
		for _, p := range ev.Participants {
			bound.Participants = append(bound.Participants, p)
		}
	}
	if len(bound.Participants) > AITaskEvidenceMaxParticipants {
		return false, "", fmt.Errorf("ai: 参与者 %d 人超过上限 %d",
			len(bound.Participants), AITaskEvidenceMaxParticipants)
	}
	if !sameAIKeys(taskKeys, bound.Participants) {
		return false, "", fmt.Errorf("ai: 证据参与者 %v 与任务参与者 %v 不一致",
			aiEvidenceKeys(bound.Participants), taskKeys)
	}
	for i := range bound.Participants {
		p := &bound.Participants[i]
		p.Key = strings.TrimSpace(p.Key)
		if p.Key == "" {
			return false, "", errors.New("ai: 证据参与者缺角色")
		}
		st, err := AIReadCapabilityState(ctx, q, p.Key)
		if err != nil {
			return false, "", fmt.Errorf("ai: 读 %s 的能力: %w", p.Key, err)
		}
		if p.Generation != 0 && p.Generation != st.Generation {
			return false, "", fmt.Errorf("ai: %s 代次 %d 与入队事务读到的 %d 不一致",
				p.Key, p.Generation, st.Generation)
		}
		if p.CapRevision != 0 && p.CapRevision != st.Revision {
			return false, "", fmt.Errorf("ai: %s 能力修订 %d 与入队事务读到的 %d 不一致（可能刚被撤权）",
				p.Key, p.CapRevision, st.Revision)
		}
		if p.Capability == "" {
			if !ev.Direct {
				return false, "", fmt.Errorf("ai: 事件参与者 %s 缺所需能力", p.Key)
			}
		} else {
			def, ok := aiconfig.CapabilityByID(p.Capability)
			if !ok || def.Event == "" {
				return false, "", fmt.Errorf("ai: %s 不是事件能力", p.Capability)
			}
			if st.State != aiconfig.CapabilityStateConfigured || !st.Has(p.Capability) {
				return false, "", fmt.Errorf("ai: %s 此刻不具备 %s（状态 %s）", p.Key, p.Capability, st.State)
			}
			if def.RequiresRoundtable && !st.Has(aiconfig.CapabilityJoinRoundtable) {
				return false, "", fmt.Errorf("ai: %s 的 %s 还要求 %s",
					p.Key, p.Capability, aiconfig.CapabilityJoinRoundtable)
			}
		}
		if p.GSPersonalityRevision != nil {
			rev, present, err := aiPersonalityRevisionOf(ctx, q, p.Key)
			if err != nil {
				return false, "", fmt.Errorf("ai: 读 %s 的四维: %w", p.Key, err)
			}
			if !present {
				return false, "", fmt.Errorf("ai: %s 没有已保存的四维，不能绑修订", p.Key)
			}
			if rev != *p.GSPersonalityRevision {
				return false, "", fmt.Errorf("ai: %s 四维修订 %d 与入队事务读到的 %d 不一致",
					p.Key, *p.GSPersonalityRevision, rev)
			}
		}
		p.Generation = st.Generation
		p.CapRevision = st.Revision
	}
	raw, err := json.Marshal(bound)
	if err != nil {
		return false, "", err
	}
	return true, string(raw), nil
}

// aiTaskEvidenceTaskKeys 取任务行自己声明的参与者键（稳定顺序）。
func aiTaskEvidenceTaskKeys(t AITask) ([]string, error) {
	if t.Kind == "roundtable" {
		var keys []string
		if strings.TrimSpace(t.Participants) != "" {
			if err := json.Unmarshal([]byte(t.Participants), &keys); err != nil {
				return nil, fmt.Errorf("ai: 任务参与者不可解析: %w", err)
			}
		}
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			if k = strings.TrimSpace(k); k != "" {
				out = append(out, k)
			}
		}
		if len(out) == 0 {
			return nil, errors.New("ai: 圆桌任务没有参与者")
		}
		return out, nil
	}
	if strings.TrimSpace(t.Agent) == "" {
		return nil, nil
	}
	return []string{strings.TrimSpace(t.Agent)}, nil
}

func aiEvidenceKeys(list []AITaskEvidenceParticipant) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.Key)
	}
	return out
}

// sameAIKeys 比较两组角色键（顺序无关，两组都必须是同一集合）。
func sameAIKeys(want []string, got []AITaskEvidenceParticipant) bool {
	if len(want) != len(got) {
		return false
	}
	seen := make(map[string]int, len(want))
	for _, k := range want {
		seen[k]++
	}
	for _, p := range got {
		if seen[p.Key] == 0 {
			return false
		}
		seen[p.Key]--
	}
	return true
}

// AITaskEvidenceOf 读一条任务的绑定载荷。第二个返回值表示载荷是否存在且可解析。
func (d *DB) AITaskEvidenceOf(ctx context.Context, taskID int64) (AITaskEvidence, bool, error) {
	var raw string
	err := d.SQL.QueryRowContext(ctx,
		`SELECT binding_json FROM ai_task_evidence WHERE task_id=?`, taskID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return AITaskEvidence{}, false, nil
	}
	if err != nil {
		return AITaskEvidence{}, false, err
	}
	var ev AITaskEvidence
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return AITaskEvidence{}, false, err
	}
	if ev.FormatVersion != AITaskEvidenceFormatV1 {
		return AITaskEvidence{}, false, fmt.Errorf("ai: 未知证据格式版 %d", ev.FormatVersion)
	}
	return ev, true, nil
}

// AIResultExpressionOf 读一条结果的表达凭据。
func (d *DB) AIResultExpressionOf(ctx context.Context, resultID int64) (AIResultExpression, bool, error) {
	var e AIResultExpression
	err := d.SQL.QueryRowContext(ctx, `
		SELECT agent_key, generation, br_personality_revision
		FROM ai_result_expression WHERE result_id=?`, resultID).
		Scan(&e.AgentKey, &e.Generation, &e.BRPersonalityRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return AIResultExpression{}, false, nil
	}
	if err != nil {
		return AIResultExpression{}, false, err
	}
	return e, true, nil
}

// AIDeliveryAuthorization 是一条频道结果在最终发出 24/27 前的复核结果。
//
// GS 唯一能自证的部分：从 result_id 回父任务，读原事件类别与逐角色绑定，
// 再用「此刻」的范围、能力与代次复核本句发言者与必要发起者。任何不一致都只
// 落终态，不补发、不改派。
//
// ResultPending 表示读回的这条结果仍待投递（delivered_ts IS NULL）。GS 轮询时
// 已把正文读进内存；若这期间任务被回收重领，同一行会被 AIExpireTasks 置
// delivered_ts=-1 并写 attempt_reclaimed，此时即使证据、范围与能力都还对，
// 也不得把旧句推出去（T-108 §9.2 E4 的发前复核）。
type AIDeliveryAuthorization struct {
	Allowed       bool
	Reason        string
	TaskID        int64
	AgentKey      string
	Category      string
	Direct        bool
	Initiator     string
	Scope         string
	ScopeKey      string
	ScopeChecked  bool
	ResultPending bool
	ResultErr     string
	Bound         AITaskEvidenceParticipant
	Current       aiconfig.CapabilityState
}

// AIDeliveryAuthorization 由 result_id 回父任务复核来源证据。
func (d *DB) AIDeliveryAuthorization(ctx context.Context, resultID int64) (AIDeliveryAuthorization, error) {
	return d.aiDeliveryAuthorization(ctx, resultID, false)
}

// AIWithChannelDelivery 持有与回收共用的锁，复核当前授权后在锁内完成实际发帧。
// continuing 仅由已放行首帧的本次频道投递使用；后续接收者仍逐个复核。
func (d *DB) AIWithChannelDelivery(ctx context.Context, resultID int64, continuing bool,
	deliver func(AIDeliveryAuthorization) error) error {
	unlock, err := d.aiDeliveryReclaimLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	auth, err := d.aiDeliveryAuthorization(ctx, resultID, continuing)
	if err != nil {
		return err
	}
	return deliver(auth)
}

func (d *DB) aiDeliveryAuthorization(ctx context.Context, resultID int64, continuing bool) (AIDeliveryAuthorization, error) {
	out := AIDeliveryAuthorization{Reason: AIEvidenceReasonMatched}
	var taskID, contactUID int64
	var deliveredTS sql.NullFloat64
	// 单频道结果的 contact_uid 只在读侧从父任务补；这里同样回父任务取，
	// 否则会把自己的播放者认成「没绑定」。任务范围也一并取回：撤订阅/改图
	// 只看任务自己的频道，不看结果正文。终态与原因同样在这一条只读里取回，
	// 发前复核据此判断「这条结果是否仍待投递」。
	err := d.SQL.QueryRowContext(ctx, `
		SELECT r.task_id, COALESCE(NULLIF(r.contact_uid,0), t.contact_uid, 0),
		       COALESCE(t.scope,''), COALESCE(t.scope_key,''),
		       r.delivered_ts, COALESCE(r.err,'')
		FROM ai_result r LEFT JOIN ai_task t ON t.id=r.task_id WHERE r.id=?`, resultID).
		Scan(&taskID, &contactUID, &out.Scope, &out.ScopeKey, &deliveredTS, &out.ResultErr)
	if errors.Is(err, sql.ErrNoRows) {
		out.Reason = AITaskEvidenceReasonTaskMissing
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.TaskID = taskID
	out.ResultPending = !deliveredTS.Valid
	ev, ok, err := d.AITaskEvidenceOf(ctx, taskID)
	if err != nil {
		out.Reason = AITaskEvidenceReasonUnreadable
		return out, nil
	}
	if !ok {
		out.Reason = AITaskEvidenceReasonMissing
		return out, nil
	}
	out.Category, out.Direct, out.Initiator = ev.Category, ev.Direct, ev.Initiator
	// 本句发言者：以结果上的 contact_uid 为准，不用任务参与者顺序推断。
	key, found, err := d.aiAgentKeyByContactUID(ctx, contactUID)
	if err != nil {
		return out, err
	}
	if !found {
		out.Reason = AITaskEvidenceReasonNoParticipant
		return out, nil
	}
	out.AgentKey = key
	part := aiEvidenceParticipantOf(ev, key)
	if part == nil {
		out.Reason = AITaskEvidenceReasonNoParticipant
		return out, nil
	}
	out.Bound = *part
	st, err := AIReadCapabilityState(ctx, d.SQL, key)
	if err != nil {
		return out, err
	}
	out.Current = st
	if part.Generation != 0 && st.Generation != part.Generation {
		out.Reason = AITaskEvidenceReasonGeneration
		return out, nil
	}
	if !ev.Direct {
		// T-108 §9.2 E2：投递前按任务 channel 复核当前范围（精确 ∪ 地图 ∪ 全局，
		// 且 enabled ∧ 未删除 ∧ joined）。撤订阅、改到别的图、撤地图级只留全局
		// 都表现为本句发言者或必要发起者不在范围里；此刻就落终态，零新帧。
		out.ScopeChecked = out.Scope == "channel" && out.ScopeKey != ""
		if reason := d.aiDeliveryScopeReason(ctx, out.Scope, out.ScopeKey, ev, key); reason != "" {
			out.Reason = reason
			return out, nil
		}
		if def, ok := aiconfig.CapabilityForEvent(ev.Category); !ok || def.Event == "" {
			out.Reason = AITaskEvidenceReasonUnknownCat
			return out, nil
		}
		if reason := aiEvidenceParticipantHolds(part, st); reason != "" {
			out.Reason = reason
			return out, nil
		}
		// 不是本句发言者发起的来源（例如别人替上鱼者报一句）：发起者也要按原
		// 事件资格和原修订复核，禁止旁观者冒充产鱼者。
		if ev.Initiator != "" && ev.Initiator != key {
			initPart := aiEvidenceParticipantOf(ev, ev.Initiator)
			if initPart == nil {
				out.Reason = AITaskEvidenceReasonInitiator
				return out, nil
			}
			initState, err := AIReadCapabilityState(ctx, d.SQL, ev.Initiator)
			if err != nil {
				return out, err
			}
			if initPart.Generation != 0 && initState.Generation != initPart.Generation {
				out.Reason = AITaskEvidenceReasonInitiator
				return out, nil
			}
			if reason := aiEvidenceParticipantHolds(initPart, initState); reason != "" {
				out.Reason = AITaskEvidenceReasonInitiator
				return out, nil
			}
		}
	}
	// 发前最后一道状态复核：这条结果此刻是否仍待投递。GS 轮询读入正文后、
	// 真正发出 24/27 前，任务可能已被 AIExpireTasks 回收重领，同一行已被
	// 置 delivered_ts=-1 并写下终态原因。此时证据与能力还可能全部对得上，
	// 但旧句不能再发；按该行已记录的终态原因拒绝（没有记录时记
	// result_not_pending），不覆盖原有终态。
	if !out.ResultPending && !(continuing && deliveredTS.Float64 >= 0) {
		if out.ResultErr != "" {
			out.Reason = out.ResultErr
		} else {
			out.Reason = AIEvidenceReasonResultNotPending
		}
		return out, nil
	}
	out.Allowed = true
	return out, nil
}

// aiDeliveryScopeReason 是 GS 投递前的范围复核：本句发言者与必要发起者都必须
// 仍在任务频道的当前范围里。返回空串表示放行。
//
// 只对频道事件生效：直接对话/事件（Direct）与 DM 任务没有频道范围口径，
// 不因撤订阅终止（T-108 §9.2 E2 的例外）。
func (d *DB) aiDeliveryScopeReason(ctx context.Context, scope, scopeKey string,
	ev AITaskEvidence, speaker string) string {
	if scope != "channel" || scopeKey == "" {
		return ""
	}
	keys, err := d.AIEventScopeKeys(ctx, scopeKey)
	if err != nil {
		// 范围证明不了就不放行；原因仍归 out_of_scope（调用方据此落终态）。
		return AIEvidenceReasonOutOfScope
	}
	inScope := make(map[string]bool, len(keys))
	for _, k := range keys {
		inScope[k] = true
	}
	if !inScope[speaker] {
		return AIEvidenceReasonOutOfScope
	}
	if ev.Initiator != "" && ev.Initiator != speaker && !inScope[ev.Initiator] {
		return AIEvidenceReasonOutOfScope
	}
	return ""
}

// aiEvidenceParticipantOf 取载荷里某个角色的绑定条目。
func aiEvidenceParticipantOf(ev AITaskEvidence, key string) *AITaskEvidenceParticipant {
	for i := range ev.Participants {
		if ev.Participants[i].Key == key {
			return &ev.Participants[i]
		}
	}
	return nil
}

// aiEvidenceParticipantHolds 复核一名参与者此刻是否仍按原修订持有原事件能力。
//
// 严格等值：撤权后重新授予会推进修订，旧意图因此同样被拒（T-108 §7）。
func aiEvidenceParticipantHolds(part *AITaskEvidenceParticipant, st aiconfig.CapabilityState) string {
	if part == nil || part.Capability == "" {
		return AITaskEvidenceReasonNoParticipant
	}
	if st.State != aiconfig.CapabilityStateConfigured || !st.Has(part.Capability) {
		return AITaskEvidenceReasonCapability
	}
	if st.Revision != part.CapRevision {
		return AITaskEvidenceReasonRevision
	}
	if def, ok := aiconfig.CapabilityByID(part.Capability); ok && def.RequiresRoundtable &&
		!st.Has(aiconfig.CapabilityJoinRoundtable) {
		return AITaskEvidenceReasonCapability
	}
	return ""
}

// aiAgentKeyByContactUID 按联系人 UID 找角色键（只读，含已停用角色）。
func (d *DB) aiAgentKeyByContactUID(ctx context.Context, contactUID int64) (string, bool, error) {
	return aiAgentKeyByContactUIDQ(ctx, d.SQL, contactUID)
}

// aiAgentKeyByContactUIDQ 是同一查询的具名连接版：读事务里必须走事务连接，
// 单连接池在事务内再开第二条会自锁。
func aiAgentKeyByContactUIDQ(ctx context.Context, q AIQueryer, contactUID int64) (string, bool, error) {
	if contactUID == 0 {
		return "", false, nil
	}
	var key string
	err := q.QueryRowContext(ctx,
		`SELECT key FROM ai_agent WHERE contact_uid=? AND deleted_ts=0 ORDER BY id LIMIT 1`, contactUID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return key, true, nil
}

// AIResultEvidenceReport 是一条结果的表达证据与两端版本比对。
type AIResultEvidenceReport struct {
	ResultID              int64
	AgentKey              string
	Generation            int64
	GSPersonalityRevision *int64
	BRRevision            int64
	HasExpression         bool
	CurrentRevision       int64
	Matched               bool
	Reason                string
}

// AITaskEvidenceReport 是同库只读查询的结果：任务绑定 + 每句表达证据 + 两端版本。
//
// 读在一个只读事务里完成，避免「保存」与「读回」交错时把新版本错标成已生效。
type AITaskEvidenceReport struct {
	TaskID     int64
	TaskFound  bool
	EvidenceOK bool
	Evidence   AITaskEvidence
	Results    []AIResultEvidenceReport
	Matched    bool
	Reason     string
}

// AITaskEvidenceReport 读一条任务的两端同版证据（供管理只读查询与回执核对）。
func (d *DB) AITaskEvidenceReport(ctx context.Context, taskID int64) (AITaskEvidenceReport, error) {
	out := AITaskEvidenceReport{TaskID: taskID}
	tx, err := d.SQL.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	var kind string
	err = tx.QueryRowContext(ctx, `SELECT kind FROM ai_task WHERE id=?`, taskID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		out.Reason = AITaskEvidenceReasonTaskMissing
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.TaskFound = true
	var raw string
	err = tx.QueryRowContext(ctx,
		`SELECT binding_json FROM ai_task_evidence WHERE task_id=?`, taskID).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		out.Reason = AITaskEvidenceReasonMissing
	case err != nil:
		return out, err
	default:
		var ev AITaskEvidence
		if jerr := json.Unmarshal([]byte(raw), &ev); jerr != nil ||
			ev.FormatVersion != AITaskEvidenceFormatV1 {
			out.Reason = AITaskEvidenceReasonUnreadable
		} else {
			out.Evidence = ev
			out.EvidenceOK = true
		}
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT r.id, COALESCE(NULLIF(r.contact_uid,0), t.contact_uid, 0),
		       COALESCE(e.agent_key,''), COALESCE(e.generation,0), COALESCE(e.br_personality_revision,0),
		       (e.result_id IS NOT NULL)
		FROM ai_result r
		LEFT JOIN ai_task t ON t.id=r.task_id
		LEFT JOIN ai_result_expression e ON e.result_id=r.id
		WHERE r.task_id=? ORDER BY r.id`, taskID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	type rowT struct {
		resultID, contactUID int64
		agentKey             string
		generation, brRev    int64
		hasExpr              bool
	}
	var list []rowT
	for rows.Next() {
		var r rowT
		var gen, br int64
		var has bool
		if err := rows.Scan(&r.resultID, &r.contactUID, &r.agentKey, &gen, &br, &has); err != nil {
			return out, err
		}
		r.generation, r.brRev, r.hasExpr = gen, br, has
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	for _, r := range list {
		rep := AIResultEvidenceReport{
			ResultID: r.resultID, AgentKey: r.agentKey, Generation: r.generation,
			BRRevision: r.brRev, HasExpression: r.hasExpr, Reason: AIEvidenceReasonMatched,
		}
		// 发言者以结果上的 contact_uid 为准；表达证据缺角色时按联系人回退核对。
		if rep.AgentKey == "" {
			if key, ok, err := aiAgentKeyByContactUIDQ(ctx, tx, r.contactUID); err == nil && ok {
				rep.AgentKey = key
			}
		}
		var bound *AITaskEvidenceParticipant
		for i := range out.Evidence.Participants {
			if out.Evidence.Participants[i].Key == rep.AgentKey {
				bound = &out.Evidence.Participants[i]
				break
			}
		}
		if bound != nil {
			rep.GSPersonalityRevision = bound.GSPersonalityRevision
		}
		rep.CurrentRevision = revOfPersonality(ctx, tx, rep.AgentKey)
		switch {
		case !out.EvidenceOK:
			rep.Reason = AITaskEvidenceReasonMissing
		case bound == nil:
			rep.Reason = AITaskEvidenceReasonNoParticipant
		case !rep.HasExpression:
			rep.Reason = AITaskEvidenceReasonNoExpression
		case bound.GSPersonalityRevision == nil:
			rep.Reason = AITaskEvidenceReasonNoGSPersona
		case *bound.GSPersonalityRevision != rep.BRRevision:
			rep.Reason = AITaskEvidenceReasonPersonaDiff
		default:
			rep.Matched = true
		}
		out.Results = append(out.Results, rep)
	}
	if !out.EvidenceOK {
		out.Reason = AITaskEvidenceReasonMissing
		return out, nil
	}
	out.Matched = len(out.Results) > 0
	for _, rep := range out.Results {
		if !rep.Matched {
			out.Matched = false
		}
	}
	if out.Matched {
		out.Reason = AIEvidenceReasonMatched
	} else if out.Reason == "" {
		out.Reason = AITaskEvidenceReasonNoExpression
	}
	return out, nil
}

// revOfPersonality 是只读小助手：同一次快照里取某角色已保存的四维修订。
func revOfPersonality(ctx context.Context, q AIQueryer, agentKey string) int64 {
	rev, _, err := aiPersonalityRevisionOf(ctx, q, agentKey)
	if err != nil {
		return 0
	}
	return rev
}

// ---------------------------------------------------------------------------
// T-108 / R-157：按角色当前版本的只读交接口径（供 T-107 管理 GET 消费）
// ---------------------------------------------------------------------------
//
// 与 AITaskEvidenceReport 的分工（两者语义都不变）：
//   - AITaskEvidenceReport 按 task_id 回答「这一条历史任务是否两端同版」；
//   - AIAgentEvidenceReport 按角色回答「此刻这一版有没有被真实消费过」。
// T-107 的 P4 只能在管理端显示「已保存 r」或「已生效 r」；后者必须来自一份
// 真实的 task/result 证据，不能由任务计数、完成数或正文推断。

// AIAgentExpressionEvidence 是一条与某角色绑定的真实表达证据。
//
// GSRevision 是父任务证据里 GS 为这名角色绑定的四维修订；nil 表示 GS 这一次
// 没有用四维做选择（例如基础询答），因此不构成两端同版的结论。
type AIAgentExpressionEvidence struct {
	TaskID     int64
	ResultID   int64
	Generation int64
	GSRevision *int64
	BRRevision int64
	Delivered  bool
}

// AIAgentEvidenceReport 是 T-107 管理 GET 可用的按角色只读投影。
//
// 一次调用只读一个快照：角色当前代次、已保存四维修订和表达证据。
// Evidence 指向最新的有效同版证据；Latest 指向最近一条表达证据，供页面诊断。
type AIAgentEvidenceReport struct {
	AgentKey           string
	AgentFound         bool
	Generation         int64
	PersonalityPresent bool
	CurrentRevision    int64
	Matched            bool
	Reason             string
	Evidence           AIAgentExpressionEvidence
	Latest             AIAgentExpressionEvidence
}

// AIAgentEvidenceReport 在一个只读事务里取「当前代次 + 已保存修订 + 匹配证据」。
//
// 匹配必须同时成立：结果带表达凭据、结果未处于回收/作废终态（delivered_ts
// 不为负）、凭据代次等于当前代次、BR 修订等于当前已保存修订、父任务证据里
// GS 为同一角色绑定的修订等于 BR 修订。任一不成立就不点亮；从新到旧取第一条
// 全部相等的证据，因此同版本旧证据仍然算数（版本没有变，确实被消费过）。
//
// 不写任何表；缺角色、删号/墓碑、无证据、证据不可读或缺 GS 绑定都返回稳定
// 原因码，不猜、不补造。
func (d *DB) AIAgentEvidenceReport(ctx context.Context, agentKey string) (AIAgentEvidenceReport, error) {
	out := AIAgentEvidenceReport{AgentKey: strings.TrimSpace(agentKey)}
	if out.AgentKey == "" {
		out.Reason = AIAgentEvidenceReasonAgentMissing
		return out, nil
	}
	tx, err := d.SQL.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	// 当前代次与存活状态：与 AIGetPersonality/AIGetCapabilities 同一条来源。
	var deletedTS float64
	var generation, tombstoned int64
	err = tx.QueryRowContext(ctx, `
		SELECT a.deleted_ts, COALESCE(c.generation,1), COALESCE(c.tombstoned,0)
		FROM ai_agent a
		LEFT JOIN ai_runtime_control c ON c.agent_key=a.key
		WHERE a.key=?`, out.AgentKey).Scan(&deletedTS, &generation, &tombstoned)
	if errors.Is(err, sql.ErrNoRows) {
		out.Reason = AIAgentEvidenceReasonAgentMissing
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if deletedTS > 0 || tombstoned != 0 {
		out.Reason = AIAgentEvidenceReasonAgentInactive
		return out, nil
	}
	out.AgentFound = true
	out.Generation = generation
	// 已保存修订：直接读 ai_setting 的原始记录（坏记录只要能读出修订号就用它），
	// 与保存侧提交 base_revision 的口径是同一个函数。
	rev, present, err := aiPersonalityRevisionOf(ctx, tx, out.AgentKey)
	if err != nil {
		return out, err
	}
	out.PersonalityPresent, out.CurrentRevision = present, rev

	rows, err := tx.QueryContext(ctx, `
		WITH latest AS (
			SELECT e.result_id FROM ai_result_expression e
			JOIN ai_result r ON r.id=e.result_id
			JOIN ai_task t ON t.id=r.task_id
			WHERE e.agent_key=?
			ORDER BY e.result_id DESC LIMIT 1
		), candidates AS (
			SELECT result_id FROM latest
			UNION
			SELECT result_id FROM ai_result_expression
			WHERE agent_key=? AND generation=? AND br_personality_revision=?
		)
		SELECT r.id, r.task_id, e.generation, e.br_personality_revision,
		       r.delivered_ts, COALESCE(r.err,''), COALESCE(tev.binding_json,'')
		FROM candidates c
		JOIN ai_result_expression e ON e.result_id=c.result_id
		JOIN ai_result r ON r.id=e.result_id
		JOIN ai_task t ON t.id=r.task_id
		LEFT JOIN ai_task_evidence tev ON tev.task_id=r.task_id
		ORDER BY e.result_id DESC`, out.AgentKey, out.AgentKey, out.Generation, out.CurrentRevision)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	firstReason := ""
	scanned := 0
	for rows.Next() {
		var (
			resultID, taskID int64
			gen, brRev       int64
			delivered        sql.NullFloat64
			errText          string
			binding          string
		)
		if err := rows.Scan(&resultID, &taskID, &gen, &brRev, &delivered, &errText, &binding); err != nil {
			return out, err
		}
		ev := AIAgentExpressionEvidence{
			TaskID: taskID, ResultID: resultID, Generation: gen, BRRevision: brRev,
			Delivered: delivered.Valid && delivered.Float64 >= 0,
		}
		gsRev, bindReason := aiAgentEvidenceGSRevision(binding, out.AgentKey)
		ev.GSRevision = gsRev
		reason := ""
		switch {
		case delivered.Valid && delivered.Float64 < 0:
			// 回收/丢弃已把这一行落终态：旧尝试的结果不能点亮任何版本。
			reason = errText
			if reason == "" {
				reason = AIAgentEvidenceReasonResultVoided
			}
		case bindReason != "":
			reason = bindReason
		case ev.Generation != out.Generation:
			reason = AITaskEvidenceReasonGeneration
		case ev.BRRevision != out.CurrentRevision:
			reason = AITaskEvidenceReasonPersonaDiff
		case gsRev == nil:
			reason = AITaskEvidenceReasonNoGSPersona
		case *gsRev != ev.BRRevision:
			reason = AITaskEvidenceReasonPersonaDiff
		}
		if scanned == 0 {
			out.Latest = ev
		}
		scanned++
		if reason == "" {
			out.Evidence = ev
			out.Matched = true
			break
		}
		if firstReason == "" {
			firstReason = reason
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	switch {
	case out.Matched:
		out.Reason = AIEvidenceReasonMatched
	case firstReason != "":
		out.Reason = firstReason
	default:
		out.Reason = AITaskEvidenceReasonNoExpression
	}
	return out, nil
}

// aiAgentEvidenceGSRevision 从父任务绑定里取出该角色的 GS 四维修订。
//
// 第二个返回值为空串表示载荷可读且该角色有绑定（GSRevision 仍可能为 nil，
// 表示这一次 GS 没有用四维）；否则返回缺载荷/不可读/未绑定该角色的原因。
func aiAgentEvidenceGSRevision(bindingJSON, agentKey string) (*int64, string) {
	if strings.TrimSpace(bindingJSON) == "" {
		return nil, AITaskEvidenceReasonMissing
	}
	var ev AITaskEvidence
	if err := json.Unmarshal([]byte(bindingJSON), &ev); err != nil ||
		ev.FormatVersion != AITaskEvidenceFormatV1 {
		return nil, AITaskEvidenceReasonUnreadable
	}
	part := aiEvidenceParticipantOf(ev, agentKey)
	if part == nil {
		return nil, AITaskEvidenceReasonNoParticipant
	}
	return part.GSPersonalityRevision, ""
}

// ---------------------------------------------------------------------------
// T-108 / DR-107：E5 唯一的只读活动事实入口
// ---------------------------------------------------------------------------
//
// competition 与 competition_member 是官方赛事在本服投影的玩家表，写者只有 GS
// （competition_handlers.go → repos_competition.go）。BR 需要活动事实时只允许
// 经本函数读一次同库投影：不新增工具、不改能力目录、不加表、不调用懒结算、
// 不写任何表，也不读 competition_member 给 AI 编造玩家身份。

// AIActivityFactsBudget 是一次只读快照的硬上限：读不到就显式 unknown，不重试。
const AIActivityFactsBudget = 800 * time.Millisecond

// AIActivityCompetitionFact 是一条本地赛事投影的可读事实。
//
// 只含赛事本身的公开字段：id、名称、状态、地图、时长与开始时刻。
// 不含成员、玩家 uid、资格或成绩——本版 AI 没有参赛身份，那些字段一律 unknown。
type AIActivityCompetitionFact struct {
	ID          string
	Name        string
	State       int64
	MapKey      string
	DurationSec int64
	StartedTS   float64
	EndsTS      float64 // 本地预计结束；0 表示投影里没有可算的结束时刻
}

// AIActivityFacts 是一次只读采样的结果：采样时刻、最多 5 条进行中/报名中/未开始
// 的赛事，以及「有一条已过本地预计结束」的陈旧标志。
type AIActivityFacts struct {
	SampledAt    time.Time
	Competitions []AIActivityCompetitionFact
	Stale        bool
}

// AIActivityFacts 是 E5 的唯一允许读路径：单次 SELECT、只读事务、≤800ms。
//
// 读到的只是本地投影：过期的行只说明「本地没结算」，不是官方已结束，
// 也不能当成退出许可。函数本身不结算、不注册事件、不写官方表。
func (d *DB) AIActivityFacts(ctx context.Context) (AIActivityFacts, error) {
	out := AIActivityFacts{SampledAt: time.Now()}
	cctx, cancel := context.WithTimeout(ctx, AIActivityFactsBudget)
	defer cancel()
	tx, err := d.SQL.BeginTx(cctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(cctx, `
		SELECT id, COALESCE(name,''), state, COALESCE(map_key,''),
		       COALESCE(duration_sec,0), COALESCE(started_ts,0)
		FROM competition
		WHERE state IN (?,?,?)
		ORDER BY created_ts DESC LIMIT 5`,
		CompetitionRegistration, CompetitionRunning, CompetitionCreated)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var f AIActivityCompetitionFact
		if err := rows.Scan(&f.ID, &f.Name, &f.State, &f.MapKey,
			&f.DurationSec, &f.StartedTS); err != nil {
			return out, err
		}
		if f.StartedTS > 0 && f.DurationSec > 0 {
			f.EndsTS = f.StartedTS + float64(f.DurationSec)
			if out.SampledAt.After(time.Unix(int64(f.EndsTS), 0)) {
				out.Stale = true
			}
		}
		out.Competitions = append(out.Competitions, f)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}
