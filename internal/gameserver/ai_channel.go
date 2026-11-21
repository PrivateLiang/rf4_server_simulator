package gameserver

// 频道订阅与多人触发：哪个频道需要哪个 Agent，就把它拉进来。
// 防循环：AI 回复只经 24/27 推给客户端，永远不会变成 24/10 再触发一轮。

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

var aiChannelCooldown sync.Map // channel|agent -> time.Time

// Agent 名称解析见 ai_contacts.go：查库，不硬编码。
// 默认零接入时，这里不会命中任何 Agent。

// handleAIChannelCommand 执行 @邀请 / @移出 的 AI 侧效果。
//
// 只有「精确解析 + AI 总闸未停止 + 目标是已接入游戏的 Agent」同时成立才算
// 命令：返回 true，并沿用原来的订阅变更与 24/27 回执。其余情况返回 false，
// 调用方按普通公聊广播原消息 —— 命令不再吞掉官方广播。
// 不能用 "/" 前缀：hSocialPublic 会把斜杠开头的消息当 GM 指令丢掉。
func (s *Server) handleAIChannelCommand(ctx *Ctx, msg, channel string) bool {
	cctx, active := s.bindAIIntent(ctx.Sess.pushCtx(), s.DB)
	if !active {
		return false
	}
	a, enable, ok := s.parseAIChannelCommand(cctx, msg)
	if !ok {
		return false
	}
	var text string
	status, err := s.submitAIChannelCommand(cctx, ctx, channel, a.Key, enable)
	switch {
	case err == nil && status == store.AIRuntimeStatusApplied:
		if enable {
			text = "已把 " + a.Display + " 拉进本频道"
		} else {
			text = "已把 " + a.Display + " 移出本频道"
		}
	case err == nil && status == store.AIRuntimeStatusPending:
		// 瞬时 busy/超时：命令已持久化，下一轮消费者会应用，不能谎报已生效。
		text = "操作已受理，稍后生效"
	default:
		if err != nil {
			s.log.Warn("频道订阅命令失败", "channel", channel, "agent", a.Key, "err", err)
		} else {
			s.log.Warn("频道订阅命令被拒绝", "channel", channel, "agent", a.Key, "status", status)
		}
		text = "操作失败，请稍后再试"
	}
	if !s.aiBlockedNow(cctx, s.DB) {
		ctx.Sess.Push(24, 27, buildChatEchoPush("", text, uint64(ctx.Sess.Userid), 1))
	}
	return true
}

// parseAIChannelCommand 精确解析 @邀请 / @移出。
//
// 前缀后的文本必须正好是一个「已接入游戏」Agent 的展示名或 key；多一个字、
// 少一个字、未知名字、AI 总闸停止、Agent 尚未接入都不是命令。这样
// “@邀请码怎么用”之类的普通句子不会再被当成命令吞掉。
func (s *Server) parseAIChannelCommand(ctx context.Context, msg string) (*store.AIAgent, bool, bool) {
	if s.DB == nil {
		return nil, false, false
	}
	enable := false
	var rest string
	switch {
	case strings.HasPrefix(msg, "@邀请"):
		enable = true
		rest = strings.TrimSpace(strings.TrimPrefix(msg, "@邀请"))
	case strings.HasPrefix(msg, "@移出"):
		rest = strings.TrimSpace(strings.TrimPrefix(msg, "@移出"))
	default:
		return nil, false, false
	}
	// 总闸停止时 AI 不参与，整条命令按普通公聊处理（仍会广播）。
	if s.aiBlockedNow(ctx, s.DB) {
		return nil, false, false
	}
	a, ok := s.aiAgentByName(ctx, rest)
	if !ok || !a.Joined {
		return nil, false, false
	}
	return a, enable, true
}

// 频道圆桌的冷却与每小时计数（进程内，重启清零可接受）。
type rtWindow struct {
	start time.Time
	count int
}

var rtStateByChannel = struct {
	sync.Mutex
	last map[string]time.Time
	hour map[string]rtWindow
}{last: map[string]time.Time{}, hour: map[string]rtWindow{}}

func rtAllow(channel string, cooldown time.Duration, hourly int) bool {
	rtStateByChannel.Lock()
	defer rtStateByChannel.Unlock()
	now := time.Now()
	if t, ok := rtStateByChannel.last[channel]; ok && now.Sub(t) < cooldown {
		return false
	}
	w := rtStateByChannel.hour[channel]
	if w.start.IsZero() || now.Sub(w.start) > time.Hour {
		w = rtWindow{start: now}
	}
	if w.count >= hourly {
		rtStateByChannel.hour[channel] = w
		return false
	}
	w.count++
	rtStateByChannel.last[channel] = now
	rtStateByChannel.hour[channel] = w
	return true
}

// aiIntSetting 读整数设置并夹在硬上限内（后台只能调小，不能突破代码上限）。
func (s *Server) aiIntSetting(ctx context.Context, key string, def, lo, hi int) int {
	v, err := strconv.Atoi(s.DB.AISetting(ctx, key, strconv.Itoa(def)))
	if err != nil {
		v = def
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}

// maybeTriggerChannelAI 决定一次真人发言要不要惊动 AI：
//  1. 叫到 ≥2 个助手 → 圆桌（点名优先，不受概率限制）
//  2. 没有范围内的点名 → 按合资格角色的性格判定是否起圆桌
//  3. 未起圆桌 → 优先由被点名的角色直接回答，否则按性格选择合资格角色
//
// 频道的「热闹」主要交给钓鱼事件（见 ai_fishing.go），这里只在有人说话时接话。
// 不推任何「本频道有哪些助手」的公告：客户端本来就会在每条消息前显示说话人的名字，
// 让角色自己在第一次对话时顺口带一句，比系统提示自然。
func (s *Server) maybeTriggerChannelAI(ctx *Ctx, channel, msg string) {
	if s.DB == nil {
		return
	}
	cctx, active := s.bindAIIntent(ctx.Sess.pushCtx(), s.DB)
	if !active {
		return
	}
	if strings.TrimSpace(msg) == "" {
		return
	}
	// T-108：圆桌开通不再读旧全局开关 roundtable_enabled——那是一个既没有页面
	// 入口、又会挡住「后台已按能力开通」的隐形闸。能不能起桌只看范围 + 接入 +
	// 「参加圆桌」能力 + T-103 派生，旧键留在库里只为旧写兼容与页面展示。
	// 点名先按频道范围收窄：joined 名单里不在本频道的角色不能因为被叫到就出现。
	mentions, mentionRejects := s.aiScopeMentions(cctx, channel,
		s.aiAgentMentions(cctx, msg))
	for _, rj := range mentionRejects {
		s.log.Info("频道点名越界", "channel", channel, "agent", rj.AgentKey, "reason", rj.Reason)
	}

	if len(mentions) >= 2 {
		maxSpeakers := s.aiRoundtableProfile(cctx).Speakers
		if len(mentions) > maxSpeakers {
			mentions = mentions[:maxSpeakers]
		}
		if s.startChannelRoundtable(ctx, channel, msg, mentions, cctx) {
			return
		}
		// 冷却/超次数/资格不足：退回单个 Agent 回复，避免完全不响应。
	}

	// 群称和其他未点名的消息共用一次候选筛选与起桌判定。
	if len(mentions) == 0 {
		qualified, rejects := s.aiEventCandidates(cctx, channel, aiconfig.EventRoundtable)
		s.logEventRejects(channel, aiconfig.EventRoundtable, rejects)
		if len(qualified) >= 2 && s.startDerivedRoundtable(ctx, channel, msg, qualified, cctx) {
			return
		}
	}

	// 单人回复：被点名的优先（直接求助不受事件能力限制）；没人叫就按性格挑一个
	// 合资格角色（只有攻略的角色不会被当替补，见 aiPickSpeaker）。
	key, contactUID, cooldownMS, ok := "", int64(0), 8000, false
	direct := false
	if len(mentions) > 0 {
		key, contactUID = mentions[0].Key, mentions[0].ContactUID
		cooldownMS = s.DB.AIChannelCooldownMS(cctx, channel)
		ok = true
		direct = true // 明确直接询答：保住基础路由，不要求事件能力
	} else {
		key, contactUID, cooldownMS, ok = s.aiPickSpeaker(cctx, channel, msg)
	}
	if !ok {
		return
	}
	ck := channel + "|" + key
	if v, ok := aiChannelCooldown.Load(ck); ok {
		if last, ok := v.(time.Time); ok && time.Since(last) < time.Duration(cooldownMS)*time.Millisecond {
			return
		}
	}
	aiChannelCooldown.Store(ck, time.Now())
	task := &store.AITask{
		Kind: "channel", Scope: "channel", ScopeKey: channel,
		UID: ctx.Sess.Userid, Agent: key, ContactUID: contactUID,
		SourceKind: "human", Text: msg,
		Loc:        s.currentLevel(ctx.Sess.Userid),
		DeadlineTS: float64(time.Now().Add(aiTaskTTL).Unix()),
	}
	if direct {
		// 直接询答/DM 维持已批准的基础路由：AIEnqueue 写「直接对话」证据。
		if _, err := s.DB.AIEnqueue(cctx, task); err != nil {
			s.log.Warn("频道 AI 任务入队失败", "channel", channel, "agent", key, "err", err)
		}
		return
	}
	// 自发接话属于主动事件：显式绑定类别与「参加圆桌」能力修订。
	ev := store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventRoundtable,
		Participants: []store.AITaskEvidenceParticipant{
			{Key: key, Capability: aiconfig.CapabilityJoinRoundtable},
		},
	}
	if _, err := s.DB.AIEnqueueTrusted(cctx, task, ev); err != nil {
		s.log.Warn("频道 AI 事件入队失败", "channel", channel, "agent", key, "err", err)
	}
}

// startDerivedRoundtable 用 T-103 的共享派生决定「这一次要不要起桌」。
//
// 每个合资格角色按自己的四维各自回答一次「我现在想不想起个话头」（act / silent
// 两个候选）；只要有一个人想，就按冷却与配额起一轮，否则就是正常冷场。
// 种子由 aiEventSeed 给（测试可固定），因此同一条消息的结果稳定可复现。
func (s *Server) startDerivedRoundtable(ctx *Ctx, channel, msg string,
	qualified []store.AIAgent, cctx context.Context) bool {

	seed := aiEventSeed(channel, msg)
	want := false
	for _, a := range qualified {
		p, derivable := s.actorPersonality(cctx, a.Key)
		if !derivable {
			continue // 显式非法：不产出新的派生决定
		}
		if ok, _ := activityWants(p, seed^store.AIActorSeed(0, a.Key),
			activityGatherFeature()); ok {
			want = true
			break
		}
	}
	if !want {
		return false
	}
	return s.startChannelRoundtable(ctx, channel, msg, qualified, cctx)
}

// logEventRejects 把一次事件筛选的拒绝原因逐条记进日志（回执可逐次核对）。
func (s *Server) logEventRejects(channel, category string, rejects []aiEventReject) {
	for _, rj := range rejects {
		s.log.Info("主动事件候选被拒绝", "channel", channel,
			"category", category, "agent", rj.AgentKey, "reason", rj.Reason)
	}
}

// startChannelRoundtable 在冷却与小时配额允许时入队一次圆桌，返回是否已入队。
// 参与者按「叫到的先后」取前 N 个；没点名时由调用方传入频道内的候选。
//
// T-108：每位参与者都必须已获准参加圆桌；不合格者被剔出，不足两人不起桌。
func (s *Server) startChannelRoundtable(ctx *Ctx, channel, msg string, speakers []store.AIAgent, cctx context.Context) bool {
	speakers, rejects := s.aiEventGroup(cctx, speakers, aiconfig.EventRoundtable)
	s.logEventRejects(channel, aiconfig.EventRoundtable, rejects)
	rt := s.aiRoundtableProfile(cctx)
	if len(speakers) > rt.Speakers {
		speakers = speakers[:rt.Speakers]
	}
	if len(speakers) < 2 {
		return false
	}
	maxMS := rt.MaxMS
	cooldownMS := time.Duration(rt.CooldownSec) * time.Second
	hourly := rt.Hourly
	if !rtAllow(channel, cooldownMS, hourly) {
		return false
	}
	keys := make([]string, 0, len(speakers))
	parts := make([]store.AITaskEvidenceParticipant, 0, len(speakers))
	for _, a := range speakers {
		keys = append(keys, a.Key)
		parts = append(parts, store.AITaskEvidenceParticipant{
			Key: a.Key, Capability: aiconfig.CapabilityJoinRoundtable,
		})
	}
	raw, _ := json.Marshal(keys)
	if _, err := s.DB.AIEnqueueTrusted(cctx, &store.AITask{
		Kind: "roundtable", Scope: "channel", ScopeKey: channel,
		UID: ctx.Sess.Userid, SourceKind: "human", Text: msg,
		Participants: string(raw),
		Loc:          s.currentLevel(ctx.Sess.Userid),
		DeadlineTS:   float64(time.Now().Add(time.Duration(maxMS+5000) * time.Millisecond).Unix()),
	}, store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventRoundtable,
		Participants:  parts,
	}); err != nil {
		s.log.Warn("圆桌任务入队失败", "channel", channel, "err", err)
		return false
	}
	return true
}

// 测试屏障分别位于最终投递锁之前和持锁发帧之前。
var aiChannelAuthorizedForTest, aiChannelWriteForTest func()

// deliverAIChannel 把 AI 回复按频道推给同频道在线会话。
// 外层 locationId 必须空串，客户端才不会静默丢弃。
// 第二返回值表示「最终推送边界拒绝或观察到急停，本条未推送」。
func (s *Server) deliverAIChannel(ctx context.Context, db *store.DB, r store.AIResult) (bool, bool, error) {
	if r.ScopeKey == "" || r.ContactUID == 0 {
		return false, false, nil
	}
	// T-108 投递前复核第一层：角色必须仍然存在且启用，否则这条不再进频道。
	agent, agentOK := s.aiAgentByContactUIDAny(ctx, r.ContactUID)
	if !agentOK || !agent.Enabled {
		s.log.Info("AI 频道投递跳过", "result", r.ID, "task", r.TaskID,
			"contact", r.ContactUID, "reason", "agent_unavailable")
		_ = db.AIDropResultWithReason(ctx, r.ID, "agent_unavailable")
		return false, true, nil
	}
	s.noteFishingStage("delivery", agent.Key, r.TaskID)
	s.mu.Lock()
	peers := make([]*Session, 0, 8)
	for _, sess := range s.sessions {
		if sess != nil && !sess.Closed() && sessChannel(sess).Name == r.ScopeKey {
			peers = append(peers, sess)
		}
	}
	s.mu.Unlock()
	sent := 0
	n := uint32(len(peers))
	for _, sess := range peers {
		if aiDeliveryBarrierForTest != nil {
			aiDeliveryBarrierForTest()
		}
		if aiChannelAuthorizedForTest != nil {
			aiChannelAuthorizedForTest()
		}
		blocked := false
		err := db.AIWithChannelDelivery(ctx, r.ID, sent > 0, func(auth store.AIDeliveryAuthorization) error {
			if !auth.Allowed {
				s.log.Info("AI 频道投递被证据拒绝", "result", r.ID, "task", r.TaskID,
					"agent", auth.AgentKey, "category", auth.Category, "reason", auth.Reason,
					"bound_revision", auth.Bound.CapRevision, "current_revision", auth.Current.Revision,
					"generation", auth.Bound.Generation, "state", auth.Current.State)
				blocked = true
				return db.AIDropResultWithReason(ctx, r.ID, auth.Reason)
			}
			reading := db.AIEmergencyState(ctx)
			if reading.Err != nil && sent == 0 {
				return reading.Err
			}
			d := aiconfig.EvaluateEmergency(reading)
			if active, _ := d.IntentActive(r.IntentEpoch); !active {
				blocked = true
				return nil
			}
			if aiChannelWriteForTest != nil {
				aiChannelWriteForTest()
			}
			sess.Push(24, 27, buildChatEchoPush("", r.ReplyText, uint64(r.ContactUID), n))
			sent++
			if sent == 1 {
				// 首帧和消费标记都在回收锁内。即使轮询取消，也完成已放行首帧的记账。
				return db.AIMarkDelivered(context.WithoutCancel(ctx), r.ID, true)
			}
			return nil
		})
		if err != nil {
			return false, false, err
		}
		if blocked {
			if sent > 0 {
				s.log.Info("AI 频道投递部分放行", "id", r.ID, "released", sent, "recipients", n)
				return true, false, nil
			}
			return false, true, nil
		}
	}
	return true, false, nil
}
