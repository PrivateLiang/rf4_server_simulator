package gameserver

// 闲时搭话的纯逻辑单测：固定时钟、固定种子，不碰数据库与真实频道。

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestAIIdleDue(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	silence := 3 * time.Minute

	cases := []struct {
		name string
		last time.Time
		want bool
	}{
		{"真人刚说过话", now.Add(-time.Minute), false},
		{"安静够了", now.Add(-4 * time.Minute), true},
		{"没有记录视为安静", time.Time{}, true},
	}
	for _, tc := range cases {
		if got := aiIdleDue(now, tc.last, silence); got != tc.want {
			t.Fatalf("%s: due = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

func TestAIIdleRoundtableChance(t *testing.T) {
	cases := map[string]float64{"quiet": 0, "normal": 0.1, "lively": 0.25, "unlimited": 0.35, "custom": 0.15}
	for mood, want := range cases {
		if got := aiIdleRoundtableChance(mood); got != want {
			t.Fatalf("%s: chance = %v，期望 %v", mood, got, want)
		}
	}
	if got := aiIdleRoundtableChance("nonsense"); got != 0 {
		t.Fatalf("未知氛围不应升级多人：%v", got)
	}
}

func TestAIIdlePickTopic(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	rng := rand.New(rand.NewSource(1))

	// 最近说过的话头不再出现。
	recent := []string{"silence", "patience", "self"}
	for i := 0; i < 30; i++ {
		tp := aiIdlePickTopic(now, map[string]time.Time{}, recent, rng)
		if tp.ID == "" {
			t.Fatal("选出了空话头")
		}
		for _, r := range recent {
			if tp.ID == r {
				t.Fatalf("选中了最近说过的话头 %s", r)
			}
		}
	}

	// 冷却期内的话头不出现。
	seen := map[string]time.Time{"ask": now.Add(-5 * time.Minute)}
	for i := 0; i < 30; i++ {
		if tp := aiIdlePickTopic(now, seen, nil, rng); tp.ID == "ask" {
			t.Fatal("选中了冷却期内的话头")
		}
	}

	// 全都在冷却/最近时退回全集，不返回空。
	all := make([]string, 0, len(aiIdleTopics))
	seenAll := map[string]time.Time{}
	for _, tp := range aiIdleTopics {
		all = append(all, tp.ID)
		seenAll[tp.ID] = now.Add(-time.Minute)
	}
	if tp := aiIdlePickTopic(now, seenAll, all, rng); tp.ID == "" {
		t.Fatal("全部冷却时应退回全集，而不是空话头")
	}
}

func TestAIIdlePickTopicDeterministic(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	a := aiIdlePickTopic(now, map[string]time.Time{}, nil, rand.New(rand.NewSource(42)))
	b := aiIdlePickTopic(now, map[string]time.Time{}, nil, rand.New(rand.NewSource(42)))
	if a.ID != b.ID {
		t.Fatalf("同种子结果不一致：%s vs %s", a.ID, b.ID)
	}
}

func TestAIIdlePromptUsesOnlyKnownFacts(t *testing.T) {
	text := aiIdlePrompt("拉多加湖", 12, aiIdleTopic{ID: "silence", Prompt: "可以提一句安静"})
	for _, want := range []string{"拉多加湖", "已经钓了 12 分钟", "安静", "不要报价格"} {
		if !strings.Contains(text, want) {
			t.Fatalf("提示词缺少 %q：%s", want, text)
		}
	}
	// 不足 5 分钟不报时长，避免把刚坐下说成「钓了很久」。
	if short := aiIdlePrompt("拉多加湖", 2, aiIdleTopics[0]); strings.Contains(short, "已经钓了") {
		t.Fatalf("短时不应报时长：%s", short)
	}
}

func TestAIIdleFeatureIsNotInvestment(t *testing.T) {
	f := activityIdleFeature()
	if f.Investment != 0 {
		t.Fatalf("投入项应为 0（闲时搭话不该由「钓得久」驱动），实际 %v", f.Investment)
	}
	if f.Social <= f.Novelty || f.Social <= 0 {
		t.Fatalf("社交应是主要特征：%+v", f)
	}
}

func TestAIIdleMinutesSince(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.Local)
	if got := aiIdleMinutesSince(now, float64(now.Add(-12*time.Minute).Unix())); got != 12 {
		t.Fatalf("分钟数 = %d，期望 12", got)
	}
	if got := aiIdleMinutesSince(now, 0); got != 0 {
		t.Fatalf("缺时间应为 0，实际 %d", got)
	}
	if got := aiIdleMinutesSince(now, float64(now.Add(time.Hour).Unix())); got != 0 {
		t.Fatalf("未来时间应夹到 0，实际 %d", got)
	}
}

// 「没鱼」用本频道最近一次上鱼尝试时间判定；只读，不推进状态。
func TestAIIdleFishingLastAttempt(t *testing.T) {
	const channel = "t_idle_last_attempt"
	aiFishingStates.Delete(channel)
	t.Cleanup(func() { aiFishingStates.Delete(channel) })
	if got := aiFishingLastAttemptAt(channel); !got.IsZero() {
		t.Fatalf("没有状态时应为零值，实际 %v", got)
	}
	now := time.Now()
	aiFishingStates.Store(channel, &aiFishingChannelState{lastAt: now})
	if got := aiFishingLastAttemptAt(channel); got.IsZero() || time.Since(got) > time.Second {
		t.Fatalf("应读到最近一次上鱼时间，实际 %v", got)
	}
}
