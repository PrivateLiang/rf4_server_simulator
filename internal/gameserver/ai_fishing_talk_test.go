package gameserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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

// TestAIFishingTalkChanceDefaults 五档概率与拍板值一致，且普通达标恒为 0。
func TestAIFishingTalkChanceDefaults(t *testing.T) {
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
	cases := []struct {
		ev   aiFishEvent
		want float64
	}{
		{evSuper, 1.0},
		{evStar, 0.9},
		{evNear, 0.35},
		{evMiss, 0.25},
		{evOk, 0},
	}
	for _, c := range cases {
		if got := s.aiFishingTalkChance(ctx, c.ev); got != c.want {
			t.Errorf("%s 概率 = %v，期望 %v", c.ev, got, c.want)
		}
	}
}
