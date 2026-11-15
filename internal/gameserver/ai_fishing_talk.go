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
	"fmt"
	"math/rand"
	"time"

	"rf4server/internal/store"
)

// aiFishingTalkChance 按档位取发言概率。ok（普通达标）恒为 0，保持静默。
// 注意：五档权重是「这次上鱼属于哪一档」的构成比例，不是发言概率，
// 两者是独立的两层，不要混在一个数里。
func (s *Server) aiFishingTalkChance(ctx context.Context, ev aiFishEvent) float64 {
	switch ev {
	case evSuper:
		return s.aiFloatSetting(ctx, "ai_fishing_talk_super_chance", 1.0, 0, 1)
	case evStar:
		return s.aiFloatSetting(ctx, "ai_fishing_talk_star_chance", 0.9, 0, 1)
	case evNear:
		return s.aiFloatSetting(ctx, "ai_fishing_talk_near_chance", 0.35, 0, 1)
	case evMiss:
		return s.aiFloatSetting(ctx, "ai_fishing_talk_miss_chance", 0.25, 0, 1)
	default:
		return 0
	}
}

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
// 返回 true 表示已经入队发言（调用方要据此占配额、推进节拍）。
// 只在抽中它的那个 channel 发一条，不广播给所有订阅频道 ——
// 这就是「只在有玩家的频道推送」：aiFishingForChannel 本身只遍历
// 在线会话所在的频道。
func (s *Server) maybeAIFishingTalk(ctx context.Context, db *store.DB, channel string,
	uid int64, agent store.AIAgent, fish aiFishTemplate, rng *rand.Rand) bool {

	if db.AISetting(ctx, "ai_fishing_talk_enabled", "0") != "1" {
		return false
	}
	if db.AISetting(ctx, "emergency_stop", "0") == "1" ||
		db.AISetting(ctx, "outbound_enabled", "1") != "1" {
		return false
	}
	// 角色级开关：全局范围只决定「在哪能见到它」，这个决定「它能不能报鱼」。
	if !agent.FishingTalkEnabled {
		return false
	}
	chance := s.aiFishingTalkChance(ctx, fish.Event)
	if chance <= 0 || rng.Float64() >= chance {
		return false
	}

	text := aiFishingTalkPrompt(fish)

	// 上蓝可按概率升级成三人圆桌，复用 rtAllow 的冷却与每小时上限。
	if fish.Event == evSuper {
		rtChance := s.aiFloatSetting(ctx, "ai_fishing_roundtable_chance", 0.5, 0, 1)
		if rtChance > 0 && rng.Float64() < rtChance && s.tryAIFishingRoundtable(ctx, db, channel, uid, agent, text) {
			return true
		}
	}

	if _, err := db.AIEnqueueSystemChannel(ctx, channel, uid, text, agent.Key, agent.ContactUID); err != nil {
		s.log.Warn("AI 上鱼播报入队失败", "agent", agent.Key, "channel", channel, "err", err)
		return false
	}
	s.log.Info("AI 上鱼播报入队", "agent", agent.Key, "channel", channel,
		"fish", fish.Name, "event", string(fish.Event))
	return true
}

// tryAIFishingRoundtable 上蓝时把参与者凑成 3 人（上鱼者 + 频道内其他 2 人）。
// 圆桌总开关关闭或冷却中时返回 false，由调用方退回单人发言。
func (s *Server) tryAIFishingRoundtable(ctx context.Context, db *store.DB, channel string,
	uid int64, speaker store.AIAgent, text string) bool {

	if db.AISetting(ctx, "roundtable_enabled", "0") != "1" {
		return false
	}
	peers := aiChimeAgents(s.aiAgentsForChannel(ctx, channel))
	keys := make([]string, 0, 3)
	keys = append(keys, speaker.Key)
	for _, a := range peers {
		if a.Key == speaker.Key {
			continue
		}
		keys = append(keys, a.Key)
		if len(keys) == 3 {
			break
		}
	}
	if len(keys) < 2 {
		return false
	}
	cooldown := time.Duration(s.aiIntSetting(ctx, "roundtable_cooldown_ms", 120000, 30000, 600000)) * time.Millisecond
	hourly := s.aiIntSetting(ctx, "roundtable_hourly_limit", 3, 1, 10)
	if !rtAllow(channel, cooldown, hourly) {
		return false
	}
	// AIEnqueueSystemRoundtable 的 participants 是全部参与者（至少 2 个），
	// handleRoundtable 会按 rtMentions 决定谁先开口，上鱼者没有被点名时
	// 仍会按活跃度被抽中，所以直接把完整名单传进去。
	if _, err := db.AIEnqueueSystemRoundtable(ctx, channel, uid, text, keys); err != nil {
		s.log.Warn("AI 上鱼圆桌入队失败", "channel", channel, "err", err)
		return false
	}
	s.log.Info("AI 上鱼圆桌入队", "channel", channel, "agents", keys)
	return true
}
