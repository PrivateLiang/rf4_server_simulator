package gameserver

// AI 自己上鱼后主动开口说话。
//
// 这条链路原本完全静默：ai_fishing.go 只把渔获写进 ai_catch 表，
// 不发任何频道消息。玩家看不见，模型也不知道自己上过鱼。
//
// 这里复用「玩家上星反应」那套投递通道（AIEnqueueSystemChannel →
// ai_result → deliverAIChannel → 24/27），只是把主语换成 AI 自己。
// 不碰协议、不碰客户端、不自己造文案，让模型按该角色的人设说。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

// aiFishingTalkPrompt 生成发给模型的提示词。主语必须是「你自己钓上来的」，
// 否则模型会当成旁观者去评价别人的鱼。
//
// 刻意不给精确重量：陪玩不是播报，钓友上鱼时会喊「好家伙，这条不小」，
// 不会念「1.12kg」。玩家想知道具体数字可以自己去查，或事后问。
func aiFishingTalkPrompt(fish aiFishTemplate) string {
	var size string
	switch {
	case fish.WeightG >= 5000:
		size = "个头挺吓人"
	case fish.WeightG >= 2000:
		size = "个头不小"
	case fish.WeightG >= 800:
		size = "还算像样"
	default:
		size = "不大"
	}
	var tone string
	switch fish.Event {
	case evSuper:
		tone = "上蓝冠了，是条了不起的大鱼"
	case evStar:
		tone = "上星了，值得高兴"
	case evNear:
		tone = "差一点点就上星，有点可惜"
	case evMiss:
		tone = "个头不怎么样，可以自嘲一句"
	default:
		tone = "顺口提一句"
	}
	return fmt.Sprintf(
		"（频道里的其他人看不见的提示）你刚钓上一条%s的%s，%s。"+
			"用你的口吻随口说一句就行，别报精确数字、别像念播报稿。",
		size, fish.Name, tone)
}

// maybeAIFishingTalk 在一次虚拟上鱼完成后决定是否开口。
//
// 返回入队的任务 id 与是否已入队（调用方据此记 enqueue 段）。
//
// T-108 / DEV-113 A：这条链只做「说还是不说」的 T-103 派生，配额已在调用方
// 按旧时点预占；旧 ai_fishing_talk_*_chance 不再参与（行为概率归性格）。
// 普通达标（ok）恒静默——没有候选，不进派生。
func (s *Server) maybeAIFishingTalk(ctx context.Context, db *store.DB, channel string,
	uid int64, agent store.AIAgent, fish aiFishTemplate, seed int64) (int64, bool) {

	ctx, active := s.bindAIIntent(ctx, db)
	if !active {
		return 0, false
	}
	if fish.Event == evOk {
		return 0, false
	}
	p, derivable, bindable := s.actorPersonalityBinding(ctx, agent.Key)
	if !derivable {
		return 0, false
	}
	want, _ := activityWants(p, seed, activityCatchFeature(fish.Event))
	if !want {
		return 0, false
	}
	// 与闲时搭话、圆桌共用频道预算；上鱼播报优先，且不看最小间隔。
	if !s.aiTalkAllow(ctx, channel, true) {
		return 0, false
	}
	taskID, err := s.aiFishingTalkEnqueue(ctx, db, channel, uid, agent, fish, seed,
		aiPersonalityRevision(p, bindable))
	if err != nil {
		return 0, false
	}
	return taskID, true
}

// aiPersonalityRevision 返回可绑定的修订指针；不能证明时返回 0（不绑定）。
func aiPersonalityRevision(cfg aiconfig.PersonalityConfig, bindable bool) int64 {
	if p := aiPersonalityRevisionPtr(cfg, bindable); p != nil {
		return *p
	}
	return 0
}

// aiFishingTalkEnqueue 是「AI 自己上鱼后说一句」的唯一投递实现：
// 文案、上蓝圆桌分支与 ai_task 入队都收在这里，随机播报
// maybeAIFishingTalk 与 T-027 测试钩子共用，避免两套路径漂移。
// 返回入队的频道任务 id；走上蓝圆桌时返回 0。
//
// gsPersonalityRevision 是本次「说还是不说」实际用到的四维修订；0 表示没有
// 可证明的修订（测试钩子不经过四维派生，因此不绑版本）。
var aiSystemEnqueueBarrierForTest func()

func (s *Server) aiFishingTalkEnqueue(ctx context.Context, db *store.DB, channel string,
	uid int64, agent store.AIAgent, fish aiFishTemplate, seed int64,
	gsPersonalityRevision int64) (int64, error) {

	// 先把原意图绑进 context：入队发生在所有屏障之后，若不绑定，停前发出的
	// 请求会以停后的代次入队（T-102 的 1→0 语义）。
	ctx, active := s.bindAIIntent(ctx, db)
	if !active {
		return 0, store.ErrAIIntentInactive
	}

	if aiSystemEnqueueBarrierForTest != nil {
		aiSystemEnqueueBarrierForTest()
	}

	// T-108：投递前复核「播报自己渔获」这一事件能力。钩子与随机播报共用本函数，
	// 因此任何一条路径都不能绕过能力集合；撤权后连文案都不生成。
	if _, err := s.aiEventAuthorized(ctx, aiconfig.EventBroadcastCatch, agent.Key); err != nil {
		s.log.Info("AI 上鱼播报被拒绝", "agent", agent.Key, "channel", channel, "reason", err.Error())
		return 0, err
	}

	text := aiFishingTalkPrompt(fish)
	var revPtr *int64
	if gsPersonalityRevision > 0 {
		rev := gsPersonalityRevision
		revPtr = &rev
	}

	// 上蓝可按 T-103 派生升级成三人圆桌，复用 rtAllow 的冷却与每小时上限。
	if fish.Event == evSuper {
		if s.tryAIFishingRoundtable(ctx, db, channel, uid, agent, text, seed, revPtr) {
			return 0, nil
		}
	}

	ev := store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventBroadcastCatch,
		Initiator:     agent.Key,
		Participants: []store.AITaskEvidenceParticipant{{
			Key: agent.Key, Capability: aiconfig.CapabilityBroadcastCatch,
			GSPersonalityRevision: revPtr,
		}},
	}
	taskID, err := db.AIEnqueueSystemChannel(ctx, channel, uid, text, agent.Key, agent.ContactUID, &ev)
	if err != nil {
		s.log.Warn("AI 上鱼播报入队失败", "agent", agent.Key, "channel", channel, "err", err)
		return 0, err
	}
	s.log.Info("AI 上鱼播报入队", "agent", agent.Key, "channel", channel,
		"fish", fish.Name, "event", string(fish.Event), "task_id", taskID)
	return taskID, nil
}

// tryAIFishingRoundtable 上蓝时把参与者凑成 3 人（上鱼者 + 频道内其他 2 人）。
// 圆桌冷却中、或发起者这一次不想升级时返回 false，由调用方退回单人发言。
//
// 参与者必须已获准「参加圆桌」（DEV-113 A 的围观筛范围及圆桌资格）；
// 要不要升级由发起者按 T-103 派生回答一次，不再用 ai_fishing_roundtable_chance。
// T-108：旧全局开关 roundtable_enabled 不再读取——能不能开只看能力与派生。
func (s *Server) tryAIFishingRoundtable(ctx context.Context, db *store.DB, channel string,
	uid int64, speaker store.AIAgent, text string, seed int64, initiatorRev *int64) bool {

	peers, rejects := s.aiEventCandidates(ctx, channel, aiconfig.EventRoundtable)
	s.logEventRejects(channel, aiconfig.EventRoundtable, rejects)
	rt := s.aiRoundtableProfile(ctx)
	keys := make([]string, 0, rt.Speakers)
	allowed := map[string]bool{}
	for _, a := range peers {
		allowed[a.Key] = true
	}
	if !allowed[speaker.Key] {
		return false
	}
	keys = append(keys, speaker.Key)
	for _, a := range peers {
		if a.Key == speaker.Key {
			continue
		}
		keys = append(keys, a.Key)
		if len(keys) >= rt.Speakers {
			break
		}
	}
	if len(keys) < 2 {
		return false
	}
	p, derivable, bindable := s.actorPersonalityBinding(ctx, speaker.Key)
	if !derivable {
		return false
	}
	want, _ := activityWants(p, seed^store.AIActorSeed(2, speaker.Key), activityGatherFeature())
	if !want {
		return false
	}
	cooldown := time.Duration(rt.CooldownSec) * time.Second
	hourly := rt.Hourly
	if !rtAllow(channel, cooldown, hourly) {
		return false
	}
	// 逐角色绑定：发起者按「播报自己渔获」+ 原四维修订，其余旁观者只按
	// 「参加圆桌」资格（T-108 §7「旁观者只按其圆桌角色资格」）。
	parts := make([]store.AITaskEvidenceParticipant, 0, len(keys))
	for _, key := range keys {
		if key == speaker.Key {
			rev := initiatorRev
			if rev == nil {
				rev = aiPersonalityRevisionPtr(p, bindable)
			}
			parts = append(parts, store.AITaskEvidenceParticipant{
				Key: key, Capability: aiconfig.CapabilityBroadcastCatch, GSPersonalityRevision: rev,
			})
			continue
		}
		parts = append(parts, store.AITaskEvidenceParticipant{
			Key: key, Capability: aiconfig.CapabilityJoinRoundtable,
		})
	}
	ev := store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventBroadcastCatch,
		Initiator:     speaker.Key,
		Participants:  parts,
	}
	// AIEnqueueSystemRoundtable 的 participants 是全部参与者（至少 2 个），
	// handleRoundtable 会按 rtMentions 决定谁先开口，上鱼者没有被点名时
	// 仍会按活跃度被抽中，所以直接把完整名单传进去。
	if _, err := db.AIEnqueueSystemRoundtable(ctx, channel, uid, text, keys, &ev); err != nil {
		s.log.Warn("AI 上鱼圆桌入队失败", "channel", channel, "err", err)
		return false
	}
	s.log.Info("AI 上鱼圆桌入队", "channel", channel, "agents", keys)
	return true
}

// ---------------------------------------------------------------------------
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
//
// 后台「AI 虚拟钓鱼」页的「发鱼测试」按钮：让指定 Agent 立刻走一次
// 「AI 自己上鱼 → 主动播报」。只跳过概率门槛与 actor 冷却；文案选择、
// ai_task 入队、ai_result、24/27 投递全部复用上面的既有路径。
//
// 跨进程方式：admin 写一行共享 KV 请求，唯一活动的 gameserver 在
// 1s 循环里消费。不新增表/迁移，也不占用 T-020 的 ai_runtime_command
// 有限命令表（那是运行态契约，本钩子只读运行态、只写 ai_task）。
// 删除清单见 spec，v2 收工时连同 KV 行一起摘除。
// ---------------------------------------------------------------------------

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
const (
	// AIFishingTalkTestKVKey 是 admin 与 gameserver 之间唯一的测试请求/结果行。
	AIFishingTalkTestKVKey = "t027:fishing-talk-test"

	aiFishingTalkTestTTL      = 2 * time.Minute
	aiFishingTalkTestInterval = time.Second

	aiFishingTalkTestStatusPending  = "pending"
	aiFishingTalkTestStatusEnqueued = "enqueued"
	aiFishingTalkTestStatusRejected = "rejected"
)

// AIFishingTalkTestReject 是强制前置校验失败的原因。
// Code 由后台映射成 HTTP 状态；Message 直接回显，不静默降级。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
type AIFishingTalkTestReject struct {
	Code    string
	Message string
}

// AIFishingTalkTestState 是一次测试触发的请求/结果。
// pending 由 admin 写入；enqueued / rejected 由 gameserver 消费后写回。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
type AIFishingTalkTestState struct {
	RequestID   string  `json:"request_id"`
	AgentKey    string  `json:"agent_key"`
	CreatedTS   float64 `json:"created_ts"`
	IntentEpoch int64   `json:"intent_epoch,omitempty"`
	Status      string  `json:"status"`
	Reason      string  `json:"reason,omitempty"`
	Message     string  `json:"message,omitempty"`
	TaskID      int64   `json:"task_id,omitempty"`
	Channel     string  `json:"channel,omitempty"`
	FishKey     string  `json:"fish_key,omitempty"`
	FishName    string  `json:"fish_name,omitempty"`
	Event       string  `json:"event,omitempty"`
	FinishedTS  float64 `json:"finished_ts,omitempty"`
}

// AIFishingTalkTestPrecheck 是测试钩子的强制前置校验，admin 与 gameserver
// 各跑一次（GS 侧防止点击后开关被改的旧请求）。返回 nil 表示可以触发。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func AIFishingTalkTestPrecheck(ctx context.Context, db *store.DB, agentKey string) (*store.AIAgent, *AIFishingTalkTestReject) {
	agentKey = strings.TrimSpace(agentKey)
	if db == nil || agentKey == "" || len(agentKey) > 64 {
		return nil, &AIFishingTalkTestReject{Code: "bad_agent_key", Message: "agent_key 不合法"}
	}
	agent, err := db.AIAgentByKey(ctx, agentKey)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && agent == nil) {
		return nil, &AIFishingTalkTestReject{Code: "agent_missing", Message: "Agent 不存在或已删除"}
	}
	if err != nil {
		return nil, &AIFishingTalkTestReject{Code: "db_error", Message: "Agent 读取失败"}
	}
	if !agent.Enabled {
		return nil, &AIFishingTalkTestReject{Code: "agent_disabled", Message: "该 Agent 已停用"}
	}
	if !agent.Joined {
		return nil, &AIFishingTalkTestReject{Code: "agent_not_joined", Message: "该 Agent 尚未接入游戏"}
	}
	// T-108：钩子不得绕过事件能力。资格只认 T-104 的能力集合
	// （播报自己渔获；按目录还要求圆桌资格），旧 ai_fishing_talk_enabled 与
	// 角色级 fishing_talk_enabled 不再作为放行条件。
	capState, capErr := store.AIReadCapabilityState(ctx, db.SQL, agent.Key)
	if capErr != nil {
		return nil, &AIFishingTalkTestReject{Code: "capability_unreadable", Message: "该 Agent 的能力配置读取失败"}
	}
	if err := authorizeEventState(capState, aiconfig.EventBroadcastCatch, agent.Key); err != nil {
		return nil, &AIFishingTalkTestReject{Code: "capability_missing",
			Message: "该 Agent 未获准「播报自己渔获」（或配置已撤权/已过期），不能触发测试发鱼"}
	}
	if blocked, _ := aiconfig.EvaluateEmergency(db.AIEmergencyState(ctx)).RecordBlocked(); blocked {
		return nil, &AIFishingTalkTestReject{Code: "emergency_stop", Message: "紧急停止已开启"}
	}
	// V2 只留急停：旧 outbound 授权与四项 consent 不再阻止播报/外发。
	return agent, nil
}

// RequestAIFishingTalkTest 由 admin 调用：先做强制前置校验，再写 pending 请求。
// 校验失败不写任何数据，直接返回原因（后台据此返回 4xx）。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func RequestAIFishingTalkTest(ctx context.Context, db *store.DB, agentKey, requestID string) (*AIFishingTalkTestState, *AIFishingTalkTestReject) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 200 {
		return nil, &AIFishingTalkTestReject{Code: "bad_request_id", Message: "request_id 不合法"}
	}
	if db == nil {
		_, rej := AIFishingTalkTestPrecheck(ctx, db, agentKey)
		return nil, rej
	}
	dec := aiconfig.EvaluateEmergency(db.AIEmergencyState(ctx))
	if blocked, reason := dec.RecordBlocked(); blocked {
		return nil, &AIFishingTalkTestReject{Code: "emergency_stop", Message: "紧急停止：" + reason}
	}
	if existing, ok, _ := GetAIFishingTalkTest(ctx, db, requestID); ok {
		return existing, nil
	}
	agent, rej := AIFishingTalkTestPrecheck(ctx, db, agentKey)
	if rej != nil {
		return nil, rej
	}
	state := &AIFishingTalkTestState{
		RequestID:   requestID,
		AgentKey:    agent.Key,
		IntentEpoch: dec.Epoch,
		CreatedTS:   float64(time.Now().Unix()),
		Status:      aiFishingTalkTestStatusPending,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, &AIFishingTalkTestReject{Code: "store_error", Message: "触发请求序列化失败"}
	}
	err = db.AIWithRuntimeTx(ctx, func(tx *store.AIRuntimeTx) error {
		r := store.ReadAIEmergency(ctx, tx.SQL())
		if r.Err != nil {
			return r.Err
		}
		if active, reason := aiconfig.EvaluateEmergency(r).IntentActive(state.IntentEpoch); !active {
			return fmt.Errorf("%w: %s", store.ErrAIIntentInactive, reason)
		}
		var prior string
		err := tx.SQL().QueryRowContext(ctx, `SELECT v FROM kv WHERE k=?`, AIFishingTalkTestKVKey).Scan(&prior)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var existing AIFishingTalkTestState
		if json.Unmarshal([]byte(prior), &existing) == nil && existing.RequestID == requestID {
			state = &existing
			return nil
		}
		_, err = tx.SQL().ExecContext(ctx, `INSERT INTO kv(k,v) VALUES(?,?)
			ON CONFLICT(k) DO UPDATE SET v=excluded.v`, AIFishingTalkTestKVKey, string(raw))
		return err
	})
	if errors.Is(err, store.ErrAIIntentInactive) {
		return nil, &AIFishingTalkTestReject{Code: "emergency_stop", Message: "紧急停止代次已变化，请重新触发"}
	}
	if err != nil {
		return nil, &AIFishingTalkTestReject{Code: "store_error", Message: "触发请求写入失败"}
	}
	return state, nil
}

// GetAIFishingTalkTest 由 admin 轮询：只返回同一个 request_id 的结果，
// 旧 request_id 不会读到后来覆盖的请求。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func GetAIFishingTalkTest(ctx context.Context, db *store.DB, requestID string) (*AIFishingTalkTestState, bool, error) {
	if db == nil || strings.TrimSpace(requestID) == "" {
		return nil, false, nil
	}
	raw, ok, err := db.KVGet(ctx, AIFishingTalkTestKVKey)
	if errors.Is(err, sql.ErrNoRows) || !ok {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var state AIFishingTalkTestState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, false, err
	}
	if state.RequestID != strings.TrimSpace(requestID) {
		return nil, false, nil
	}
	return &state, true, nil
}

// startAIFishingTalkTest 启动测试请求消费者；挂在 StartAIFishing 生命周期上。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func (s *Server) startAIFishingTalkTest(db *store.DB, stop <-chan struct{}) {
	if db == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(aiFishingTalkTestInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.consumeAIFishingTalkTest(ctx, db)
				cancel()
			}
		}
	}()
}

// consumeAIFishingTalkTest 消费一条 pending 测试请求：重跑前置校验 →
// 找一个该 Agent 可见且有在线玩家的频道 → 复用 aiFishingTalkEnqueue 入队。
// 任一环节失败都写回 rejected 原因，且不产生 ai_task 行。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func (s *Server) consumeAIFishingTalkTest(ctx context.Context, db *store.DB) {
	if db == nil {
		return
	}
	raw, ok, err := db.KVGet(ctx, AIFishingTalkTestKVKey)
	if err != nil || !ok {
		return
	}
	var state AIFishingTalkTestState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return
	}
	if state.Status != aiFishingTalkTestStatusPending || state.RequestID == "" {
		return
	}
	dec := s.aiEmergencyNow(ctx, db)
	if dec.Fault == aiconfig.FaultReadError {
		return
	}
	if active, reason := dec.IntentActive(state.IntentEpoch); !active {
		s.finishAIFishingTalkTest(ctx, db, &state, aiFishingTalkTestStatusRejected, reason, "触发请求的急停代次已失效")
		return
	}
	ctx = store.AIIntentContext(ctx, state.IntentEpoch)
	now := float64(time.Now().Unix())
	if state.CreatedTS > 0 && now-state.CreatedTS > aiFishingTalkTestTTL.Seconds() {
		s.finishAIFishingTalkTest(ctx, db, &state, aiFishingTalkTestStatusRejected,
			"expired", "触发请求已过期（gameserver 未及时消费）")
		return
	}
	agent, rej := AIFishingTalkTestPrecheck(ctx, db, state.AgentKey)
	if rej != nil {
		s.finishAIFishingTalkTest(ctx, db, &state, aiFishingTalkTestStatusRejected, rej.Code, rej.Message)
		return
	}
	channel, uid, ok := s.aiFishingTalkTestChannel(ctx, db, agent.Key)
	if !ok {
		s.finishAIFishingTalkTest(ctx, db, &state, aiFishingTalkTestStatusRejected,
			"no_online_channel", "该 Agent 不在任何有在线玩家的频道（检查「范围订阅」与「接入游戏」）")
		return
	}
	// 鱼必须来自该频道所在地图的真实 roster；地图解析不出来时退回 Agent 当前水域。
	loc, _ := db.ChannelMapKey(ctx, channel)
	if loc == "" {
		if actor, aerr := db.AIGetActor(ctx, agent.Key); aerr == nil && actor != nil {
			loc = actor.Loc
		}
	}
	if loc == "" {
		s.finishAIFishingTalkTest(ctx, db, &state, aiFishingTalkTestStatusRejected,
			"no_loc", "无法确定该频道的鱼群地图")
		return
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	fish, ok := s.pickAIFish(ctx, rng, loc, evStar)
	if !ok {
		s.finishAIFishingTalkTest(ctx, db, &state, aiFishingTalkTestStatusRejected,
			"no_fish_available", "该地图没有可用的鱼（静态鱼表缺数据）")
		return
	}
	taskID, err := s.aiFishingTalkEnqueue(ctx, db, channel, uid, *agent, fish,
		aiEventSeed(channel, "test-hook|"+agent.Key), 0)
	if err != nil {
		s.finishAIFishingTalkTest(ctx, db, &state, aiFishingTalkTestStatusRejected,
			"enqueue_failed", "播报任务入队失败")
		return
	}
	state.TaskID = taskID
	state.Channel = channel
	state.FishKey = fish.FishKey
	state.FishName = fish.Name
	state.Event = string(fish.Event)
	s.finishAIFishingTalkTest(ctx, db, &state, aiFishingTalkTestStatusEnqueued, "", "")
	s.log.Info("T-027 发鱼测试已入队", "agent", agent.Key, "channel", channel,
		"task", taskID, "fish", fish.Name)
}

// aiFishingTalkTestChannel 选一个「该 Agent 可见且有在线玩家」的频道。
// 可见性沿用 AIChannelSubscribed（地图级 ∪ 精确 ∪ 全局），与正常播报同源。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func (s *Server) aiFishingTalkTestChannel(ctx context.Context, db *store.DB, agentKey string) (string, int64, bool) {
	s.mu.Lock()
	channels := map[string]int64{}
	for _, sess := range s.sessions {
		if sess != nil && !sess.Closed() && sess.Userid > 0 {
			channels[sessChannel(sess).Name] = sess.Userid
		}
	}
	s.mu.Unlock()
	names := make([]string, 0, len(channels))
	for name := range channels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		keys, err := db.AIChannelSubscribed(ctx, name)
		if err != nil {
			continue
		}
		for _, key := range keys {
			if key == agentKey {
				return name, channels[name], true
			}
		}
	}
	return "", 0, false
}

// finishAIFishingTalkTest 把结果写回同一行 KV；失败只记日志，不影响游戏。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func (s *Server) finishAIFishingTalkTest(ctx context.Context, db *store.DB,
	state *AIFishingTalkTestState, status, reason, message string) {

	state.Status = status
	state.Reason = reason
	state.Message = message
	if status != aiFishingTalkTestStatusPending {
		state.FinishedTS = float64(time.Now().Unix())
	}
	raw, err := json.Marshal(state)
	if err != nil {
		s.log.Warn("T-027 发鱼测试结果序列化失败", "err", err)
		return
	}
	if _, err := db.SQL.ExecContext(ctx, `UPDATE kv SET v=? WHERE k=? AND
		CASE WHEN json_valid(v) THEN json_extract(v,'$.request_id')=? AND json_extract(v,'$.status')='pending' ELSE 0 END`,
		string(raw), AIFishingTalkTestKVKey, state.RequestID); err != nil {
		s.log.Warn("T-027 发鱼测试结果写入失败", "err", err)
	}
}
