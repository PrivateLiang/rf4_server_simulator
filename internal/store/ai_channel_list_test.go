package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAIListChannelsWithLabels(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "chlist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()

	if err := d.AIUpsertChannel(ctx, "nav1_03"); err != nil {
		t.Fatal(err)
	}
	if err := d.AIUpsertChannel(ctx, "nav1_03"); err != nil {
		t.Fatalf("重复记录应幂等: %v", err)
	}
	if err := d.AIUpsertChannel(ctx, "nav12_02"); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetChannelLabel(ctx, "nav1_03", "克马羚诺也湖"); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetChannelAgent(ctx, "nav1_03", "companion", true); err != nil {
		t.Fatal(err)
	}
	// 只存在于订阅表、没经过 AIUpsertChannel 的频道也要出现在列表里。
	if err := d.AISetChannelAgent(ctx, "nav9_09", "fishguide", true); err != nil {
		t.Fatal(err)
	}

	list, err := d.AIListChannels(ctx)
	if err != nil {
		t.Fatalf("AIListChannels: %v", err)
	}
	byName := map[string]AIChannelInfo{}
	for _, c := range list {
		byName[c.Name] = c
	}
	c1, ok := byName["nav1_03"]
	if !ok || c1.Label != "克马羚诺也湖" || c1.Count != 1 || len(c1.Agents) != 1 || c1.Agents[0] != "AI 伙伴" {
		t.Fatalf("nav1_03 = %+v", c1)
	}
	c2, ok := byName["nav9_09"]
	if !ok || c2.Count != 1 || c2.Label != "" {
		t.Fatalf("订阅表频道缺失: %+v", c2)
	}
	if _, ok := byName["nav12_02"]; !ok {
		t.Fatal("AIUpsertChannel 记录的频道缺失")
	}
}
