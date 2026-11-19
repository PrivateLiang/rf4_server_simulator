package gameserver

import (
	"testing"

	"rf4server/internal/store"
)

// 客户端不会给 AI 联系人弹 @ 补全，所以「直接叫名字」必须能命中；
// @ 只是可选写法，命中位置要一致，才能保证说话顺序稳定。
func TestAIAgentHitMatchesNameWithoutAt(t *testing.T) {
	agent1 := store.AIAgent{Key: "agent1", Display: "Agent 1"}
	cases := []struct {
		msg  string
		want bool
	}{
		{"Agent 1你觉得这图用什么饵", true},
		{"@Agent 1 你觉得呢", true},
		{"agent1 在吗", true},
		{"今天风好大", false},
	}
	for _, c := range cases {
		if got := aiAgentHit(c.msg, agent1) >= 0; got != c.want {
			t.Fatalf("%q 命中=%v，期望 %v", c.msg, got, c.want)
		}
	}
}

// 单字名字太容易误伤普通聊天，不予匹配。
func TestAIAgentHitSkipsSingleRuneName(t *testing.T) {
	a := store.AIAgent{Key: "a", Display: "鱼"}
	if aiAgentHit("今天鱼口不错", a) >= 0 {
		t.Fatal("单字名字不应参与匹配")
	}
}

// 同一句里叫到多人时，按叫到的先后排序（先叫的先说）。
func TestAIHitOrder(t *testing.T) {
	agent2 := store.AIAgent{Key: "agent2", Display: "Agent 2"}
	agent1 := store.AIAgent{Key: "agent1", Display: "Agent 1"}
	if aiAgentHit("Agent 1先来，Agent 2再说", agent1) > aiAgentHit("Agent 1先来，Agent 2再说", agent2) {
		t.Fatal("应按出现位置排序")
	}
	if aiAgentHit("Agent 2先说，Agent 1补充", agent1) < aiAgentHit("Agent 2先说，Agent 1补充", agent2) {
		t.Fatal("应随出现位置变化")
	}
}
