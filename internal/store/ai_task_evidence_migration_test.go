package store

// T-108 / DEV-121 A：两张从属凭据表的迁移与事务证据。
//
// 断言口径来自 T-108 §6 / §8 E7：
//   - 候选号 00907 只登记一次，schema 与 ai_task/ai_result 同事务建立；
//   - 空库、既有 00906 库、重复启动都是幂等；
//   - 登记失败时整条迁移回滚，旧库仍可读、旧历史（含 DM 与已终结结果）不丢；
//   - 证据与父行必须同事务：失败时不留半套证据；
//   - 清理按父行回收，不产生孤儿，也不改写旧行。

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
)

// TestAITaskEvidenceMigration 覆盖迁移的空库/既有库/重复/失败重试与历史可读。
func TestAITaskEvidenceMigration(t *testing.T) {
	const migrationName = "migrations/00907_ai_task_evidence.sql"
	ctx := t.Context()

	// 候选号复核：本轮现取，官方与两支最大号都是 00906，00907 为空才可用。
	var maxVersion int64
	if err := openEvidenceScratchDB(t).SQL.QueryRow(
		`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&maxVersion); err != nil {
		t.Fatal(err)
	}
	if maxVersion != 907 {
		t.Fatalf("迁移最大号 = %d，期望 907（00906 + 本轮一次迁移）", maxVersion)
	}
	names, err := fs.Glob(migrationsFS, "migrations/00907*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != migrationName {
		t.Fatalf("00907 段必须有且只有一条迁移，实得 %v", names)
	}
	var want int
	if err := openEvidenceScratchDB(t).SQL.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE name=?`, migrationName).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if want != 1 {
		t.Fatalf("00907 登记行 = %d，期望 1", want)
	}

	// 既有 00906 库：写入旧 DM、旧频道结果与已终结结果，再升级。
	db := openCatchHistoryDBAtVersion(t, 906)
	exec := func(q string) {
		t.Helper()
		if _, err := db.SQL.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
		('ai_task_evidence','ai_result_expression')`, 0)
	if _, err := db.EnsureProfile(ctx, 1); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO ai_task(id,kind,scope,scope_key,uid,agent,contact_uid,source_kind,text,state,created_ts)
		VALUES
		 (11,'dm','uid','1',1,'companion',2147483645,'human','老私聊','done',100),
		 (12,'channel','channel','legacy-ch',1,'companion',2147483645,'human','','done',101),
		 (13,'channel','channel','legacy-ch',1,'companion',2147483645,'system','','fallback',102);
		INSERT INTO ai_result(task_id,status,reply_text,created_ts,delivered_ts,contact_uid,err)
		VALUES (11,'done','旧私聊回复',100,100,2147483645,''),
		       (12,'done','旧频道回复',101,101,2147483645,''),
		       (13,'fallback','',102,-1,2147483645,'event_not_granted');`)

	// 登记失败注入：00907 的建表与登记必须整体回滚。
	exec(`CREATE TRIGGER reject_evidence_registration BEFORE INSERT ON schema_migrations
		WHEN NEW.version=907 BEGIN SELECT RAISE(ABORT,'test migration failure'); END;`)
	if err := db.Migrate(); err == nil {
		t.Fatal("期望迁移在登记失败时原子回滚")
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
		('ai_task_evidence','ai_result_expression')`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE version=907`, 0)
	// 旧历史必须仍然可读（失败不破坏旧库）。
	assertCount(t, db, `SELECT COUNT(*) FROM ai_result WHERE task_id IN (11,12) AND reply_text<>''`, 2)
	assertCount(t, db, `SELECT COUNT(*) FROM ai_result WHERE task_id=13 AND err='event_not_granted'`, 1)

	exec(`DROP TRIGGER reject_evidence_registration`)
	for i := 0; i < 2; i++ { // 失败后重试 + 重复启动
		if err := db.Migrate(); err != nil {
			t.Fatalf("第 %d 次迁移: %v", i+1, err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE version=907`, 1)
	var named int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE name=?`, migrationName).Scan(&named); err != nil || named != 1 {
		t.Fatalf("00907 登记行 = %d err=%v，期望 1", named, err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ai_task_evidence'`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ai_result_expression'`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_ai_result_expression_agent_version'`, 1)
	// 旧行不补造证据：既有任务/结果在迁移后仍然是「无凭据」，只能读。
	assertCount(t, db, `SELECT COUNT(*) FROM ai_task_evidence`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM ai_result_expression`, 0)
	// 历史可读：旧 DM、旧频道结果与已终结结果都能按原形状读出。
	var dmText, chText, terminalReason string
	if err := db.SQL.QueryRow(
		`SELECT reply_text FROM ai_result WHERE task_id=11 AND delivered_ts=100`).Scan(&dmText); err != nil {
		t.Fatalf("旧 DM 结果不可读: %v", err)
	}
	if err := db.SQL.QueryRow(
		`SELECT reply_text FROM ai_result WHERE task_id=12 AND delivered_ts=101`).Scan(&chText); err != nil {
		t.Fatalf("旧频道结果不可读: %v", err)
	}
	if err := db.SQL.QueryRow(
		`SELECT err FROM ai_result WHERE task_id=13 AND delivered_ts=-1`).Scan(&terminalReason); err != nil {
		t.Fatalf("已终结结果不可读: %v", err)
	}
	if dmText != "旧私聊回复" || chText != "旧频道回复" || terminalReason != "event_not_granted" {
		t.Fatalf("历史行被改写: dm=%q ch=%q terminal=%q", dmText, chText, terminalReason)
	}
	if err := ValidateTableOwnership(ctx, db); err != nil {
		t.Fatalf("升级后表归属不一致: %v", err)
	}
	// 混版停写：旧程序写下的在途行没有凭据，新一侧只能读、不能补造，
	// 投递端按「不可证」拒绝，不把旧频道结果降格成直接对话。
	var legacyResultID int64
	if err := db.SQL.QueryRow(`SELECT id FROM ai_result WHERE task_id=12`).Scan(&legacyResultID); err != nil {
		t.Fatalf("读旧频道结果 id: %v", err)
	}
	if _, ok, err := db.AITaskEvidenceOf(ctx, 12); err != nil || ok {
		t.Fatalf("旧任务不应被补造凭据: ok=%v err=%v", ok, err)
	}
	auth, err := db.AIDeliveryAuthorization(ctx, legacyResultID)
	if err != nil {
		t.Fatalf("旧结果投递复核: %v", err)
	}
	if auth.Allowed || auth.Reason != AITaskEvidenceReasonMissing {
		t.Fatalf("旧在途结果应按不可证拒绝，实得 allowed=%v reason=%q", auth.Allowed, auth.Reason)
	}

	// 事务性：证据写失败时任务行必须一起回滚（不留半套证据）。
	if _, err := db.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AISaveCapabilities(ctx, "fishguide", 0,
		[]string{aiconfig.CapabilityJoinRoundtable}, nil); err != nil {
		t.Fatal(err)
	}
	exec(`CREATE TRIGGER reject_task_evidence BEFORE INSERT ON ai_task_evidence
		BEGIN SELECT RAISE(ABORT,'evidence blocked'); END;`)
	ev := AITaskEvidence{
		FormatVersion: AITaskEvidenceFormatV1,
		Category:      aiconfig.EventRoundtable,
		Participants: []AITaskEvidenceParticipant{
			{Key: "companion", Capability: aiconfig.CapabilityJoinRoundtable},
			{Key: "fishguide", Capability: aiconfig.CapabilityJoinRoundtable},
		},
	}
	if _, err := db.AIEnqueueSystemRoundtable(ctx, "rt-fail", 1, "x",
		[]string{"companion", "fishguide"}, &ev); err == nil {
		t.Fatal("证据写失败时入队应整体失败")
	}
	assertCount(t, db, `SELECT COUNT(*) FROM ai_task WHERE scope_key='rt-fail'`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM ai_task_evidence`, 0)
	exec(`DROP TRIGGER reject_task_evidence`)

	// 正常路径：任务与证据同事务落库。
	taskID, err := db.AIEnqueueSystemChannel(ctx, "ch-ok", 1, "钓到一条", "companion", 2147483645, &AITaskEvidence{
		FormatVersion: AITaskEvidenceFormatV1,
		Category:      aiconfig.EventReplyStar,
		Participants: []AITaskEvidenceParticipant{
			{Key: "companion", Capability: aiconfig.CapabilityReplyStar},
		},
	})
	if err != nil {
		t.Fatalf("正常入队: %v", err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM ai_task`, 4)
	var boundRows int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_task_evidence WHERE task_id=?`, taskID).Scan(&boundRows); err != nil || boundRows != 1 {
		t.Fatalf("任务证据行 = %d err=%v，期望 1", boundRows, err)
	}
	bound, ok, err := db.AITaskEvidenceOf(ctx, taskID)
	if err != nil || !ok {
		t.Fatalf("读任务证据: ok=%v err=%v", ok, err)
	}
	if bound.Category != aiconfig.EventReplyStar || len(bound.Participants) != 1 ||
		bound.Participants[0].Capability != aiconfig.CapabilityReplyStar {
		t.Fatalf("任务证据载荷不符: %+v", bound)
	}
	if bound.Participants[0].Generation != 1 || bound.Participants[0].CapRevision != 1 {
		t.Fatalf("任务证据应绑定入队事务读到的代次/修订: %+v", bound.Participants[0])
	}

	// 清理按父行回收：先造一条可清理的任务+结果+两类证据，再把时间推后清理。
	if _, err := db.SQL.Exec(`UPDATE ai_task SET state='processing' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	// 这里手工把任务置回 processing（没有经过 AIPopTask），所以尝试号仍是 0。
	if err := db.AICompleteWithExpression(ctx, taskID, 0, "done", "好家伙", "", "",
		&AIResultExpression{AgentKey: "companion", Generation: 1, BRPersonalityRevision: 1}); err != nil {
		t.Fatalf("写结果: %v", err)
	}
	assertCount(t, db, `SELECT COUNT(*) FROM ai_result_expression`, 1)
	if _, err := db.SQL.Exec(
		`UPDATE ai_result SET delivered_ts=?, reply_text='' WHERE task_id=?`, float64(1), taskID); err != nil {
		t.Fatal(err)
	}
	if err := db.AIPrune(ctx, 1e12); err != nil {
		t.Fatalf("清理: %v", err)
	}
	if n := countOrphans(t, db); n != 0 {
		t.Fatalf("清理后遗留孤儿凭据 %d 行", n)
	}
	for _, table := range []string{"ai_task", "ai_result", "ai_task_evidence", "ai_result_expression"} {
		var n int
		if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE 1=0`).Scan(&n); err != nil {
			t.Fatalf("%s 不可读: %v", table, err)
		}
	}
	assertCount(t, db, `SELECT COUNT(*) FROM ai_task_evidence`, 0)
	assertCount(t, db, `SELECT COUNT(*) FROM ai_result_expression`, 0)
	t.Log("00907 empty/existing/repeat/failure-injection migrate kept legacy rows readable and pruned evidence with parents")
}

// TestAITaskEvidenceIsolatedDatabase 复核「真实二进制建出来的隔离库」。
//
// 由外部流程先用 cmd/seed（真实进程入口）在临时目录建库，再把路径放进
// T108_ISOLATED_DB 运行本用例：只读校验 00907 已登记、两张表与索引已落地、
// 表归属一致，不做任何迁移或写入。
func TestAITaskEvidenceIsolatedDatabase(t *testing.T) {
	path := os.Getenv("T108_ISOLATED_DB")
	if path == "" {
		t.Skip("未指定 T108_ISOLATED_DB（隔离库复核只在整组换版取证时运行）")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("隔离库不存在: %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("打开隔离库: %v", err)
	}
	defer db.Close()
	ctx := t.Context()
	assertCount(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE version=907`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ai_task_evidence'`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ai_result_expression'`, 1)
	assertCount(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_ai_result_expression_agent_version'`, 1)
	if err := ValidateTableOwnership(ctx, db); err != nil {
		t.Fatalf("隔离库表归属不一致: %v", err)
	}
	var name string
	if err := db.SQL.QueryRow(`SELECT name FROM schema_migrations WHERE version=907`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	t.Logf("隔离库 %s：version=907 name=%s，两张表与索引已落地，表归属一致", path, name)
}

// openEvidenceScratchDB 建一份全量迁移后的空库（只用于现取迁移号）。
func openEvidenceScratchDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "evidence-scratch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	return db
}

// countOrphans 统计没有父行的凭据（任务证据与表达证据各自核对）。
func countOrphans(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(`
		SELECT (SELECT COUNT(*) FROM ai_task_evidence e
		          WHERE NOT EXISTS (SELECT 1 FROM ai_task t WHERE t.id=e.task_id))
		     + (SELECT COUNT(*) FROM ai_result_expression x
		          WHERE NOT EXISTS (SELECT 1 FROM ai_result r WHERE r.id=x.result_id))`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------------------
// T-108 / R-157：发前复核的状态守卫与按角色只读投影
// ---------------------------------------------------------------------------

// aiEvidenceSetup 准备一名具备「回应玩家星蓝 / 参加圆桌」能力且保存过四维的角色。
func aiEvidenceSetup(t *testing.T, db *DB, agentKey string, soc, exp, per, reg int) (capRev, personaRev int64) {
	t.Helper()
	ctx := t.Context()
	st, err := db.AISaveCapabilities(ctx, agentKey, 0,
		[]string{aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable}, nil)
	if err != nil {
		t.Fatalf("保存 %s 能力: %v", agentKey, err)
	}
	personaRev, err = db.AISavePersonality(ctx, agentKey, 0, mustPersonality(t, soc, exp, per, reg))
	if err != nil {
		t.Fatalf("保存 %s 四维: %v", agentKey, err)
	}
	return st.Revision, personaRev
}

// aiEvidenceChannelTask 走真实入队入口写一条带「回应玩家星蓝」证据的频道任务。
// 入队前先接入并订阅该频道，投递前的范围复核才有真实范围可查。
func aiEvidenceChannelTask(t *testing.T, db *DB, channel, agentKey string, contactUID, personaRev int64) int64 {
	t.Helper()
	ctx := t.Context()
	if err := db.AISetAgentJoined(ctx, agentKey, true, 0); err != nil {
		t.Fatalf("接入 %s: %v", agentKey, err)
	}
	if err := db.AISetChannelAgent(ctx, channel, agentKey, true); err != nil {
		t.Fatalf("订阅 %s: %v", agentKey, err)
	}
	rev := personaRev
	ev := AITaskEvidence{
		FormatVersion: AITaskEvidenceFormatV1,
		Category:      aiconfig.EventReplyStar,
		Participants: []AITaskEvidenceParticipant{{
			Key: agentKey, Capability: aiconfig.CapabilityReplyStar,
			GSPersonalityRevision: &rev,
		}},
	}
	id, err := db.AIEnqueueSystemChannel(ctx, channel, 1, "玩家刚上了一条鱼",
		agentKey, contactUID, &ev)
	if err != nil {
		t.Fatalf("入队频道任务: %v", err)
	}
	return id
}

// aiEvidenceResultRow 读一条结果的终态形状。
func aiEvidenceResultRow(t *testing.T, db *DB, resultID int64) (delivered float64, errText, reply string) {
	t.Helper()
	var ts *float64
	if err := db.SQL.QueryRow(`SELECT delivered_ts, COALESCE(err,''), COALESCE(reply_text,'')
		FROM ai_result WHERE id=?`, resultID).Scan(&ts, &errText, &reply); err != nil {
		t.Fatalf("读结果 %d: %v", resultID, err)
	}
	if ts == nil {
		return -999, errText, reply
	}
	return *ts, errText, reply
}

// TestAIFinalPushStateGuards 复核 T-108 §9.2 E4 的最终发前状态守卫（R-157）：
//
//   - 正常待投递结果：AIDeliveryAuthorization 放行且报「仍待投递」；
//   - 同一行被真实回收后（AIExpireTasks）：发前复核必须拒绝，原因保留回收终态；
//   - 迟到的成功标记不得把回收终态覆盖成「已投递」；
//   - 回收后的结果不再进入待投递集合。
func TestAIFinalPushStateGuards(t *testing.T) {
	ctx := t.Context()
	db := openEvidenceScratchDB(t)
	capRev, personaRev := aiEvidenceSetup(t, db, "companion", 88, 12, 64, 31)
	if capRev != 1 || personaRev != 1 {
		t.Fatalf("初始能力/四维修订 = %d/%d，期望 1/1", capRev, personaRev)
	}
	taskID := aiEvidenceChannelTask(t, db, "ch-final-push", "companion", 2147483645, personaRev)

	task, ok, err := db.AIPopTask(ctx)
	if err != nil || !ok || task.ID != taskID || task.Attempt != 1 {
		t.Fatalf("第一次认领 = %+v ok=%v err=%v，期望 id=%d attempt=1", task, ok, err, taskID)
	}
	expr := AIResultExpression{AgentKey: "companion", Generation: 1, BRPersonalityRevision: personaRev}
	const oldText = "这句话不该发"
	if err := db.AIAddChannelResultWithExpression(ctx, taskID, task.Attempt, 2147483645, oldText, &expr); err != nil {
		t.Fatalf("写待发结果: %v", err)
	}
	var resultID int64
	if err := db.SQL.QueryRow(`SELECT id FROM ai_result WHERE task_id=?`, taskID).Scan(&resultID); err != nil {
		t.Fatalf("读结果 ID: %v", err)
	}

	// 正常待投递：发前复核放行。
	auth, err := db.AIDeliveryAuthorization(ctx, resultID)
	if err != nil {
		t.Fatalf("发前复核: %v", err)
	}
	if !auth.Allowed || !auth.ResultPending || auth.Reason != AIEvidenceReasonMatched {
		t.Fatalf("待投递结果应放行: allowed=%v pending=%v reason=%q",
			auth.Allowed, auth.ResultPending, auth.Reason)
	}

	// 真实回收：把 deadline 推早后走真实 AIExpireTasks（attempt=1，可重试一次）。
	if _, err := db.SQL.Exec(`UPDATE ai_task SET deadline_ts=? WHERE id=?`,
		float64(time.Now().Unix())-10, taskID); err != nil {
		t.Fatalf("推早 deadline: %v", err)
	}
	if _, recovered, err := db.AIExpireTasks(ctx, float64(time.Now().Unix()), 0); err != nil || recovered != 1 {
		t.Fatalf("回收: recovered=%d err=%v，期望 1", recovered, err)
	}
	delivered, errText, reply := aiEvidenceResultRow(t, db, resultID)
	if delivered != -1 || errText != AIEvidenceReasonAttemptReclaimed || reply != "" {
		t.Fatalf("回收后旧结果应为终态: delivered=%v err=%q reply=%q",
			delivered, errText, reply)
	}

	// 回收后发前复核：即使证据/能力/范围都还对，也不得放行旧句。
	auth, err = db.AIDeliveryAuthorization(ctx, resultID)
	if err != nil {
		t.Fatalf("回收后发前复核: %v", err)
	}
	if auth.Allowed || auth.ResultPending {
		t.Fatalf("回收后发前复核必须拒绝: allowed=%v pending=%v", auth.Allowed, auth.ResultPending)
	}
	if auth.Reason != AIEvidenceReasonAttemptReclaimed {
		t.Fatalf("发前复核原因 = %q，期望保留回收终态 %q",
			auth.Reason, AIEvidenceReasonAttemptReclaimed)
	}

	// 迟到的成功标记不得覆盖终态。
	if err := db.AIMarkDelivered(ctx, resultID, true); err != nil {
		t.Fatalf("迟到成功标记: %v", err)
	}
	delivered, errText, reply = aiEvidenceResultRow(t, db, resultID)
	if delivered != -1 || errText != AIEvidenceReasonAttemptReclaimed || reply != "" {
		t.Fatalf("成功标记覆盖了回收终态: delivered=%v err=%q reply=%q",
			delivered, errText, reply)
	}
	if err := db.AIMarkDelivered(ctx, resultID, false); err != nil {
		t.Fatalf("迟到失败标记: %v", err)
	}
	if delivered, _, _ = aiEvidenceResultRow(t, db, resultID); delivered != -1 {
		t.Fatalf("失败标记改写了已回收结果: delivered=%v", delivered)
	}

	// 不再出现在待投递集合。
	pending, err := db.AIPendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("待投递集合: %v", err)
	}
	for _, r := range pending {
		if r.TaskID == taskID {
			t.Fatalf("回收后旧结果仍出现在待投递集合: %+v", r)
		}
	}
	t.Logf("E4 final-push guard task=%d result=%d 终态 delivered=-1 err=%q reply=%q；发前复核拒绝 reason=%q；成功标记未覆盖终态",
		taskID, resultID, errText, reply, auth.Reason)
}

// TestAIAgentEvidenceReportReadOnly 复核 R-157 的按角色只读投影契约：
//
//   - 一个只读快照同时给出当前代次、已保存四维修订与匹配的真实 task/result；
//   - 未保存/无证据/缺角色/证据不可读/证据已清理都不点亮，返回稳定原因码；
//   - 保存 r+1 后旧证据只作「最近一条」展示，新任务按 r+1 运行时才点亮；
//   - 调用不写库（结果与两类证据行数、终态不变）。
func TestAIAgentEvidenceReportReadOnly(t *testing.T) {
	ctx := t.Context()

	t.Run("CurrentVersionAndNextSave", func(t *testing.T) {
		db := openEvidenceScratchDB(t)
		_, personaRev := aiEvidenceSetup(t, db, "companion", 88, 12, 64, 31)

		// 缺角色与无证据：不猜、不补造。
		if rep, err := db.AIAgentEvidenceReport(ctx, "ghost"); err != nil ||
			rep.AgentFound || rep.Reason != AIAgentEvidenceReasonAgentMissing {
			t.Fatalf("缺角色应报 agent_not_found: %+v err=%v", rep, err)
		}
		rep, err := db.AIAgentEvidenceReport(ctx, "companion")
		if err != nil {
			t.Fatal(err)
		}
		if !rep.AgentFound || rep.Generation != 1 || !rep.PersonalityPresent ||
			rep.CurrentRevision != personaRev || rep.Matched ||
			rep.Reason != AITaskEvidenceReasonNoExpression {
			t.Fatalf("无证据时报告不符: %+v", rep)
		}

		// 真实入队 + 认领 + 写一条待投递结果与表达凭据。
		taskID := aiEvidenceChannelTask(t, db, "ch-agent-ev", "companion", 2147483645, personaRev)
		task, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || task.ID != taskID || task.Attempt != 1 {
			t.Fatalf("认领: %+v ok=%v err=%v", task, ok, err)
		}
		expr := AIResultExpression{AgentKey: "companion", Generation: 1, BRPersonalityRevision: personaRev}
		if err := db.AIAddChannelResultWithExpression(ctx, taskID, task.Attempt, 2147483645, "好家伙", &expr); err != nil {
			t.Fatalf("写结果: %v", err)
		}
		var resultID int64
		if err := db.SQL.QueryRow(`SELECT id FROM ai_result WHERE task_id=?`, taskID).Scan(&resultID); err != nil {
			t.Fatal(err)
		}

		rep, err = db.AIAgentEvidenceReport(ctx, "companion")
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Matched || rep.Reason != AIEvidenceReasonMatched ||
			rep.Evidence.TaskID != taskID || rep.Evidence.ResultID != resultID ||
			rep.Evidence.Delivered || rep.Evidence.Generation != 1 ||
			rep.Evidence.BRRevision != personaRev || rep.Evidence.GSRevision == nil ||
			*rep.Evidence.GSRevision != personaRev {
			t.Fatalf("待投递的真实证据应点亮当前版: %+v", rep)
		}

		// 投递后仍匹配，Delivered 反映真实投递。
		if err := db.AIMarkDelivered(ctx, resultID, true); err != nil {
			t.Fatalf("标记投递: %v", err)
		}
		rep, err = db.AIAgentEvidenceReport(ctx, "companion")
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Matched || !rep.Evidence.Delivered || rep.Evidence.ResultID != resultID {
			t.Fatalf("已投递的真实证据应点亮当前版: %+v", rep)
		}

		// 保存 r+1：旧证据立即回到等待，只作为「最近一条」展示。
		next, err := db.AISavePersonality(ctx, "companion", personaRev, mustPersonality(t, 20, 80, 30, 70))
		if err != nil {
			t.Fatalf("保存 r+1: %v", err)
		}
		rep, err = db.AIAgentEvidenceReport(ctx, "companion")
		if err != nil {
			t.Fatal(err)
		}
		if rep.Matched || rep.CurrentRevision != next ||
			rep.Reason != AITaskEvidenceReasonPersonaDiff ||
			rep.Evidence.ResultID != 0 || rep.Latest.ResultID != resultID ||
			rep.Latest.BRRevision != personaRev || rep.Latest.GSRevision == nil ||
			*rep.Latest.GSRevision != personaRev {
			t.Fatalf("保存 r+1 后旧证据不得点亮当前版: %+v", rep)
		}

		// 新任务按 r+1 运行：新证据点亮新版本，旧任务不混用新修订。
		task2 := aiEvidenceChannelTask(t, db, "ch-agent-ev", "companion", 2147483645, next)
		task2row, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || task2row.ID != task2 || task2row.Attempt != 1 {
			t.Fatalf("第二次认领: %+v ok=%v err=%v", task2row, ok, err)
		}
		expr2 := AIResultExpression{AgentKey: "companion", Generation: 1, BRPersonalityRevision: next}
		if err := db.AIAddChannelResultWithExpression(ctx, task2, task2row.Attempt, 2147483645, "新版本的一句", &expr2); err != nil {
			t.Fatalf("写 r+1 结果: %v", err)
		}
		var result2 int64
		if err := db.SQL.QueryRow(`SELECT id FROM ai_result WHERE task_id=?`, task2).Scan(&result2); err != nil {
			t.Fatal(err)
		}
		rep, err = db.AIAgentEvidenceReport(ctx, "companion")
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Matched || rep.CurrentRevision != next || rep.Evidence.TaskID != task2 ||
			rep.Evidence.ResultID != result2 || rep.Evidence.BRRevision != next {
			t.Fatalf("按 r+1 运行的新任务应点亮新版: %+v", rep)
		}

		// 只读：连续两次报告不改变结果与两类证据的行数。
		before := aiEvidenceRowCounts(t, db)
		if _, err := db.AIAgentEvidenceReport(ctx, "companion"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AIAgentEvidenceReport(ctx, "companion"); err != nil {
			t.Fatal(err)
		}
		if after := aiEvidenceRowCounts(t, db); after != before {
			t.Fatalf("按角色报告写库: 前=%v 后=%v", before, after)
		}
		t.Logf("R-157 role evidence task=%d result=%d rev=%d → task=%d result=%d rev=%d matched=true",
			taskID, resultID, personaRev, task2, result2, next)
	})

	t.Run("ReclaimedOnly", func(t *testing.T) {
		db := openEvidenceScratchDB(t)
		_, personaRev := aiEvidenceSetup(t, db, "companion", 88, 12, 64, 31)
		taskID := aiEvidenceChannelTask(t, db, "ch-agent-reclaimed", "companion", 2147483645, personaRev)
		task, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || task.Attempt != 1 {
			t.Fatalf("认领: %+v ok=%v err=%v", task, ok, err)
		}
		expr := AIResultExpression{AgentKey: "companion", Generation: 1, BRPersonalityRevision: personaRev}
		if err := db.AIAddChannelResultWithExpression(ctx, taskID, task.Attempt, 2147483645, "回收前写的一句", &expr); err != nil {
			t.Fatalf("写结果: %v", err)
		}
		var resultID int64
		if err := db.SQL.QueryRow(`SELECT id FROM ai_result WHERE task_id=?`, taskID).Scan(&resultID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL.Exec(`UPDATE ai_task SET deadline_ts=? WHERE id=?`,
			float64(time.Now().Unix())-10, taskID); err != nil {
			t.Fatal(err)
		}
		if _, recovered, err := db.AIExpireTasks(ctx, float64(time.Now().Unix()), 0); err != nil || recovered != 1 {
			t.Fatalf("回收: recovered=%d err=%v", recovered, err)
		}
		rep, err := db.AIAgentEvidenceReport(ctx, "companion")
		if err != nil {
			t.Fatal(err)
		}
		if rep.Matched || rep.Reason != AIEvidenceReasonAttemptReclaimed ||
			rep.Latest.ResultID != resultID || rep.Latest.Delivered ||
			rep.Latest.BRRevision != personaRev || rep.Latest.GSRevision == nil ||
			*rep.Latest.GSRevision != personaRev {
			t.Fatalf("已回收结果不得点亮当前版: %+v", rep)
		}
		t.Logf("R-157 reclaimed result=%d task=%d rev=%d matched=false reason=%q",
			resultID, taskID, personaRev, rep.Reason)
	})

	t.Run("UnreadableAndPrunedEvidence", func(t *testing.T) {
		db := openEvidenceScratchDB(t)
		_, personaRev := aiEvidenceSetup(t, db, "companion", 88, 12, 64, 31)
		taskID := aiEvidenceChannelTask(t, db, "ch-agent-broken", "companion", 2147483645, personaRev)
		task, ok, err := db.AIPopTask(ctx)
		if err != nil || !ok || task.Attempt != 1 {
			t.Fatalf("认领: %+v ok=%v err=%v", task, ok, err)
		}
		expr := AIResultExpression{AgentKey: "companion", Generation: 1, BRPersonalityRevision: personaRev}
		if err := db.AIAddChannelResultWithExpression(ctx, taskID, task.Attempt, 2147483645, "一句", &expr); err != nil {
			t.Fatalf("写结果: %v", err)
		}
		if rep, err := db.AIAgentEvidenceReport(ctx, "companion"); err != nil || !rep.Matched {
			t.Fatalf("基线应匹配: %+v err=%v", rep, err)
		}
		// 证据不可读：不点亮，原因稳定。
		if _, err := db.SQL.Exec(`UPDATE ai_task_evidence SET binding_json='{"format_version":1,'
			WHERE task_id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		rep, err := db.AIAgentEvidenceReport(ctx, "companion")
		if err != nil {
			t.Fatal(err)
		}
		if rep.Matched || rep.Reason != AITaskEvidenceReasonUnreadable {
			t.Fatalf("不可读证据不得点亮: %+v", rep)
		}
		// 证据被清理（父任务仍在）：按缺证据处理。
		if _, err := db.SQL.Exec(`DELETE FROM ai_task_evidence WHERE task_id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		rep, err = db.AIAgentEvidenceReport(ctx, "companion")
		if err != nil {
			t.Fatal(err)
		}
		if rep.Matched || rep.Reason != AITaskEvidenceReasonMissing {
			t.Fatalf("已清理证据不得点亮: %+v", rep)
		}
		// 表达凭据被清理：没有证据行，按无表达凭据处理。
		if _, err := db.SQL.Exec(`DELETE FROM ai_result_expression WHERE result_id IN
			(SELECT id FROM ai_result WHERE task_id=?)`, taskID); err != nil {
			t.Fatal(err)
		}
		rep, err = db.AIAgentEvidenceReport(ctx, "companion")
		if err != nil {
			t.Fatal(err)
		}
		if rep.Matched || rep.Reason != AITaskEvidenceReasonNoExpression {
			t.Fatalf("无表达凭据不得点亮: %+v", rep)
		}
		t.Logf("R-157 unreadable/pruned evidence task=%d reason=%q/%q/%q",
			taskID, AITaskEvidenceReasonUnreadable, AITaskEvidenceReasonMissing,
			AITaskEvidenceReasonNoExpression)
	})
}

// aiEvidenceRowCounts 统计结果/任务证据/表达证据行数，用于证明只读入口不写库。
func aiEvidenceRowCounts(t *testing.T, db *DB) [3]int {
	t.Helper()
	var out [3]int
	for i, table := range []string{"ai_result", "ai_task_evidence", "ai_result_expression"} {
		if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&out[i]); err != nil {
			t.Fatalf("统计 %s: %v", table, err)
		}
	}
	return out
}

// TestAIAgentEvidenceFixtureDBRead 复核交给 T-107 P4 的同版运行夹具库：
// 打开既有库（不迁移、不写），按角色读到匹配的真实 task/result。
// 由外部流程用 T108_EVIDENCE_DB 生成夹具库（见 gameserver 的同名用例），
// 再用 T108_EVIDENCE_DB_READ 指向该库运行本用例。
func TestAIAgentEvidenceFixtureDBRead(t *testing.T) {
	path := os.Getenv("T108_EVIDENCE_DB_READ")
	if path == "" {
		t.Skip("未指定 T108_EVIDENCE_DB_READ（同版运行夹具库复核只在交接取证时运行）")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("夹具库不存在: %v", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatalf("打开夹具库: %v", err)
	}
	defer db.Close()
	rep, err := db.AIAgentEvidenceReport(t.Context(), "companion")
	if err != nil {
		t.Fatalf("读夹具库报告: %v", err)
	}
	if !rep.AgentFound || rep.Generation != 1 || !rep.PersonalityPresent ||
		rep.CurrentRevision != 1 || !rep.Matched ||
		rep.Evidence.TaskID != 1 || rep.Evidence.ResultID != 1 ||
		!rep.Evidence.Delivered || rep.Evidence.BRRevision != 1 ||
		rep.Evidence.GSRevision == nil || *rep.Evidence.GSRevision != 1 {
		t.Fatalf("夹具库报告不符: %+v", rep)
	}
	t.Logf("夹具库 %s：agent=companion generation=%d revision=%d task=%d result=%d matched=true",
		path, rep.Generation, rep.CurrentRevision, rep.Evidence.TaskID, rep.Evidence.ResultID)
}

// TestAITaskEvidenceOrderingKeepsMigrationSegmentContiguous 校验 00907 段仍然连续：
// 一次迁移只建两张表，没有顺手插号或复用旧号。
func TestAITaskEvidenceOrderingKeepsMigrationSegmentContiguous(t *testing.T) {
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	var versions []int64
	for _, name := range names {
		if v := migrationVersion(name); v >= 900 {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		t.Fatal("AI 段没有迁移")
	}
	for i := 1; i < len(versions); i++ {
		if versions[i] <= versions[i-1] {
			t.Fatalf("AI 段迁移号不递增: %v", versions)
		}
	}
	if versions[len(versions)-1] != 907 {
		t.Fatalf("AI 段最大号 = %d，期望 907", versions[len(versions)-1])
	}
}
