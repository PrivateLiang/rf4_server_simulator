package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestAIAgentCreateJoinDelete(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()

	// 预置的 5 个 Agent 默认都不接入游戏。
	if list, err := d.AIJoinedAgents(ctx); err != nil || len(list) != 0 {
		t.Fatalf("默认应有零个已接入 Agent: %v, %v", list, err)
	}
	uid, err := d.AINextContactUID(ctx)
	if err != nil || uid != 2147483640 {
		t.Fatalf("下一个可用 UID = %d, %v", uid, err)
	}

	uid, revived, err := d.AICreateAgent(ctx, AIAgent{
		Key: "custom", Display: "自定义助手", Role: "顾问",
		Prompt: "只依据内部资料回答", ThinkingEffort: "low",
	}, `["species_lookup"]`)
	if err != nil || revived || uid == 0 {
		t.Fatalf("创建: uid=%d revived=%v err=%v", uid, revived, err)
	}
	a, err := d.AIAgentByKey(ctx, "custom")
	if err != nil || a == nil {
		t.Fatalf("读取: %v", err)
	}
	if a.Joined {
		t.Fatal("新建 Agent 默认不应接入")
	}
	if _, err := d.AIAgentByContactUID(ctx, uid); err == nil {
		t.Fatal("未接入时按 UID 不应命中")
	}

	if err := d.AISetAgentJoined(ctx, "custom", true, 0); err != nil {
		t.Fatalf("接入: %v", err)
	}
	a, _ = d.AIAgentByKey(ctx, "custom")
	if !a.Joined {
		t.Fatal("接入状态未保存")
	}
	got, err := d.AIAgentByContactUID(ctx, uid)
	if err != nil || got.Key != "custom" || got.Display != "自定义助手" {
		t.Fatalf("接入后按 UID 查询失败: %+v, %v", got, err)
	}

	// 逻辑删除：即使已接入也允许；普通查询读不到，但 UID 继续占位。
	if err := d.AIDeleteAgent(ctx, "custom"); err != nil {
		t.Fatalf("逻辑删除: %v", err)
	}
	if a, _ := d.AIAgentByKey(ctx, "custom"); a != nil {
		t.Fatal("逻辑删除后不应再被普通查询读到")
	}
	next, _ := d.AINextContactUID(ctx)
	if next == uid {
		t.Fatal("已删除 Agent 的联系人 UID 不能被新角色复用")
	}
	// 手动指定已删除角色的 UID 也要被拒绝：聊天对象不能被别的角色接管。
	if _, _, err := d.AICreateAgent(ctx, AIAgent{
		Key: "intruder", Display: "冒名者", ContactUID: uid,
	}, `[]`); !errors.Is(err, ErrAIContactUIDUsed) {
		t.Fatalf("复用已删除 UID 应被拒绝，实际: %v", err)
	}

	// 用同一个 key 新建 = 恢复原角色，UID 与历史保留，配置按新填的覆盖。
	uid2, revived2, err := d.AICreateAgent(ctx, AIAgent{
		Key: "custom", Display: "恢复后的助手", Role: "助手",
		Prompt: "恢复",
	}, `[]`)
	if err != nil || !revived2 || uid2 != uid {
		t.Fatalf("恢复: uid=%d revived=%v err=%v", uid2, revived2, err)
	}
	back, _ := d.AIAgentByKey(ctx, "custom")
	if back == nil || back.Display != "恢复后的助手" || back.Joined {
		t.Fatalf("恢复后的状态不对: %+v", back)
	}

	// 已存在的在用 key 不能被静默覆盖。
	if _, _, err := d.AICreateAgent(ctx, AIAgent{Key: "custom", Display: "另一个"}, `[]`); err == nil {
		t.Fatal("在用 key 重复创建应该报错")
	}
}

func TestAIActorRerollAlwaysChanges(t *testing.T) {
	cfg := AIActorConfig{FishingMin: 1000, FishingMax: 2000, RestMin: 500, RestMax: 800, RestChance: 0.35}
	a := AIActorStart("k", "level_1", "测试湖", 100, cfg, func() float64 { return 0.5 })
	if a.Activity != "fishing" {
		t.Fatalf("前置状态应为 fishing，实际 %q", a.Activity)
	}
	// 固定随机值也会一直抽到 fishing；重掷必须兜底换到别的状态。
	AIActorReroll(a, 200, cfg, func() float64 { return 0.5 })
	if a.Activity == "fishing" {
		t.Fatal("重掷后活动没有变化")
	}
	if a.SinceTS != 200 {
		t.Fatalf("since = %v, want 200", a.SinceTS)
	}
}
