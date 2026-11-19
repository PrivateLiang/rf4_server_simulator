package gameserver

import (
	"strings"
	"testing"
)

func TestAIChannelAgentResolutionUsesDB(t *testing.T) {
	s, db := newProgressSrv(t)
	ctx := t.Context()

	if _, ok := s.aiAgentByMention(ctx, "@AI 伙伴 在吗"); ok {
		t.Fatal("没有 Agent 接入时不应命中 @")
	}
	if err := db.AISetAgentJoined(ctx, "companion", true, 2147483645); err != nil {
		t.Fatalf("接入 Agent: %v", err)
	}
	a, ok := s.aiAgentByMention(ctx, "@AI 伙伴 在吗")
	if !ok || a.Key != "companion" || a.ContactUID != 2147483645 {
		t.Fatalf("@展示名解析失败: %+v ok=%v", a, ok)
	}
	if a, ok := s.aiAgentByName(ctx, "AI 伙伴"); !ok || a.ContactUID != 2147483645 {
		t.Fatalf("按名字查找失败: %+v ok=%v", a, ok)
	}
	if names := s.aiAgentNames(ctx); !strings.Contains(names, "AI 伙伴") {
		t.Fatalf("已接入名单缺失: %q", names)
	}
	// 移出后不再命中
	if err := db.AISetAgentJoined(ctx, "companion", false, 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.aiAgentByMention(ctx, "@AI 伙伴 在吗"); ok {
		t.Fatal("移出后仍能命中")
	}
}
