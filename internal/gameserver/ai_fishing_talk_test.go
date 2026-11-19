package gameserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

// TestAIFishingTalkPromptHuman 播报提示词也必须拟人：不报精确重量，
// 主语是「你自己钓上来的」，别让模型念数字或像播报员。
func TestAIFishingTalkPromptHuman(t *testing.T) {
	cases := []struct {
		name     string
		fish     aiFishTemplate
		want     []string
		unwanted []string
	}{
		{
			"上蓝大鱼", aiFishTemplate{Name: "欧洲无须鳕", WeightG: 6200, Event: evSuper},
			[]string{"你刚钓上", "个头挺吓人", "欧洲无须鳕", "上蓝冠"},
			[]string{"kg", "6.20"},
		},
		{
			"上星", aiFishTemplate{Name: "银鲷鱼", WeightG: 2400, Event: evStar},
			[]string{"个头不小", "银鲷鱼", "上星"},
			[]string{"kg", "2.40"},
		},
		{
			"小鱼自嘲", aiFishTemplate{Name: "圆腹雅罗鱼", WeightG: 400, Event: evMiss},
			[]string{"不大", "圆腹雅罗鱼", "自嘲"},
			[]string{"kg", "0.40"},
		},
	}
	for _, c := range cases {
		got := aiFishingTalkPrompt(c.fish)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s 缺 %q: %s", c.name, w, got)
			}
		}
		for _, w := range c.unwanted {
			if strings.Contains(got, w) {
				t.Errorf("%s 不该出现 %q: %s", c.name, w, got)
			}
		}
		if !strings.Contains(got, "别报精确数字") {
			t.Errorf("%s 应提醒别报数: %s", c.name, got)
		}
	}
}

// TestAIFishingTalkDerivedSilence 行为概率归性格（T-108 / DEV-113 A）：
// 普通达标没有候选，恒静默；其余档位由 T-103 共享派生决定，旧 *_chance 不参与。
func TestAIFishingTalkDerivedSilence(t *testing.T) {
	d, err := store.Open(filepath.Join(t.TempDir(), "talk.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: d}
	ctx := context.Background()
	// 普通达标：没有候选，派生也不该被调用。
	if _, spoken := s.maybeAIFishingTalk(ctx, d, "ch", 1, store.AIAgent{Key: "companion"},
		aiFishTemplate{Name: "小鲫鱼", Event: evOk}, 1); spoken {
		t.Fatal("普通达标档不得说话")
	}
	// 上蓝的特征比未达标更值得开口：特征本身单调，不掺概率。
	if activityCatchFeature(evSuper).Novelty <= activityCatchFeature(evMiss).Novelty {
		t.Fatal("上蓝的事实特征应高于未达标")
	}
	// 同输入同种子必须得到同一个决定。
	p := aiconfig.DefaultPersonality()
	first, _ := activityWants(p, 42, activityCatchFeature(evStar))
	second, _ := activityWants(p, 42, activityCatchFeature(evStar))
	if first != second {
		t.Fatal("同输入同种子的派生结果必须一致")
	}
	// 显式非法配置不产出决定。
	bad := aiconfig.PersonalityConfig{SchemaVersion: 1, Persistence: 500}
	if choice := activityDecide(bad, []aiconfig.PersonalityCandidate{
		{ID: "act", Feasible: true, Feature: activityCatchFeature(evStar)},
	}, 1); choice.Decided {
		t.Fatal("非法配置不得产出派生决定")
	}
}
