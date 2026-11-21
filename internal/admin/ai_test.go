package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/gameserver"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

func TestSafeAIErrorMasksKey(t *testing.T) {
	got := safeAIError(errors.New("厂商返回 401: sk-abcdefgh123456 unauthorized"))
	if strings.Contains(got, "abcdefgh") {
		t.Fatalf("错误信息泄露密钥: %q", got)
	}
	if !strings.Contains(got, "sk-***") {
		t.Fatalf("未替换为掩码: %q", got)
	}
	if safeAIError(nil) != "" {
		t.Fatal("nil 错误应返回空")
	}
}

func TestValidBaseURL(t *testing.T) {
	for _, ok := range []string{"https://api.deepseek.com", "http://127.0.0.1:11434/v1"} {
		if !validBaseURL(ok) {
			t.Fatalf("合法地址被拒绝: %s", ok)
		}
	}
	for _, bad := range []string{"", "file:///etc/passwd", "api.deepseek.com", "ftp://x"} {
		if validBaseURL(bad) {
			t.Fatalf("非法地址被接受: %s", bad)
		}
	}
}

func TestSafeAIErrorTruncates(t *testing.T) {
	long := strings.Repeat("错误信息", 100)
	got := safeAIError(errors.New(long))
	if len([]rune(got)) > 200 {
		t.Fatalf("未截断: %d", len([]rune(got)))
	}
}

// ---- T-105 四步开聊：总览证据、自动 UID、接入与恢复 ----

type t105Step struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Display string `json:"display"`
	BudgetS int    `json:"budget_s"`
}

type t105AgentEvidence struct {
	ContactVisible  bool    `json:"contact_visible"`
	VisibleEvidence string  `json:"visible_evidence"`
	PendingCommands int     `json:"pending_commands"`
	CatchTotal      int     `json:"catch_total"`
	CatchToday      int     `json:"catch_today"`
	LastMailTS      float64 `json:"last_mail_ts"`
	LastReply       struct {
		Status      string  `json:"status"`
		Error       string  `json:"error"`
		Display     string  `json:"display"`
		TS          float64 `json:"ts"`
		DeliveredTS float64 `json:"delivered_ts"`
	} `json:"last_reply"`
}

type t105Onboarding struct {
	Status struct {
		Priority string `json:"priority"`
		Display  string `json:"display"`
		Next     string `json:"next"`
	} `json:"status"`
	Steps           []t105Step                   `json:"steps"`
	Agents          map[string]t105AgentEvidence `json:"agents"`
	JoinedTotal     int                          `json:"joined_total"`
	VisibleTotal    int                          `json:"visible_total"`
	DeliveredTotal  int                          `json:"delivered_total"`
	AgentsTotal     int                          `json:"agents_total"`
	PendingCommands int                          `json:"pending_commands"`
	TargetTotalS    int                          `json:"target_total_s"`
	BufferS         int                          `json:"buffer_s"`
}

func t105GetStatus(t *testing.T, ts *httptest.Server, token string) (t105Onboarding, map[string]any) {
	t.Helper()
	code, body := capabilityRequest(t, ts, token, "GET", "/api/ai/status", "")
	if code != 200 {
		t.Fatalf("status = %d: %s", code, body)
	}
	var raw struct {
		Onboarding t105Onboarding `json:"onboarding"`
		Credential map[string]any `json:"credential"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("解析 status: %v: %s", err, body)
	}
	var all map[string]any
	_ = json.Unmarshal([]byte(body), &all)
	return raw.Onboarding, all
}

func t105StepByID(steps []t105Step, id string) t105Step {
	for _, step := range steps {
		if step.ID == id {
			return step
		}
	}
	return t105Step{}
}

// TestT105OnboardingStatusAndAgentContract 覆盖四步证据、自动 UID、接入只写管理配置、
// 玩家可见与「已保存」分离、旧角色恢复以及急停优先级。
func TestT105OnboardingStatusAndAgentContract(t *testing.T) {
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	token := signAdminToken(t, srv)

	onboarding, _ := t105GetStatus(t, ts, token)
	if len(onboarding.Steps) != 4 {
		t.Fatalf("四步数量 = %d，期望 4", len(onboarding.Steps))
	}
	order := []string{"connection", "create", "join", "hello"}
	for i, id := range order {
		if onboarding.Steps[i].ID != id {
			t.Fatalf("第 %d 步 = %q，期望 %q", i+1, onboarding.Steps[i].ID, id)
		}
	}
	total := 0
	for _, step := range onboarding.Steps {
		total += step.BudgetS
	}
	if total != 240 || onboarding.TargetTotalS != 300 || onboarding.BufferS != 60 {
		t.Fatalf("四步预算 = %d + 缓冲 %d / 总 %d，期望 240+60=300", total, onboarding.BufferS, onboarding.TargetTotalS)
	}
	if onboarding.Status.Priority == "" || onboarding.Steps[0].State == "" {
		t.Fatalf("初始状态缺失: %+v", onboarding)
	}

	// 新增：不传 UID，服务端自动分配；手填 UID 一律拒绝。
	code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/agent/create",
		`{"key":"t105a","display":"首句角色","role":"测试","prompt":"你是测试角色","thinking_effort":"medium","thinking_enabled":true}`)
	if code != 200 {
		t.Fatalf("新增 Agent = %d: %s", code, body)
	}
	var created struct {
		ContactUID int64 `json:"contact_uid"`
		Revived    bool  `json:"revived"`
		UIDAuto    bool  `json:"uid_auto"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.ContactUID < 2147480000 || created.ContactUID > 2147483645 || created.Revived || !created.UIDAuto {
		t.Fatalf("新增回执 = %+v", created)
	}
	if code, _ := capabilityRequest(t, ts, token, "POST", "/api/ai/agent/create",
		`{"key":"t105b","display":"手填 UID","contact_uid":2147480001}`); code != 400 {
		t.Fatalf("手填 UID 状态码 = %d，期望 400", code)
	}

	// 接入只传 key/joined：200 直接写库，不伪造 202；玩家可见仍待证据。
	code, body = capabilityRequest(t, ts, token, "POST", "/api/ai/agent/join",
		fmt.Sprintf(`{"key":"t105a","joined":true,"contact_uid":2147480001}`))
	if code != 200 {
		t.Fatalf("接入 = %d: %s", code, body)
	}
	var joined struct {
		Saved      bool  `json:"saved"`
		Joined     bool  `json:"joined"`
		ContactUID int64 `json:"contact_uid"`
	}
	if err := json.Unmarshal([]byte(body), &joined); err != nil {
		t.Fatal(err)
	}
	if !joined.Saved || !joined.Joined || joined.ContactUID != created.ContactUID {
		t.Fatalf("接入回执 = %+v（手填 UID 不应覆盖自动 UID）", joined)
	}
	onboarding, _ = t105GetStatus(t, ts, token)
	if onboarding.JoinedTotal != 1 || onboarding.VisibleTotal != 0 {
		t.Fatalf("接入后 joined/visible = %d/%d，期望 1/0", onboarding.JoinedTotal, onboarding.VisibleTotal)
	}
	joinStep := t105StepByID(onboarding.Steps, "join")
	if joinStep.State != "current" || !strings.Contains(joinStep.Display, "玩家可见") {
		t.Fatalf("仅接入时第 3 步 = %+v，期望 current 且区分玩家可见", joinStep)
	}
	ev := onboarding.Agents["t105a"]
	if ev.ContactVisible || ev.VisibleEvidence != "" {
		t.Fatalf("没有游戏端证据时不应标玩家可见: %+v", ev)
	}

	// 运行命令等待：能力/运行意图入队后，总览按角色给出待应用数量。
	if _, _, err := db.AIEnqueueRuntimeCommand(context.Background(), store.AIRuntimeEnqueueRequest{
		RequestID: "t105-pending-command", Source: store.AIRuntimeSourceAdmin,
		Kind:     store.AIRuntimeKindActorSetEnabled,
		AgentKey: "t105a", Payload: []byte(`{"enabled":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	onboarding, _ = t105GetStatus(t, ts, token)
	if onboarding.Agents["t105a"].PendingCommands != 1 || onboarding.PendingCommands != 1 {
		t.Fatalf("待应用命令数 = %+v/%d，期望 1/1", onboarding.Agents["t105a"], onboarding.PendingCommands)
	}

	// 造一条真实链路：玩家私聊入队 → AI 回复 → GS 投递邮件。
	now := float64(time.Now().Unix())
	res, err := db.SQL.Exec(`INSERT INTO ai_task(kind,scope,scope_key,uid,agent,contact_uid,source_kind,text,state,deadline_ts,created_ts)
		VALUES('dm','uid','9100001',9100001,'t105a',?,'human','你好','done',0,?)`, created.ContactUID, now)
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := res.LastInsertId()
	if _, err := db.SQL.Exec(`INSERT INTO ai_result(task_id,status,reply_text,delivered_ts,created_ts,contact_uid)
		VALUES(?,'done','你好，钓友',?,?,?)`, taskID, now, now, created.ContactUID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.Exec(`INSERT INTO mail(uid,sender,kind,text,created_ts,read)
		VALUES(9100001,?,0,'你好，钓友',?,0)`, created.ContactUID, now); err != nil {
		t.Fatal(err)
	}
	onboarding, all := t105GetStatus(t, ts, token)
	if onboarding.VisibleTotal != 1 || onboarding.DeliveredTotal != 1 {
		t.Fatalf("有私聊/投递后 visible/delivered = %d/%d，期望 1/1", onboarding.VisibleTotal, onboarding.DeliveredTotal)
	}
	joinStep = t105StepByID(onboarding.Steps, "join")
	helloStep := t105StepByID(onboarding.Steps, "hello")
	if joinStep.State != "done" || helloStep.State != "done" {
		t.Fatalf("有证据后第 3/4 步 = %s/%s，期望 done/done", joinStep.State, helloStep.State)
	}
	ev = onboarding.Agents["t105a"]
	if !ev.ContactVisible || ev.VisibleEvidence != "dm" {
		t.Fatalf("玩家可见证据 = %+v，期望 dm", ev)
	}
	// 页面证据只给状态与时间，不把玩家正文带进后台。
	blob, _ := json.Marshal(all)
	if strings.Contains(string(blob), "你好，钓友") {
		t.Fatalf("总览不应回显玩家/回复正文: %s", blob)
	}

	// 删除后同 key 恢复：复用原 UID，历史保留由 joined=0 + 原记录体现。
	if _, err := db.SQL.Exec(`UPDATE ai_agent SET deleted_ts=? WHERE key='t105a'`, now); err != nil {
		t.Fatal(err)
	}
	code, body = capabilityRequest(t, ts, token, "POST", "/api/ai/agent/create",
		`{"key":"t105a","display":"首句角色","role":"测试","prompt":"恢复"}`)
	if code != 200 {
		t.Fatalf("恢复 Agent = %d: %s", code, body)
	}
	var revived struct {
		ContactUID       int64 `json:"contact_uid"`
		Revived          bool  `json:"revived"`
		HistoryPreserved bool  `json:"history_preserved"`
	}
	if err := json.Unmarshal([]byte(body), &revived); err != nil {
		t.Fatal(err)
	}
	if !revived.Revived || !revived.HistoryPreserved || revived.ContactUID != created.ContactUID {
		t.Fatalf("恢复回执 = %+v，期望复用 UID %d", revived, created.ContactUID)
	}

	// 急停优先于缺 Key / 命令等待。
	if err := db.AISetSettings(context.Background(), map[string]string{"emergency_stop": "1"}); err != nil {
		t.Fatal(err)
	}
	onboarding, _ = t105GetStatus(t, ts, token)
	if onboarding.Status.Priority != "emergency" || !strings.Contains(onboarding.Status.Display, "急停") {
		t.Fatalf("急停后优先级 = %+v，期望 emergency", onboarding.Status)
	}
	if onboarding.Steps[0].State != "blocked" {
		t.Fatalf("急停时第 1 步 = %s，期望 blocked", onboarding.Steps[0].State)
	}
}

// ---- T-105 首句误判修复：失败提示邮件不等于首句完成 ----

// t105AgentView 是 /api/ai/status 顶层 agents[] 里页面实际消费的字段。
type t105AgentView struct {
	Key             string  `json:"key"`
	Joined          bool    `json:"joined"`
	ContactVisible  bool    `json:"contact_visible"`
	VisibleEvidence string  `json:"visible_evidence"`
	LastDMTS        float64 `json:"last_dm_ts"`
	LastReplyStatus string  `json:"last_reply_status"`
	LastReplyError  string  `json:"last_reply_error"`
	LastDeliveredTS float64 `json:"last_delivered_ts"`
	LastMailTS      float64 `json:"last_mail_ts"`
}

type t105StatusProbe struct {
	Onboarding t105Onboarding  `json:"onboarding"`
	Agents     []t105AgentView `json:"agents"`
	LastError  struct {
		Agent string  `json:"agent"`
		Error string  `json:"error"`
		TS    float64 `json:"ts"`
	} `json:"last_error"`
}

func t105ProbeStatus(t *testing.T, ts *httptest.Server, token string) t105StatusProbe {
	t.Helper()
	code, body := capabilityRequest(t, ts, token, "GET", "/api/ai/status", "")
	if code != 200 {
		t.Fatalf("status = %d: %s", code, body)
	}
	var probe t105StatusProbe
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("解析 status: %v: %s", err, body)
	}
	return probe
}

func t105ViewOf(views []t105AgentView, key string) t105AgentView {
	for _, view := range views {
		if view.Key == key {
			return view
		}
	}
	return t105AgentView{}
}

// TestT105FallbackMailEvidence 锁住 R-142 的首句完成口径：GS 会把失败提示也投进
// 个人消息，所以 ai_result 里会留下 status='fallback' + 正数 delivered_ts，并伴随
// 一封失败提示邮件。两者都不能把第 4 步标成完成；首句完成证据只能是同一角色私聊
// 任务里 status='done' 且 delivered_ts>0 的回复结果，且后续失败不撤销已完成的首句。
func TestT105FallbackMailEvidence(t *testing.T) {
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	token := signAdminToken(t, srv)

	// 已接入角色：新增时服务端分配 UID，接入只写管理侧事实。
	code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/agent/create",
		`{"key":"t105fb","display":"首句失败角色","role":"测试","prompt":"你是测试角色"}`)
	if code != 200 {
		t.Fatalf("新增 Agent = %d: %s", code, body)
	}
	var created struct {
		ContactUID int64 `json:"contact_uid"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil {
		t.Fatal(err)
	}
	if created.ContactUID == 0 {
		t.Fatalf("新增回执没有 contact_uid: %s", body)
	}
	if code, body = capabilityRequest(t, ts, token, "POST", "/api/ai/agent/join",
		`{"key":"t105fb","joined":true}`); code != 200 {
		t.Fatalf("接入 = %d: %s", code, body)
	}

	// 造真实链路留下的行：玩家私聊入队 → BR 写结果 → GS 投递到个人消息（写 mail）。
	const playerUID = int64(9100101)
	now := float64(time.Now().Unix())
	newDM := func(state string) int64 {
		t.Helper()
		res, err := db.SQL.Exec(`INSERT INTO ai_task(kind,scope,scope_key,uid,agent,contact_uid,source_kind,text,state,deadline_ts,created_ts)
			VALUES('dm','uid','9100101',?,'t105fb',?,'human','你好',?,0,?)`,
			playerUID, created.ContactUID, state, now)
		if err != nil {
			t.Fatalf("写 ai_task: %v", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// delivered 传 nil = 还没投递；正数 = GS 已投递；-1 = 不可投递终态。
	newResult := func(taskID int64, status, reply, errText string, delivered any) int64 {
		t.Helper()
		res, err := db.SQL.Exec(`INSERT INTO ai_result(task_id,status,reply_text,raw_usage,err,created_ts,delivered_ts,contact_uid)
			VALUES(?,?,?,'',?,?,?,?)`, taskID, status, reply, errText, now, delivered, created.ContactUID)
		if err != nil {
			t.Fatalf("写 ai_result: %v", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	newMail := func(text string) {
		t.Helper()
		if _, err := db.SQL.Exec(`INSERT INTO mail(uid,sender,kind,text,created_ts,read)
			VALUES(?,?,0,?,?,0)`, playerUID, created.ContactUID, text, now); err != nil {
			t.Fatalf("写 mail: %v", err)
		}
	}

	// 1) 失败提示：fallback 结果同样被 GS 投递（正数 delivered_ts），并留下失败提示邮件。
	//    这不等于「AI 回复了」，第 4 步必须仍未完成。
	newResult(newDM("done"), "fallback", "AI 暂时无法回复，请稍后再试。", "T-105 厂商超时", now+1)
	newMail("AI 暂时无法回复，请稍后再试。")
	probe := t105ProbeStatus(t, ts, token)
	hello := t105StepByID(probe.Onboarding.Steps, "hello")
	if hello.State == "done" {
		t.Fatalf("失败提示邮件 + fallback 结果把首句标成了完成: %+v", hello)
	}
	if hello.State != "current" {
		t.Fatalf("失败提示后第 4 步 = %+v，期望 current", hello)
	}
	if probe.Onboarding.DeliveredTotal != 0 {
		t.Fatalf("delivered_total = %d，期望 0（只有 status=done 且已投递的回复才算）", probe.Onboarding.DeliveredTotal)
	}
	if probe.Onboarding.VisibleTotal != 1 {
		t.Fatalf("visible_total = %d，期望 1（失败提示邮件仍是联系人可见证据）", probe.Onboarding.VisibleTotal)
	}
	view := t105ViewOf(probe.Agents, "t105fb")
	if !view.ContactVisible || view.VisibleEvidence != "dm" || view.LastMailTS <= 0 {
		t.Fatalf("联系人可见证据 = %+v，期望 dm 且失败提示邮件已落库", view)
	}
	if view.LastDeliveredTS != 0 {
		t.Fatalf("成功投递证据 = %v，期望 0（fallback 不算首句完成）", view.LastDeliveredTS)
	}
	if view.LastReplyStatus != "fallback" || !strings.Contains(view.LastReplyError, "厂商超时") {
		t.Fatalf("最近回复 = %q / %q，期望 fallback 且保留原因", view.LastReplyStatus, view.LastReplyError)
	}
	if got := probe.Onboarding.Agents["t105fb"]; got.LastReply.DeliveredTS != 0 || got.LastMailTS <= 0 {
		t.Fatalf("onboarding 角色证据 = %+v，期望无成功投递证据但有邮件时间", got)
	}

	// 2) 正常回复已生成、还没投递：首句仍未完成（完成以 GS 投递为准）。
	okID := newResult(newDM("done"), "done", "你好，钓友", "", nil)
	probe = t105ProbeStatus(t, ts, token)
	if hello = t105StepByID(probe.Onboarding.Steps, "hello"); hello.State == "done" {
		t.Fatalf("未投递的 done 结果把首句标成了完成: %+v", hello)
	}
	if probe.Onboarding.DeliveredTotal != 0 {
		t.Fatalf("未投递时 delivered_total = %d，期望 0", probe.Onboarding.DeliveredTotal)
	}

	// 3) 写入 GS 成功投递后的持久化证据：只留投递时间并清空 reply_text。
	deliveredTS := now + 5
	if _, err := db.SQL.Exec(`UPDATE ai_result SET delivered_ts=?, reply_text='' WHERE id=?`,
		deliveredTS, okID); err != nil {
		t.Fatalf("模拟 GS 投递: %v", err)
	}
	probe = t105ProbeStatus(t, ts, token)
	hello = t105StepByID(probe.Onboarding.Steps, "hello")
	if hello.State != "done" {
		t.Fatalf("GS 投递成功后第 4 步 = %+v，期望 done", hello)
	}
	if probe.Onboarding.DeliveredTotal != 1 {
		t.Fatalf("投递成功后 delivered_total = %d，期望 1", probe.Onboarding.DeliveredTotal)
	}
	view = t105ViewOf(probe.Agents, "t105fb")
	if view.LastDeliveredTS != deliveredTS {
		t.Fatalf("成功投递证据 = %v，期望 %v", view.LastDeliveredTS, deliveredTS)
	}
	if got := probe.Onboarding.Agents["t105fb"].LastReply; got.DeliveredTS != deliveredTS || got.Status != "done" {
		t.Fatalf("onboarding 首句证据 = %+v，期望 done 且投递时间 %v", got, deliveredTS)
	}

	// 4) 之后又失败一次：已成功投递的首句不撤销，最近失败仍可查。
	newResult(newDM("done"), "fallback", "AI 暂时无法回复，请稍后再试。", "T-105 上游 502", now+9)
	newMail("AI 暂时无法回复，请稍后再试。")
	probe = t105ProbeStatus(t, ts, token)
	hello = t105StepByID(probe.Onboarding.Steps, "hello")
	if hello.State != "done" {
		t.Fatalf("后续失败撤销了已投递的首句: %+v", hello)
	}
	if probe.Onboarding.DeliveredTotal != 1 {
		t.Fatalf("后续失败后 delivered_total = %d，期望仍为 1", probe.Onboarding.DeliveredTotal)
	}
	view = t105ViewOf(probe.Agents, "t105fb")
	if view.LastReplyStatus != "fallback" || !strings.Contains(view.LastReplyError, "502") {
		t.Fatalf("最近一次失败不可查: %+v", view)
	}
	if view.LastDeliveredTS != deliveredTS {
		t.Fatalf("成功投递证据被后续失败覆盖: %v，期望 %v", view.LastDeliveredTS, deliveredTS)
	}
	if probe.LastError.Agent != "t105fb" || !strings.Contains(probe.LastError.Error, "502") {
		t.Fatalf("last_error = %+v，期望保留最近失败", probe.LastError)
	}
	if display := probe.Onboarding.Agents["t105fb"].LastReply.Display; !strings.Contains(display, "失败") {
		t.Fatalf("最近回复展示 = %q，期望明确写失败而不是「已投递」", display)
	}
}

// ---- T-104 角色能力：目录、按修订保存、鉴权与旧表单兼容 ----

func capabilityServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, signAdminToken(t, srv)
}

func capabilityRequest(t *testing.T, ts *httptest.Server, token, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s: %v", path, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func TestAICapabilitiesHTTP(t *testing.T) {
	ts, token := capabilityServer(t)

	// 没有管理鉴权：读也不行。
	if code, _ := capabilityRequest(t, ts, "", "GET", "/api/ai/capabilities", ""); code != 401 {
		t.Fatalf("匿名读取状态码 = %d，期望 401", code)
	}
	if code, _ := capabilityRequest(t, ts, "", "POST", "/api/ai/capabilities",
		`{"agent_key":"companion","base_revision":0,"capabilities":["guide_help"]}`); code != 401 {
		t.Fatalf("匿名保存状态码 = %d，期望 401", code)
	}

	code, body := capabilityRequest(t, ts, token, "GET", "/api/ai/capabilities", "")
	if code != 200 {
		t.Fatalf("读取状态码 = %d: %s", code, body)
	}
	var view struct {
		Catalog []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"catalog"`
		Tools []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"tools"`
		Agents []struct {
			Key        string   `json:"key"`
			State      string   `json:"state"`
			Legacy     bool     `json:"legacy"`
			ReadTools  []string `json:"read_tools"`
			WriteTools []string `json:"write_tools"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("解析目录: %v", err)
	}
	if len(view.Catalog) != 11 || len(view.Tools) != 12 {
		t.Fatalf("目录 = %d 能力 / %d 工具", len(view.Catalog), len(view.Tools))
	}
	var companion *struct {
		Key        string   `json:"key"`
		State      string   `json:"state"`
		Legacy     bool     `json:"legacy"`
		ReadTools  []string `json:"read_tools"`
		WriteTools []string `json:"write_tools"`
	}
	for i := range view.Agents {
		if view.Agents[i].Key == "companion" {
			companion = &view.Agents[i]
		}
	}
	if companion == nil || !companion.Legacy || len(companion.ReadTools) != 1 ||
		companion.ReadTools[0] != "map_vibe" {
		t.Fatalf("旧快照读取 = %+v", companion)
	}

	// 保存：base 0 → 修订 1。
	code, body = capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
		`{"agent_key":"companion","base_revision":0,"capabilities":["guide_help","accept_target_suggest"]}`)
	if code != 200 {
		t.Fatalf("保存状态码 = %d: %s", code, body)
	}
	var saved struct {
		Revision   int64    `json:"revision"`
		ReadTools  []string `json:"read_tools"`
		WriteTools []string `json:"write_tools"`
		State      string   `json:"state"`
	}
	if err := json.Unmarshal([]byte(body), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 || strings.Join(saved.ReadTools, ",") != "map_vibe" || len(saved.WriteTools) != 1 ||
		saved.State != "configured" {
		t.Fatalf("保存回执 = %+v", saved)
	}

	// 过期 base → 409，不覆盖。
	if code, _ := capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
		`{"agent_key":"companion","base_revision":0,"capabilities":["guide_help"]}`); code != 409 {
		t.Fatalf("过期 base 状态码 = %d，期望 409", code)
	}
	// 未知能力 → 400。
	if code, _ := capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
		`{"agent_key":"companion","base_revision":1,"capabilities":["nope"]}`); code != 400 {
		t.Fatalf("未知能力状态码 = %d，期望 400", code)
	}
	// null 与缺记录无法区分 → 400；显式空要传 []。
	if code, _ := capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
		`{"agent_key":"companion","base_revision":1,"capabilities":null}`); code != 400 {
		t.Fatalf("null 能力状态码 = %d，期望 400", code)
	}
	// 不存在的角色 → 404。
	if code, _ := capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
		`{"agent_key":"nobody","base_revision":0,"capabilities":[]}`); code != 404 {
		t.Fatalf("不存在角色状态码 = %d，期望 404", code)
	}
	// 显式空：保存成功且工具全拒绝。
	code, body = capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
		`{"agent_key":"companion","base_revision":1,"capabilities":[]}`)
	if code != 200 {
		t.Fatalf("显式空状态码 = %d: %s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 2 || saved.State != "empty" || len(saved.ReadTools) != 0 || len(saved.WriteTools) != 0 {
		t.Fatalf("显式空回执 = %+v", saved)
	}
}

// 旧表单（/api/ai/agent/update 的 tools）只能写降级快照，
// 不能覆盖新版能力意图：新记录存在时，能力与工具投影保持新值。
func TestAICapabilityNotOverriddenByLegacyForm(t *testing.T) {
	ts, token := capabilityServer(t)
	code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
		`{"agent_key":"companion","base_revision":0,"capabilities":["guide_help","accept_target_suggest"]}`)
	if code != 200 {
		t.Fatalf("保存能力 = %d: %s", code, body)
	}
	code, body = capabilityRequest(t, ts, token, "POST", "/api/ai/agent/update",
		`{"key":"companion","display":"AI 伙伴","role":"陪聊","prompt":"旧表单","tools":["shop_price"]}`)
	if code != 200 {
		t.Fatalf("旧表单保存 = %d: %s", code, body)
	}
	code, body = capabilityRequest(t, ts, token, "GET", "/api/ai/capabilities", "")
	if code != 200 {
		t.Fatalf("读取能力 = %d", code)
	}
	var view struct {
		Agents []struct {
			Key        string   `json:"key"`
			Revision   int64    `json:"revision"`
			Legacy     bool     `json:"legacy"`
			ReadTools  []string `json:"read_tools"`
			WriteTools []string `json:"write_tools"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	for _, a := range view.Agents {
		if a.Key != "companion" {
			continue
		}
		if a.Revision != 1 || a.Legacy || strings.Join(a.ReadTools, ",") != "map_vibe" || len(a.WriteTools) != 1 {
			t.Fatalf("旧表单覆盖了新意图: %+v", a)
		}
		return
	}
	t.Fatal("响应里没有 companion")
}

// ---- T-104 返工：能力保存 → 既有 GS 应用路径 → 可用性/授权差量 ----

type t104CapSave struct {
	Revision      int64    `json:"revision"`
	Changed       bool     `json:"changed"`
	State         string   `json:"state"`
	Generation    int64    `json:"generation"`
	Availability  string   `json:"availability"`
	CommandID     int64    `json:"command_id"`
	RequestID     string   `json:"request_id"`
	CommandStatus string   `json:"command_status"`
	ReadTools     []string `json:"read_tools"`
	WriteTools    []string `json:"write_tools"`
	Runtime       struct {
		Desired      bool   `json:"desired"`
		Actual       bool   `json:"actual"`
		Availability string `json:"availability"`
		CommandID    int64  `json:"command_id"`
		Status       string `json:"status"`
	} `json:"runtime"`
	Commands []struct {
		ID       int64  `json:"id"`
		Status   string `json:"status"`
		Enabled  bool   `json:"enabled"`
		Revision int64  `json:"revision"`
	} `json:"commands"`
}

type t104CapAgent struct {
	Key          string   `json:"key"`
	Revision     int64    `json:"revision"`
	Generation   int64    `json:"generation"`
	State        string   `json:"state"`
	ActorEnabled bool     `json:"actor_enabled"`
	Availability string   `json:"availability"`
	ReadTools    []string `json:"read_tools"`
	WriteTools   []string `json:"write_tools"`
	Runtime      struct {
		Desired      bool   `json:"desired"`
		Actual       bool   `json:"actual"`
		Availability string `json:"availability"`
		CommandID    int64  `json:"command_id"`
		Status       string `json:"status"`
	} `json:"runtime"`
}

func t104StartGSRuntime(t *testing.T, db *store.DB) func() {
	t.Helper()
	gs := gameserver.NewServer(config.Game{Tunnel: config.Tunnel{PSK: "t104-runtime"}},
		logging.Discard(), nil)
	gs.SetDB(db)
	stop := make(chan struct{})
	gs.StartAIRuntimeCommands(db, stop)
	var once bool
	return func() {
		if !once {
			once = true
			close(stop)
		}
	}
}

func t104WaitCommand(t *testing.T, db *store.DB, id int64) *store.AIRuntimeCommand {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cmd, err := db.AIGetRuntimeCommand(context.Background(), id)
		if err == nil && cmd.Status != store.AIRuntimeStatusPending {
			return cmd
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("命令 %d 未在期限内终结", id)
	return nil
}

func t104SaveCaps(t *testing.T, ts *httptest.Server, token string, base int64,
	capsJSON string) (int, t104CapSave) {
	t.Helper()
	code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
		fmt.Sprintf(`{"agent_key":"companion","base_revision":%d,"capabilities":%s}`, base, capsJSON))
	var out t104CapSave
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析保存回执: %v: %s", err, body)
	}
	return code, out
}

func t104GetAgent(t *testing.T, ts *httptest.Server, token, key string) t104CapAgent {
	t.Helper()
	code, body := capabilityRequest(t, ts, token, "GET", "/api/ai/capabilities", "")
	if code != 200 {
		t.Fatalf("读取能力 = %d: %s", code, body)
	}
	var view struct {
		Agents []t104CapAgent `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	for _, a := range view.Agents {
		if a.Key == key {
			return a
		}
	}
	t.Fatalf("响应里没有 %s", key)
	return t104CapAgent{}
}

func t104ActorEnabled(t *testing.T, db *store.DB, key string) bool {
	t.Helper()
	var actor int
	if err := db.SQL.QueryRow(`SELECT actor_enabled FROM ai_agent WHERE key=?`, key).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	return actor != 0
}

// TestT104CapabilitySaveAppliesThroughGS 复验 R-138 的第二组缺口：
// 已鉴权 POST 必须按修订补派既有 actor_set_enabled，并由真实 GS 应用；
// 重复保存幂等、以当前修订补派、旧后台写入不改变新集合。
func TestT104CapabilitySaveAppliesThroughGS(t *testing.T) {
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	token := signAdminToken(t, srv)
	stopGS := t104StartGSRuntime(t, db)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			stopGS()
		}
	})

	// 1) 给「模拟关闭」的角色保存虚拟垂钓：修订 1，补派启用命令。
	code, saved := t104SaveCaps(t, ts, token, 0, `["virtual_fishing"]`)
	if code != 200 {
		t.Fatalf("保存虚拟垂钓 = %d: %+v", code, saved)
	}
	if saved.Revision != 1 || !saved.Changed || saved.State != "configured" ||
		saved.Availability != store.AICapabilityAvailabilityPending || saved.CommandID == 0 ||
		!saved.Runtime.Desired || saved.Runtime.Actual {
		t.Fatalf("首次保存 = %+v", saved)
	}
	cmd := t104WaitCommand(t, db, saved.CommandID)
	if cmd.Status != store.AIRuntimeStatusApplied || cmd.Kind != store.AIRuntimeKindActorSetEnabled ||
		cmd.ExpectedRevision != 1 {
		t.Fatalf("启用命令 = %+v", cmd)
	}
	if !t104ActorEnabled(t, db, "companion") {
		t.Fatal("GS 应用后 actor_enabled 仍为 0")
	}
	t.Logf("启用：revision=%d command_id=%d request_id=%s status=%s actor=%v",
		saved.Revision, cmd.ID, cmd.RequestID, cmd.Status, t104ActorEnabled(t, db, "companion"))

	// 回读：已应用证据（命令 ID/状态/运行态）与授权差量。
	agent := t104GetAgent(t, ts, token, "companion")
	if agent.Revision != 1 || agent.State != "configured" || !agent.ActorEnabled ||
		agent.Availability != store.AICapabilityAvailabilityAvailable ||
		agent.Runtime.Status != store.AIRuntimeStatusApplied || !agent.Runtime.Actual {
		t.Fatalf("回读 = %+v", agent)
	}

	// 2) 同一集合按当前修订重复保存：修订不跳、复用同一命令。
	code, again := t104SaveCaps(t, ts, token, 1, `["virtual_fishing"]`)
	if code != 200 || again.Revision != 1 || again.Changed ||
		again.CommandID != saved.CommandID ||
		again.Availability != store.AICapabilityAvailabilityAvailable {
		t.Fatalf("重复保存 = %d: %+v", code, again)
	}

	// 3) 版本冲突：旧 base 409，不覆盖。
	if code, _ := t104SaveCaps(t, ts, token, 0, `["guide_help"]`); code != 409 {
		t.Fatalf("版本冲突状态码 = %d，期望 409", code)
	}

	// 4) 保留攻略、撤销垂钓：禁用命令推进代次，攻略仍可用。
	code, revoked := t104SaveCaps(t, ts, token, 1, `["guide_help"]`)
	if code != 200 || revoked.Revision != 2 || !revoked.Changed ||
		revoked.Availability != store.AICapabilityAvailabilityPending || revoked.CommandID == 0 {
		t.Fatalf("撤销垂钓 = %d: %+v", code, revoked)
	}
	disable := t104WaitCommand(t, db, revoked.CommandID)
	if disable.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("禁用命令 = %+v", disable)
	}
	if t104ActorEnabled(t, db, "companion") {
		t.Fatal("撤销垂钓后 actor_enabled 仍为 1")
	}
	ctl, err := db.AIGetRuntimeControl(context.Background(), "companion")
	if err != nil || ctl.Generation != 2 {
		t.Fatalf("禁用后 generation = %+v/%v", ctl, err)
	}
	agent = t104GetAgent(t, ts, token, "companion")
	if agent.Revision != 2 || agent.Generation != 2 || agent.State != "configured" ||
		strings.Join(agent.ReadTools, ",") != "map_vibe" || agent.ActorEnabled ||
		agent.Availability != store.AICapabilityAvailabilityAvailable {
		t.Fatalf("撤销后回读 = %+v", agent)
	}
	t.Logf("撤销：revision=%d command_id=%d status=%s generation=%d actor=%v read_tools=%d availability=%s",
		revoked.Revision, disable.ID, disable.Status, ctl.Generation,
		t104ActorEnabled(t, db, "companion"), len(agent.ReadTools), agent.Availability)

	// 5) 旧后台写入：能改运行事实，但不能改能力修订，也不构成授权。
	if _, _, err := db.AIEnqueueRuntimeCommand(context.Background(), store.AIRuntimeEnqueueRequest{
		RequestID: "ad:t104-old-background", Source: store.AIRuntimeSourceAdmin,
		Kind: store.AIRuntimeKindActorSetEnabled, AgentKey: "companion",
		Payload: []byte(`{"enabled":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	oldCmd, err := db.AIGetRuntimeCommandByRequest(context.Background(), "ad:t104-old-background")
	if err != nil {
		t.Fatal(err)
	}
	t104WaitCommand(t, db, oldCmd.ID)
	if !t104ActorEnabled(t, db, "companion") {
		t.Fatal("旧后台写入未生效（测试前提不成立）")
	}
	agent = t104GetAgent(t, ts, token, "companion")
	if agent.Revision != 2 || len(agent.WriteTools) != 0 || strings.Join(agent.ReadTools, ",") != "map_vibe" ||
		agent.Availability != store.AICapabilityAvailabilityFailed {
		t.Fatalf("旧后台写入改变了新集合 = %+v", agent)
	}
	t.Logf("旧后台写入：revision=%d actor=%v availability=%s（能力记录未被改写）",
		agent.Revision, t104ActorEnabled(t, db, "companion"), agent.Availability)
	// 同集合再保存一次：按当前修订补派，运行态回到撤销后的意图。
	code, repaired := t104SaveCaps(t, ts, token, 2, `["guide_help"]`)
	if code != 200 || repaired.Revision != 2 || repaired.CommandID == 0 ||
		repaired.CommandID == revoked.CommandID {
		t.Fatalf("修复补派 = %d: %+v", code, repaired)
	}
	if got := t104WaitCommand(t, db, repaired.CommandID); got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("修复命令 = %+v", got)
	}
	if t104ActorEnabled(t, db, "companion") {
		t.Fatal("旧后台写入的启用效果未被新修订修正")
	}

	// 6) 失败后按当前版本补派：暂停 GS，制造一条 rejected 命令，再保存同一集合。
	stopGS()
	stopped = true
	time.Sleep(700 * time.Millisecond) // 等消费者退出，确保下一条命令不会被自动应用
	code, granted := t104SaveCaps(t, ts, token, 2, `["guide_help","virtual_fishing"]`)
	if code != 200 || granted.Revision != 3 || granted.CommandID == 0 {
		t.Fatalf("重新授权 = %d: %+v", code, granted)
	}
	if _, err := db.SQL.Exec(`UPDATE ai_runtime_command SET status='rejected', reason='simulated_failure'
		WHERE id=? AND status='pending'`, granted.CommandID); err != nil {
		t.Fatal(err)
	}
	code, retried := t104SaveCaps(t, ts, token, 3, `["guide_help","virtual_fishing"]`)
	if code != 200 || retried.Revision != 3 || retried.Changed ||
		retried.CommandID == granted.CommandID || retried.Availability != store.AICapabilityAvailabilityPending {
		t.Fatalf("失败后补派 = %d: %+v", code, retried)
	}
	retryCmd, err := db.AIGetRuntimeCommand(context.Background(), retried.CommandID)
	if err != nil || !strings.HasSuffix(retryCmd.RequestID, "-a2") ||
		retryCmd.ExpectedRevision != 3 {
		t.Fatalf("补派命令 = %+v/%v", retryCmd, err)
	}
	stopGS = t104StartGSRuntime(t, db)
	stopped = false
	if got := t104WaitCommand(t, db, retried.CommandID); got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("补派命令未应用 = %+v", got)
	}
	agent = t104GetAgent(t, ts, token, "companion")
	if agent.Revision != 3 || !agent.ActorEnabled ||
		agent.Availability != store.AICapabilityAvailabilityAvailable ||
		strings.Join(agent.ReadTools, ",") != "map_vibe" {
		t.Fatalf("补派后回读 = %+v", agent)
	}
	retryFinal, err := db.AIGetRuntimeCommand(context.Background(), retried.CommandID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("失败后补派：revision=%d rejected_id=%d retry_id=%d retry_request=%s status=%s actor=%v availability=%s",
		retried.Revision, granted.CommandID, retryFinal.ID, retryFinal.RequestID,
		retryFinal.Status, t104ActorEnabled(t, db, "companion"), agent.Availability)
}
