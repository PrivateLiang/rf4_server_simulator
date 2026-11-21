package gameserver

// 说话预算的纯逻辑与配置解析：固定时钟，不碰真实频道。

import (
	"context"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
)

func talkCfg(budget, burst int, weights [4]float64) aiTalkConfig {
	return aiTalkConfig{Budget: budget, Burst: burst, MinGap: 8 * time.Minute, Weights: weights}
}

func TestAITalkAllowedAt(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	uniform := [4]float64{1, 1, 1, 1}

	if got := aiTalkAllowedAt(base, talkCfg(6, 2, uniform)); got != 2 {
		t.Fatalf("整点应至少给突发容量 2，实际 %d", got)
	}
	if got := aiTalkAllowedAt(base.Add(30*time.Minute), talkCfg(6, 2, uniform)); got != 5 {
		t.Fatalf("半小时应约 3 句曲线 + 2 突发 = 5，实际 %d", got)
	}
	if got := aiTalkAllowedAt(base.Add(59*time.Minute), talkCfg(6, 2, uniform)); got != 6 {
		t.Fatalf("接近整点应封顶到预算 6，实际 %d", got)
	}
	if got := aiTalkAllowedAt(base, talkCfg(3, 1, uniform)); got != 1 {
		t.Fatalf("冷清整点应给 1，实际 %d", got)
	}
	// 前段权重高：15 分钟就应拿到 12*1.5/4=4.5 的曲线，加突发 3。
	lively := [4]float64{1.5, 1, 0.75, 0.75}
	if got := aiTalkAllowedAt(base.Add(15*time.Minute), talkCfg(12, 3, lively)); got != 7 {
		t.Fatalf("热闹前段 = 4+3 = 7，实际 %d", got)
	}
	// 全 0 权重退回均匀，不能永久沉默。
	if got := aiTalkAllowedAt(base, talkCfg(6, 2, [4]float64{0, 0, 0, 0})); got != 2 {
		t.Fatalf("全 0 权重应退回均匀，实际 %d", got)
	}
}

func TestAITalkTakePriorityAndReserve(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	cfg := talkCfg(6, 2, [4]float64{1, 1, 1, 1})

	st := &aiTalkState{}
	if !aiTalkTake(base, st, cfg, true) || !aiTalkTake(base, st, cfg, true) {
		t.Fatal("整点应允许两次上鱼播报（突发 2）")
	}
	if aiTalkTake(base, st, cfg, true) {
		t.Fatal("突发用完后不应继续放行")
	}

	// 闲时搭话要给上鱼留一句：整点只有 2 格时只能拿 1 格。
	idleState := &aiTalkState{}
	if !aiTalkTake(base, idleState, cfg, false) {
		t.Fatal("第一句闲时搭话应放行")
	}
	if aiTalkTake(base, idleState, cfg, false) {
		t.Fatal("第二句闲时搭话应被预留挡住")
	}
	if !aiTalkTake(base, idleState, cfg, true) {
		t.Fatal("上鱼播报应能用掉预留的一格")
	}
}

func TestAITalkMinGapAndHourReset(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	cfg := talkCfg(6, 2, [4]float64{1, 1, 1, 1})

	st := &aiTalkState{}
	if !aiTalkTake(base, st, cfg, false) {
		t.Fatal("第一句应放行")
	}
	if aiTalkTake(base.Add(time.Minute), st, cfg, false) {
		t.Fatal("最小间隔内不应放行闲时搭话")
	}
	if !aiTalkTake(base.Add(time.Minute), st, cfg, true) {
		t.Fatal("上鱼播报可以插队，不受最小间隔约束")
	}

	// 跨整点：额度重置。
	next := base.Add(time.Hour)
	if !aiTalkTake(next, st, cfg, true) {
		t.Fatal("跨整点后应重新有额度")
	}
	if st.spent != 1 {
		t.Fatalf("跨整点后 spent 应重置为 1，实际 %d", st.spent)
	}
}

func TestAITalkTakeUnlimited(t *testing.T) {
	base := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	cfg := talkCfg(0, 1, [4]float64{1, 1, 1, 1})
	cfg.MinGap = time.Minute

	st := &aiTalkState{}
	if !aiTalkTake(base, st, cfg, false) {
		t.Fatal("不限档第一句应放行")
	}
	if aiTalkTake(base.Add(30*time.Second), st, cfg, false) {
		t.Fatal("不限档仍应遵守最小间隔")
	}
	if !aiTalkTake(base.Add(30*time.Second), st, cfg, true) {
		t.Fatal("不限档的上鱼播报可以插队")
	}
	// 连续播报 40 次仍不应被小时上限拦住。
	for i := 0; i < 40; i++ {
		if !aiTalkTake(base.Add(time.Duration(i+2)*time.Minute), st, cfg, true) {
			t.Fatalf("不限档不应有小时上限，第 %d 次被拦", i+2)
		}
	}
}

func TestAITalkRoundtableProfile(t *testing.T) {
	ctx := context.Background()
	db := openStaticTestDB(t)
	s := &Server{DB: db}
	set := func(mood string) {
		if err := db.AISetSetting(ctx, "ai_fishing_mood", mood); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		mood string
		want aiconfig.RoundtableProfile
	}{
		{"quiet", aiconfig.RoundtableProfile{Speakers: 3, Turns: 4, CooldownSec: 300, Hourly: 1, MaxMS: 30000}},
		{"normal", aiconfig.RoundtableProfile{Speakers: 5, Turns: 6, CooldownSec: 120, Hourly: 3, MaxMS: 45000}},
		{"lively", aiconfig.RoundtableProfile{Speakers: 10, Turns: 12, CooldownSec: 45, Hourly: 6, MaxMS: 90000}},
		{"unlimited", aiconfig.RoundtableProfile{Speakers: 12, Turns: 16, CooldownSec: 20, Hourly: 12, MaxMS: 120000}},
	}
	for _, tc := range cases {
		set(tc.mood)
		if got := s.aiRoundtableProfile(ctx); got != tc.want {
			t.Fatalf("%s 圆桌配置 = %+v，期望 %+v", tc.mood, got, tc.want)
		}
	}
	set("custom")
	if err := db.AISetSetting(ctx, "ai_talk_roundtable_speakers", "8"); err != nil {
		t.Fatal(err)
	}
	if got := s.aiRoundtableProfile(ctx); got.Speakers != 8 || got.Turns != 10 || got.MaxMS != 90000 {
		t.Fatalf("自定义 8 人解析错误: %+v", got)
	}
	if err := db.AISetSetting(ctx, "ai_talk_roundtable_speakers", "99"); err != nil {
		t.Fatal(err)
	}
	if got := s.aiRoundtableProfile(ctx); got.Speakers != aiconfig.RoundtableMaxParticipants {
		t.Fatalf("自定义人数应夹到 %d，实际 %+v", aiconfig.RoundtableMaxParticipants, got)
	}
	set("nonsense")
	if got := s.aiRoundtableProfile(ctx); got.Speakers != 5 {
		t.Fatalf("未知氛围应退回正常 5 人，实际 %+v", got)
	}
}

func TestAITalkConfigResolution(t *testing.T) {
	ctx := context.Background()
	db := openStaticTestDB(t)
	s := &Server{DB: db}

	if cfg := s.aiTalkConfig(ctx); cfg.Budget != 6 || cfg.Burst != 2 {
		t.Fatalf("缺配置应为正常档，实际 %+v", cfg)
	}
	if err := db.AISetSetting(ctx, "ai_fishing_mood", "quiet"); err != nil {
		t.Fatal(err)
	}
	if cfg := s.aiTalkConfig(ctx); cfg.Budget != 3 || cfg.MinGap != 15*time.Minute {
		t.Fatalf("冷清档解析错误: %+v", cfg)
	}
	if err := db.AISetSetting(ctx, "ai_fishing_mood", "lively"); err != nil {
		t.Fatal(err)
	}
	if cfg := s.aiTalkConfig(ctx); cfg.Budget != 12 || cfg.Weights[0] != 1.5 {
		t.Fatalf("热闹档解析错误: %+v", cfg)
	}
	if err := db.AISetSetting(ctx, "ai_fishing_mood", "unlimited"); err != nil {
		t.Fatal(err)
	}
	if cfg := s.aiTalkConfig(ctx); cfg.Budget != 0 || cfg.MinGap != time.Minute {
		t.Fatalf("不限档解析错误: %+v", cfg)
	}
	if err := db.AISetSetting(ctx, "ai_fishing_mood", "custom"); err != nil {
		t.Fatal(err)
	}
	if err := db.AISetSettings(ctx, map[string]string{
		"ai_talk_hourly_limit": "4", "ai_talk_min_gap_s": "60", "ai_talk_burst": "4",
		"ai_talk_weight_0": "2", "ai_talk_weight_1": "0", "ai_talk_weight_2": "0", "ai_talk_weight_3": "0",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := s.aiTalkConfig(ctx)
	if cfg.Budget != 4 || cfg.MinGap != time.Minute || cfg.Burst != 4 || cfg.Weights[0] != 2 {
		t.Fatalf("自定义解析错误: %+v", cfg)
	}
	if err := db.AISetSetting(ctx, "ai_fishing_mood", "nonsense"); err != nil {
		t.Fatal(err)
	}
	if cfg := s.aiTalkConfig(ctx); cfg.Budget != 6 {
		t.Fatalf("未知氛围应退回正常，实际 %+v", cfg)
	}
}

func TestAITalkReserveFor(t *testing.T) {
	uniform := [4]float64{1, 1, 1, 1}
	cases := []struct {
		name string
		cfg  aiTalkConfig
		want int
	}{
		{"冷清给上鱼留一句", talkCfg(3, 1, uniform), 1},
		{"正常给上鱼留一句", talkCfg(6, 2, uniform), 1},
		{"热闹给上鱼留两句", talkCfg(12, 3, uniform), 2},
		{"自定义突发 4 也只留两句", talkCfg(24, 4, uniform), 2},
		{"预算只有一句时无法预留", talkCfg(1, 1, uniform), 0},
	}
	for _, tc := range cases {
		if got := aiTalkReserveFor(tc.cfg); got != tc.want {
			t.Fatalf("%s: reserve = %d，期望 %d", tc.name, got, tc.want)
		}
	}
}

func TestAITalkHourStartHandlesHalfHourZone(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata") // UTC+5:30，Truncate 会截到 :30
	if err != nil {
		t.Skip("时区数据不可用")
	}
	now := time.Date(2026, 11, 21, 12, 30, 15, 0, loc)
	start := aiTalkHourStart(now)
	if start.Hour() != 12 || start.Minute() != 0 || start.Second() != 0 {
		t.Fatalf("整点应为 12:00:00，实际 %v", start)
	}
}

func TestTalkMoodPresets(t *testing.T) {
	presets := aiconfig.TalkMoodPresets()
	if len(presets) != 4 {
		t.Fatalf("预设数量 = %d，期望 4", len(presets))
	}
	for _, m := range presets {
		if m.Budget < 0 || m.Burst < 1 || m.MinGapSec <= 0 {
			t.Fatalf("预设参数不合理: %+v", m)
		}
		if m.Budget > 0 && m.Burst > m.Budget {
			t.Fatalf("突发不应超过预算: %+v", m)
		}
		sum := 0.0
		for _, w := range m.Weights {
			sum += w
		}
		if sum <= 0 {
			t.Fatalf("预设权重全 0: %+v", m)
		}
		if m.RoundtableSpeakers < 2 || m.RoundtableSpeakers > aiconfig.RoundtableMaxParticipants {
			t.Fatalf("圆桌人数越界: %+v", m)
		}
		if m.RoundtableTurns < m.RoundtableSpeakers || m.RoundtableTurns > 16 {
			t.Fatalf("圆桌条数应 ≥ 人数且 ≤16: %+v", m)
		}
		if m.RoundtableCooldownSec <= 0 || m.RoundtableHourly < 1 || m.RoundtableMaxMS < 5000 {
			t.Fatalf("圆桌冷却/配额/时限不合理: %+v", m)
		}
	}
	if _, ok := aiconfig.TalkMoodByID(aiconfig.TalkMoodCustom); ok {
		t.Fatal("custom 不应出现在预设表里")
	}
	if m, ok := aiconfig.TalkMoodByID("unlimited"); !ok || m.Budget != 0 || m.MinGapSec != 60 {
		t.Fatalf("不限档定义错误: %+v ok=%v", m, ok)
	}
}
