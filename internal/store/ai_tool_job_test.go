package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAIToolJobLifecycle(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "tool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	parentID, err := d.AIEnqueue(ctx, &AITask{Kind: "dm", SourceKind: "human"})
	if err != nil {
		t.Fatal(err)
	}
	task, claimed, err := d.AIPopTask(ctx)
	if err != nil || !claimed || task.ID != parentID {
		t.Fatalf("claim parent: %v/%v", claimed, err)
	}
	id, err := d.AIEnqueueToolJob(ctx, parentID, "fish_query", "河鲈", 1000001)
	if err != nil {
		t.Fatalf("入队: %v", err)
	}
	jobs, err := d.AIPendingToolJobs(ctx, 8)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("取待处理 = %v, %v", jobs, err)
	}
	j := jobs[0]
	if j.ID != id || j.TaskID != parentID || j.Tool != "fish_query" || j.Args != "河鲈" || j.UID != 1000001 {
		t.Fatalf("作业内容错误: %+v", j)
	}
	if again, _ := d.AIPendingToolJobs(ctx, 8); len(again) != 0 {
		t.Fatalf("同一作业被重复取出: %v", again)
	}
	if _, ok, _ := d.AIGetToolResult(ctx, id); ok {
		t.Fatal("未完成不应返回结果")
	}
	if err := d.AICompleteToolJob(ctx, id, "河鲈 · 区域 X:1～Y:2"); err != nil {
		t.Fatalf("写结果: %v", err)
	}
	got, ok, err := d.AIGetToolResult(ctx, id)
	if err != nil || !ok || got != "河鲈 · 区域 X:1～Y:2" {
		t.Fatalf("读结果 = %q %v %v", got, ok, err)
	}
}

// gameserver 异常退出后，停在 processing 的工具作业应被标记为超时，不能永远挂着。
func TestAIExpireToolJob(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "tool-expire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	parentID, err := d.AIEnqueue(ctx, &AITask{Kind: "dm", SourceKind: "human"})
	if err != nil {
		t.Fatal(err)
	}
	task, claimed, err := d.AIPopTask(ctx)
	if err != nil || !claimed || task.ID != parentID {
		t.Fatalf("claim parent: %v/%v", claimed, err)
	}
	id, err := d.AIEnqueueToolJob(ctx, parentID, "fish_query", "河鲈", 1000001)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AIPendingToolJobs(ctx, 8); err != nil {
		t.Fatal(err)
	}
	if n, err := d.AIExpireToolJobs(ctx, float64(time.Now().Add(time.Minute).Unix())); err != nil || n != 1 {
		t.Fatalf("超时清理 = %d, %v", n, err)
	}
	got, ok, err := d.AIGetToolResult(ctx, id)
	if err != nil || !ok || got != "（查询超时）" {
		t.Fatalf("超时结果 = %q ok=%v err=%v", got, ok, err)
	}
}
