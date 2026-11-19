package aibridge

import (
	"strings"
	"testing"
)

func TestBuildMessagesDropsOldest(t *testing.T) {
	system := strings.Repeat("系", 100)
	history := []Message{
		{Role: "user", Content: strings.Repeat("旧", 500)},
		{Role: "assistant", Content: strings.Repeat("答", 500)},
		{Role: "user", Content: "最近一句"},
	}
	b := ContextBudget{MaxTokens: 1000, HistoryMessages: 6, HistoryChars: 1200, InternalChars: 800}
	msgs := buildMessages(system, history, "现在的问题", b)

	total := 0
	for _, m := range msgs {
		total += len([]rune(m.Content))
	}
	if total > b.MaxTokens {
		t.Fatalf("总长度 %d 超出预算 %d", total, b.MaxTokens)
	}
	keptLatest := false
	for _, m := range msgs {
		if m.Content == "最近一句" {
			keptLatest = true
		}
		if m.Content == strings.Repeat("旧", 500) {
			t.Fatal("最旧的长历史未被丢弃")
		}
	}
	if !keptLatest {
		t.Fatal("最新历史被丢弃")
	}
	if msgs[0].Role != "system" || msgs[len(msgs)-1].Role != "user" {
		t.Fatalf("首尾角色错误: %s / %s", msgs[0].Role, msgs[len(msgs)-1].Role)
	}
}

func TestBuildMessagesMessageLimit(t *testing.T) {
	b := ContextBudget{MaxTokens: 10000, HistoryMessages: 2, HistoryChars: 6000}
	history := []Message{{Content: "a"}, {Content: "b"}, {Content: "c"}}
	msgs := buildMessages("s", history, "u", b)
	if len(msgs) != 4 {
		t.Fatalf("消息数 = %d, want system+2历史+user", len(msgs))
	}
	if msgs[1].Content != "b" || msgs[2].Content != "c" {
		t.Fatalf("历史取错: %+v", msgs)
	}
}

// 思考过程与正文共享 max_tokens，档位越高要预留越多输出空间。
func TestThinkingHeadroom(t *testing.T) {
	if thinkingHeadroom("off") != 0 || thinkingHeadroom("low") != 256 ||
		thinkingHeadroom("medium") != 512 || thinkingHeadroom("high") != 1024 ||
		thinkingHeadroom("max") != 2048 {
		t.Fatalf("思考余量档位错误: %d %d %d %d %d",
			thinkingHeadroom("off"), thinkingHeadroom("low"), thinkingHeadroom("medium"),
			thinkingHeadroom("high"), thinkingHeadroom("max"))
	}
}

// 游戏内聊天是单行：换行、制表符和零宽字符必须压平。
func TestSanitizeReply(t *testing.T) {
	got := sanitizeReply("  第一行\n第二行\t带\u200b零宽  ")
	if got != "第一行 第二行 带零宽" {
		t.Fatalf("sanitizeReply = %q", got)
	}
}

// 「钓鱼」这类通用词不能被单字回退当成某条鱼，否则角色会莫名换目标。
func TestFishMentionedRejectsGenericTalk(t *testing.T) {
	if fishMentioned("今天钓鱼口不错", "鲈鱼") {
		t.Fatal("通用钓鱼词不应命中具体鱼")
	}
	if !fishMentioned("去钓河鲈", "鲈鱼") {
		t.Fatal("别名应命中")
	}
	if !fishMentioned("这条鲈鱼真大", "鲈鱼") {
		t.Fatal("完整鱼名应命中")
	}
}

func TestRoundtableWindowKeepsRecentOnly(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "原话"},
		{Role: "assistant", Content: "一"},
		{Role: "assistant", Content: "二"},
		{Role: "assistant", Content: "三"},
	}
	out := roundtableWindow(msgs, 2)
	if len(out) != 4 {
		t.Fatalf("窗口长度 = %d", len(out))
	}
	if out[0].Content != "原话" || out[1].Content != "二" || out[2].Content != "三" {
		t.Fatalf("窗口内容错误: %+v", out)
	}
	if out[3].Role != "user" {
		t.Fatal("末尾应补一条 user 提示")
	}
}
