package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAIQueueOnlyAcceptsHumanTasks(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "ai.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate 重复执行应幂等: %v", err)
	}
	seedAIDirtyData(t, d)

	ctx := context.Background()
	if _, err := d.AIEnqueue(ctx, &AITask{SourceKind: "agent", Text: "agent 回复"}); err == nil {
		t.Fatal("Agent 来源的消息不应入队（防循环根本保证）")
	}

	id, err := d.AIEnqueue(ctx, &AITask{
		Kind: "dm", Scope: "uid", ScopeKey: "1000001",
		UID: 1000001, Agent: "companion", ContactUID: 2147483645,
		SourceKind: "human", Text: "在吗",
		DeadlineTS: float64(time.Now().Add(time.Minute).Unix()),
	})
	if err != nil {
		t.Fatalf("AIEnqueue: %v", err)
	}

	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("AIPopTask = %v, %v", ok, err)
	}
	if task.ID != id || task.Agent != "companion" || task.SourceKind != "human" {
		t.Fatalf("取到错误任务: %+v", task)
	}
	if _, ok, _ := d.AIPopTask(ctx); ok {
		t.Fatal("同一任务被重复取出")
	}

	if err := d.AIComplete(ctx, id, "done", "你好", `{"total_tokens":10}`, ""); err != nil {
		t.Fatalf("AIComplete: %v", err)
	}
	results, err := d.AIPendingResults(ctx, 10)
	if err != nil || len(results) != 1 {
		t.Fatalf("AIPendingResults = %v, %v", results, err)
	}
	if results[0].ReplyText != "你好" || results[0].ContactUID != 2147483645 || results[0].UID != 1000001 {
		t.Fatalf("结果缺少投递信息: %+v", results[0])
	}
	if err := d.AIMarkDelivered(ctx, results[0].ID, true); err != nil {
		t.Fatalf("AIMarkDelivered: %v", err)
	}
	again, _ := d.AIPendingResults(ctx, 10)
	if len(again) != 0 {
		t.Fatalf("已投递结果被重复取出: %v", again)
	}
}

// 旧库升级：历史上建过但从未写入的 ai_session / ai_message 应被清掉，
// 否则表归属校验会因为「未登记的表」拒绝启动。
func TestMigrateAIDropsLegacySessionTables(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.SQL.Exec(`
		CREATE TABLE ai_session(id INTEGER PRIMARY KEY, scope TEXT);
		CREATE TABLE ai_message(id INTEGER PRIMARY KEY, session_id INTEGER);`); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTableOwnership(context.Background(), d); err != nil {
		t.Fatalf("升级后表归属不一致: %v", err)
	}
	var n int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name IN ('ai_session','ai_message')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("旧会话表未清理: n=%d err=%v", n, err)
	}
}

// 过期任务不再处理；异常退出留下的 processing 任务只重试一次，第二次落 fallback。
func TestAIExpireAndRecover(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "expire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	now := float64(time.Now().Unix())

	if _, err := d.AIEnqueue(ctx, &AITask{
		Kind: "dm", Scope: "uid", ScopeKey: "1", UID: 1, Agent: "a",
		ContactUID: 2147483645, SourceKind: "human", Text: "过期消息",
		DeadlineTS: now - 10,
	}); err != nil {
		t.Fatal(err)
	}
	liveID, err := d.AIEnqueue(ctx, &AITask{
		Kind: "dm", Scope: "uid", ScopeKey: "1", UID: 1, Agent: "a",
		ContactUID: 2147483645, SourceKind: "human", Text: "有效消息",
		DeadlineTS: now + 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	expired, recovered, err := d.AIExpireTasks(ctx, now, 60)
	if err != nil || expired != 1 || recovered != 0 {
		t.Fatalf("首次清理 = %d/%d, %v", expired, recovered, err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok || task.ID != liveID {
		t.Fatalf("过期任务仍被取出: %+v ok=%v err=%v", task, ok, err)
	}
	var text string
	if err := d.SQL.QueryRow(`SELECT text FROM ai_task WHERE id=?`, task.ID).Scan(&text); err != nil || text == "" {
		t.Fatalf("有效任务原文不应被清空: %q %v", text, err)
	}
	// 模拟 worker 崩溃：processing 超过 deadline+grace 后退回 pending。
	if _, recovered, err = d.AIExpireTasks(ctx, now+200, 60); err != nil || recovered != 1 {
		t.Fatalf("回收 processing = %d, %v", recovered, err)
	}
	task, ok, err = d.AIPopTask(ctx)
	if err != nil || !ok || task.ID != liveID {
		t.Fatalf("重试任务错误: %+v ok=%v err=%v", task, ok, err)
	}
	// 第二次崩溃不再重试。
	if _, _, err = d.AIExpireTasks(ctx, now+400, 60); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ = d.AIPopTask(ctx); ok {
		t.Fatal("超过重试次数的任务不应再被取出")
	}
}

// 连发的私聊应并入尚未处理的 DM 任务，只产生一次模型请求。
func TestAIMergePendingDM(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "merge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	deadline := float64(time.Now().Add(time.Minute).Unix())
	id, err := d.AIEnqueue(ctx, &AITask{
		Kind: "dm", Scope: "uid", ScopeKey: "1", UID: 1, Agent: "a",
		ContactUID: 2147483645, SourceKind: "human", Text: "第一句",
		DeadlineTS: deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	merged, err := d.AIMergePendingDM(ctx, 1, "a", "第二句", deadline, 500)
	if err != nil || !merged {
		t.Fatalf("应合并: %v %v", merged, err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok || task.ID != id || task.Text != "第一句\n第二句" {
		t.Fatalf("合并结果错误: %+v ok=%v err=%v", task, ok, err)
	}
	if merged, _ := d.AIMergePendingDM(ctx, 1, "a", "第三句", deadline, 500); merged {
		t.Fatal("已 processing 的任务不应再合并")
	}
}

// 任务完成与投递后清空聊天正文，避免 AI 队列表变成永久聊天日志。
func TestAIContentScrubbed(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "scrub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	id, err := d.AIEnqueue(ctx, &AITask{
		Kind: "dm", Scope: "uid", ScopeKey: "1", UID: 1, Agent: "a",
		ContactUID: 2147483645, SourceKind: "human", Text: "玩家原话",
		DeadlineTS: float64(time.Now().Add(time.Minute).Unix()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := d.AIPopTask(ctx); err != nil || !ok {
		t.Fatalf("取任务: %v %v", ok, err)
	}
	if err := d.AIComplete(ctx, id, "done", "模型回复", "", ""); err != nil {
		t.Fatal(err)
	}
	var text string
	if err := d.SQL.QueryRow(`SELECT text FROM ai_task WHERE id=?`, id).Scan(&text); err != nil || text != "" {
		t.Fatalf("完成后原文未清空: %q %v", text, err)
	}
	results, err := d.AIPendingResults(ctx, 10)
	if err != nil || len(results) != 1 {
		t.Fatalf("待投递结果 = %v, %v", results, err)
	}
	if err := d.AIMarkDelivered(ctx, results[0].ID, true); err != nil {
		t.Fatal(err)
	}
	var reply string
	if err := d.SQL.QueryRow(`SELECT reply_text FROM ai_result WHERE id=?`, results[0].ID).Scan(&reply); err != nil || reply != "" {
		t.Fatalf("投递后回复未清空: %q %v", reply, err)
	}
}

// 联系人 UID 只能落在虚拟号段，不能覆盖真实玩家号段。
func TestAIContactUIDValidation(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "uid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	if _, _, err := d.AICreateAgent(ctx, AIAgent{
		Key: "bad-uid", Display: "坏 UID", ContactUID: 1000001,
	}, "[]"); !errors.Is(err, ErrAIContactUIDInvalid) {
		t.Fatalf("真实玩家号段应被拒绝: %v", err)
	}
	if _, _, err := d.AICreateAgent(ctx, AIAgent{
		Key: "good-uid", Display: "好 UID", ContactUID: 2147483000,
	}, "[]"); err != nil {
		t.Fatalf("虚拟号段应允许: %v", err)
	}
	if err := d.AISetAgentJoined(ctx, "companion", true, 1000001); !errors.Is(err, ErrAIContactUIDInvalid) {
		t.Fatalf("真实玩家号段应被拒绝: %v", err)
	}
	if err := d.AISetAgentJoined(ctx, "companion", true, 2147483645); err != nil {
		t.Fatalf("合法 UID 加入失败: %v", err)
	}
}

// 批量设置一次事务写入；任一键失败时整批不落库。
func TestAISetSettingsBulk(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	if err := d.AISetSettings(ctx, map[string]string{
		"outbound_enabled": "0", "web_search_enabled": "1",
	}); err != nil {
		t.Fatal(err)
	}
	if d.AISetting(ctx, "outbound_enabled", "") != "0" || d.AISetting(ctx, "web_search_enabled", "") != "1" {
		t.Fatal("批量设置未生效")
	}
}

func TestAISettingRoundTrip(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "ai2.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seedAIDirtyData(t, d)
	ctx := context.Background()
	if got := d.AISetting(ctx, "web_search_enabled", "fallback"); got != "0" {
		t.Fatalf("联网搜索默认值应为关，得到 %q", got)
	}
	if got := d.AISetting(ctx, "outbound_enabled", "fallback"); got != "1" {
		t.Fatalf("模型调用默认值应为开，得到 %q", got)
	}
	if got := d.AISetting(ctx, "ai_map_card_enabled", "missing"); got != "missing" {
		t.Fatalf("已移除的鱼情卡设置不应再写入: %q", got)
	}
	if err := d.AISetSetting(ctx, "web_search_enabled", "1"); err != nil {
		t.Fatalf("AISetSetting: %v", err)
	}
	if got := d.AISetting(ctx, "web_search_enabled", ""); got != "1" {
		t.Fatalf("后台手动打开未生效: %q", got)
	}
}
