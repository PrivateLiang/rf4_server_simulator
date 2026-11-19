package gameserver

import (
	"testing"
)

// 五档区间必须首尾相接且严格递增，否则会出现跨档的怪重量。
func TestFishWeightBandMonotonic(t *testing.T) {
	c := aiFishCand{key: "crucian", trophyLine: 2000, superLine: 3200}
	bands := []struct {
		ev     aiFishEvent
		lo, hi int64
	}{
		{evMiss, 0, 0}, {evOk, 0, 0}, {evNear, 0, 0}, {evStar, 0, 0}, {evSuper, 0, 0},
	}
	for i := range bands {
		bands[i].lo, bands[i].hi = fishWeightBand(c, bands[i].ev, 0.35, 0.9)
		if bands[i].hi <= bands[i].lo {
			t.Fatalf("%s 区间为空: [%d,%d)", bands[i].ev, bands[i].lo, bands[i].hi)
		}
	}
	for i := 1; i < len(bands); i++ {
		if bands[i].lo < bands[i-1].hi-1 {
			t.Fatalf("%s 与 %s 区间重叠: %d < %d",
				bands[i-1].ev, bands[i].ev, bands[i].lo, bands[i-1].hi)
		}
	}
	// 蓝冠线缺失时要按星线推算，不能退化成 0。
	if lo, hi := fishWeightBand(aiFishCand{trophyLine: 2000, superLine: 0}, evSuper, 0.35, 0.9); lo <= 2000 || hi <= lo {
		t.Fatalf("蓝线缺失时推算错误: [%d,%d)", lo, hi)
	}
}

func TestMaxI64(t *testing.T) {
	if maxI64(3, 5) != 5 || maxI64(9, 2) != 9 {
		t.Fatal("maxI64 错误")
	}
}
