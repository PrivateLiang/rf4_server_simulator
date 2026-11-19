package gameserver

import (
	"strings"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

func TestPollAIToolJobsWritesResult(t *testing.T) {
	s, db := newProgressSrv(t)
	ctx := t.Context()
	grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityGuideHelp)
	parent := seedProcessingToolTask(t, db)
	id, err := db.AIEnqueueToolJob(ctx, parent, "fish_query", "常见拟鲤", 1000001)
	if err != nil {
		t.Fatalf("入队: %v", err)
	}
	s.pollAIToolJobs(ctx, db)
	got, ok, err := db.AIGetToolResult(ctx, id)
	if err != nil || !ok || got == "" {
		t.Fatalf("工具结果 = %q ok=%v err=%v", got, ok, err)
	}
	if pending, _ := db.AIPendingToolJobs(ctx, 8); len(pending) != 0 {
		t.Fatalf("处理完仍有待处理作业: %v", pending)
	}
}

func TestPollAIToolJobsUnknownToolStillCompletes(t *testing.T) {
	s, db := newProgressSrv(t)
	ctx := t.Context()
	parent := seedProcessingToolTask(t, db)
	id, err := db.AIEnqueueToolJob(ctx, parent, "not_a_tool", "x", 1000001)
	if err != nil {
		t.Fatal(err)
	}
	s.pollAIToolJobs(ctx, db)
	got, ok, _ := db.AIGetToolResult(ctx, id)
	if !ok || got != "未知工具" {
		t.Fatalf("未知工具应回固定文案: %q %v", got, ok)
	}
}

func seedProcessingToolTask(t *testing.T, db *store.DB) int64 {
	t.Helper()
	id, err := db.AIEnqueue(t.Context(), &store.AITask{Kind: "dm", SourceKind: "human", Agent: runtimeGSAgent})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := db.AIPopTask(t.Context())
	if !ok || err != nil || task.ID != id {
		t.Fatalf("claim tool parent: %v/%v", ok, err)
	}
	return id
}

func TestT102ToolJobStopBoundaries(t *testing.T) {
	for _, stage := range []string{"claimed", "writeback"} {
		t.Run(stage, func(t *testing.T) {
			s, db := newProgressSrv(t)
			ctx := t.Context()
			task := seedProcessingToolTask(t, db)
			job, err := db.AIEnqueueToolJob(ctx, task, "not_a_tool", "synthetic", 1)
			if err != nil {
				t.Fatal(err)
			}
			switched := false
			aiToolJobBarrierForTest = func(at string) {
				if stage == at && !switched {
					switched = true
					liftNow(t, db)
				}
			}
			t.Cleanup(func() { aiToolJobBarrierForTest = nil })
			s.pollAIToolJobs(ctx, db)
			s.pollAIToolJobs(ctx, db)
			var state, reason, result string
			if err := db.SQL.QueryRow(`SELECT state, terminal_reason, result_text FROM ai_tool_job WHERE id=?`,
				job).Scan(&state, &reason, &result); err != nil {
				t.Fatal(err)
			}
			if !switched || state != "done" || reason != "queued_before_resume" || result != "" {
				t.Fatalf("job %d at %s: state=%s reason=%s result=%q", job, stage, state, reason, result)
			}
			t.Logf("job %d: %s, terminal %s/%s, zero result; duplicate poll inert", job, stage, state, reason)
		})
	}
}

// 撤权后旧只读作业不能再拿数据；远程读只走 ai_tool_job，不进写命令队列。
func TestAIToolJobRevokedCapabilityBlocksRead(t *testing.T) {
	s, db := newProgressSrv(t)
	ctx := t.Context()
	rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityGuideHelp)
	parent := seedProcessingToolTask(t, db)
	id, err := db.AIEnqueueToolJob(ctx, parent, "fish_query", "常见拟鲤", 1000001)
	if err != nil {
		t.Fatal(err)
	}
	// 管理员显式清空能力：下一条作业按当前权限拒绝。
	if _, err := db.AISaveCapabilities(ctx, runtimeGSAgent, rev, []string{}, nil); err != nil {
		t.Fatalf("撤权: %v", err)
	}
	s.pollAIToolJobs(ctx, db)
	got, ok, _ := db.AIGetToolResult(ctx, id)
	if !ok || !strings.Contains(got, "授权已撤销") {
		t.Fatalf("撤权后工具结果 = %q ok=%v", got, ok)
	}
	var cmds int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&cmds); err != nil {
		t.Fatal(err)
	}
	if cmds != 0 {
		t.Fatalf("只读工具写进了写命令队列: %d 行", cmds)
	}
}
