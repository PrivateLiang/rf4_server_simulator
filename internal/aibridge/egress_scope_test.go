package aibridge

// T-108-DoD-v3：BR 侧的出站范围复核、attempt 透传与唯一读路径静态断言。
//
// 这一组只跑 aibridge + store，不依赖 GS 投递侧：
//   - 出站/写结果前的范围复核用真实任务 + 真实 Runner，厂商是本地 TLS 计数桩；
//   - attempt 透传与「只用 attempt 绑定入口」用 runner.go 源码静态断言守住；
//   - E5 的裸 SQL 禁止同样用静态断言守住（运行证据在 gameserver 的
//     TestT108EvidenceRuntime/ActivityFactsPath）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// TestAIBridgeNoRawOfficialSQL 是 E5 的静态断言：aibridge 里不许再出现对
// competition / competition_member / tunables 的裸 SQL，活动事实只经
// store.AIActivityFacts 一处读。
func TestAIBridgeNoRawOfficialSQL(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录: %v", err)
	}
	banned := []string{
		"FROM competition",
		"FROM competition_member",
		"JOIN competition",
		"INTO competition",
		"FROM tunables",
		"event_switches",
	}
	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		scanned++
		text := string(src)
		for _, want := range banned {
			if strings.Contains(text, want) {
				t.Errorf("%s 仍有官方表裸 SQL：%q（只允许经 store.AIActivityFacts）", name, want)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("没有扫描到任何 aibridge 源文件")
	}
	actor, err := os.ReadFile("actor.go")
	if err != nil {
		t.Fatalf("读取 actor.go: %v", err)
	}
	if !strings.Contains(string(actor), "r.db.AIActivityFacts(ctx)") {
		t.Error("actor.go 的 activityFactSnapshot 没有走 store.AIActivityFacts")
	}
	// 唯一读入口：store 侧的 competition 查询只出现在 AIActivityFacts 一处。
	storeSrc, err := os.ReadFile("../store/repos_ai.go")
	if err != nil {
		t.Fatalf("读取 store/repos_ai.go: %v", err)
	}
	if n := strings.Count(string(storeSrc), "FROM competition"); n != 1 {
		t.Errorf("store/repos_ai.go 里 FROM competition 出现 %d 次，期望 1（AIActivityFacts）", n)
	}
	if n := strings.Count(string(storeSrc), "FROM tunables"); n != 0 {
		t.Errorf("store/repos_ai.go 里不应有 FROM tunables（%d 次）", n)
	}
}

// TestT108EgressScopeStatic 是 E2/E4 的静态断言：
//
//   - runner.go 只调用 attempt 绑定入口（带 t.Attempt / task.Attempt）；
//   - 出站前与写结果前的范围复核都接线了；
//   - 没有任何「按当前 processing 状态」的兼容入口被生产路径调用。
func TestT108EgressScopeStatic(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatalf("读取 runner.go: %v", err)
	}
	text := string(src)
	must := []string{
		"AICompleteWithExpression(ctx, t.ID, t.Attempt,",
		"AIAddChannelResultWithExpression(ctx, t.ID, t.Attempt,",
		"AIFinishTaskWithReason(ctx, t.ID, t.Attempt,",
		"AIFinishTaskWithReason(context.Background(), task.ID, task.Attempt,",
		"r.egressScopeReason(ctx, t, binding, agent.Key)",
		"r.writeScopeReason(ctx, t, binding,",
		"r.db.AIEventScopeHas(ctx, t.ScopeKey, a.Key)",
	}
	for _, want := range must {
		if !strings.Contains(text, want) {
			t.Errorf("runner.go 缺接线：%s", want)
		}
	}
	banned := []string{
		".AIComplete(ctx,",
		".AIAddChannelResult(ctx,",
		".AIFinishTask(ctx,",
		".AIFinishTaskWithReason(ctx, t.ID, \"",
		".AIFinishTaskWithReason(context.Background(), task.ID, \"",
	}
	for _, bad := range banned {
		if strings.Contains(text, bad) {
			t.Errorf("runner.go 仍在用未绑定 attempt 的入口：%s", bad)
		}
	}
	// 圆桌每轮出站前的范围复核必须存在（E2：圆桌中途撤）。
	if n := strings.Count(text, "AIEventScopeHas(ctx, t.ScopeKey, a.Key)"); n != 1 {
		t.Errorf("圆桌每轮范围复核出现 %d 次，期望 1", n)
	}
}

// scopeTestVendor 是可计数的本地 TLS 厂商。
type scopeTestVendor struct {
	srv  *httptest.Server
	hits int32
}

func newScopeTestVendor(t *testing.T) *scopeTestVendor {
	t.Helper()
	v := &scopeTestVendor{}
	v.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&v.hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"好家伙"}}]}`))
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *scopeTestVendor) calls() int { return int(atomic.LoadInt32(&v.hits)) }

// newScopeTestRunner 准备一份带本地厂商的 Runner 与一个已接入、已订阅的角色。
func newScopeTestRunner(t *testing.T, channel string) (*store.DB, *Runner, *scopeTestVendor) {
	t.Helper()
	ctx := context.Background()
	v := newScopeTestVendor(t)
	d, masterKey := newGateTestDB(t, v.srv.URL)
	if err := d.AISetAgentJoined(ctx, "companion", true, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetChannelAgent(ctx, channel, "companion", true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable}, nil); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = v.srv.Client().Transport
	return d, r, v
}

// starEvent 是一条 reply_star 系统频道事件的受信证据。
func starEvent() *store.AITaskEvidence {
	return &store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventReplyStar,
		Participants: []store.AITaskEvidenceParticipant{
			{Key: "companion", Capability: aiconfig.CapabilityReplyStar},
		},
	}
}

// TestRunnerStopsOutOfScopeBeforeEgress 撤订阅（能力不动）后，BR 在真正出站前
// 按任务频道复核当前范围：不调用厂商、按 out_of_scope 落不可投递终态。
func TestRunnerStopsOutOfScopeBeforeEgress(t *testing.T) {
	ctx := context.Background()
	const channel = "t108_scope_br"
	d, r, vendor := newScopeTestRunner(t, channel)
	taskID, err := d.AIEnqueueSystemChannel(ctx, channel, 1, "系统事件", "companion", 2147483645, starEvent())
	if err != nil {
		t.Fatal(err)
	}
	// 只撤频道订阅，能力与代次都不动（R-154 复现的正是这一种）。
	if err := d.AISetChannelAgent(ctx, channel, "companion", false); err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("认领任务: ok=%v err=%v", ok, err)
	}
	r.handle(ctx, task)

	if got := vendor.calls(); got != 0 {
		t.Fatalf("撤订阅后 BR 不得调用厂商，实得 %d 次", got)
	}
	status, reply, errText := aiResultOf(t, d, taskID)
	if status != "fallback" || reply != "" {
		t.Fatalf("应落不可投递终态，实得 (%q,%q)", status, reply)
	}
	if errText != store.AIEvidenceReasonOutOfScope {
		t.Fatalf("终止原因 = %q，期望 %q", errText, store.AIEvidenceReasonOutOfScope)
	}

	// 撤后重授（订阅不是修订号）：新任务照常放行，旧任务保持终态。
	if err := d.AISetChannelAgent(ctx, channel, "companion", true); err != nil {
		t.Fatal(err)
	}
	secondID, err := d.AIEnqueueSystemChannel(ctx, channel, 1, "系统事件2", "companion", 2147483645, starEvent())
	if err != nil {
		t.Fatal(err)
	}
	task2, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("认领第二个任务: ok=%v err=%v", ok, err)
	}
	r.handle(ctx, task2)
	if got := vendor.calls(); got != 1 {
		t.Fatalf("重授后应调用一次厂商，实得 %d", got)
	}
	status2, reply2, _ := aiResultOf(t, d, secondID)
	if status2 != "done" || reply2 == "" {
		t.Fatalf("重授后应产生正常结果，实得 (%q,%q)", status2, reply2)
	}
	// 旧任务仍是 out_of_scope 终态，不补发、不改派。
	statusOld, _, errOld := aiResultOf(t, d, taskID)
	if statusOld != "fallback" || errOld != store.AIEvidenceReasonOutOfScope {
		t.Fatalf("旧任务被改写: (%q,%q)", statusOld, errOld)
	}
}

// TestRunnerDirectDialogIgnoresScope 是 E2 的 Direct/DM 对照：直接询答与 DM
// 不受订阅范围限制（撤订阅后仍能答）。
func TestRunnerDirectDialogIgnoresScope(t *testing.T) {
	ctx := context.Background()
	const channel = "t108_scope_dm"
	d, r, vendor := newScopeTestRunner(t, channel)
	// 撤掉频道订阅：频道事件会拒绝，但 DM / 直接询答不受影响。
	if err := d.AISetChannelAgent(ctx, channel, "companion", false); err != nil {
		t.Fatal(err)
	}
	// 直接询答：频道任务但证据是 Direct（AIEnqueue 写「直接对话」）。
	directID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "channel", Scope: "channel", ScopeKey: channel, UID: 1,
		Agent: "companion", ContactUID: 2147483645, SourceKind: "human", Text: "你在吗",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("认领直接询答: ok=%v err=%v", ok, err)
	}
	r.handle(ctx, task)
	if got := vendor.calls(); got != 1 {
		t.Fatalf("直接询答不应受范围限制，厂商调用 %d 次", got)
	}
	status, reply, _ := aiResultOf(t, d, directID)
	if status != "done" || reply == "" {
		t.Fatalf("直接询答应正常回答，实得 (%q,%q)", status, reply)
	}

	// DM：同样的证据（Direct）且与任何频道无关。
	dmID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
		Agent: "companion", ContactUID: 2147483645, SourceKind: "human", Text: "私聊",
	})
	if err != nil {
		t.Fatal(err)
	}
	task2, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("认领 DM: ok=%v err=%v", ok, err)
	}
	r.handle(ctx, task2)
	if got := vendor.calls(); got != 2 {
		t.Fatalf("DM 不应受范围限制，厂商调用 %d 次", got)
	}
	status, reply, _ = aiResultOf(t, d, dmID)
	if status != "done" || reply == "" {
		t.Fatalf("DM 应正常回答，实得 (%q,%q)", status, reply)
	}
}

// TestRunnerMentionGateOnChannelEvent mention 是硬门：频道主动事件不选它，
// 但被点名的直接询答照常回答。
func TestRunnerMentionGateOnChannelEvent(t *testing.T) {
	ctx := context.Background()
	const channel = "t108_scope_mention"
	d, r, vendor := newScopeTestRunner(t, channel)
	if err := d.AISetAgentBehavior(ctx, "companion", "mention", 50); err != nil {
		t.Fatal(err)
	}
	eventID, err := d.AIEnqueueSystemChannel(ctx, channel, 1, "系统事件", "companion", 2147483645, starEvent())
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("认领事件: ok=%v err=%v", ok, err)
	}
	r.handle(ctx, task)
	if got := vendor.calls(); got != 0 {
		t.Fatalf("mention 不参与频道主动事件，实得 %d 次厂商调用", got)
	}
	status, reply, _ := aiResultOf(t, d, eventID)
	if status != "done" || reply != "" {
		t.Fatalf("mention 的频道事件应静默丢弃，实得 (%q,%q)", status, reply)
	}

	// 被点名的直接询答：仍然回答。
	directID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "channel", Scope: "channel", ScopeKey: channel, UID: 1,
		Agent: "companion", ContactUID: 2147483645, SourceKind: "human", Text: "@伙伴 你说呢",
	})
	if err != nil {
		t.Fatal(err)
	}
	task2, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("认领点名任务: ok=%v err=%v", ok, err)
	}
	r.handle(ctx, task2)
	if got := vendor.calls(); got != 1 {
		t.Fatalf("被点名的直接询答应回答，实得 %d 次厂商调用", got)
	}
	status, reply, _ = aiResultOf(t, d, directID)
	if status != "done" || reply == "" {
		t.Fatalf("点名询答应有正文，实得 (%q,%q)", status, reply)
	}
}

// TestRunnerAttemptRejectsLateResult 是 E4 的 BR 侧形状：认领 attempt 之后
// 任务被回收重领，旧尝试写结果必须被拒（ErrAITaskNotActive），且不产生结果行。
func TestRunnerAttemptRejectsLateResult(t *testing.T) {
	ctx := context.Background()
	const channel = "t108_scope_attempt"
	d, _, _ := newScopeTestRunner(t, channel)
	// 用真实认领拿 attempt=1，再把任务回收成 pending（模拟 worker 崩溃后的回收）。
	taskID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
		Agent: "companion", ContactUID: 2147483645, SourceKind: "human", Text: "第一句",
		DeadlineTS: 1, // 已过期，供回收
	})
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok || first.Attempt != 1 {
		t.Fatalf("第一次认领应返回 attempt=1: %+v ok=%v err=%v", first, ok, err)
	}
	if _, _, err := d.AIExpireTasks(ctx, 100, 0); err != nil {
		t.Fatal(err)
	}
	second, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok || second.Attempt != 2 {
		t.Fatalf("回收后第二次认领应返回 attempt=2: %+v ok=%v err=%v", second, ok, err)
	}
	// 旧尝试（attempt=1）的迟到写入。
	if err := d.AICompleteWithExpression(ctx, taskID, first.Attempt, "done", "迟到的一句", "", "", nil); err == nil {
		t.Fatal("旧尝试的迟到写入应被拒绝")
	} else if !strings.Contains(err.Error(), "not active") {
		t.Fatalf("迟到写入的失败原因 = %v，期望 ErrAITaskNotActive", err)
	}
	var state string
	var attempt int
	if err := d.SQL.QueryRow(`SELECT state, attempt FROM ai_task WHERE id=?`, taskID).Scan(&state, &attempt); err != nil {
		t.Fatal(err)
	}
	if state != "processing" || attempt != 2 {
		t.Fatalf("迟到写入改了任务状态: state=%q attempt=%d", state, attempt)
	}
	if n := t108CountResults(t, d, taskID); n != 0 {
		t.Fatalf("旧尝试不应写出结果，实得 %d 行", n)
	}
	// 新尝试照常写入。
	if err := d.AICompleteWithExpression(ctx, taskID, second.Attempt, "done", "新的一句", "", "", nil); err != nil {
		t.Fatalf("新尝试写入失败: %v", err)
	}
	if n := t108CountResults(t, d, taskID); n != 1 {
		t.Fatalf("新尝试应写出一行结果，实得 %d", n)
	}
}

// t108CountResults 统计某任务的结果行数。
func t108CountResults(t *testing.T, d *store.DB, taskID int64) int {
	t.Helper()
	var n int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_result WHERE task_id=?`, taskID).Scan(&n); err != nil {
		t.Fatalf("统计结果: %v", err)
	}
	return n
}
