package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAICatchAddAndStats(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "catch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()

	for _, c := range []AICatch{
		{AgentKey: "k", FishKey: "perch", FishName: "河鲈", WeightG: 500, Loc: "l1", LocName: "湖一", Event: "ok"},
		{AgentKey: "k", FishKey: "bream", FishName: "银鲷", WeightG: 1200, Loc: "l1", LocName: "湖一", Event: "star"},
		{AgentKey: "k", FishKey: "pike", FishName: "狗鱼", WeightG: 2500, Loc: "l2", LocName: "湖二", Event: "super"},
	} {
		if err := d.AIAddCatch(ctx, c); err != nil {
			t.Fatalf("AIAddCatch: %v", err)
		}
	}
	st, err := d.AICatchStats(ctx, "k")
	if err != nil {
		t.Fatalf("AICatchStats: %v", err)
	}
	if st.Total != 3 || st.TotalG != 4200 || st.Today != 3 {
		t.Fatalf("汇总不对: %+v", st)
	}
	if st.BestName != "狗鱼" || st.BestWeightG != 2500 || st.BestLoc != "湖二" {
		t.Fatalf("最大渔获不对: %+v", st)
	}
	recent, err := d.AIRecentCatches(ctx, "k", 2)
	if err != nil || len(recent) != 2 || recent[0].FishName != "狗鱼" {
		t.Fatalf("最近渔获不对: %+v, %v", recent, err)
	}
	empty, _ := d.AICatchStats(ctx, "other")
	if empty.Total != 0 {
		t.Fatalf("无渔获应为 0: %+v", empty)
	}
}
