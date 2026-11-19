package store

// T-020 / DEV-004：运行态命令存储层测试。
//
// 这里只验证「持久命令 + 去重 + 代次 + 同事务应用」的存储语义：
// 崩溃三点、双消费者、TTL/队列满、坏命令不堵队、旧行缺 control、
// 软删除 + 五类脏数据。业务应用规则（reroll 概率、tick 竞争等）在
// gameserver 的 TestAIRuntimeCommand* 里用真实消费者验证。

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const runtimeTestAgent = "companion"

func openRuntimeTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		t.Fatalf("迁移: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func runtimeEnqueue(t *testing.T, db *DB, req AIRuntimeEnqueueRequest) *AIRuntimeCommand {
	t.Helper()
	cmd, _, err := db.AIEnqueueRuntimeCommand(context.Background(), req)
	if err != nil {
		t.Fatalf("入队 %+v: %v", req, err)
	}
	return cmd
}

func runtimeCount(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("统计 %q: %v", query, err)
	}
	return n
}

func runtimeCommandStatus(t *testing.T, db *DB, id int64) string {
	t.Helper()
	cmd, err := db.AIGetRuntimeCommand(context.Background(), id)
	if err != nil {
		t.Fatalf("读命令 %d: %v", id, err)
	}
	return cmd.Status
}

func runtimeSeedMemory(t *testing.T, db *DB, agentKey string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := db.AIAddMemory(context.Background(), agentKey, "star", "perch", "鲈鱼", 1200+int64(i), "level_001_lake"); err != nil {
			t.Fatalf("seed 记忆: %v", err)
		}
	}
}

func runtimeSeedActor(t *testing.T, db *DB, agentKey, activity string) {
	t.Helper()
	now := float64(time.Now().Unix())
	if _, err := db.SQL.Exec(`
		INSERT INTO ai_actor_state(agent_key, loc, loc_name, activity, target_fish, target_name,
		                           since_ts, loc_since_ts, switch_ts, activity_until_ts, energy, updated_ts)
		VALUES(?, 'level_001_lake','测试湖',?,'perch','鲈鱼',?,?,0,?,55,?)
		ON CONFLICT(agent_key) DO UPDATE SET activity=excluded.activity, activity_until_ts=excluded.activity_until_ts`,
		agentKey, activity, now, now, now+3600, now); err != nil {
		t.Fatalf("seed 角色状态: %v", err)
	}
	if _, err := db.SQL.Exec(`UPDATE ai_agent SET actor_enabled=1 WHERE key=?`, agentKey); err != nil {
		t.Fatalf("打开角色开关: %v", err)
	}
}

func runtimeActorActivity(t *testing.T, db *DB, agentKey string) string {
	t.Helper()
	var activity string
	if err := db.SQL.QueryRow(`SELECT activity FROM ai_actor_state WHERE agent_key=?`, agentKey).Scan(&activity); err != nil {
		t.Fatalf("读角色活动: %v", err)
	}
	return activity
}

func runtimeCreateEffectTable(t *testing.T, db *DB) {
	t.Helper()
	if _, err := db.SQL.Exec(`CREATE TABLE IF NOT EXISTS runtime_test_effect(
		request_id TEXT PRIMARY KEY, ts REAL NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("建副作用表: %v", err)
	}
}

// runtimeEffectApply 是「一次应用」的独立副作用记录器：每个命令恰好插一行。
func runtimeEffectApply(effects *int64) AIRuntimeApplyFunc {
	return func(tx *AIRuntimeTx, cmd *AIRuntimeCommand) (bool, string, error) {
		if _, err := tx.SQL().ExecContext(tx.Context(),
			`INSERT INTO runtime_test_effect(request_id, ts) VALUES(?,?)`,
			cmd.RequestID, float64(time.Now().Unix())); err != nil {
			return false, "", err
		}
		atomic.AddInt64(effects, 1)
		_, err := tx.BumpControl(cmd.AgentKey, 0, false)
		return true, "", err
	}
}

func runtimeEffectCount(t *testing.T, db *DB) int {
	t.Helper()
	return runtimeCount(t, db, `SELECT COUNT(*) FROM runtime_test_effect`)
}

// TestAIRuntimeCommandMigration：现取号迁移、两表/索引、control 种子、重复迁移不重置。
func TestAIRuntimeCommandMigration(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	ctx := context.Background()

	for _, table := range []string{"ai_runtime_command", "ai_runtime_control"} {
		if got := runtimeCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table); got != 1 {
			t.Fatalf("表 %s 不存在", table)
		}
	}
	for _, index := range []string{"idx_ai_runtime_command_status_id", "idx_ai_runtime_command_agent_id"} {
		if got := runtimeCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, index); got != 1 {
			t.Fatalf("索引 %s 不存在", index)
		}
	}
	// 迁移号现取：AI 段最大号 = 00907（T-108 / DEV-121 A 的凭据表），且只登记一次。
	var maxAI int64
	if err := db.SQL.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE version>=900`).Scan(&maxAI); err != nil {
		t.Fatal(err)
	}
	if maxAI != 907 {
		t.Fatalf("AI 迁移最大号 = %d，期望 907（执行时现取）", maxAI)
	}
	if got := runtimeCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE name='migrations/00905_ai_runtime_commands.sql'`); got != 1 {
		t.Fatalf("00905 登记行 = %d", got)
	}
	// 现有 Agent 首次接管：5 个种子 Agent 都有 generation=1/revision=0。
	if got := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_runtime_control WHERE generation=1 AND revision=0 AND tombstoned=0`); got < 5 {
		t.Fatalf("control 种子 = %d，期望 ≥5", got)
	}
	// 重复迁移不重置代次。
	if _, err := db.SQL.Exec(`UPDATE ai_runtime_control SET generation=7, revision=9 WHERE agent_key=?`, runtimeTestAgent); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("第二次迁移: %v", err)
	}
	var gen, rev int64
	if err := db.SQL.QueryRow(`SELECT generation, revision FROM ai_runtime_control WHERE agent_key=?`,
		runtimeTestAgent).Scan(&gen, &rev); err != nil {
		t.Fatal(err)
	}
	if gen != 7 || rev != 9 {
		t.Fatalf("重复迁移重置了代次: generation=%d revision=%d", gen, rev)
	}
	// 新表必须登记归属，否则备份/拆分工具会静默漏表。
	if err := ValidateTableOwnership(ctx, db); err != nil {
		t.Fatalf("表归属校验: %v", err)
	}
}

// TestAIRuntimeCommandDedupSameRequest：同 request + 同 payload 只落一行。
func TestAIRuntimeCommandDedupSameRequest(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	req := AIRuntimeEnqueueRequest{
		RequestID: "ad:dup-1", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	}
	first := runtimeEnqueue(t, db, req)
	second, dup, err := db.AIEnqueueRuntimeCommand(context.Background(), req)
	if err != nil {
		t.Fatalf("第二次入队: %v", err)
	}
	if !dup || second.ID != first.ID {
		t.Fatalf("重复请求未返回既有行: dup=%v id=%d/%d", dup, second.ID, first.ID)
	}
	if got := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_runtime_command`); got != 1 {
		t.Fatalf("命令行数 = %d，期望 1", got)
	}
}

// TestAIRuntimeCommandDedupPayloadConflict：同键不同 payload 必须拒绝。
func TestAIRuntimeCommandDedupPayloadConflict(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	base := AIRuntimeEnqueueRequest{
		RequestID: "ad:conflict", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindActorSetEnabled, AgentKey: runtimeTestAgent,
	}
	base.Payload = []byte(`{"enabled":true}`)
	runtimeEnqueue(t, db, base)
	base.Payload = []byte(`{"enabled":false}`)
	_, _, err := db.AIEnqueueRuntimeCommand(context.Background(), base)
	if !errors.Is(err, ErrAIRuntimeCommandConflict) {
		t.Fatalf("同键不同 payload 错误 = %v，期望 conflict", err)
	}
	if got := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_runtime_command`); got != 1 {
		t.Fatalf("冲突请求仍写入了新行: %d", got)
	}
}

// TestAIRuntimeCommandPayloadCanonical：JSON 键序/空白不同但意图相同 → 去重命中。
func TestAIRuntimeCommandPayloadCanonical(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	first, _, err := db.AIEnqueueRuntimeCommand(context.Background(), AIRuntimeEnqueueRequest{
		RequestID: "ad:canonical", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindActorSetEnabled, AgentKey: runtimeTestAgent,
		Payload: []byte(`{"enabled":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, dup, err := db.AIEnqueueRuntimeCommand(context.Background(), AIRuntimeEnqueueRequest{
		RequestID: "ad:canonical", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindActorSetEnabled, AgentKey: runtimeTestAgent,
		Payload: []byte(" { \"enabled\" : true } "),
	})
	if err != nil {
		t.Fatalf("规范 payload 去重失败: %v", err)
	}
	if !dup || second.ID != first.ID || second.Payload != `{"enabled":true}` {
		t.Fatalf("规范 payload = %q dup=%v id=%d", second.Payload, dup, second.ID)
	}
}

// TestAIRuntimeCommandInvalidPayload：未知字段/空 payload/超长参数一律拒绝。
func TestAIRuntimeCommandInvalidPayload(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	cases := []AIRuntimeEnqueueRequest{
		{RequestID: "ad:bad-1", Source: AIRuntimeSourceAdmin, Kind: AIRuntimeKindActorSetEnabled,
			AgentKey: runtimeTestAgent, Payload: []byte(`{"enabled":true,"extra":1}`)},
		{RequestID: "ad:bad-2", Source: AIRuntimeSourceAdmin, Kind: AIRuntimeKindActorSetEnabled,
			AgentKey: runtimeTestAgent, Payload: nil},
		{RequestID: "ad:bad-3", Source: AIRuntimeSourceBridge, Kind: AIRuntimeKindActorSetEnabled,
			AgentKey: runtimeTestAgent, Payload: []byte(`{"enabled":true}`)},
		{RequestID: "ad:bad-4", Source: AIRuntimeSourceAdmin, Kind: "drop_database",
			AgentKey: runtimeTestAgent},
		{RequestID: "ad:bad-5", Source: AIRuntimeSourceAdmin, Kind: AIRuntimeKindChannelSetMember,
			AgentKey: runtimeTestAgent, Payload: []byte(`{"channel":"","enabled":true}`)},
	}
	for _, req := range cases {
		if _, _, err := db.AIEnqueueRuntimeCommand(context.Background(), req); !errors.Is(err, ErrAIRuntimeInvalidCommand) {
			t.Errorf("%s/{%s}: err=%v，期望 invalid", req.Kind, req.Payload, err)
		}
	}
}

// TestAIRuntimeCommandQueueLimits：单 Agent 32、全局 1024，满即 429 语义。
func TestAIRuntimeCommandQueueLimits(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	for i := 0; i < AIRuntimeMaxPendingAgent; i++ {
		runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
			RequestID: "ad:agent-full-" + itoaTest(i), Source: AIRuntimeSourceAdmin,
			Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
		})
	}
	_, _, err := db.AIEnqueueRuntimeCommand(context.Background(), AIRuntimeEnqueueRequest{
		RequestID: "ad:agent-over", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	})
	if !errors.Is(err, ErrAIRuntimeQueueFull) {
		t.Fatalf("单 Agent 第 %d 条错误 = %v，期望 queue full", AIRuntimeMaxPendingAgent+1, err)
	}

	// 全局上限：直接填入 pending 行（构造前置状态，不绕过被测入队逻辑）。
	other, _ := openRuntimeTestDB(t)
	if _, err := other.SQL.Exec(`DELETE FROM ai_runtime_command`); err != nil {
		t.Fatal(err)
	}
	tx, err := other.SQL.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < AIRuntimeMaxPendingGlobal; i++ {
		if _, err := tx.Exec(`
			INSERT INTO ai_runtime_command(request_id, source, kind, agent_key, payload,
				payload_hash, created_ts, expires_ts, status)
			VALUES(?,'admin','memory_clear',?,'{}','h',?,?,'pending')`,
			"fill-"+itoaTest(i), runtimeTestAgent, float64(time.Now().Unix()), float64(time.Now().Add(time.Hour).Unix())); err != nil {
			t.Fatalf("填全局队列: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, _, err = other.AIEnqueueRuntimeCommand(context.Background(), AIRuntimeEnqueueRequest{
		RequestID: "ad:global-over", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	})
	if !errors.Is(err, ErrAIRuntimeQueueFull) {
		t.Fatalf("全局第 %d 条错误 = %v，期望 queue full", AIRuntimeMaxPendingGlobal+1, err)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestAIRuntimeCommandTTLExpires：到期命令不执行，标 expired，后续不再重放。
func TestAIRuntimeCommandTTLExpires(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	runtimeCreateEffectTable(t, db)
	base := float64(time.Now().Unix()) - 120
	cmd := runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:ttl", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
		TTL: 30 * time.Second, Now: base,
	})
	var effects int64
	got, err := db.AIApplyNextRuntimeCommand(context.Background(), 0, runtimeEffectApply(&effects))
	if err != nil {
		t.Fatalf("应用过期命令: %v", err)
	}
	if got == nil || got.Status != AIRuntimeStatusExpired || got.ID != cmd.ID {
		t.Fatalf("过期命令结果 = %+v，期望 expired", got)
	}
	if effects != 0 {
		t.Fatalf("过期命令被执行了 %d 次", effects)
	}
	next, err := db.AIApplyNextRuntimeCommand(context.Background(), 0, runtimeEffectApply(&effects))
	if err != nil || next != nil {
		t.Fatalf("过期后仍有待处理命令: %+v err=%v", next, err)
	}
}

// TestAIRuntimeCommandBadRowDoesNotBlockQueue：坏命令标 rejected，后续好命令照常应用。
func TestAIRuntimeCommandBadRowDoesNotBlockQueue(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	runtimeCreateEffectTable(t, db)
	now := float64(time.Now().Unix())
	if _, err := db.SQL.Exec(`
		INSERT INTO ai_runtime_command(request_id, source, kind, agent_key, payload,
			payload_hash, created_ts, expires_ts, status)
		VALUES('bad:unknown-kind','admin','drop_everything',?,'{}','deadbeef',?,?,'pending')`,
		runtimeTestAgent, now, now+600); err != nil {
		t.Fatalf("插入坏命令: %v", err)
	}
	runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:after-bad", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	})
	var effects int64
	bad, err := db.AIApplyNextRuntimeCommand(context.Background(), 0, runtimeEffectApply(&effects))
	if err != nil {
		t.Fatalf("应用坏命令: %v", err)
	}
	if bad.Status != AIRuntimeStatusRejected || bad.Reason != "bad_command" {
		t.Fatalf("坏命令终态 = %s/%s，期望 rejected/bad_command", bad.Status, bad.Reason)
	}
	good, err := db.AIApplyNextRuntimeCommand(context.Background(), 0, runtimeEffectApply(&effects))
	if err != nil {
		t.Fatalf("应用好命令: %v", err)
	}
	if good.Status != AIRuntimeStatusApplied || effects != 1 {
		t.Fatalf("好命令被坏命令堵住: status=%s effects=%d", good.Status, effects)
	}
}

// TestAIRuntimeCommandLegacyRowWithoutControl：旧行没有 control 时按 1/0 建立，不阻断应用。
func TestAIRuntimeCommandLegacyRowWithoutControl(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	ctx := context.Background()
	if _, err := db.SQL.Exec(`DELETE FROM ai_runtime_control WHERE agent_key=?`, runtimeTestAgent); err != nil {
		t.Fatal(err)
	}
	runtimeCreateEffectTable(t, db)
	runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:no-control", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	})
	var effects int64
	cmd, err := db.AIApplyNextRuntimeCommand(ctx, 0, runtimeEffectApply(&effects))
	if err != nil || cmd.Status != AIRuntimeStatusApplied {
		t.Fatalf("缺 control 的应用 = %+v err=%v", cmd, err)
	}
	ctl, err := db.AIGetRuntimeControl(ctx, runtimeTestAgent)
	if err != nil {
		t.Fatal(err)
	}
	if ctl.Generation != 1 || ctl.Revision != 1 {
		t.Fatalf("control 默认值 = gen%d rev%d，期望 1/1", ctl.Generation, ctl.Revision)
	}
}

// TestAIRuntimeCommandClearThenNewMemorySurvivesRetry：重复 clear 不能删掉清空后新写的记忆。
func TestAIRuntimeCommandClearThenNewMemorySurvivesRetry(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	ctx := context.Background()
	runtimeSeedMemory(t, db, runtimeTestAgent, 2)
	clear := func(tx *AIRuntimeTx, cmd *AIRuntimeCommand) (bool, string, error) {
		if err := tx.ClearMemories(cmd.AgentKey); err != nil {
			return false, "", err
		}
		_, err := tx.BumpControl(cmd.AgentKey, 0, false)
		return true, "", err
	}
	req := AIRuntimeEnqueueRequest{
		RequestID: "ad:clear-once", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	}
	first := runtimeEnqueue(t, db, req)
	if _, err := db.AIApplyRuntimeCommand(ctx, first.ID, 0, clear); err != nil {
		t.Fatalf("应用 clear: %v", err)
	}
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_memory WHERE agent_key=?`, runtimeTestAgent); n != 0 {
		t.Fatalf("clear 后记忆 = %d", n)
	}
	// 清空后新写入一条记忆，再重试同一个 request（网络重试/页面重发）。
	runtimeSeedMemory(t, db, runtimeTestAgent, 1)
	retry := runtimeEnqueue(t, db, req)
	if retry.ID != first.ID {
		t.Fatalf("重试未命中既有命令: %d/%d", retry.ID, first.ID)
	}
	if _, err := db.AIApplyRuntimeCommand(ctx, retry.ID, 0, clear); err != nil {
		t.Fatalf("重试应用: %v", err)
	}
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_memory WHERE agent_key=?`, runtimeTestAgent); n != 1 {
		t.Fatalf("重复 clear 删掉了新记忆: 剩余 %d 条", n)
	}
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_runtime_command`); n != 1 {
		t.Fatalf("重复请求产生了 %d 行命令", n)
	}
}

// TestAIRuntimeCommandRerollAppliedOnlyOnce：同一次 reroll 不会再次掷骰。
func TestAIRuntimeCommandRerollAppliedOnlyOnce(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	ctx := context.Background()
	runtimeSeedActor(t, db, runtimeTestAgent, "fishing")
	rerolls := 0
	apply := func(tx *AIRuntimeTx, cmd *AIRuntimeCommand) (bool, string, error) {
		a, err := tx.Actor(cmd.AgentKey)
		if err != nil || a == nil {
			return false, "no_actor_state", err
		}
		old := a.Activity
		storeReroll := AIActorReroll
		rng := func() float64 { return 0.9 } // 固定掷骰：只关心「是否再次进入应用」
		storeReroll(a, float64(time.Now().Unix()), tx.ActorConfig(), rng)
		if a.Activity == old {
			a.Activity = "resting"
		}
		rerolls++
		if err := tx.SaveActor(a); err != nil {
			return false, "", err
		}
		_, err = tx.BumpControl(cmd.AgentKey, 0, false)
		return true, "", err
	}
	cmd := runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:reroll-once", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindActorReroll, AgentKey: runtimeTestAgent,
	})
	if _, err := db.AIApplyRuntimeCommand(ctx, cmd.ID, 0, apply); err != nil {
		t.Fatalf("应用 reroll: %v", err)
	}
	activity := runtimeActorActivity(t, db, runtimeTestAgent)
	// 进程重启后再按 id 应用同一命令：已终态，不重新掷骰。
	if _, err := db.AIApplyRuntimeCommand(ctx, cmd.ID, 0, apply); err != nil {
		t.Fatalf("重复应用 reroll: %v", err)
	}
	if rerolls != 1 {
		t.Fatalf("reroll 应用次数 = %d，期望 1", rerolls)
	}
	if got := runtimeActorActivity(t, db, runtimeTestAgent); got != activity {
		t.Fatalf("重复 reroll 改变了状态: %s → %s", activity, got)
	}
}

// TestAISingleWriterGSStoppedThenRecovered：GS 停机时意图持久等待，恢复后按序应用。
func TestAISingleWriterGSStoppedThenRecovered(t *testing.T) {
	db, path := openRuntimeTestDB(t)
	runtimeCreateEffectTable(t, db)
	for i := 0; i < 3; i++ {
		runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
			RequestID: "ad:stopped-" + itoaTest(i), Source: AIRuntimeSourceAdmin,
			Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
		})
	}
	// 模拟 GS 停机期间重启：关库再开，命令仍在 pending。
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("重开库: %v", err)
	}
	defer reopened.Close()
	var effects int64
	applied := 0
	for {
		cmd, err := reopened.AIApplyNextRuntimeCommand(context.Background(), 0, runtimeEffectApply(&effects))
		if err != nil {
			t.Fatalf("恢复后应用: %v", err)
		}
		if cmd == nil {
			break
		}
		applied++
	}
	if applied != 3 || effects != 3 {
		t.Fatalf("恢复后应用 %d 条、副作用 %d 次，期望 3/3", applied, effects)
	}
	if n := runtimeCount(t, reopened, `SELECT COUNT(*) FROM ai_runtime_command WHERE status='pending'`); n != 0 {
		t.Fatalf("恢复后仍有 %d 条 pending", n)
	}
}

// TestAISingleWriterCrashBeforeApply：应用前崩溃，重启后只应用一次。
func TestAISingleWriterCrashBeforeApply(t *testing.T) {
	db, path := openRuntimeTestDB(t)
	runtimeCreateEffectTable(t, db)
	runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:crash-before", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var effects int64
	cmd, err := reopened.AIApplyNextRuntimeCommand(context.Background(), 0, runtimeEffectApply(&effects))
	if err != nil || cmd.Status != AIRuntimeStatusApplied {
		t.Fatalf("重启后应用 = %+v err=%v", cmd, err)
	}
	if effects != 1 {
		t.Fatalf("崩溃恢复后副作用 = %d，期望 1", effects)
	}
}

// TestAISingleWriterCrashInsideApply：事务中崩溃整体回滚，命令保持 pending，重试只应用一次。
func TestAISingleWriterCrashInsideApply(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	ctx := context.Background()
	runtimeSeedMemory(t, db, runtimeTestAgent, 2)
	cmd := runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:crash-inside", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	})
	_, err := db.AIApplyNextRuntimeCommand(ctx, 0, func(tx *AIRuntimeTx, cmd *AIRuntimeCommand) (bool, string, error) {
		if err := tx.ClearMemories(cmd.AgentKey); err != nil {
			return false, "", err
		}
		return false, "", errors.New("模拟事务中崩溃")
	})
	if err == nil {
		t.Fatal("事务中崩溃应返回错误")
	}
	// 状态与终态全部回滚：记忆仍在、命令仍 pending。
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_memory WHERE agent_key=?`, runtimeTestAgent); n != 2 {
		t.Fatalf("崩溃回滚后记忆 = %d，期望 2", n)
	}
	if st := runtimeCommandStatus(t, db, cmd.ID); st != AIRuntimeStatusPending {
		t.Fatalf("崩溃后命令状态 = %s，期望 pending", st)
	}
	applied := 0
	got, err := db.AIApplyNextRuntimeCommand(ctx, 0, func(tx *AIRuntimeTx, cmd *AIRuntimeCommand) (bool, string, error) {
		applied++
		if err := tx.ClearMemories(cmd.AgentKey); err != nil {
			return false, "", err
		}
		_, err := tx.BumpControl(cmd.AgentKey, 0, false)
		return true, "", err
	})
	if err != nil || got.Status != AIRuntimeStatusApplied || applied != 1 {
		t.Fatalf("重试应用 = %+v applied=%d err=%v", got, applied, err)
	}
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_memory WHERE agent_key=?`, runtimeTestAgent); n != 0 {
		t.Fatalf("重试后记忆 = %d", n)
	}
}

// TestAISingleWriterCrashAfterCommit：提交后未回显，重启按同键查询得到 applied，不重放。
func TestAISingleWriterCrashAfterCommit(t *testing.T) {
	db, path := openRuntimeTestDB(t)
	runtimeCreateEffectTable(t, db)
	req := AIRuntimeEnqueueRequest{
		RequestID: "ad:crash-after", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	}
	cmd := runtimeEnqueue(t, db, req)
	var effects int64
	if _, err := db.AIApplyNextRuntimeCommand(context.Background(), 0, runtimeEffectApply(&effects)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil { // 提交后、回显前「崩溃」
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	byRequest, err := reopened.AIGetRuntimeCommandByRequest(context.Background(), req.RequestID)
	if err != nil || byRequest.Status != AIRuntimeStatusApplied {
		t.Fatalf("重启用同键查询 = %+v err=%v", byRequest, err)
	}
	// 再用同一个 request 重试：去重命中既有的 applied，不产生第二次应用。
	again, dup, err := reopened.AIEnqueueRuntimeCommand(context.Background(), req)
	if err != nil || !dup || again.Status != AIRuntimeStatusApplied {
		t.Fatalf("重启后重试 = %+v dup=%v err=%v", again, dup, err)
	}
	if _, err := reopened.AIApplyRuntimeCommand(context.Background(), cmd.ID, 0, runtimeEffectApply(&effects)); err != nil {
		t.Fatal(err)
	}
	if effects != 1 {
		t.Fatalf("重启后副作用 = %d，期望 1", effects)
	}
}

// TestAISingleWriterTwoConsumersExactlyOnce：双 DB 连接抢同一行，独立期望是「一次应用」。
func TestAISingleWriterTwoConsumersExactlyOnce(t *testing.T) {
	db1, path := openRuntimeTestDB(t)
	runtimeCreateEffectTable(t, db1)
	const total = 24
	for i := 0; i < total; i++ {
		runtimeEnqueue(t, db1, AIRuntimeEnqueueRequest{
			RequestID: "ad:race-" + itoaTest(i), Source: AIRuntimeSourceAdmin,
			Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
		})
	}
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("第二个连接: %v", err)
	}
	defer db2.Close()

	var effects int64
	var processed int64
	var wg sync.WaitGroup
	consume := func(db *DB) {
		defer wg.Done()
		for {
			cmd, err := db.AIApplyNextRuntimeCommand(context.Background(), 0, runtimeEffectApply(&effects))
			if err != nil {
				// 单写者语义允许 busy 重试；这里失败即测试失败（busy_timeout 10s）。
				t.Errorf("消费者应用失败: %v", err)
				return
			}
			if cmd == nil {
				return
			}
			atomic.AddInt64(&processed, 1)
			time.Sleep(200 * time.Microsecond)
		}
	}
	wg.Add(2)
	go consume(db1)
	go consume(db2)
	wg.Wait()

	if processed != total {
		t.Fatalf("两个消费者共处理 %d 条，期望 %d", processed, total)
	}
	if effects != total {
		t.Fatalf("副作用 %d 次，期望 %d（同一行只能应用一次）", effects, total)
	}
	if n := runtimeCount(t, db1, `SELECT COUNT(*) FROM ai_runtime_command WHERE status='applied'`); n != total {
		t.Fatalf("applied 行 = %d，期望 %d", n, total)
	}
	if n := runtimeCount(t, db1, `SELECT COUNT(*) FROM runtime_test_effect`); n != total {
		t.Fatalf("副作用表 = %d 行，期望 %d", n, total)
	}
}

// TestAIRuntimeCommandSoftDeleteWithDirtyData：软删除 + 五类脏数据下不越界、不复活。
func TestAIRuntimeCommandSoftDeleteWithDirtyData(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	ctx := context.Background()
	seed := seedAIDirtyData(t, db)
	// 待删除的是一个真实存在的 Agent；脏数据里的幽灵行保持原样。
	runtimeSeedActor(t, db, runtimeTestAgent, "fishing")
	if err := db.AISetChannelAgent(ctx, "runtime_del_room", runtimeTestAgent, true); err != nil {
		t.Fatal(err)
	}
	if err := db.AIAddMemory(ctx, runtimeTestAgent, "star", "perch", "鲈鱼", 1000, "x"); err != nil {
		t.Fatal(err)
	}
	apply := func(tx *AIRuntimeTx, cmd *AIRuntimeCommand) (bool, string, error) {
		if _, err := tx.DeleteAgent(cmd.AgentKey); err != nil {
			return false, "", err
		}
		_, err := tx.BumpControl(cmd.AgentKey, 1, true)
		return true, "", err
	}
	cmd := runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:delete-agent", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindAgentDelete, AgentKey: runtimeTestAgent,
	})
	got, err := db.AIApplyRuntimeCommand(ctx, cmd.ID, 0, apply)
	if err != nil || got.Status != AIRuntimeStatusApplied {
		t.Fatalf("删除 Agent = %+v err=%v", got, err)
	}
	// 运行态级联：actor 删掉、频道成员禁用、未投递结果作废。
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_actor_state WHERE agent_key=?`, runtimeTestAgent); n != 0 {
		t.Fatalf("删除后 actor 行 = %d", n)
	}
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_channel_agent WHERE channel='runtime_del_room' AND enabled=1`); n != 0 {
		t.Fatalf("删除后频道成员仍启用: %d", n)
	}
	// 历史与记忆保留；control 留墓碑且代次单调（种子 1 → 删除后 2）。
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_memory WHERE agent_key=?`, runtimeTestAgent); n != 1 {
		t.Fatalf("删除后记忆 = %d，期望保留 1", n)
	}
	ctl, err := db.AIGetRuntimeControl(ctx, runtimeTestAgent)
	if err != nil {
		t.Fatal(err)
	}
	if !ctl.Tombstoned || ctl.Generation != 2 {
		t.Fatalf("control = %+v，期望 tombstoned 且 generation=2", ctl)
	}
	// 五类脏数据中的多模型行与幽灵登记不受影响；表归属仍自洽。
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_model WHERE provider_id=?`, seed.ProviderID); n != 2 {
		t.Fatalf("脏模型行 = %d，期望 2", n)
	}
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_actor_state WHERE agent_key=?`, dirtyGhostAgentKey); n != 1 {
		t.Fatalf("幽灵 Agent 的孤儿运行态被误删: %d", n)
	}
	if err := ValidateTableOwnership(ctx, db); err != nil {
		t.Fatalf("表归属校验: %v", err)
	}
}

// TestAISingleWriterStateAndAckAtomic：运行态更新与命令终态必须同事务：
// 应用返回后要么两者都在、要么两者都不在，不存在「状态已改但命令仍 pending」。
func TestAISingleWriterStateAndAckAtomic(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	ctx := context.Background()
	runtimeSeedMemory(t, db, runtimeTestAgent, 2)
	cmd := runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:state-ack", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	})
	applied, err := db.AIApplyNextRuntimeCommand(ctx, 0, func(tx *AIRuntimeTx, cmd *AIRuntimeCommand) (bool, string, error) {
		if err := tx.ClearMemories(cmd.AgentKey); err != nil {
			return false, "", err
		}
		_, err := tx.BumpControl(cmd.AgentKey, 0, false)
		return true, "", err
	})
	if err != nil {
		t.Fatalf("应用返回错误（状态与 ack 未同事务）: %v", err)
	}
	var memories int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_memory WHERE agent_key=?`, runtimeTestAgent).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	status := runtimeCommandStatus(t, db, cmd.ID)
	if memories == 0 && status != AIRuntimeStatusApplied {
		t.Fatalf("状态已应用但命令终态 = %s（半应用）", status)
	}
	if memories != 0 || status != AIRuntimeStatusApplied {
		t.Fatalf("应用后 memories=%d status=%s applied=%v", memories, status, applied)
	}
}

// TestAIRuntimeCommandApplyRollsBackOnFnError 证明「状态与终态同一事务」：
// fn 写状态后返回 error，两者都不落库。
func TestAIRuntimeCommandApplyRollsBackOnFnError(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	ctx := context.Background()
	runtimeSeedActor(t, db, runtimeTestAgent, "fishing")
	cmd := runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:atomic", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindActorReroll, AgentKey: runtimeTestAgent,
	})
	_, err := db.AIApplyRuntimeCommand(ctx, cmd.ID, 0, func(tx *AIRuntimeTx, cmd *AIRuntimeCommand) (bool, string, error) {
		if _, err := tx.SQL().ExecContext(tx.Context(),
			`UPDATE ai_actor_state SET activity='resting' WHERE agent_key=?`, cmd.AgentKey); err != nil {
			return false, "", err
		}
		return false, "", errors.New("模拟 ack 写入失败")
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if got := runtimeActorActivity(t, db, runtimeTestAgent); got != "fishing" {
		t.Fatalf("回滚后活动 = %s，期望仍是 fishing", got)
	}
	if st := runtimeCommandStatus(t, db, cmd.ID); st != AIRuntimeStatusPending {
		t.Fatalf("回滚后命令状态 = %s", st)
	}
}

// TestAIRuntimeCommandTerminalCapGC：总行数达到 100000 时先 GC 过期终态，
// 仍满才拒绝新命令。
func TestAIRuntimeCommandTerminalCapGC(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	now := float64(time.Now().Unix())
	tx, err := db.SQL.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < AIRuntimeMaxTotalRows; i++ {
		if _, err := tx.Exec(`
			INSERT INTO ai_runtime_command(request_id, source, kind, agent_key, payload,
				payload_hash, created_ts, expires_ts, status, applied_ts)
			VALUES(?,'admin','memory_clear',?,'{}','h',?,?,'applied',?)`,
			"terminal-"+itoaTest(i), runtimeTestAgent, now, now+600, now); err != nil {
			t.Fatalf("填终态行 %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// 终态都还在保留期内：达到总上限，拒绝新命令。
	if _, _, err := db.AIEnqueueRuntimeCommand(context.Background(), AIRuntimeEnqueueRequest{
		RequestID: "ad:cap-full", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	}); !errors.Is(err, ErrAIRuntimeQueueFull) {
		t.Fatalf("终态满时错误 = %v，期望 queue full", err)
	}
	// 把终态改成 8 天前（超过 7 天保留期）：下一次入队先 GC，再写入新命令。
	old := now - (AIRuntimeTerminalKeep.Seconds() + 86400)
	if _, err := db.SQL.Exec(`UPDATE ai_runtime_command SET applied_ts=?`, old); err != nil {
		t.Fatal(err)
	}
	cmd := runtimeEnqueue(t, db, AIRuntimeEnqueueRequest{
		RequestID: "ad:cap-gc", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	})
	if cmd == nil || cmd.Status != AIRuntimeStatusPending {
		t.Fatalf("GC 后入队 = %+v", cmd)
	}
	if n := runtimeCount(t, db, `SELECT COUNT(*) FROM ai_runtime_command`); n != 1 {
		t.Fatalf("GC 后总行数 = %d，期望 1", n)
	}
}

// TestAIRuntimeCommandValidateSourceMatrix：入口不能自报来源绕过矩阵。
func TestAIRuntimeCommandValidateSourceMatrix(t *testing.T) {
	db, _ := openRuntimeTestDB(t)
	if _, _, err := db.AIEnqueueRuntimeCommand(context.Background(), AIRuntimeEnqueueRequest{
		RequestID: "ad:matrix", Source: AIRuntimeSourceGameServer,
		Kind: AIRuntimeKindAgentDelete, AgentKey: runtimeTestAgent,
	}); !errors.Is(err, ErrAIRuntimeInvalidCommand) {
		t.Fatalf("gameserver 提交 agent_delete 错误 = %v，期望 invalid", err)
	}
	if _, _, err := db.AIEnqueueRuntimeCommand(context.Background(), AIRuntimeEnqueueRequest{
		RequestID: "ad:matrix2", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindActorTargetSuggest, AgentKey: runtimeTestAgent,
		Payload: []byte(`{"fish_key":"perch","loc_key":"level_001_lake"}`),
	}); !errors.Is(err, ErrAIRuntimeInvalidCommand) {
		t.Fatalf("admin 提交建议错误 = %v，期望 invalid", err)
	}
	if _, _, err := db.AIEnqueueRuntimeCommand(context.Background(), AIRuntimeEnqueueRequest{
		RequestID: "bad id with space", Source: AIRuntimeSourceAdmin,
		Kind: AIRuntimeKindMemoryClear, AgentKey: runtimeTestAgent,
	}); !errors.Is(err, ErrAIRuntimeInvalidCommand) {
		t.Fatalf("非法 request_id 错误 = %v", err)
	}
}
