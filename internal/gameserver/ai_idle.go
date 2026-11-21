package gameserver

// 闲时搭话：没鱼、频道里也没人说话时，AI 自己找个话头。
//
// 与「上鱼播报」是两条独立通道：
//   - 能力独立（idle_chat），升级时不自动授予，服主显式勾选才生效；
//   - 触发独立（真人静默 + 自己正在钓鱼），不依赖上鱼；
//   - 节奏交给频道级「说话预算」（ai_talk_budget.go），这里只管「该不该、
//     谁来说、说什么」。
//
// 边界：只在钓鱼状态、只在有玩家的频道、只走既有 ai_task/24-27 通道；
// 不新增协议、不写玩家数据、不改 ai_catch 语义。
//
// 为什么静默计时只由真人消息重置：若 AI 自己的消息也重置，它会不断给自己
// 续一段「安静」，越说越密。这里只认人。

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

const (
	aiIdleDefaultSilenceMin = 3
	// 所有频道合计的进程级安全阀；单频道仍受说话预算约束。
	aiIdleGlobalHourly  = 60
	aiIdleProbeInterval = 30 * time.Second
	aiIdleTopicCooldown = 30 * time.Minute
	aiIdleRecentTopics  = 3
	// 距上次上鱼不足这个时间就不搭话：让「没鱼」是真的，而不是刚钓到就自说自话。
	// 不做成 5 分钟：上鱼节拍本身是 3–10 分钟，5 分钟会把闲时通道几乎全部挡掉。
	aiIdleMinSinceCatch = time.Minute
)

// aiIdleTopic 是一个「话头方向」。只给方向不给事实，事实由 GS 注入，
// 这样重复与幻觉都能在服务端控制，而不是靠模型自觉。
type aiIdleTopic struct {
	ID     string
	Prompt string
}

var aiIdleTopics = []aiIdleTopic{
	{ID: "silence", Prompt: "可以提一句频道里安静得有点过分"},
	{ID: "patience", Prompt: "可以说钓鱼就是等，等也是一种本事"},
	{ID: "self", Prompt: "可以自嘲一句，或者给自己打打气"},
	{ID: "ask", Prompt: "可以问一句其他人今天钓得怎么样"},
	{ID: "place", Prompt: "可以提一句这个钓点，但只准说地名"},
	{ID: "smalltalk", Prompt: "随便找个轻松的话头"},
	{ID: "weather", Prompt: "可以随口说一句今天像是什么天气，但不要报具体数据"},
	{ID: "gear", Prompt: "可以聊一句手里的竿或线，但不要报价格"},
	{ID: "tired", Prompt: "可以说坐久了有点累，换个姿势继续"},
	{ID: "fishstory", Prompt: "可以提一句最近钓到的鱼，但不要报精确重量"},
}

type aiIdleState struct {
	mu           sync.Mutex
	lastHumanAt  time.Time
	lastTryAt    time.Time
	topicSeen    map[string]time.Time
	recentTopics []string
}

var aiIdleStates sync.Map // channel -> *aiIdleState

var aiIdleGlobal struct {
	mu        sync.Mutex
	hourStart time.Time
	count     int
}

func aiIdleStateFor(channel string, now time.Time) *aiIdleState {
	if v, ok := aiIdleStates.Load(channel); ok {
		return v.(*aiIdleState)
	}
	st := &aiIdleState{lastHumanAt: now, topicSeen: map[string]time.Time{}}
	v, _ := aiIdleStates.LoadOrStore(channel, st)
	return v.(*aiIdleState)
}

// noteAIHumanMessage 记录一次真人公聊。只有它能重置静默计时。
func (s *Server) noteAIHumanMessage(channel string) {
	if channel == "" {
		return
	}
	now := time.Now()
	st := aiIdleStateFor(channel, now)
	st.mu.Lock()
	st.lastHumanAt = now
	st.mu.Unlock()
}

// aiIdleDue 只回答「真人安静够久了吗」；句数/间隔由说话预算判定。
func aiIdleDue(now time.Time, lastHumanAt time.Time, silence time.Duration) bool {
	if lastHumanAt.IsZero() {
		return true
	}
	return now.Sub(lastHumanAt) >= silence
}

// aiIdlePickTopic 选一个最近没说过、且不在冷却期的话头。
// 全都在冷却时退回「最近没说过」；再退不回就允许重复（宁可重复也不永久沉默）。
func aiIdlePickTopic(now time.Time, seen map[string]time.Time, recent []string, rng *rand.Rand) aiIdleTopic {
	isRecent := func(id string) bool {
		for _, r := range recent {
			if r == id {
				return true
			}
		}
		return false
	}
	cands := make([]aiIdleTopic, 0, len(aiIdleTopics))
	for _, tp := range aiIdleTopics {
		if isRecent(tp.ID) {
			continue
		}
		if t, ok := seen[tp.ID]; ok && now.Sub(t) < aiIdleTopicCooldown {
			continue
		}
		cands = append(cands, tp)
	}
	if len(cands) == 0 {
		for _, tp := range aiIdleTopics {
			if !isRecent(tp.ID) {
				cands = append(cands, tp)
			}
		}
	}
	if len(cands) == 0 {
		cands = aiIdleTopics
	}
	return cands[rng.Intn(len(cands))]
}

// aiIdlePrompt 只注入 GS 确定知道的事实：地点与已经钓了多久。
// 价格、坐标、天气、鱼口一律禁止——这些没有可信来源。
func aiIdlePrompt(loc string, minutes int, topic aiIdleTopic) string {
	lead := "现在频道里安静了一阵"
	if loc != "" {
		lead += "，你在" + loc
	}
	if minutes >= 5 {
		lead += fmt.Sprintf("，已经钓了 %d 分钟", minutes)
	}
	return fmt.Sprintf("（频道里的其他人看不见的提示）%s。%s。"+
		"用你自己的口吻随口说一两句，别像播报；只准提这个地点和你钓了多久，"+
		"不要报价格、坐标、天气、鱼口或别人的数据，也别重复你刚才说过的内容。",
		lead, topic.Prompt)
}

// aiIdleRoundtablePrompt 是多人接话的提示：不绑定某个人的地点，避免把
// 「你在 X 湖」塞给另一个在不同地图的角色。
func aiIdleRoundtablePrompt(topic aiIdleTopic) string {
	return fmt.Sprintf("（频道里的其他人看不见的提示）频道里安静了一阵。%s。"+
		"用你自己的口吻接一两句，可以回应上一位；别像播报，不要报价格、坐标、天气、鱼口或别人的数据。",
		topic.Prompt)
}

func aiIdleMinutesSince(now time.Time, sinceTS float64) int {
	if sinceTS <= 0 {
		return 0
	}
	m := int((float64(now.Unix()) - sinceTS) / 60)
	if m < 0 {
		return 0
	}
	return m
}

// activityIdleFeature 是「闲时搭话」的事实特征：由社交与尝新驱动，与投入无关。
// 投入项留 0，避免把「钓得久」解释成「更想说话」——专注的人本来就不闲聊。
func activityIdleFeature() aiconfig.PersonalityFeature {
	return aiconfig.PersonalityFeature{Social: 0.8, Novelty: 0.45, Investment: 0, Rhythm: 0.35}
}

// aiIdleRoundtableRollForTest 仅测试用：固定「是否升级多人」的抽样。
// 生产恒为 nil，使用真实随机数。
var aiIdleRoundtableRollForTest func() float64

// aiIdleRoundtableChance 是「这一句闲时搭话升级成两人对话」的概率。
// 安静档不升级；越热闹越容易两个人接起来。
func aiIdleRoundtableChance(mood string) float64 {
	switch mood {
	case "lively":
		return 0.25
	case "unlimited":
		return 0.35
	case "custom":
		return 0.15
	case "normal":
		return 0.1
	default:
		return 0
	}
}

func aiIdleGlobalHasRoom(now time.Time) bool {
	aiIdleGlobal.mu.Lock()
	defer aiIdleGlobal.mu.Unlock()
	if aiIdleGlobal.hourStart.IsZero() || now.Sub(aiIdleGlobal.hourStart) >= time.Hour {
		aiIdleGlobal.hourStart = now
		aiIdleGlobal.count = 0
	}
	return aiIdleGlobal.count < aiIdleGlobalHourly
}

func aiIdleGlobalCommit() {
	aiIdleGlobal.mu.Lock()
	aiIdleGlobal.count++
	aiIdleGlobal.mu.Unlock()
}

// aiFishingLastAttemptAt 返回本频道最近一次上鱼尝试的时间；没有状态时为零值。
// 只读，不推进任何状态。
func aiFishingLastAttemptAt(channel string) time.Time {
	v, ok := aiFishingStates.Load(channel)
	if !ok {
		return time.Time{}
	}
	st, ok := v.(*aiFishingChannelState)
	if !ok {
		return time.Time{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastAt
}

// maybeAIIdleChat 在一次 tick 里判断要不要让某个正在钓鱼的角色搭话。
// 返回 true 表示已入队，调用方应跳过本 tick 的上鱼，避免两条消息挤在一起。
func (s *Server) maybeAIIdleChat(ctx context.Context, db *store.DB, channel string,
	uid int64, fishing []aiFishingAgent) bool {

	if db == nil || channel == "" || len(fishing) == 0 {
		return false
	}
	now := time.Now()
	silence := time.Duration(s.aiIntSetting(ctx, "ai_idle_silence_min", aiIdleDefaultSilenceMin, 1, 15)) * time.Minute

	st := aiIdleStateFor(channel, now)
	st.mu.Lock()
	due := aiIdleDue(now, st.lastHumanAt, silence)
	if due && !st.lastTryAt.IsZero() && now.Sub(st.lastTryAt) < aiIdleProbeInterval {
		due = false
	}
	if due {
		st.lastTryAt = now
	}
	st.mu.Unlock()
	if !due {
		return false
	}
	if !aiIdleGlobalHasRoom(now) {
		return false
	}
	// 「没鱼」不是靠感觉：距本频道上一次上鱼太近就先不搭话。
	// 没有钓鱼状态（还没上过鱼）视为一直没口，可以搭话。
	if last := aiFishingLastAttemptAt(channel); !last.IsZero() && now.Sub(last) < aiIdleMinSinceCatch {
		return false
	}

	// 候选 = 已获准「闲时搭话」的角色 ∩ 此刻真的在钓鱼的角色。
	// 前者同时校验订阅范围、接入状态、reply_mode 与能力修订。
	allowed, rejects := s.aiEventCandidates(ctx, channel, aiconfig.EventIdleChat)
	s.logEventRejects(channel, aiconfig.EventIdleChat, rejects)
	if len(allowed) == 0 {
		return false
	}
	canIdle := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		canIdle[a.Key] = struct{}{}
	}
	candidates := make([]aiFishingAgent, 0, len(fishing))
	for _, f := range fishing {
		if _, ok := canIdle[f.agent.Key]; ok {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	// 稳定顺序：同一小时、同一话头下，谁愿意说只由种子与四维决定，可复现。
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].agent.Key < candidates[j].agent.Key })

	rng := rand.New(rand.NewSource(now.UnixNano()))
	st.mu.Lock()
	topic := aiIdlePickTopic(now, st.topicSeen, st.recentTopics, rng)
	st.mu.Unlock()

	seed := aiEventSeed(channel, "idle|"+topic.ID+"|"+strconv.FormatInt(now.Unix()/3600, 10))
	chosen := -1
	var revPtr *int64
	for i := range candidates {
		key := candidates[i].agent.Key
		p, derivable, bindable := s.actorPersonalityBinding(ctx, key)
		if !derivable {
			continue
		}
		want, _ := activityWants(p, seed^store.AIActorSeed(3, key), activityIdleFeature())
		if !want {
			continue
		}
		chosen = i
		revPtr = aiPersonalityRevisionPtr(p, bindable)
		break
	}
	if chosen < 0 {
		return false
	}
	// 与上鱼播报共用频道预算；闲时只花剩余额度，并给上鱼留一句（不限档不留）。
	if !s.aiTalkAllow(ctx, channel, false) {
		return false
	}

	agent := candidates[chosen]
	// 多人接话：只有还有另一个获准「闲时搭话」的钓鱼角色时才可能升级。
	if len(candidates) > 1 {
		cfg := s.aiTalkConfig(ctx)
		roll := rng.Float64()
		if aiIdleRoundtableRollForTest != nil {
			roll = aiIdleRoundtableRollForTest()
		}
		if chance := aiIdleRoundtableChance(cfg.Mood); chance > 0 && roll < chance {
			peer := candidates[(chosen+1+rng.Intn(len(candidates)-1))%len(candidates)]
			parts := []store.AITaskEvidenceParticipant{
				{Key: agent.agent.Key, Capability: aiconfig.CapabilityIdleChat, GSPersonalityRevision: revPtr},
				{Key: peer.agent.Key, Capability: aiconfig.CapabilityIdleChat},
			}
			ev := store.AITaskEvidence{
				FormatVersion: store.AITaskEvidenceFormatV1,
				Category:      aiconfig.EventIdleChat,
				Initiator:     agent.agent.Key,
				Participants:  parts,
			}
			keys := []string{agent.agent.Key, peer.agent.Key}
			if taskID, err := db.AIEnqueueSystemRoundtable(ctx, channel, uid,
				aiIdleRoundtablePrompt(topic), keys, &ev); err == nil {
				aiIdleGlobalCommit()
				s.log.Info("AI 闲时圆桌入队", "agents", keys, "channel", channel,
					"topic", topic.ID, "task_id", taskID)
				return true
			}
			s.log.Warn("AI 闲时圆桌入队失败，退回单人", "agent", agent.agent.Key, "channel", channel)
		}
	}

	text := aiIdlePrompt(s.actorLocName(ctx, agent.actor.Loc),
		aiIdleMinutesSince(now, agent.actor.SinceTS), topic)
	ev := store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventIdleChat,
		Initiator:     agent.agent.Key,
		Participants: []store.AITaskEvidenceParticipant{{
			Key: agent.agent.Key, Capability: aiconfig.CapabilityIdleChat,
			GSPersonalityRevision: revPtr,
		}},
	}
	taskID, err := db.AIEnqueueSystemChannel(ctx, channel, uid, text,
		agent.agent.Key, agent.agent.ContactUID, &ev)
	if err != nil {
		s.log.Warn("AI 闲时搭话入队失败", "agent", agent.agent.Key, "channel", channel, "err", err)
		return false
	}
	aiIdleGlobalCommit()
	s.log.Info("AI 闲时搭话入队", "agent", agent.agent.Key, "channel", channel,
		"topic", topic.ID, "task_id", taskID)
	return true
}
