package store

import "testing"

// 角色状态推进是纯函数：活动没到点就不动，到点才决定下一段。
func TestAIActorAdvanceWaitsUntilDue(t *testing.T) {
	a := &AIActor{Activity: "fishing", ActivityUntilTS: 100, SinceTS: 10, Energy: 80}
	cfg := AIActorConfig{FishingMin: 1000, FishingMax: 2000, RestMin: 500, RestMax: 800}
	if a.Advance(50, cfg, func() float64 { return 0 }) {
		t.Fatal("未到活动结束时间不应改状态")
	}
	if a.Activity != "fishing" {
		t.Fatalf("activity = %q, want fishing", a.Activity)
	}
}

// T-108：休息与否只看调用方给出的决定（T-103 共享派生），不再读 RestChance。
func TestAIActorAdvanceFishingToRest(t *testing.T) {
	a := &AIActor{Activity: "fishing", ActivityUntilTS: 100, SinceTS: 10, Energy: 80}
	cfg := AIActorConfig{FishingMin: 1000, FishingMax: 2000, RestMin: 500, RestMax: 800}
	in := AIActorAdvanceInput{
		Now: 200, Cfg: cfg, Roll: func() float64 { return 0 },
		RestAfterFishing: func(float64) bool { return true },
	}
	if !a.AdvanceWith(in) {
		t.Fatal("到点后应该推进状态")
	}
	if a.Activity != "resting" {
		t.Fatalf("activity = %q, want resting", a.Activity)
	}
	if a.SinceTS != 200 {
		t.Fatalf("since = %v, want 200", a.SinceTS)
	}
	if a.ActivityUntilTS != 700 {
		t.Fatalf("until = %v, want 700", a.ActivityUntilTS)
	}
}

// 旧 RestChance 必须失效：即使配置里写着 1（必休息），只要派生决定是「不休息」，
// 这一段就继续钓。恢复旧读取时本用例会变红。
func TestAIActorAdvanceIgnoresLegacyRestChance(t *testing.T) {
	a := &AIActor{Activity: "fishing", ActivityUntilTS: 100, SinceTS: 10, Energy: 90}
	cfg := AIActorConfig{FishingMin: 1000, FishingMax: 2000, RestMin: 500, RestMax: 800, RestChance: 1}
	in := AIActorAdvanceInput{
		Now: 200, Cfg: cfg, Roll: func() float64 { return 0 },
		RestAfterFishing: func(float64) bool { return false },
	}
	if !a.AdvanceWith(in) {
		t.Fatal("到点后应该推进状态")
	}
	if a.Activity != "fishing" {
		t.Fatalf("旧 RestChance 仍参与决定：activity = %q，期望 fishing", a.Activity)
	}
}

func TestAIActorAdvanceLowEnergyForcesRest(t *testing.T) {
	a := &AIActor{Activity: "fishing", ActivityUntilTS: 100, Energy: 20}
	cfg := AIActorConfig{FishingMin: 1000, FishingMax: 2000, RestMin: 500, RestMax: 800}
	in := AIActorAdvanceInput{
		Now: 200, Cfg: cfg, Roll: func() float64 { return 0.99 },
		RestAfterFishing: func(float64) bool { return false },
	}
	a.AdvanceWith(in)
	if a.Activity != "resting" {
		t.Fatalf("体力低应该强制休息，activity = %q", a.Activity)
	}
}

func TestAIActorAdvanceRestToFishing(t *testing.T) {
	a := &AIActor{Activity: "resting", ActivityUntilTS: 100, Energy: 60}
	cfg := AIActorConfig{FishingMin: 1000, FishingMax: 2000, RestMin: 500, RestMax: 800}
	a.Advance(200, cfg, func() float64 { return 0.1 })
	if a.Activity != "fishing" {
		t.Fatalf("体力恢复后应该回到钓鱼，activity = %q", a.Activity)
	}
}

// 时长覆盖：调用方给出的这一段时长优先于区间随机，用来表达 Trest 与硬驻留。
func TestAIActorAdvanceUsesProvidedDurations(t *testing.T) {
	a := &AIActor{Activity: "fishing", ActivityUntilTS: 100, SinceTS: 10, Energy: 90}
	cfg := AIActorConfig{FishingMin: 1000, FishingMax: 2000, RestMin: 500, RestMax: 800}
	in := AIActorAdvanceInput{
		Now: 200, Cfg: cfg, Roll: func() float64 { return 0 },
		FishingSeconds:   func() float64 { return 1234 },
		RestAfterFishing: func(float64) bool { return false },
	}
	if !a.AdvanceWith(in) {
		t.Fatal("到点后应该推进状态")
	}
	if a.ActivityUntilTS != 200+1234 {
		t.Fatalf("until = %v，期望按调用方给出的时长 1434", a.ActivityUntilTS)
	}
	a2 := &AIActor{Activity: "resting", ActivityUntilTS: 100, Energy: 20}
	in2 := AIActorAdvanceInput{
		Now: 200, Cfg: cfg, Roll: func() float64 { return 0.99 },
		RestSeconds: func() float64 { return 432 },
	}
	if !a2.AdvanceWith(in2) {
		t.Fatal("休息段到点后应该推进状态")
	}
	if a2.ActivityUntilTS != 200+432 {
		t.Fatalf("休息 until = %v，期望 632", a2.ActivityUntilTS)
	}
}

func TestAIActorStartCanBeginResting(t *testing.T) {
	cfg := AIActorConfig{FishingMin: 1000, FishingMax: 2000, RestMin: 500, RestMax: 800}
	a := AIActorStart("fishpal", "level_1", "测试湖", 100, cfg, func() float64 { return 0.1 })
	if a.Activity != "resting" {
		t.Fatalf("activity = %q, want resting", a.Activity)
	}
	if a.ActivityUntilTS != 630 {
		t.Fatalf("until = %v, want 630", a.ActivityUntilTS)
	}
}
