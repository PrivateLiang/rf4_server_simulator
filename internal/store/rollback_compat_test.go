package store

// T-108-DoD-v3 / DR-108（O1）：回退窗口的兼容证据。
//
// 口径（v6 §2.2）：
//   - 「旧程序可读」= 旧版三进程在升级库上启动并能读取既有 DM、已终结结果、
//     配置与配额；旧版 cmd/seed 拒绝属预期安全边界；
//   - 回退只能发生在 NeedsSeed=false；
//   - 新版 AIPrune 在同一事务里清扫「无父行孤儿」：旧版不认识两张附表，
//     清理父行时不会跟着删，回退窗口因此会留下没有父行的凭据行。
//
// 本文件只造测试数据、不改迁移，也不删除任何有父行的历史。

import (
	"context"
	"testing"

	"rf4server/internal/aiconfig"
)

// TestAIPruneSweepsRollbackOrphans 是 G16 的绿侧：无父行孤儿必须被同事务清扫，
// 有父行的历史（任务/结果/证据/表达）一行不动。撤掉 AIPrune 的孤儿清扫会让
// 「回退窗口孤儿凭据未清扫」变红。
func TestAIPruneSweepsRollbackOrphans(t *testing.T) {
	ctx := t.Context()
	db := openEvidenceScratchDB(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.SQL.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.SQL.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("统计 %q: %v", q, err)
		}
		return n
	}

	// 1) 有父行的历史：任务 + 证据 + 未投递结果 + 表达凭据。
	//    未投递结果不会被保留期清理，因此这组父行在 AIPrune 之后必须还在。
	if _, err := db.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityReplyStar}, nil); err != nil {
		t.Fatal(err)
	}
	keptTask, err := db.AIEnqueueSystemChannel(ctx, "rb-keep", 1, "钓到一条", "companion", 2147483645,
		&AITaskEvidence{
			FormatVersion: AITaskEvidenceFormatV1,
			Category:      aiconfig.EventReplyStar,
			Participants: []AITaskEvidenceParticipant{
				{Key: "companion", Capability: aiconfig.CapabilityReplyStar},
			},
		})
	if err != nil {
		t.Fatalf("造有父行任务: %v", err)
	}
	exec(`UPDATE ai_task SET state='processing' WHERE id=?`, keptTask)
	if err := db.AICompleteWithExpression(ctx, keptTask, 0, "done", "好家伙", "", "",
		&AIResultExpression{AgentKey: "companion", Generation: 1, BRPersonalityRevision: 1}); err != nil {
		t.Fatalf("写有父行结果: %v", err)
	}
	var keptResult int64
	if err := db.SQL.QueryRow(`SELECT id FROM ai_result WHERE task_id=?`, keptTask).Scan(&keptResult); err != nil {
		t.Fatal(err)
	}

	// 2) 模拟旧版回退窗口：旧版只删 ai_task / ai_result，两张附表留下孤儿。
	orphanTask, err := db.AIEnqueueSystemChannel(ctx, "rb-orphan-task", 1, "旧版任务", "companion", 2147483645,
		&AITaskEvidence{
			FormatVersion: AITaskEvidenceFormatV1,
			Category:      aiconfig.EventReplyStar,
			Participants: []AITaskEvidenceParticipant{
				{Key: "companion", Capability: aiconfig.CapabilityReplyStar},
			},
		})
	if err != nil {
		t.Fatalf("造孤儿任务证据: %v", err)
	}
	exec(`UPDATE ai_task SET state='done', text='' WHERE id=?`, orphanTask)
	exec(`INSERT INTO ai_result(task_id,status,reply_text,created_ts,delivered_ts,contact_uid)
		VALUES(?, 'done', '旧版结果', 100, 100, 2147483645)`, orphanTask)
	var orphanResult int64
	if err := db.SQL.QueryRow(`SELECT id FROM ai_result WHERE task_id=?`, orphanTask).Scan(&orphanResult); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO ai_result_expression(result_id,agent_key,generation,br_personality_revision)
		VALUES(?, 'companion', 1, 1)`, orphanResult)
	// 旧版只认 ai_task / ai_result（这里是它的清理结果）。
	exec(`DELETE FROM ai_task WHERE id=?`, orphanTask)
	exec(`DELETE FROM ai_result WHERE id=?`, orphanResult)

	beforeEvidence := count(`SELECT COUNT(*) FROM ai_task_evidence`)
	beforeExpr := count(`SELECT COUNT(*) FROM ai_result_expression`)
	orphansBefore := countOrphans(t, db)
	if beforeEvidence != 2 || beforeExpr != 2 {
		t.Fatalf("回退窗口夹具不符: evidence=%d expressions=%d，期望 2/2", beforeEvidence, beforeExpr)
	}
	if orphansBefore != 2 {
		t.Fatalf("回退窗口应留下 2 行孤儿，实得 %d", orphansBefore)
	}

	// 3) 新版 AIPrune：同事务清扫无父行孤儿。
	if err := db.AIPrune(ctx, 1e12); err != nil {
		t.Fatalf("AIPrune: %v", err)
	}
	afterEvidence := count(`SELECT COUNT(*) FROM ai_task_evidence`)
	afterExpr := count(`SELECT COUNT(*) FROM ai_result_expression`)
	if n := countOrphans(t, db); n != 0 {
		t.Fatalf("回退窗口孤儿凭据未清扫：仍剩 %d 行（evidence %d->%d，expressions %d->%d）",
			n, beforeEvidence, afterEvidence, beforeExpr, afterExpr)
	}
	if afterEvidence != 1 || afterExpr != 1 {
		t.Fatalf("清理幅度不符: evidence %d->%d expressions %d->%d，期望各只少 1",
			beforeEvidence, afterEvidence, beforeExpr, afterExpr)
	}
	// 有父行的历史一行不动：任务、结果、两行证据、正文都在。
	if n := count(`SELECT COUNT(*) FROM ai_task WHERE id=?`, keptTask); n != 1 {
		t.Fatal("有父行任务被删")
	}
	if n := count(`SELECT COUNT(*) FROM ai_task_evidence WHERE task_id=?`, keptTask); n != 1 {
		t.Fatal("有父行任务证据被删")
	}
	if n := count(`SELECT COUNT(*) FROM ai_result WHERE id=?`, keptResult); n != 1 {
		t.Fatal("有父行结果被删")
	}
	if n := count(`SELECT COUNT(*) FROM ai_result_expression WHERE result_id=?`, keptResult); n != 1 {
		t.Fatal("有父行表达凭据被删")
	}
	var reply string
	if err := db.SQL.QueryRow(`SELECT reply_text FROM ai_result WHERE id=?`, keptResult).Scan(&reply); err != nil {
		t.Fatal(err)
	}
	if reply != "好家伙" {
		t.Fatalf("有父行结果正文被改写: %q", reply)
	}

	// 4) 幂等：再跑一次不产生新删除。
	evidenceOnce, exprOnce := afterEvidence, afterExpr
	if err := db.AIPrune(ctx, 1e12); err != nil {
		t.Fatalf("第二次 AIPrune: %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM ai_task_evidence`); n != evidenceOnce {
		t.Fatalf("重复清扫改动了有父行证据: %d -> %d", evidenceOnce, n)
	}
	if n := count(`SELECT COUNT(*) FROM ai_result_expression`); n != exprOnce {
		t.Fatalf("重复清扫改动了有父行表达: %d -> %d", exprOnce, n)
	}
	t.Logf("回退窗口孤儿清扫: 孤儿=%d -> 0（evidence %d->%d，expressions %d->%d）；有父行历史保留",
		orphansBefore, beforeEvidence, afterEvidence, beforeExpr, afterExpr)
}

// TestAIRollbackWindowKeepsOldRowsReadable 证明回退窗口里的旧行只读可用、
// 新版不补造证据，也不把旧频道结果降格成直接对话。
//
// 这是「旧程序可读」的新版一侧对照：旧版写下的任务/结果没有绑定载荷，
// 新版按「证据不可证」拒绝投递，但仍能读出历史正文与已终结原因。
func TestAIRollbackWindowKeepsOldRowsReadable(t *testing.T) {
	ctx := t.Context()
	db := openEvidenceScratchDB(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.SQL.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// 旧版形状：没有 ai_task_evidence / ai_result_expression 的 DM 与频道行。
	exec(`INSERT INTO ai_task(id,kind,scope,scope_key,uid,agent,contact_uid,source_kind,text,state,created_ts)
		VALUES
		 (201,'dm','uid','1',1,'companion',2147483645,'human','旧私聊','done',100),
		 (202,'channel','channel','rb-legacy',1,'companion',2147483645,'human','','done',101),
		 (203,'roundtable','channel','rb-legacy',1,'','0','system','','fallback',102)`)
	exec(`INSERT INTO ai_result(task_id,status,reply_text,created_ts,delivered_ts,contact_uid,err)
		VALUES (201,'done','旧私聊回复',100,100,2147483645,''),
		       (202,'done','旧频道回复',101,NULL,2147483645,''),
		       (203,'fallback','',102,-1,2147483645,'event_not_granted')`)
	if err := db.Migrate(); err != nil {
		t.Fatalf("回退窗口重跑迁移: %v", err)
	}
	if n := countOrphans(t, db); n != 0 {
		t.Fatalf("回退窗口不应有孤儿: %d", n)
	}
	// 旧行仍可读且不被补造证据。
	for _, want := range []struct {
		taskID int64
		reply  string
	}{{201, "旧私聊回复"}, {202, "旧频道回复"}} {
		var reply string
		if err := db.SQL.QueryRow(`SELECT reply_text FROM ai_result WHERE task_id=?`, want.taskID).Scan(&reply); err != nil {
			t.Fatalf("旧结果 %d 不可读: %v", want.taskID, err)
		}
		if reply != want.reply {
			t.Fatalf("旧结果正文被改写: %q", reply)
		}
		if _, ok, err := db.AITaskEvidenceOf(ctx, want.taskID); err != nil || ok {
			t.Fatalf("旧任务 %d 不应被补造证据: ok=%v err=%v", want.taskID, ok, err)
		}
	}
	// 旧在途频道结果：新版投递复核按「不可证」拒绝，不改派、不降级。
	var channelResult int64
	if err := db.SQL.QueryRow(`SELECT id FROM ai_result WHERE task_id=202`).Scan(&channelResult); err != nil {
		t.Fatal(err)
	}
	auth, err := db.AIDeliveryAuthorization(ctx, channelResult)
	if err != nil {
		t.Fatalf("旧结果投递复核: %v", err)
	}
	if auth.Allowed || auth.Reason != AITaskEvidenceReasonMissing {
		t.Fatalf("旧在途结果应按不可证拒绝: allowed=%v reason=%q", auth.Allowed, auth.Reason)
	}
	// 旧 DM 已投递行保持已终结，不会重新进入待投递集合。
	pending, err := db.AIPendingResults(ctx, 10)
	if err != nil {
		t.Fatalf("AIPendingResults: %v", err)
	}
	for _, r := range pending {
		if r.TaskID == 201 {
			t.Fatalf("已投递旧 DM 又进入待投递集合: %+v", r)
		}
	}
	t.Logf("回退窗口旧行只读：DM/频道/终结结果均可读，未补造证据，投递按 %q 拒绝",
		AITaskEvidenceReasonMissing)
}

// TestAIRollbackWindowNeedsSeedBoundary 记录回退前置的代码事实：旧版拒绝重播种
// 的判定点是「表归属」而不是三进程读取。本用例只读校验：升级库里两张附表存在，
// 且 ValidateTableOwnership 在本版通过；旧版拒绝属预期边界（回执记录原文）。
func TestAIRollbackWindowNeedsSeedBoundary(t *testing.T) {
	ctx := context.Background()
	db := openEvidenceScratchDB(t)
	if err := ValidateTableOwnership(ctx, db); err != nil {
		t.Fatalf("本版表归属校验应通过: %v", err)
	}
	for _, table := range []string{"ai_task_evidence", "ai_result_expression"} {
		var n int
		if err := db.SQL.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("升级库缺表 %s", table)
		}
	}
	var maxVersion int64
	if err := db.SQL.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&maxVersion); err != nil {
		t.Fatal(err)
	}
	if maxVersion != 907 {
		t.Fatalf("升级库最大迁移号 = %d，期望 907（回退前置 NeedsSeed=false 的口径依赖它）", maxVersion)
	}
	t.Logf("回退前置：升级库 max_version=%d、两张附表在、本版表归属校验通过（旧版 seed 拒绝见回执原文）",
		maxVersion)
}
