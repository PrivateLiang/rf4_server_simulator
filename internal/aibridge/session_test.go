package aibridge

import (
	"context"
	"testing"
)

func TestSessionHistoryAppendAndClear(t *testing.T) {
	s := NewSessionStore()
	s.Append("uid|1000001|companion",
		Message{Role: "user", Content: "在吗"},
		Message{Role: "assistant", Content: "在的"})
	h := s.History("uid|1000001|companion", 6)
	if len(h) != 2 || h[0].Content != "在吗" || h[1].Role != "assistant" {
		t.Fatalf("历史错误: %+v", h)
	}
	if got := s.History("other", 6); len(got) != 0 {
		t.Fatalf("不同会话串了: %+v", got)
	}
	for i := 0; i < sessionMaxMessages+10; i++ {
		s.Append("uid|1000001|companion", Message{Role: "user", Content: "x"})
	}
	if len(s.sessions["uid|1000001|companion"]) != sessionMaxMessages {
		t.Fatalf("会话未裁剪: %d", len(s.sessions["uid|1000001|companion"]))
	}
	s.Clear("uid|1000001|companion")
	if got := s.History("uid|1000001|companion", 6); len(got) != 0 {
		t.Fatalf("清空失败: %+v", got)
	}
}

func TestSessionStoreEvictsOldestKeys(t *testing.T) {
	s := NewSessionStore()
	for i := 0; i < sessionMaxKeys+10; i++ {
		s.Append(string(rune('a'+i%26))+string(rune('a'+i/26)), Message{Role: "user", Content: "x"})
	}
	if len(s.sessions) > sessionMaxKeys {
		t.Fatalf("会话数超出上限: %d", len(s.sessions))
	}
}

func TestWebSearchDisabledWhenEndpointEmpty(t *testing.T) {
	k := &Knowledge{}
	if got := k.WebSearch(context.Background(), "河鲈", ""); got != "" {
		t.Fatalf("空端点不应产生外部结果: %q", got)
	}
}
