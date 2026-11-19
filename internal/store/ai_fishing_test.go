package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAIEnqueueSystemRoundtable(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "fishing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()

	id, err := d.AIEnqueueSystemRoundtable(ctx, "nav1_03", 1000001, "【鱼情助手】星了！", []string{"a", "b"}, nil)
	if err != nil || id == 0 {
		t.Fatalf("系统圆桌入队失败: %v", err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务: %v %v", ok, err)
	}
	if task.Kind != "roundtable" || task.SourceKind != "system" || task.Participants != `["a","b"]` {
		t.Fatalf("系统圆桌字段错误: %+v", task)
	}

	if _, err := d.AIEnqueueSystemRoundtable(ctx, "nav1_03", 1, "x", []string{"a"}, nil); err == nil {
		t.Fatal("少于 2 个参与者应拒绝")
	}
	// 普通入队仍然只接受 human：system 不能借普通路径回流。
	if _, err := d.AIEnqueue(ctx, &AITask{Kind: "dm", SourceKind: "system", Text: "x"}); err == nil {
		t.Fatal("普通入队不应接受 system")
	}
}

func TestAIFishingDefaults(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "fishing2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	want := map[string]string{
		"ai_fishing_enabled":          "0",
		"ai_fishing_hourly_limit":     "12",
		"ai_fishing_base_interval_ms": "300000",
		"ai_fishing_max_interval_ms":  "600000",
		"ai_fishing_star_weight":      "0.02",
		"ai_fishing_super_weight":     "0.002",
	}
	for k, v := range want {
		if got := d.AISetting(ctx, k, "missing"); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}
