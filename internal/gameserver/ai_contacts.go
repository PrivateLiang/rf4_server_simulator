package gameserver

// AI 联系人的查询封装。代码里不再保存任何 Agent 常量：
// 后台创建 Agent 并点「接入游戏」后才进入社交列表、才接收 19/1 私聊。

import (
	"context"
	"sort"
	"strings"
	"sync"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

func (s *Server) aiAgentByContactUID(ctx context.Context, uid int64) (*store.AIAgent, bool) {
	if s.DB == nil {
		return nil, false
	}
	a, err := s.DB.AIAgentByContactUID(ctx, uid)
	if err != nil || a == nil {
		return nil, false
	}
	return a, true
}

// aiAgentByContactUIDAny 按联系人 UID 查角色，不要求「已接入游戏」。
//
// 用途只有一处：频道投递前复核「这个发送者是否还存在且启用」。投递端不该
// 因为角色改了接入状态就吞掉一条已经生成的消息（V2 只留急停，DEV-104），
// 但删除/停用后不能再替它发话。
func (s *Server) aiAgentByContactUIDAny(ctx context.Context, uid int64) (*store.AIAgent, bool) {
	if s.DB == nil || uid == 0 {
		return nil, false
	}
	for _, a := range s.aiAllAgents(ctx) {
		if a.ContactUID == uid {
			return &a, true
		}
	}
	return nil, false
}

func (s *Server) aiJoinedAgents(ctx context.Context) []store.AIAgent {
	if s.DB == nil {
		return nil
	}
	list, err := s.DB.AIJoinedAgents(ctx)
	if err != nil {
		return nil
	}
	return list
}

func (s *Server) aiAllAgents(ctx context.Context) []store.AIAgent {
	if s.DB == nil {
		return nil
	}
	list, err := s.DB.AIListAgents(ctx)
	if err != nil {
		return nil
	}
	return list
}

func (s *Server) aiAgentByKey(ctx context.Context, key string) (*store.AIAgent, bool) {
	if s.DB == nil {
		return nil, false
	}
	a, err := s.DB.AIAgentByKey(ctx, key)
	if err != nil || a == nil {
		return nil, false
	}
	return a, true
}

// aiAgentByName 支持按展示名或 key 找 Agent（@邀请 / @移出 用）。
func (s *Server) aiAgentByName(ctx context.Context, name string) (*store.AIAgent, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false
	}
	for _, a := range s.aiAllAgents(ctx) {
		if a.Display == name || a.Key == name {
			return &a, true
		}
	}
	return nil, false
}

// aiAgentByMention 在已接入的 Agent 里按「@名字」或「直接叫名字」找。
func (s *Server) aiAgentByMention(ctx context.Context, msg string) (*store.AIAgent, bool) {
	if list := s.aiAgentMentions(ctx, msg); len(list) > 0 {
		return &list[0], true
	}
	return nil, false
}

// aiAgentHit 返回名字在消息里出现的位置，未出现返回 -1。
// 客户端不会为 AI 联系人弹 @ 补全，所以「直接叫名字」必须能生效；
// 「@名字」只是可选写法，命中位置按去掉 @ 后的名字算，保证顺序一致。
func aiAgentHit(msg string, a store.AIAgent) int {
	best := -1
	for _, name := range []string{a.Display, a.Key} {
		runes := len([]rune(name))
		if runes < 2 {
			continue // 单字名字太容易误伤普通聊天
		}
		// key 是对外可见的英文标识，2 字 key（如 "ai"）在英文句子里太容易误命中。
		if name == a.Key && runes < 3 {
			continue
		}
		if i := strings.Index(msg, name); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// aiAgentMentions 收集一条消息里叫到的全部已接入 Agent，按叫到的先后排序。
func (s *Server) aiAgentMentions(ctx context.Context, msg string) []store.AIAgent {
	type hit struct {
		pos int
		ag  store.AIAgent
	}
	hits := make([]hit, 0, 3)
	for _, a := range s.aiJoinedAgents(ctx) {
		if i := aiAgentHit(msg, a); i >= 0 {
			hits = append(hits, hit{pos: i, ag: a})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	out := make([]store.AIAgent, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ag)
	}
	return out
}

// aiLastSpeaker 记录每个频道上一次回答的 Agent，避免连着两次都是同一个人。
var aiLastSpeaker sync.Map // channel -> string

// 旧 reply_mode / talkativeness 的选人辅助（aiAgentCanChime / aiDropSilent /
// aiChimeAgents）已随 T-108 删除：主动事件的候选只由「范围 + 接入 + 能力」决定，
// 不再按旧字段算第二份概率。DM 与直接求助不经过这些辅助。

// aiReplyModeAllowsSpeech 报告该角色此刻是否允许参与「会在频道里开口」的主动事件。
//
// T-108 §9.2 E6：reply_mode 只作硬门，不作概率——
//   - silent：频道内一律静默（与改造前一致）；
//   - mention：只回应被点名/直接对话，频道主动事件不选它；
//   - social（或空值）：放行。
//
// DM 与明确直接询答不经过这里，维持既有行为；虚拟钓鱼不是「开口」，也不经过这里。
func aiReplyModeAllowsSpeech(a store.AIAgent) bool {
	switch a.ReplyMode {
	case "silent", "mention":
		return false
	}
	return true
}

// aiPickSpeaker 按 T-103 共享派生挑一个频道内的 Agent 回答；频道里只有一个人时就是它。
// 没有愿意接话的 Agent 时返回 false——频道安静着也正常。
//
// T-108 §9.2 E6：不再用「10 + talkativeness」加权随机。候选按稳定顺序（范围
// 优先级、角色 id）逐个回答一次「这一次愿不愿意接一句」（activityWants，种子由
// aiEventSeed(channel, 原话) 与角色键派生）；第一个愿意的人发言，全部不愿意就是
// 合法沉默。last-speaker 去重、频道冷却与既有硬限原样保留，不新增第二份概率。
//
// T-108：没人点名时的单人接话属于「主动事件」，候选必须已获准参加圆桌
// （频道事件能力）；只有攻略能力的角色不会被当替补。点名/直接求助不走这里，
// 那条路径保留基础对话（仅攻略也能直接答）。
func (s *Server) aiPickSpeaker(ctx context.Context, channel, text string) (key string, contactUID int64, cooldownMS int, ok bool) {
	if s.DB == nil {
		return "", 0, 0, false
	}
	cands, _ := s.aiEventCandidates(ctx, channel, aiconfig.EventRoundtable)
	if len(cands) == 0 {
		return "", 0, 0, false
	}
	last := ""
	if v, loaded := aiLastSpeaker.Load(channel); loaded {
		last, _ = v.(string)
	}
	if len(cands) > 1 {
		others := make([]store.AIAgent, 0, len(cands))
		for _, a := range cands {
			if a.Key != last {
				others = append(others, a)
			}
		}
		if len(others) > 0 {
			cands = others
		}
	}
	seed := aiEventSeed(channel, text)
	for _, a := range cands {
		p, derivable := s.actorPersonality(ctx, a.Key)
		if !derivable {
			continue // 显式非法配置：不产出新的派生决定
		}
		if want, _ := activityWants(p, seed^store.AIActorSeed(0, a.Key),
			activitySpeakerFeature(0, 0, 0)); want {
			aiLastSpeaker.Store(channel, a.Key)
			return a.Key, a.ContactUID, s.DB.AIChannelCooldownMS(ctx, channel), true
		}
	}
	// 全部不愿意：合法沉默，不硬凑一个人出来。
	return "", 0, 0, false
}

// ---------------------------------------------------------------------------
// 主动事件的准入（T-108 / DEV-111 A / DEV-113 A）
// ---------------------------------------------------------------------------
//
// 三条真实事件链（真人圆桌、玩家星蓝、自己上鱼）共用的唯一入口：
//   范围 = AIChannelSubscribed 的精确 ∪ 地图 ∪ 全局（按角色去重）；
//   资格 = 已接入 + T-104 的受信事件合同（TrustedEvent + AuthorizeEvent）；
//   证据 = 本次读到的能力修订，随拒绝原因一起回给调用方记录。
//
// 这里不读 reply_mode / talkativeness / 旧 *_chance：准入只看能力，
// 要不要说由 T-103 的共享派生决定（行为概率归性格）。

// aiEventReject 是一次事件候选被拒的记录：角色 + 可读原因。
type aiEventReject struct {
	AgentKey string
	Reason   string
}

// aiEventCandidates 返回频道范围内获准某类主动事件的角色（稳定顺序）。
//
// T-108 §9.2 E6：除虚拟钓鱼（不是「开口」）以外，主动事件还要过 reply_mode
// 硬门——silent 一律静默、mention 只回应被点名/直接对话，都不进候选。
// 这是权限门，不是概率：候选顺序与派生都不看它们。
func (s *Server) aiEventCandidates(ctx context.Context, channel, category string) ([]store.AIAgent, []aiEventReject) {
	if s.DB == nil || channel == "" {
		return nil, nil
	}
	agents := s.aiAgentsForChannel(ctx, channel)
	out := make([]store.AIAgent, 0, len(agents))
	rejects := make([]aiEventReject, 0, 2)
	for _, a := range agents {
		if category != aiconfig.EventVirtualFishing && !aiReplyModeAllowsSpeech(a) {
			rejects = append(rejects, aiEventReject{AgentKey: a.Key, Reason: "reply_mode_gate"})
			continue
		}
		if _, err := s.aiEventAuthorized(ctx, category, a.Key); err != nil {
			rejects = append(rejects, aiEventReject{AgentKey: a.Key, Reason: err.Error()})
			continue
		}
		out = append(out, a)
	}
	return out, rejects
}

// aiEventGroup 是「一起上」的一组候选：组内每个角色都要单独获准。
// 任一人不合格就把整组收窄到合格者；不足两人时调用方不得起桌。
//
// 这里的候选来自点名/已筛过的名单：silent 仍然一律不进（频道内硬静默），
// mention 若是被点名进来的则保留（只回应被点名，正好是这条路径）。
func (s *Server) aiEventGroup(ctx context.Context, agents []store.AIAgent, category string) ([]store.AIAgent, []aiEventReject) {
	out := make([]store.AIAgent, 0, len(agents))
	rejects := make([]aiEventReject, 0, 2)
	for _, a := range agents {
		if a.ReplyMode == "silent" {
			rejects = append(rejects, aiEventReject{AgentKey: a.Key, Reason: "reply_mode_gate"})
			continue
		}
		if _, err := s.aiEventAuthorized(ctx, category, a.Key); err != nil {
			rejects = append(rejects, aiEventReject{AgentKey: a.Key, Reason: err.Error()})
			continue
		}
		out = append(out, a)
	}
	return out, rejects
}

// aiScopeMentions 把「被点名」的候选收窄到频道范围内的已接入角色。
//
// 点名来自 joined 名单，而事件范围是频道订阅；不核对范围就等于让不在本频道
// 的角色冒出来发言。DM 不受此处限制（DM 根本不经过频道范围）。
func (s *Server) aiScopeMentions(ctx context.Context, channel string, mentions []store.AIAgent) ([]store.AIAgent, []aiEventReject) {
	if len(mentions) == 0 {
		return nil, nil
	}
	inScope := map[string]bool{}
	for _, a := range s.aiAgentsForChannel(ctx, channel) {
		inScope[a.Key] = true
	}
	out := make([]store.AIAgent, 0, len(mentions))
	rejects := make([]aiEventReject, 0, 2)
	for _, a := range mentions {
		if !inScope[a.Key] {
			rejects = append(rejects, aiEventReject{AgentKey: a.Key, Reason: "out_of_scope"})
			continue
		}
		out = append(out, a)
	}
	return out, rejects
}

// aiChannelBroadcasters 返回频道内「已配置、生效」的播报角色（DEV-113 A）。
//
// 这是替代旧全局开关 ai_fishing_talk_enabled 的唯一判据：频道里有实际可播报的
// 角色时沿用旧预占节拍，没有时回到全程静默的抽取节拍。
func (s *Server) aiChannelBroadcasters(ctx context.Context, channel string) ([]store.AIAgent, []aiEventReject) {
	return s.aiEventCandidates(ctx, channel, aiconfig.EventBroadcastCatch)
}

// aiFishingAgents 返回频道内获准虚拟垂钓的角色（代替旧 aiAgentCanChime 依赖）。
func (s *Server) aiFishingAgents(ctx context.Context, channel string) ([]store.AIAgent, []aiEventReject) {
	return s.aiEventCandidates(ctx, channel, aiconfig.EventVirtualFishing)
}

func (s *Server) aiAgentNames(ctx context.Context) string {
	names := make([]string, 0, 4)
	for _, a := range s.aiJoinedAgents(ctx) {
		names = append(names, a.Display)
	}
	if len(names) == 0 {
		return "（还没有 Agent 接入游戏，请先在后台添加并接入）"
	}
	return strings.Join(names, "、")
}
