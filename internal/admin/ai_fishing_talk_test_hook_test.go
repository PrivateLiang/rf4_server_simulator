package admin

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
//
// 后台「发鱼测试」入口：
//   - 四种前置不满足各返回明确 4xx，且不写触发请求、不产生任何投递；
//   - 全满足时返回 202 + request_id，结果由 GET 同路径查询；
//   - 路由经 mux 注册并受 JWT 保护。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/gameserver"
	"rf4server/internal/store"
)

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027AdminSetup(t *testing.T, withConsent bool) (*Server, *store.DB) {
	t.Helper()
	srv, db := consentServer(t)
	ctx := context.Background()
	if err := db.AISetAgentJoined(ctx, "companion", true, 0); err != nil {
		t.Fatalf("接入 Agent: %v", err)
	}
	if err := db.AISetAgentFishingTalk(ctx, "companion", true); err != nil {
		t.Fatalf("打开上鱼报话: %v", err)
	}
	// T-108：钩子资格来自 T-104 的能力集合（播报自己渔获；按目录还需圆桌资格）。
	if _, err := db.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityJoinRoundtable, aiconfig.CapabilityBroadcastCatch}, nil); err != nil {
		t.Fatalf("保存能力: %v", err)
	}
	if err := db.AISetSettings(ctx, map[string]string{
		aiconfig.KeyOutboundEnabled: "1",
		aiconfig.KeyEmergencyStop:   "0",
	}); err != nil {
		t.Fatalf("写设置: %v", err)
	}
	if withConsent {
		saveProviderConfig(t, srv, "https://vendor.invalid/v1", "model-a")
		if rec := confirmConsent(t, srv, aiconfig.ConsentItemOutbound); rec.Code != 200 {
			t.Fatalf("确认外发授权失败 %d: %s", rec.Code, rec.Body.String())
		}
	}
	return srv, db
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027Post(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postAs(t, srv, srv.handleAIFishingTalkTest, "/api/ai/test/fishing-talk", body, "tester")
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027Get(t *testing.T, srv *Server, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/ai/test/fishing-talk?request_id="+requestID, nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxAdmin{}, "tester"))
	rec := httptest.NewRecorder()
	srv.handleAIFishingTalkTestGet(rec, req)
	return rec
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027NoRequestStored(t *testing.T, db *store.DB) {
	t.Helper()
	if _, ok, err := db.KVGet(context.Background(), gameserver.AIFishingTalkTestKVKey); ok || err == nil {
		t.Fatal("前置校验失败时不得写入触发请求")
	}
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_task`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("前置校验失败时不得产生 ai_task，实得 %d", n)
	}
}

// TestAIFishingTalkTestPreconditionsRejected 覆盖票 §4 的 DoD 3：
// 未开开关 / 急停 / 未授权 / Agent 不存在，各自明确 4xx 且零投递。
func TestAIFishingTalkTestPreconditionsRejected(t *testing.T) {
	cases := []struct {
		name    string
		consent bool
		mutate  func(t *testing.T, srv *Server, db *store.DB)
		key     string
		status  int
		want    string
	}{
		{
			name: "Agent 不存在", consent: true,
			key: "no-such-agent", status: http.StatusNotFound, want: "不存在",
		},
		{
			name: "Agent 未接入游戏", consent: true,
			mutate: func(t *testing.T, srv *Server, db *store.DB) {
				if err := db.AISetAgentJoined(context.Background(), "companion", false, 0); err != nil {
					t.Fatal(err)
				}
			},
			key: "companion", status: http.StatusConflict, want: "接入游戏",
		},
		{
			// T-108：旧角色级报话开关不再放行/拒绝；资格只看能力集合。
			name: "未获准播报自己渔获", consent: true,
			mutate: func(t *testing.T, srv *Server, db *store.DB) {
				ctx := context.Background()
				if _, err := db.AISaveCapabilities(ctx, "companion", 1,
					[]string{aiconfig.CapabilityJoinRoundtable}, nil); err != nil {
					t.Fatal(err)
				}
			},
			key: "companion", status: http.StatusConflict, want: "播报自己渔获",
		},
		{
			name: "能力记录损坏", consent: true,
			mutate: func(t *testing.T, srv *Server, db *store.DB) {
				if _, err := db.SQL.Exec(`UPDATE ai_setting SET value='{"schema_version":9}'
					WHERE key=?`, store.AICapabilityKey("companion")); err != nil {
					t.Fatal(err)
				}
			},
			key: "companion", status: http.StatusConflict, want: "播报自己渔获",
		},
		{
			name: "紧急停止", consent: true,
			mutate: func(t *testing.T, srv *Server, db *store.DB) {
				if err := db.AISetSettings(context.Background(), map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
					t.Fatal(err)
				}
			},
			key: "companion", status: http.StatusConflict, want: "紧急停止",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, db := t027AdminSetup(t, c.consent)
			if c.mutate != nil {
				c.mutate(t, srv, db)
			}
			rec := t027Post(t, srv, `{"agent_key":`+jsonString(c.key)+`}`)
			if rec.Code != c.status {
				t.Fatalf("状态码 = %d，期望 %d（body=%s）", rec.Code, c.status, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("解析错误体: %v", err)
			}
			msg, _ := body["error"].(string)
			if !strings.Contains(msg, c.want) {
				t.Fatalf("错误说明 = %q，期望包含 %q", msg, c.want)
			}
			t027NoRequestStored(t, db)
		})
	}
}

// TestAIFishingTalkTestLegacyConsentDoesNotBlock V2 只留急停：旧 outbound 开关与
// 四项 consent 缺失都不再挡住有效调用（T-102 修订）。
func TestAIFishingTalkTestLegacyConsentDoesNotBlock(t *testing.T) {
	srv, db := t027AdminSetup(t, false)
	rec := t027Post(t, srv, `{"agent_key":"companion"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("旧 consent 缺失不应阻止有效调用，状态码 = %d（body=%s）", rec.Code, rec.Body.String())
	}
	// 只写 pending 请求，不产生 ai_task。
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_task`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("触发请求不得产生 ai_task，实得 %d", n)
	}
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestAIFishingTalkTestAcceptedAndQuery 全满足时只写 pending 请求（不碰 ai_task），
// 结果可经同路径按 request_id 查询。
func TestAIFishingTalkTestAcceptedAndQuery(t *testing.T) {
	srv, db := t027AdminSetup(t, true)
	ctx := context.Background()

	rec := t027Post(t, srv, `{"agent_key":"companion"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202（body=%s）", rec.Code, rec.Body.String())
	}
	var accepted struct {
		OK        bool   `json:"ok"`
		Status    string `json:"status"`
		RequestID string `json:"request_id"`
		AgentKey  string `json:"agent_key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("解析 202: %v", err)
	}
	if !accepted.OK || accepted.Status != "pending" || accepted.RequestID == "" || accepted.AgentKey != "companion" {
		t.Fatalf("202 响应 = %+v", accepted)
	}
	if n := t027TaskCount(t, db); n != 0 {
		t.Fatalf("admin 只写触发请求，不得直接写 ai_task，实得 %d", n)
	}

	state, ok, err := gameserver.GetAIFishingTalkTest(ctx, db, accepted.RequestID)
	if err != nil || !ok {
		t.Fatalf("读触发请求: ok=%v err=%v", ok, err)
	}
	if state.Status != "pending" || state.AgentKey != "companion" {
		t.Fatalf("触发请求 = %+v", state)
	}

	// 模拟 gameserver 的消费结果写回（真实环境下由 GS 的 ai_task 入队后写）。
	state.Status = "enqueued"
	state.TaskID = 42
	state.Channel = "nav1_03"
	state.FishKey = "bream"
	state.FishName = "鲷鱼"
	state.Event = "star"
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.KVSet(ctx, gameserver.AIFishingTalkTestKVKey, string(raw)); err != nil {
		t.Fatal(err)
	}

	getRec := t027Get(t, srv, accepted.RequestID)
	if getRec.Code != http.StatusOK {
		t.Fatalf("查询状态码 = %d，期望 200（body=%s）", getRec.Code, getRec.Body.String())
	}
	var got gameserver.AIFishingTalkTestState
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解析查询结果: %v", err)
	}
	if got.Status != "enqueued" || got.TaskID != 42 || got.Channel != "nav1_03" || got.FishName != "鲷鱼" {
		t.Fatalf("查询结果 = %+v", got)
	}

	// 旧 request_id 与空 body 不静默成功。
	if rec := t027Get(t, srv, "t027-not-exist"); rec.Code != http.StatusNotFound {
		t.Fatalf("未知 request_id 状态码 = %d，期望 404", rec.Code)
	}
	if rec := t027Post(t, srv, `{"agent_key":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("空 agent_key 状态码 = %d，期望 400", rec.Code)
	}
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027TaskCount(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_task`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAIFishingTalkTestRouteRegistered 两条测试路由必须经 mux 注册且受 JWT 保护；
// 直接调 handler 的用例无法发现「忘了注册路由」。
func TestAIFishingTalkTestRouteRegistered(t *testing.T) {
	srv, _ := consentServer(t)
	cases := []struct {
		method, path string
		want         int
	}{
		{"POST", "/api/ai/test/fishing-talk", http.StatusUnauthorized},
		{"GET", "/api/ai/test/fishing-talk?request_id=x", http.StatusUnauthorized},
		{"POST", "/api/ai/test/not-registered", http.StatusNotFound},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s = %d，期望 %d", c.method, c.path, rec.Code, c.want)
		}
	}
}
