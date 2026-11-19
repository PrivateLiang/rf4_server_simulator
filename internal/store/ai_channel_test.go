package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAIChannelSubscription(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "ch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	const channel = "nav1_03"

	if list, _ := d.AIChannelAgents(ctx, channel); len(list) != 0 {
		t.Fatalf("未订阅时不应有 Agent: %+v", list)
	}
	if err := d.AISetChannelAgent(ctx, channel, "fishguide", true); err != nil {
		t.Fatalf("订阅: %v", err)
	}
	agents, err := d.AIChannelAgents(ctx, channel)
	if err != nil || len(agents) != 1 {
		t.Fatalf("取订阅 Agent = %v, %v", agents, err)
	}
	if agents[0].Key != "fishguide" || agents[0].ContactUID != 2147483644 {
		t.Fatalf("订阅内容错误: %+v", agents[0])
	}
	subs, err := d.AIChannelSubscribed(ctx, channel)
	if err != nil || len(subs) != 1 || subs[0] != "fishguide" {
		t.Fatalf("订阅列表 = %v, %v", subs, err)
	}
	// 重复订阅不应产生第二条（PRIMARY KEY + OR IGNORE）。
	if err := d.AISetChannelAgent(ctx, channel, "fishguide", true); err != nil {
		t.Fatalf("重复订阅: %v", err)
	}
	subs, _ = d.AIChannelSubscribed(ctx, channel)
	if len(subs) != 1 {
		t.Fatalf("重复订阅产生了多条: %v", subs)
	}
	if err := d.AISetChannelAgent(ctx, channel, "fishguide", false); err != nil {
		t.Fatalf("移出: %v", err)
	}
	if list, _ := d.AIChannelAgents(ctx, channel); len(list) != 0 {
		t.Fatalf("移出后仍可触发: %+v", list)
	}
}
