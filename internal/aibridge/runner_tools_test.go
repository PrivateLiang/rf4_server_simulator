package aibridge

// T-104：Runner 只按能力投影生成工具清单与受信环境，不再读 agent.tools_json 原文。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/gameserver"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

func TestRunnerToolEnvFollowsCapabilities(t *testing.T) {
	d := openBridgeRuntimeDB(t)
	ctx := context.Background()
	k := bridgeStaticKnowledge(t)
	r := &Runner{db: d, knowledge: k, registry: mustRegistry(t, k), log: bridgeTestLogger()}
	agent, err := d.AIAgentByKey(ctx, "companion")
	if err != nil || agent == nil {
		t.Fatalf("读 Agent: %v", err)
	}
	task := &store.AITask{ID: 1, Text: "河鲈在哪", Loc: "level_001_lake"}

	// 缺新记录：只保留旧 tools_json 里可证明的只读子集（companion 是 map_vibe）。
	caps := r.capabilityState(ctx, agent)
	if !caps.Legacy || len(caps.ReadTools) != 1 || caps.ReadTools[0] != aiconfig.ToolMapVibe {
		t.Fatalf("旧快照投影 = %+v", caps)
	}
	env := r.toolEnv(ctx, task, agent, caps, ctx)
	if len(env.Allowed) != 1 || !env.Allowed[aiconfig.ToolMapVibe] ||
		env.Allowed[aiconfig.ToolActorTargetSuggest] {
		t.Fatalf("旧快照允许集合 = %v", env.Allowed)
	}
	if specs := r.registry.Specs(caps.Tools()); len(specs) != 1 || specs[0].Name != aiconfig.ToolMapVibe {
		t.Fatalf("旧快照 specs = %+v", specs)
	}

	// 显式空：任何工具都不在清单里。
	if _, err := d.AISaveCapabilities(ctx, "companion", 0, []string{}, nil); err != nil {
		t.Fatal(err)
	}
	caps = r.capabilityState(ctx, agent)
	if caps.State != aiconfig.CapabilityStateEmpty || len(caps.Tools()) != 0 {
		t.Fatalf("显式空投影 = %+v", caps)
	}
	if specs := r.registry.Specs(caps.Tools()); len(specs) != 0 {
		t.Fatalf("显式空仍给了工具: %+v", specs)
	}

	// 攻略 + 接受建议：八个读 + 一个写；写工具最后。
	if _, err := d.AISaveCapabilities(ctx, "companion", 1,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil); err != nil {
		t.Fatal(err)
	}
	caps = r.capabilityState(ctx, agent)
	specs := r.registry.Specs(caps.Tools())
	if len(specs) != 9 || specs[8].Name != aiconfig.ToolActorTargetSuggest {
		t.Fatalf("完整能力 specs = %+v", specs)
	}
	if specs[8].Kind != ToolKindWrite || specs[8].Route != ToolRouteGSWrite {
		t.Fatalf("写工具路由 = %+v", specs[8])
	}
	env = r.toolEnv(ctx, task, agent, caps, ctx)
	if env.Generation != agent.Generation || env.Revision != agent.Revision {
		t.Fatalf("工具环境代次 = %d/%d，期望 %d/%d", env.Generation, env.Revision,
			agent.Generation, agent.Revision)
	}
}

// 工具循环只走 registry.Dispatch：写工具真正入队并回固定受理话术，
// 未知工具明确报错；没有按名字分发的 switch 可绕开权限与参数校验。
func TestRunnerToolCallsRouteThroughDispatch(t *testing.T) {
	d := openBridgeRuntimeDB(t)
	ctx := context.Background()
	seedBridgeActor(t, d, "companion", "level_002_river", "测试河", "pike", "狗鱼",
		float64(time.Now().Unix())+3600)
	k := bridgeStaticKnowledge(t)
	r := &Runner{db: d, knowledge: k, registry: mustRegistry(t, k), log: bridgeTestLogger()}
	if _, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil); err != nil {
		t.Fatal(err)
	}
	agent, err := d.AIAgentByKey(ctx, "companion")
	if err != nil || agent == nil {
		t.Fatal(err)
	}
	id, err := d.AIEnqueue(ctx, &store.AITask{Kind: "dm", SourceKind: "human", Agent: "companion", Text: "换鲈鱼吧"})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok || task.ID != id {
		t.Fatalf("claim: %v/%v", ok, err)
	}
	ctx = withAITaskIntent(ctx, task)
	caps := r.capabilityState(ctx, agent)
	if !caps.Has(aiconfig.CapabilityAcceptTarget) {
		t.Fatalf("能力未生效: %+v", caps)
	}

	msgs, stopped, reason := r.runToolCalls(ctx, task, agent, caps, []ToolCall{
		{ID: "call-write", Name: aiconfig.ToolActorTargetSuggest, Arguments: `{"fish":"鲈鱼"}`},
		{ID: "call-read", Name: aiconfig.ToolSpeciesLookup, Arguments: `{"query":"鲈鱼"}`},
		{ID: "call-unknown", Name: "not_a_tool", Arguments: `{}`},
	}, ContextBudget{InternalChars: 800})
	if stopped || reason != "" {
		t.Fatalf("不应停: %v/%s", stopped, reason)
	}
	if len(msgs) != 3 {
		t.Fatalf("工具消息 = %d", len(msgs))
	}
	if msgs[0].Content != "已提交，等待游戏服务确认。" {
		t.Fatalf("写回执 = %q", msgs[0].Content)
	}
	if !strings.Contains(msgs[1].Content, "鲈鱼") {
		t.Fatalf("本地读 = %q", msgs[1].Content)
	}
	if msgs[2].Content != "（工具不存在）" {
		t.Fatalf("未知工具 = %q", msgs[2].Content)
	}
	var rows int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("写命令 = %d 行", rows)
	}
	var payload string
	if err := d.SQL.QueryRow(`SELECT payload FROM ai_runtime_command`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"capability":"accept_target_suggest"`) ||
		!strings.Contains(payload, `"cap_revision":1`) {
		t.Fatalf("写命令缺可复核来源: %s", payload)
	}
}

// ---- T-104 返工：写回执、下一轮终态、同任务本地撤权 ----

func t104BridgeRunner(t *testing.T, d *store.DB, master []byte, vendor *httptest.Server) *Runner {
	t.Helper()
	r := NewRunner(d, bridgeStaticKnowledge(t),
		config.AI{Enabled: true, RequestTimeoutSec: 5}, master, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	return r
}

func t104DMTask(t *testing.T, d *store.DB, text string) *store.AITask {
	t.Helper()
	ctx := context.Background()
	if _, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1, Agent: "companion",
		ContactUID: 2147483645, SourceKind: "human", Text: text,
	}); err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	return task
}

// 写工具与旧旁路都必须固定玩家回执：模型在受理后仍说「已经换好目标」时，
// ai_result.reply_text 只能是「已提交，等待游戏服务确认。」
func TestT104WriteReceiptFixedForToolAndLegacyPaths(t *testing.T) {
	t.Run("tool", func(t *testing.T) {
		var hits int32
		vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if atomic.AddInt32(&hits, 1) == 1 {
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[`+
					`{"id":"call-write","type":"function","function":{"name":"actor_target_suggest",`+
					`"arguments":"{\"fish\":\"鲈鱼\"}"}}]}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"已经换好目标，改钓鲈鱼了"}}]}`)
		}))
		defer vendor.Close()
		d, master := newGateTestDB(t, vendor.URL)
		ctx := context.Background()
		seedBridgeActor(t, d, "companion", "level_002_river", "测试河", "pike", "狗鱼",
			float64(time.Now().Unix())+3600)
		if _, err := d.AISaveCapabilities(ctx, "companion", 0,
			[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil); err != nil {
			t.Fatal(err)
		}
		task := t104DMTask(t, d, "换个目标吧")
		r := t104BridgeRunner(t, d, master, vendor)
		r.handle(ctx, task)

		status, reply, errText := aiResultOf(t, d, task.ID)
		if status != "done" || reply != aiWriteReceiptText {
			t.Fatalf("玩家回执 = (%q,%q,%q)，期望 done/%q", status, reply, errText, aiWriteReceiptText)
		}
		var cmdStatus, requestID, payload string
		if err := d.SQL.QueryRow(`SELECT status, request_id, payload FROM ai_runtime_command
			WHERE parent_task_id=?`, task.ID).Scan(&cmdStatus, &requestID, &payload); err != nil {
			t.Fatal(err)
		}
		if cmdStatus != store.AIRuntimeStatusPending || !strings.HasPrefix(requestID, "ai-") ||
			!strings.Contains(payload, `"capability":"accept_target_suggest"`) {
			t.Fatalf("命令 = %s/%s/%s", cmdStatus, requestID, payload)
		}
		t.Logf("tool 路径：request_id=%s status=%s player_reply=%q", requestID, cmdStatus, reply)
	})

	t.Run("legacy", func(t *testing.T) {
		var hits int32
		vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"已经换好目标，改钓鲈鱼了"}}]}`)
		}))
		defer vendor.Close()
		d, master := newGateTestDB(t, vendor.URL)
		ctx := context.Background()
		seedBridgeActor(t, d, "companion", "level_002_river", "测试河", "pike", "狗鱼",
			float64(time.Now().Unix())+3600)
		if _, err := d.AISaveCapabilities(ctx, "companion", 0,
			[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil); err != nil {
			t.Fatal(err)
		}
		task := t104DMTask(t, d, "鲈鱼今天口不错，我去钓鲈鱼")
		r := t104BridgeRunner(t, d, master, vendor)
		r.handle(ctx, task)

		status, reply, _ := aiResultOf(t, d, task.ID)
		if status != "done" || reply != aiWriteReceiptText {
			t.Fatalf("旧旁路玩家回执 = (%q,%q)，期望 done/%q", status, reply, aiWriteReceiptText)
		}
		var rows int
		if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command WHERE parent_task_id=?`,
			task.ID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || atomic.LoadInt32(&hits) != 1 {
			t.Fatalf("旧旁路命令/请求 = %d/%d", rows, hits)
		}
		var requestID, cmdStatus string
		if err := d.SQL.QueryRow(`SELECT request_id, status FROM ai_runtime_command
			WHERE parent_task_id=?`, task.ID).Scan(&requestID, &cmdStatus); err != nil {
			t.Fatal(err)
		}
		t.Logf("legacy 路径：request_id=%s status=%s player_reply=%q", requestID, cmdStatus, reply)
	})
}

// 下一个任务只读本角色至多 8 条终态：他角结果、pending 不混入，
// 缺原因用 unknown；没有下一轮时不主动推送、不轮询。
func TestT104NextTaskReadsOwnTerminalStatesOnly(t *testing.T) {
	var mu sync.Mutex
	var requests []string
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"好"}}]}`)
	}))
	defer vendor.Close()
	d, master := newGateTestDB(t, vendor.URL)
	ctx := context.Background()
	st, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"fish_key":"perch","loc_key":"level_001_lake","capability":"accept_target_suggest","cap_revision":` +
		strconv.FormatInt(st.Revision, 10) + `}`)

	makeCmd := func(agent, requestID, reason, status string) {
		parent, err := d.AIEnqueue(ctx, &store.AITask{Kind: "dm", SourceKind: "human",
			Agent: agent, Text: "旧任务"})
		if err != nil {
			t.Fatal(err)
		}
		cmd, _, err := d.AIEnqueueRuntimeCommand(ctx, store.AIRuntimeEnqueueRequest{
			RequestID: requestID, Source: store.AIRuntimeSourceBridge,
			Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: agent,
			ParentTaskID: parent, Payload: payload,
		})
		if err != nil {
			t.Fatalf("入队旧命令 %s: %v", requestID, err)
		}
		if status == store.AIRuntimeStatusPending {
			if _, err := d.SQL.Exec(`UPDATE ai_runtime_command SET reason=? WHERE id=?`, reason, cmd.ID); err != nil {
				t.Fatal(err)
			}
			return
		}
		if _, err := d.SQL.Exec(`UPDATE ai_runtime_command SET status=?, reason=?, applied_ts=? WHERE id=?`,
			status, reason, float64(time.Now().Unix()), cmd.ID); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 9; i++ {
		makeCmd("companion", fmt.Sprintf("bridge:%d:companion:1", 600+i),
			fmt.Sprintf("reason-%d", i), store.AIRuntimeStatusRejected)
	}
	// 最新一条终态缺原因：必须显示 unknown，而不是猜成功。
	makeCmd("companion", "bridge:610:companion:1", "", store.AIRuntimeStatusRejected)
	// 同角色的 pending 与他角终态都不能进入本角色上下文。
	makeCmd("companion", "bridge:611:companion:1", "pending_reason", store.AIRuntimeStatusPending)
	makeCmd("fishguide", "bridge:612:fishguide:1", "other_agent_reason", store.AIRuntimeStatusRejected)

	task := t104DMTask(t, d, "刚才那条鱼呢")
	r := t104BridgeRunner(t, d, master, vendor)
	r.handle(ctx, task)
	if status, _, _ := aiResultOf(t, d, task.ID); status != "done" {
		t.Fatalf("下一任务终态 = %s", status)
	}

	mu.Lock()
	body := strings.Join(requests, "\n---\n")
	count := len(requests)
	mu.Unlock()
	for _, want := range []string{"reason-9", "reason-3", "unknown"} {
		if !strings.Contains(body, want) {
			t.Fatalf("下一轮上下文缺 %q：%s", want, body)
		}
	}
	for _, bad := range []string{"reason-1", "reason-2", "other_agent_reason", "pending_reason"} {
		if strings.Contains(body, bad) {
			t.Fatalf("下一轮上下文混入 %q：%s", bad, body)
		}
	}
	// 没有下一轮就不该再发请求：单次任务只调一次模型，等待后也不增加。
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	after := len(requests)
	mu.Unlock()
	if count != 1 || after != 1 {
		t.Fatalf("无下一轮时发生了额外请求：%d → %d", count, after)
	}
}

// 下一轮上下文里的终态必须来自真实 GS 应用路径：一条真的 applied、一条真的
// rejected（unknown_fish），各自带 request_id 与原因。
func TestT104NextTaskShowsRealGSApplyAndReject(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var requests []string
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"好"}}]}`)
	}))
	defer vendor.Close()
	d, master := newGateTestDB(t, vendor.URL)
	if _, err := d.SQL.Exec(`INSERT OR IGNORE INTO map_fish_roster(loc, fish_key)
		VALUES('level_001_lake','perch')`); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSetting(ctx, "ai_actor_switch_chance", "1"); err != nil {
		t.Fatal(err)
	}
	seedBridgeActor(t, d, "companion", "level_002_river", "测试河", "pike", "狗鱼",
		float64(time.Now().Unix())+3600)
	st, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parentTask := func(text string) *store.AITask {
		if _, err := d.AIEnqueue(ctx, &store.AITask{
			Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1, Agent: "companion",
			ContactUID: 2147483645, SourceKind: "human", Text: text,
		}); err != nil {
			t.Fatal(err)
		}
		task, ok, err := d.AIPopTask(ctx)
		if err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		return task
	}

	// 先入队必然被 GS 拒绝的一条（鱼不在 roster），再入队可应用的一条。
	rejectTask := parentTask("换个不存在的鱼")
	rejectPayload := []byte(fmt.Sprintf(
		`{"fish_key":"definitely_missing","loc_key":"level_001_lake","capability":"accept_target_suggest","cap_revision":%d}`,
		st.Revision))
	rejectCmd, _, err := d.AIEnqueueRuntimeCommand(ctx, store.AIRuntimeEnqueueRequest{
		RequestID: "t104-reject-context", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: "companion",
		ParentTaskID: rejectTask.ID, ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: rejectPayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	applyTask := parentTask("换鲈鱼")
	bare := &Runner{db: d, knowledge: bridgeStaticKnowledge(t), log: bridgeTestLogger()}
	accepted, err := bare.enqueueTargetSuggest(ctx, applyTask, "companion", st.Revision, "鲈鱼", "", "call-apply")
	if err != nil || accepted.RequestID == "" {
		t.Fatalf("受理建议 = %+v/%v", accepted, err)
	}

	gs := gameserver.NewServer(config.Game{Tunnel: config.Tunnel{PSK: "t104-context"}},
		logging.Discard(), nil)
	gs.SetDB(d)
	stop := make(chan struct{})
	gs.StartAIRuntimeCommands(d, stop)
	defer close(stop)
	rejectDone := t104WaitRuntimeTerminal(t, d, rejectCmd.RequestID)
	applyDone := t104WaitRuntimeTerminal(t, d, accepted.RequestID)
	if rejectDone.Status != store.AIRuntimeStatusRejected || rejectDone.Reason != "unknown_fish" {
		t.Fatalf("拒绝命令 = %s/%s", rejectDone.Status, rejectDone.Reason)
	}
	if applyDone.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("应用命令 = %s/%s", applyDone.Status, applyDone.Reason)
	}

	next := parentTask("刚才换鱼成功了吗")
	r := t104BridgeRunner(t, d, master, vendor)
	r.handle(ctx, next)
	if status, _, _ := aiResultOf(t, d, next.ID); status != "done" {
		t.Fatalf("下一任务终态 = %s", status)
	}
	mu.Lock()
	body := strings.Join(requests, "\n---\n")
	mu.Unlock()
	for _, want := range []string{applyDone.RequestID, "已应用", "鲈鱼",
		rejectDone.RequestID, "被拒绝", "unknown_fish"} {
		if !strings.Contains(body, want) {
			t.Fatalf("真实 GS 终态未进入下一轮上下文，缺 %q：%s", want, body)
		}
	}
	t.Logf("GS applied request_id=%s status=%s；GS rejected request_id=%s status=%s reason=%s",
		applyDone.RequestID, applyDone.Status, rejectDone.RequestID, rejectDone.Status, rejectDone.Reason)
}

func t104WaitRuntimeTerminal(t *testing.T, d *store.DB, requestID string) *store.AIRuntimeCommand {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cmd, err := d.AIGetRuntimeCommandByRequest(context.Background(), requestID)
		if err == nil && cmd.Status != store.AIRuntimeStatusPending {
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("命令 %s 未在期限内终结", requestID)
	return nil
}

// t104BlockingToolVendor 第一次请求返回指定工具调用并阻塞到 release，
// 第二次请求捕获 tool 消息；用于固定「任务开始 → 撤权 → 下一次 Dispatch」顺序。
func t104BlockingToolVendor(t *testing.T, toolName, args, finalText string) (
	*httptest.Server, <-chan struct{}, chan struct{}, *int32, func() string) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var hits int32
	var mu sync.Mutex
	var toolMsg string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		raw, _ := io.ReadAll(r.Body)
		once.Do(func() { close(started) })
		<-release
		if n > 1 {
			var req struct {
				Messages []Message `json:"messages"`
			}
			_ = json.Unmarshal(raw, &req)
			mu.Lock()
			for _, m := range req.Messages {
				if m.Role == "tool" {
					toolMsg = m.Content
				}
			}
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			body := map[string]any{"choices": []map[string]any{{"message": map[string]any{
				"content": "",
				"tool_calls": []map[string]any{{
					"id": "call-1", "type": "function",
					"function": map[string]string{"name": toolName, "arguments": args},
				}},
			}}}}
			encoded, _ := json.Marshal(body)
			_, _ = w.Write(encoded)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":`+strconv.Quote(finalText)+`}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, started, release, &hits, func() string {
		mu.Lock()
		defer mu.Unlock()
		return toolMsg
	}
}

// 同一个已开始的任务：管理员保存空集合后，下一次本地 Dispatch 必须按当前
// 授权拒绝，不能继续用任务开始时的快照读本地资料。
func TestT104LocalRevocationStopsSameTaskDispatch(t *testing.T) {
	ctx := context.Background()
	var localCalls int32
	aiToolLocalExecHookForTest = func(name string) {
		if name == aiconfig.ToolSpeciesLookup {
			atomic.AddInt32(&localCalls, 1)
		}
	}
	t.Cleanup(func() { aiToolLocalExecHookForTest = nil })

	revokeVendor, started, release, hits, toolMsg := t104BlockingToolVendor(t, aiconfig.ToolSpeciesLookup,
		`{"query":"鲈鱼"}`, "查不了")
	d, master := newGateTestDB(t, revokeVendor.URL)
	if _, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp}, nil); err != nil {
		t.Fatal(err)
	}
	task := t104DMTask(t, d, "鲈鱼在哪")
	r := t104BridgeRunner(t, d, master, revokeVendor)
	done := runTaskAsync(t, r, task)
	<-started // 任务已开始：能力快照与预注入已经发生，模型请求在途
	if _, err := d.AISaveCapabilities(ctx, "companion", 1, []string{}, nil); err != nil {
		t.Fatalf("撤权: %v", err)
	}
	close(release)
	<-done
	if status, _, _ := aiResultOf(t, d, task.ID); status != "done" {
		t.Fatalf("任务终态 = %s", status)
	}
	if got := toolMsg(); got != "（该工具未获授权）" {
		t.Fatalf("撤权后工具消息 = %q", got)
	}
	if atomic.LoadInt32(&localCalls) != 0 {
		t.Fatalf("撤权后仍执行了 %d 次本地工具处理器", localCalls)
	}
	if atomic.LoadInt32(hits) < 2 {
		t.Fatalf("模型请求次数 = %d", hits)
	}
	t.Logf("撤权屏障：tool_message=%q local_calls=%d vendor_hits=%d",
		toolMsg(), atomic.LoadInt32(&localCalls), atomic.LoadInt32(hits))

	// 对照：未撤权时同一工具会真正执行并拿到本地资料。
	controlVendor, controlStarted, controlRelease, _, controlMsg := t104BlockingToolVendor(
		t, aiconfig.ToolSpeciesLookup, `{"query":"鲈鱼"}`, "查到了")
	if _, err := d.SQL.Exec(`UPDATE ai_provider SET base_url=? WHERE name='deepseek'`,
		controlVendor.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AISaveCapabilities(ctx, "companion", 2,
		[]string{aiconfig.CapabilityGuideHelp}, nil); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(&localCalls, 0)
	task2 := t104DMTask(t, d, "鲈鱼在哪")
	r2 := t104BridgeRunner(t, d, master, controlVendor)
	done2 := runTaskAsync(t, r2, task2)
	select {
	case <-controlStarted:
	case <-time.After(3 * time.Second):
		var status, reply, errText string
		_ = d.SQL.QueryRow(`SELECT status, reply_text, err FROM ai_result WHERE task_id=?`, task2.ID).
			Scan(&status, &reply, &errText)
		t.Fatalf("未发出模型请求: status=%q reply=%q err=%q", status, reply, errText)
	}
	close(controlRelease)
	<-done2
	if atomic.LoadInt32(&localCalls) != 1 {
		t.Fatalf("未撤权时本地处理器执行 %d 次，期望 1", localCalls)
	}
	if got := controlMsg(); !strings.Contains(got, "鲈鱼") {
		t.Fatalf("未撤权工具消息 = %q", got)
	}
}
