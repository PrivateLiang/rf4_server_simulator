package gameserver

// T-107 P4 联验夹具（仅测试，env 门控）。
//
// 管理端 TestT107EffectiveFromRuntime 用 go test -c 构建本包的测试二进制，再以子进程
// 方式调用本用例：同一份临时库里，管理端先用真实 JWT/Handler 保存性格，再由本夹具
// 跑真实 GS 选择（maybeAIPlayerCatch 的玩家上星链）与真实 BR 表达，并用屏障文件把
// 「已入队」「已表达」两个时点交给管理端各做一次真实 GET。
//
// 本文件只编排既有真实入口，不写 personality/ai_setting 性格键、不改任务与结果的
// 写入语义，也不手填 effective 或按完成数推断；库内的角色、管理员保存与 GET 都由
// 调用方准备。未设置 T107_EFFECT_* 时跳过，不影响常规 go test。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

const t107DriverBarrierWait = 120 * time.Second

// t107DriverMarker 是交给管理端的时点回执，字段与管理端解码结构同形。
type t107DriverMarker struct {
	Stage         string `json:"stage"`
	Agent         string `json:"agent"`
	TaskID        int64  `json:"task_id"`
	ResultID      int64  `json:"result_id"`
	Revision      int64  `json:"revision"`
	Generation    int64  `json:"generation"`
	Delivered     bool   `json:"delivered"`
	HasExpression bool   `json:"has_expression"`
	ResultErr     string `json:"result_err"`
	Matched       bool   `json:"matched"`
	Reason        string `json:"reason"`
	Error         string `json:"error"`
}

func t107DriverWriteMarker(t *testing.T, dir, name string, m t107DriverMarker) {
	t.Helper()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("编码标记 %s: %v", name, err)
	}
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatalf("写标记 %s: %v", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Fatalf("落标记 %s: %v", name, err)
	}
}

func t107DriverWaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(t107DriverBarrierWait)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待管理端屏障超时: %s", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func t107DriverLatestTask(t *testing.T, db *store.DB, agent string) int64 {
	t.Helper()
	var id int64
	if err := db.SQL.QueryRow(`SELECT COALESCE(MAX(id),0) FROM ai_task WHERE agent=?`, agent).Scan(&id); err != nil {
		t.Fatalf("读 %s 最新任务: %v", agent, err)
	}
	return id
}

func TestT107EffectiveRuntimeDriver(t *testing.T) {
	dbPath := os.Getenv("T107_EFFECT_DB")
	barrier := os.Getenv("T107_EFFECT_BARRIER")
	agent := os.Getenv("T107_EFFECT_AGENT")
	channel := os.Getenv("T107_EFFECT_CHANNEL")
	reply := os.Getenv("T107_EFFECT_REPLY")
	if dbPath == "" || barrier == "" || agent == "" || channel == "" {
		t.Skip("未指定 T107_EFFECT_DB/BARRIER/AGENT/CHANNEL（只由 T-107 P4 联验子进程调用）")
	}
	if reply == "" {
		reply = "这句话进了频道"
	}
	if err := os.MkdirAll(barrier, 0o755); err != nil {
		t.Fatalf("建屏障目录: %v", err)
	}
	ctx := context.Background()

	// 打开管理端准备的同一份临时库；性格配置已由真实管理 Handler 保存。
	db := openStaticDBAtPath(t, dbPath)
	personality, err := db.AIGetPersonality(ctx, agent)
	if err != nil {
		t.Fatalf("读管理端已保存的性格 %s: %v", agent, err)
	}
	if !personality.Present {
		t.Fatalf("%s 还没有已保存的性格，联验顺序错误（应先经管理端保存）", agent)
	}
	revision := personality.Config.Revision
	generation := personality.CurrentGeneration

	rt := newT108EvidenceRuntimeWithDB(t, channel, reply, db)
	// 只给目标角色接入/订阅/能力；其它角色不参与本频道选择，避免选人漂移。
	if err := db.AISetAgentJoined(ctx, agent, true, 0); err != nil {
		t.Fatalf("接入 %s: %v", agent, err)
	}
	if err := db.AISetChannelAgent(ctx, channel, agent, true); err != nil {
		t.Fatalf("订阅 %s: %v", agent, err)
	}
	t108SetCapabilities(t, db, agent, aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
	if os.Getenv("T107_EFFECT_VENDOR") == "off" {
		// 负例「只有 fallback、没有成功表达」：放行 BR 前把厂商地址改成不可达，
		// 让真实 BR 走失败降级，不写表达凭据。
		if _, err := db.SQL.Exec(
			`UPDATE ai_provider SET base_url='https://127.0.0.1:1' WHERE name='deepseek'`); err != nil {
			t.Fatalf("切断厂商地址: %v", err)
		}
	}
	t108SeedFixed(t, t108WantSeed(t, db, agent, activityReplyFeature(3),
		store.AIActorSeed(0, agent)))

	before := t107DriverLatestTask(t, db, agent)
	rt.srv.maybeAIPlayerCatch(ctx, t013SelfUID, playerStarFish())
	taskID := t107DriverLatestTask(t, db, agent)
	if taskID <= before {
		t.Fatalf("真实 GS 上星链没有入队：before=%d after=%d（agent=%s channel=%s）",
			before, taskID, agent, channel)
	}
	t107WriteEnqueued(t, db, barrier, t107DriverMarker{
		Stage: "enqueued", Agent: agent, TaskID: taskID,
		Revision: revision, Generation: generation,
	})

	t107DriverWaitFile(t, filepath.Join(barrier, "release"))

	if os.Getenv("T107_EFFECT_VENDOR") == "off" {
		rt.vendor.srv.Close()
	}
	task, ok, err := db.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("认领任务失败: ok=%v err=%v", ok, err)
	}
	if task.ID != taskID {
		t.Fatalf("认领的任务 %d 不是本次入队的 %d", task.ID, taskID)
	}
	rt.runner.HandleTaskForTest(ctx, task)
	if os.Getenv("T107_EFFECT_VENDOR") != "off" {
		rt.poll()
		rt.expectFrame(reply)
	} else {
		rt.poll()
	}

	var (
		resultID   int64
		delivered  float64
		resultErr  string
		hasExpr    bool
		brRevision int64
	)
	err = db.SQL.QueryRow(`
		SELECT r.id, COALESCE(r.delivered_ts,-999), COALESCE(r.err,''),
		       (e.result_id IS NOT NULL), COALESCE(e.br_personality_revision,0)
		FROM ai_result r LEFT JOIN ai_result_expression e ON e.result_id=r.id
		WHERE r.task_id=? ORDER BY r.id DESC LIMIT 1`, taskID).
		Scan(&resultID, &delivered, &resultErr, &hasExpr, &brRevision)
	if err != nil {
		t.Fatalf("读本次结果: %v", err)
	}
	rep, err := db.AIAgentEvidenceReport(ctx, agent)
	if err != nil {
		t.Fatalf("读按角色证据: %v", err)
	}
	t107DriverWriteMarker(t, barrier, "expressed.json", t107DriverMarker{
		Stage: "expressed", Agent: agent, TaskID: taskID, ResultID: resultID,
		Revision: revision, Generation: generation, Delivered: delivered >= 0,
		HasExpression: hasExpr, ResultErr: resultErr, Matched: rep.Matched, Reason: rep.Reason,
	})
	t.Logf("T-107 P4 driver agent=%s channel=%s revision=%d generation=%d task=%d result=%d expr=%v delivered=%v matched=%v reason=%q",
		agent, channel, revision, generation, taskID, resultID, hasExpr, delivered >= 0,
		rep.Matched, rep.Reason)
}

// t107WriteEnqueued 在入队后额外核对任务证据里的 GS 四维修订就是管理端保存的修订，
// 把「真实 GS 选择消费了哪一版」写进标记，避免只凭任务行数判断。
func t107WriteEnqueued(t *testing.T, db *store.DB, barrier string, m t107DriverMarker) {
	t.Helper()
	ev, ok, err := db.AITaskEvidenceOf(context.Background(), m.TaskID)
	if err != nil {
		t.Fatalf("读任务 %d 证据: %v", m.TaskID, err)
	}
	if !ok {
		t.Fatalf("任务 %d 没有任务证据（真实 GS 选择应写入绑定）", m.TaskID)
	}
	for _, part := range ev.Participants {
		if part.Key != m.Agent || part.GSPersonalityRevision == nil {
			continue
		}
		if *part.GSPersonalityRevision != m.Revision {
			t.Fatalf("任务证据 GS 修订=%d，管理端保存=%d（不应跨版）",
				*part.GSPersonalityRevision, m.Revision)
		}
		t107DriverWriteMarker(t, barrier, "enqueued.json", m)
		return
	}
	t.Fatalf("任务 %d 的证据没有绑定 %s 的 GS 四维修订: %+v", m.TaskID, m.Agent, ev.Participants)
}
