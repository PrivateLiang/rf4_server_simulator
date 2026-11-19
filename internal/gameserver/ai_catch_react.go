package gameserver

// 玩家自己上星/上蓝时，让同频道的 Agent 接一句。
// 与 AI 钓鱼演出是两条链路：那条是 AI 自己上鱼，这条是「看别人上鱼的反应」，
// 不产生任何鱼、不动经济，只往频道里加一句人话。
//
// T-108：这条链的准入、选择与升级全部换到新口径——
//   - 资格 = 同频道范围 + 已接入 + 已获准「回应玩家星蓝」（T-104 能力，不再看旧开关）；
//   - 要不要接、谁接 = T-103 共享派生（行为概率归性格，旧 ai_catch_*_chance 不再读）；
//   - 上蓝升级圆桌 = 再派生一次，且每位参与者都要有圆桌资格。

import (
	"context"
	"fmt"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

// aiCatchReactCooldown 防止玩家连上几条星鱼时 AI 每杆都接话。
const aiCatchReactCooldown = 30 * time.Second

func (s *Server) maybeAIPlayerCatch(ctx context.Context, uid int64, f *RolledFish) {
	if s.DB == nil || f == nil || f.Grade < 3 {
		return // 3=奖杯（星），4=蓝冠；达标与未达标不惊动 AI
	}
	ctx, active := s.bindAIIntent(ctx, s.DB)
	if !active {
		s.log.Info("AI 上星反应跳过", "uid", uid, "grade", f.Grade, "reason", "emergency_stop")
		return
	}
	sess := s.liveSessionByUID(uid)
	if sess == nil {
		s.log.Info("AI 上星反应跳过", "uid", uid, "grade", f.Grade, "reason", "session_missing")
		return
	}
	channel := sessChannel(sess).Name
	// 资格只看能力：同频道范围内已接入、且仍获准「回应玩家星蓝」的角色。
	agents, rejects := s.aiEventCandidates(ctx, channel, aiconfig.EventReplyStar)
	s.logEventRejects(channel, aiconfig.EventReplyStar, rejects)
	if len(agents) == 0 {
		s.log.Info("AI 上星反应跳过", "uid", uid, "grade", f.Grade, "channel", channel,
			"reason", "no_capable_agents")
		return
	}
	cooldownKey := channel + "|catch"
	if v, ok := aiChannelCooldown.Load(cooldownKey); ok {
		if last, ok := v.(time.Time); ok && time.Since(last) < aiCatchReactCooldown {
			s.log.Info("AI 上星反应跳过", "uid", uid, "grade", f.Grade, "channel", channel,
				"reason", "cooldown", "remain_ms", (aiCatchReactCooldown - time.Since(last)).Milliseconds())
			return
		}
	}
	name := f.Name
	if name == "" {
		name = s.speciesZhName(ctx, f.Species)
	}
	if name == "" {
		name = f.Species
	}
	grade := "上星了"
	if f.Grade >= 4 {
		grade = "上蓝冠了"
	}
	seed := aiEventSeed(channel, fmt.Sprintf("catch|%d|%s", f.Grade, name))

	// 要不要接：每个合资格角色按自己的四维回答一次（act / silent）。
	// 都没人想接就是合法沉默——旧 ai_catch_star_chance 不再参与。
	willing := s.aiWillingEventAgents(ctx, agents, seed, activityReplyFeature(int(f.Grade)))
	if len(willing) == 0 {
		s.log.Info("AI 上星反应跳过", "uid", uid, "grade", f.Grade, "channel", channel,
			"fish", name, "agents", len(agents), "reason", "personality_silent")
		return
	}
	pick := willing[stableIndex(seed, len(willing))]
	text := fmt.Sprintf("（频道里的其他人看不见的提示）玩家刚上了一条 %.2fkg 的%s，%s。"+
		"你也在同一个频道钓鱼，作为旁观者接一句，别背数据、别像播报。",
		float64(f.WeightG)/1000, name, grade)

	// 上蓝更容易引发围观：再做一次 T-103 派生，决定要不要升级成三人圆桌。
	// 圆桌冷却中或超出每小时上限时，退回单人接话，不绕过统一护栏。
	// T-108：这里不再读旧全局开关 roundtable_enabled——能不能开只看能力与派生。
	if f.Grade >= 4 {
		if keys, ok := s.upgradeToRoundtable(ctx, channel, seed, pick, agents); ok {
			ev := store.AITaskEvidence{
				FormatVersion: store.AITaskEvidenceFormatV1,
				Category:      aiconfig.EventReplyStar,
				Initiator:     pick.Key,
				Participants:  s.aiReplyStarParticipants(ctx, keys, pick, channel),
			}
			if _, err := s.DB.AIEnqueueSystemRoundtable(ctx, channel, uid, text, keys, &ev); err == nil {
				aiChannelCooldown.Store(cooldownKey, time.Now())
				s.log.Info("AI 上星反应入队", "uid", uid, "grade", f.Grade, "channel", channel,
					"fish", name, "kind", "roundtable", "agents", len(keys))
				return
			}
		}
	}

	// 单人回应：把「回应玩家星蓝」的能力修订随任务同事务绑进去，
	// BR 消费前与 GS 投递前都用它复核（撤权/重授都表现为修订不等）。
	cfg, _, bindable := s.actorPersonalityBinding(ctx, pick.Key)
	ev := store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventReplyStar,
		Participants: []store.AITaskEvidenceParticipant{{
			Key: pick.Key, Capability: aiconfig.CapabilityReplyStar,
			GSPersonalityRevision: aiPersonalityRevisionPtr(cfg, bindable),
		}},
	}
	if _, err := s.DB.AIEnqueueSystemChannel(ctx, channel, uid, text, pick.Key, pick.ContactUID, &ev); err != nil {
		s.log.Warn("玩家上鱼触发 AI 失败", "channel", channel, "agent", pick.Key, "err", err)
	} else {
		aiChannelCooldown.Store(cooldownKey, time.Now())
		s.log.Info("AI 上星反应入队", "uid", uid, "grade", f.Grade, "channel", channel,
			"fish", name, "kind", "single", "agent", pick.Key, "gs_personality_revision", cfg.Revision)
	}
}

// aiReplyStarParticipants 组装玩家星蓝升级圆桌的逐角色绑定。
//
// 按 T-108 §7：蓝升桌时每位参与者须同时具备「回应玩家星蓝」与「参加圆桌」；
// 这里把入口能力 reply_star 记在每人名下（roundtable 统一要求 join_roundtable
// 由载荷校验与两处复核补上），只有真正用四维做过这一次升级判断的发起者
// 才带 gs_personality_revision。
func (s *Server) aiReplyStarParticipants(ctx context.Context, keys []string,
	initiator store.AIAgent, channel string) []store.AITaskEvidenceParticipant {

	parts := make([]store.AITaskEvidenceParticipant, 0, len(keys))
	for _, key := range keys {
		p := store.AITaskEvidenceParticipant{
			Key: key, Capability: aiconfig.CapabilityReplyStar,
		}
		if key == initiator.Key {
			cfg, _, bindable := s.actorPersonalityBinding(ctx, key)
			p.GSPersonalityRevision = aiPersonalityRevisionPtr(cfg, bindable)
		}
		parts = append(parts, p)
	}
	return parts
}

// aiWillingEventAgents 用 T-103 共享派生筛出「这一次愿意做」的角色（稳定顺序）。
//
// 显式非法配置的角色不产出派生决定，直接不参与；缺值按中性默认参与。
func (s *Server) aiWillingEventAgents(ctx context.Context, agents []store.AIAgent,
	seed int64, feature aiconfig.PersonalityFeature) []store.AIAgent {

	out := make([]store.AIAgent, 0, len(agents))
	for _, a := range agents {
		p, derivable := s.actorPersonality(ctx, a.Key)
		if !derivable {
			continue
		}
		if want, _ := activityWants(p, seed^store.AIActorSeed(0, a.Key), feature); want {
			out = append(out, a)
		}
	}
	return out
}

// stableIndex 把任意种子折成 [0,n) 的下标；负数种子也能落回正区间。
func stableIndex(seed int64, n int) int {
	if n <= 1 {
		return 0
	}
	if seed < 0 {
		seed = -seed
	}
	return int(seed % int64(n))
}

// upgradeToRoundtable 判断上蓝这一次要不要升级成圆桌，并给出参与者名单。
//
// 参与者先过「参加圆桌」资格（含发起者本人）；不足两人不升级。
// 再由发起者按自己的四维回答一次；冷却与每小时上限仍由 rtAllow 统一把关，
// 不因为「上蓝」而放宽。
func (s *Server) upgradeToRoundtable(ctx context.Context, channel string, seed int64,
	initiator store.AIAgent, agents []store.AIAgent) ([]string, bool) {

	qualified, rejects := s.aiEventGroup(ctx, agents, aiconfig.EventRoundtable)
	s.logEventRejects(channel, aiconfig.EventRoundtable, rejects)
	keys := make([]string, 0, 3)
	for _, a := range qualified {
		if a.Key == initiator.Key {
			continue
		}
		keys = append(keys, a.Key)
	}
	initiatorQualified := false
	for _, a := range qualified {
		if a.Key == initiator.Key {
			initiatorQualified = true
		}
	}
	if !initiatorQualified || len(keys) == 0 {
		return nil, false
	}
	p, derivable := s.actorPersonality(ctx, initiator.Key)
	if !derivable {
		return nil, false
	}
	want, _ := activityWants(p, seed^store.AIActorSeed(1, initiator.Key), activityGatherFeature())
	if !want {
		return nil, false
	}
	cooldown := time.Duration(s.aiIntSetting(ctx, "roundtable_cooldown_ms", 120000, 30000, 600000)) * time.Millisecond
	hourly := s.aiIntSetting(ctx, "roundtable_hourly_limit", 3, 1, 10)
	if !rtAllow(channel, cooldown, hourly) {
		return nil, false
	}
	// 发起者排第一，其余按频道顺序补到 3 人。
	all := append([]string{initiator.Key}, keys...)
	if len(all) > 3 {
		all = all[:3]
	}
	return all, true
}
