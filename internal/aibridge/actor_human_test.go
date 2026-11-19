package aibridge

import (
	"strings"
	"testing"
	"time"

	"rf4server/internal/store"
)

// TestActorSentenceNoExactMinutes 守住拟人感：状态句不该报精确分钟数。
// 没人会记得自己蹲了 27 分钟，报数既出戏又让前缀缓存每分钟失效。
func TestActorSentenceNoExactMinutes(t *testing.T) {
	r := &Runner{}
	base := time.Now()
	cases := []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"刚到", 2 * time.Minute, "刚到没多久"},
		{"钓了一阵", 18 * time.Minute, "待了一阵子了"},
		{"蹲了挺久", 2 * time.Hour, "磨了挺久了"},
	}
	for _, c := range cases {
		a := &store.AIActor{
			Loc: "level_001_lake", LocName: "克马羚诺也湖",
			Activity: "fishing", TargetName: "茴鱼",
			SinceTS: float64(base.Add(-c.ago).Unix()),
		}
		got := r.actorSentence(a)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s 应含 %q，实际 %q", c.name, c.want, got)
		}
		if strings.Contains(got, "分钟") || strings.Contains(got, "小时") {
			t.Errorf("%s 不该出现精确时长单位: %q", c.name, got)
		}
		if !strings.Contains(got, "别报精确时间") {
			t.Errorf("%s 应提醒模型别报精确时间: %q", c.name, got)
		}
		if !strings.Contains(got, "克马羚诺也湖") || !strings.Contains(got, "茴鱼") {
			t.Errorf("%s 丢了地点/目标鱼: %q", c.name, got)
		}
	}
}

// TestActorSentenceResting 休息时不能再说「正钓着」。
func TestActorSentenceResting(t *testing.T) {
	r := &Runner{}
	a := &store.AIActor{
		Loc: "level_001_lake", LocName: "克马羚诺也湖",
		Activity: "resting", SinceTS: float64(time.Now().Add(-10 * time.Minute).Unix()),
	}
	got := r.actorSentence(a)
	if !strings.Contains(got, "歇着") || strings.Contains(got, "正钓着") {
		t.Fatalf("休息状态句不对: %q", got)
	}
}
