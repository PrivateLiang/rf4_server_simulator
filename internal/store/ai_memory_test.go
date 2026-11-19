package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAIMemoryLifecycle(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()

	if err := d.AIAddMemory(ctx, "fishguide", "star", "perch", "鲈鱼", 1200, "level_001_lake"); err != nil {
		t.Fatal(err)
	}
	if err := d.AIAddMemory(ctx, "fishguide", "super", "pike", "狗鱼", 4800, "level_001_lake"); err != nil {
		t.Fatal(err)
	}
	if err := d.AIAddMemory(ctx, "other", "star", "carp", "鲤鱼", 900, "x"); err != nil {
		t.Fatal(err)
	}

	recent, err := d.AIRecentMemories(ctx, "fishguide", 3)
	if err != nil || len(recent) != 2 {
		t.Fatalf("recent = %d, %v", len(recent), err)
	}
	if recent[0].FishName != "狗鱼" {
		t.Fatalf("最近一条应是狗鱼: %+v", recent[0])
	}
	best, err := d.AIBestMemories(ctx, "fishguide", 1)
	if err != nil || len(best) != 1 || best[0].Kind != "super" {
		t.Fatalf("最佳应优先蓝冠: %+v, %v", best, err)
	}

	// 裁剪只留最近 1 条。
	if err := d.AIPruneMemories(ctx, "fishguide", 1); err != nil {
		t.Fatal(err)
	}
	recent, _ = d.AIRecentMemories(ctx, "fishguide", 10)
	if len(recent) != 1 {
		t.Fatalf("裁剪后 = %d", len(recent))
	}

	if err := d.AIClearMemories(ctx, "fishguide"); err != nil {
		t.Fatal(err)
	}
	recent, _ = d.AIRecentMemories(ctx, "fishguide", 10)
	if len(recent) != 0 {
		t.Fatalf("清空后仍有 %d 条", len(recent))
	}
	// 不影响其他 Agent。
	other, _ := d.AIRecentMemories(ctx, "other", 10)
	if len(other) != 1 {
		t.Fatalf("误删其他 Agent 的记忆: %d", len(other))
	}
}
