package aibridge

// 上下文预算：按 token 估算裁剪私聊与圆桌的请求。
// 中文按 1 字 ≈ 1 token 保守估算；优先保留 system 与当前问题，历史从最旧开始丢。

import "context"

// ContextBudget 是一次请求的硬上限。
type ContextBudget struct {
	MaxTokens       int // 总预算（system + history + user）
	HistoryMessages int // 历史最多几条
	HistoryChars    int // 历史最多多少字
	InternalChars   int // 内部资料/工具结果最多多少字
}

func estimateTokens(s string) int { return len([]rune(s)) }

func trimToTokens(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// profile 是场景档位：私聊放长、频道中等、圆桌最短。
type profile string

const (
	profileDM         profile = "dm"
	profileChannel    profile = "channel"
	profileRoundtable profile = "roundtable"
)

func (r *Runner) contextBudget(ctx context.Context, p profile) ContextBudget {
	var tokens, msgs, chars int
	switch p {
	case profileDM:
		tokens = r.intSetting(ctx, "ai_profile_dm_context_tokens", 12000, 1000, 30000)
		msgs = r.intSetting(ctx, "ai_profile_dm_history_messages", 200, 0, 300)
		chars = r.intSetting(ctx, "ai_profile_dm_history_chars", 8000, 0, 20000)
	case profileRoundtable:
		tokens = r.intSetting(ctx, "ai_profile_roundtable_context_tokens", 4000, 800, 12000)
		msgs = r.intSetting(ctx, "ai_profile_roundtable_context_messages", 2, 1, 4)
		chars = 800
	default:
		tokens = r.intSetting(ctx, "ai_profile_channel_context_tokens", 10000, 800, 30000)
		msgs = r.intSetting(ctx, "ai_profile_channel_history_messages", 150, 0, 300)
		chars = r.intSetting(ctx, "ai_profile_channel_history_chars", 6000, 200, 20000)
	}
	return ContextBudget{
		MaxTokens: tokens, HistoryMessages: msgs, HistoryChars: chars,
		InternalChars: r.intSetting(ctx, "ai_profile_internal_chars", 800, 200, 4000),
	}
}

// replySoftChars 是写进提示词的目标长度，靠模型自觉；hard 由 replyChars 截断。
func (r *Runner) replySoftChars(ctx context.Context, p profile) int {
	switch p {
	case profileDM:
		return r.intSetting(ctx, "ai_profile_dm_reply_soft_chars", 80, 20, 800)
	case profileRoundtable:
		return r.intSetting(ctx, "ai_profile_roundtable_reply_soft_chars", 20, 10, 120)
	default:
		return r.intSetting(ctx, "ai_profile_channel_reply_soft_chars", 25, 10, 200)
	}
}

// replyChars 是游戏内单条回复的字数上限（宽上限，不是无限）。
func (r *Runner) replyChars(ctx context.Context, p profile) int {
	switch p {
	case profileDM:
		return r.intSetting(ctx, "ai_profile_dm_reply_chars", 120, 100, 2000)
	case profileRoundtable:
		return r.intSetting(ctx, "ai_profile_roundtable_reply_chars", 30, 30, 300)
	default:
		return r.intSetting(ctx, "ai_profile_channel_reply_chars", 45, 20, 500)
	}
}

// buildMessages 组一次请求：system + 裁剪后的历史 + 当前问题。
// 历史从最新往前取，条数与字符双限；总预算超出时继续丢最旧的历史。
func buildMessages(system string, history []Message, user string, b ContextBudget) []Message {
	if b.MaxTokens <= 0 {
		b.MaxTokens = 3000
	}
	user = trimToTokens(user, b.MaxTokens/4)
	system = trimToTokens(system, b.MaxTokens/3)

	historyLimit := b.MaxTokens*4/5 - estimateTokens(system) - estimateTokens(user)
	if historyLimit < 0 {
		historyLimit = 0
	}
	if b.HistoryChars > 0 && historyLimit > b.HistoryChars {
		historyLimit = b.HistoryChars
	}

	picked := make([]Message, 0, b.HistoryMessages)
	used := 0
	for i := len(history) - 1; i >= 0 && len(picked) < b.HistoryMessages; i-- {
		c := estimateTokens(history[i].Content)
		if used+c > historyLimit {
			break
		}
		used += c
		picked = append(picked, history[i])
	}

	out := make([]Message, 0, len(picked)+2)
	out = append(out, Message{Role: "system", Content: system})
	for i := len(picked) - 1; i >= 0; i-- {
		out = append(out, picked[i])
	}
	out = append(out, Message{Role: "user", Content: user})
	return out
}

// roundtableWindow 只带原话 + 最近 n 条发言，避免圆桌越聊越长。
func roundtableWindow(msgs []Message, n int) []Message {
	if len(msgs) == 0 {
		return msgs
	}
	if n <= 0 {
		n = 2
	}
	out := make([]Message, 0, n+2)
	out = append(out, msgs[0]) // 真人原话
	if len(msgs) > 1 {
		start := len(msgs) - n
		if start < 1 {
			start = 1
		}
		out = append(out, msgs[start:]...)
	}
	out = append(out, Message{Role: "user", Content: "（轮到你接一句，别重复别人）"})
	return out
}
