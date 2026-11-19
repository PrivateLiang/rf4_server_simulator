package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAIRoundtableResultsAndHumanPreempt(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "rt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	deadline := float64(time.Now().Add(time.Minute).Unix())

	id, err := d.AIEnqueue(ctx, &AITask{
		Kind: "roundtable", Scope: "channel", ScopeKey: "nav1_03",
		UID: 1000001, SourceKind: "human", Text: "@甲 @乙 聊聊",
		Participants: `["a","b"]`, DeadlineTS: deadline,
	})
	if err != nil {
		t.Fatalf("入队: %v", err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务: %v %v", ok, err)
	}
	if task.Kind != "roundtable" || task.Participants != `["a","b"]` {
		t.Fatalf("圆桌字段丢失: %+v", task)
	}

	if err := d.AIAddChannelResult(ctx, id, 2147483645, "甲说"); err != nil {
		t.Fatal(err)
	}
	if err := d.AIAddChannelResult(ctx, id, 2147483646, "乙说"); err != nil {
		t.Fatal(err)
	}
	if err := d.AIFinishTask(ctx, id, "done"); err != nil {
		t.Fatal(err)
	}
	results, err := d.AIPendingResults(ctx, 10)
	if err != nil || len(results) != 2 {
		t.Fatalf("结果数 = %d, %v", len(results), err)
	}
	if results[0].ContactUID != 2147483645 || results[1].ContactUID != 2147483646 {
		t.Fatalf("每条发言的发送者错误: %+v", results)
	}
	if results[0].Kind != "channel" || results[0].ScopeKey != "nav1_03" {
		t.Fatalf("投递目标错误: %+v", results[0])
	}

	// 圆桌进行中如果有更新的真人消息，应能检测到（人类优先中止）。
	if newer, _ := d.AIHasNewerHumanTask(ctx, id); newer {
		t.Fatal("没有新消息时不应判真")
	}
	if _, err := d.AIEnqueue(ctx, &AITask{
		Kind: "dm", Scope: "uid", ScopeKey: "1000001", UID: 1000001,
		Agent: "companion", ContactUID: 2147483645,
		SourceKind: "human", Text: "插一句", DeadlineTS: deadline,
	}); err != nil {
		t.Fatal(err)
	}
	if newer, _ := d.AIHasNewerHumanTask(ctx, id); !newer {
		t.Fatal("新真人消息未被检测到")
	}
}
