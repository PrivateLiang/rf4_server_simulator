package admin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
)

// TestSecuritySpecDefaultsSingleSource 后台声明的默认值必须来自 aiconfig；
// V2 起注册表里不再有 outbound_enabled（旧闸已撤）。
func TestSecuritySpecDefaultsSingleSource(t *testing.T) {
	want := map[string]string{
		aiconfig.KeyEmergencyStop: aiconfig.DefaultEmergencyStop,
		aiconfig.KeyActorEnabled:  aiconfig.DefaultActorEnabled,
		aiconfig.KeyCatchTalk:     aiconfig.DefaultCatchTalk,
		aiconfig.KeyMemoryInject:  aiconfig.DefaultMemoryInject,
	}
	for key, def := range want {
		spec, ok := settingByKey[key]
		if !ok {
			t.Fatalf("注册表缺少 %s", key)
		}
		if spec.Def != def {
			t.Errorf("%s 的 Def = %q，期望与 aiconfig 一致为 %q", key, spec.Def, def)
		}
	}
	if _, ok := settingByKey[aiconfig.KeyOutboundEnabled]; ok {
		t.Fatal("注册表不得再暴露 outbound_enabled")
	}
}

// TestAIStatusExposesEmergencyAndDestination /api/ai/status 给出急停判定、
// 实际去向、修订、凭据与复测状态；旧的 outbound 开关不再出现在 settings。
func TestAIStatusExposesEmergencyAndDestination(t *testing.T) {
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())

	rec := httptest.NewRecorder()
	srv.handleAIStatus(rec, httptest.NewRequest("GET", "/api/ai/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status 返回 %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Settings   map[string]string     `json:"settings"`
		Security   aiconfig.SecurityView `json:"security"`
		Revision   int64                 `json:"revision"`
		ConfigSt   string                `json:"config_state"`
		Credential map[string]any        `json:"credential"`
		Retest     map[string]any        `json:"retest"`
		Emergency  map[string]any        `json:"emergency_stop"`
		Saved      string                `json:"saved_display"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if _, ok := body.Settings[aiconfig.KeyOutboundEnabled]; ok {
		t.Fatal("settings 不得再暴露 outbound_enabled")
	}
	// 没有急停：放行；本地行为键仍按历史规则显示「未确认」。
	if !body.Security.Egress.Allowed {
		t.Fatal("没有急停时 security.egress.allowed 应为 true")
	}
	if _, ok := body.Security.Keys[aiconfig.KeyOutboundEnabled]; ok {
		t.Fatal("security.keys 不得再暴露 outbound_enabled")
	}
	for _, key := range []string{aiconfig.KeyActorEnabled, aiconfig.KeyCatchTalk, aiconfig.KeyMemoryInject} {
		st, ok := body.Security.Keys[key]
		if !ok {
			t.Fatalf("security.keys 缺少 %s", key)
		}
		if st.Display != "未确认" {
			t.Errorf("%s 显示 %q，期望 未确认", key, st.Display)
		}
	}
	if body.ConfigSt != "legacy" && body.ConfigSt != "ok" {
		t.Fatalf("config_state = %q", body.ConfigSt)
	}
	if body.Revision != 0 {
		t.Fatalf("旧安装修订 = %d，期望 0", body.Revision)
	}
	if body.Credential["state"] == nil || body.Retest["display"] != "待复测" {
		t.Fatalf("凭据/复测状态缺失: %+v / %+v", body.Credential, body.Retest)
	}
	if body.Saved == "" {
		t.Fatal("saved_display 缺失")
	}
	// 状态卡显示急停本身，不显示「外发开/关」；解除后有明确的可发起提示。
	if body.Emergency["stopped"] != false {
		t.Fatalf("初始 emergency_stop.stopped = %v，期望 false", body.Emergency["stopped"])
	}
	if disp, _ := body.Emergency["display"].(string); !strings.Contains(disp, "急停已解除") {
		t.Fatalf("初始急停显示 = %q，期望「急停已解除」", disp)
	}

	// 急停后同一接口给出「急停已开启」，并补「已发出的请求可能仍会返回」。
	ctx := context.Background()
	if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	srv.handleAIStatus(rec, httptest.NewRequest("GET", "/api/ai/status", nil))
	if !strings.Contains(rec.Body.String(), "急停已开启") {
		t.Fatalf("急停后状态未反映: %s", rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析急停响应失败: %v", err)
	}
	if body.Emergency["stopped"] != true {
		t.Fatalf("急停后 emergency_stop.stopped = %v，期望 true", body.Emergency["stopped"])
	}
	if disp, _ := body.Emergency["display"].(string); !strings.Contains(disp, "已发出的请求可能仍会返回") {
		t.Fatalf("急停显示缺少补充说明: %q", disp)
	}

	// 代次记录损坏：与「真实缺历史」区分，状态卡显示待确认。
	if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE ai_setting SET value='not-a-time' WHERE key=?`,
		aiconfig.KeyEmergencyEpochRecord); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	srv.handleAIStatus(rec, httptest.NewRequest("GET", "/api/ai/status", nil))
	if !strings.Contains(rec.Body.String(), "状态待确认") {
		t.Fatalf("代次记录损坏未显示待确认: %s", rec.Body.String())
	}
}

// TestWholeCardSaveDoesNotCreateOldGate 模拟前端 saveAIModule("conn")：
// 只有联网搜索/急停/端点/工具轮数四个键；旧 outbound 键被忽略，整卡保存不制造新闸。
func TestWholeCardSaveDoesNotCreateOldGate(t *testing.T) {
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())

	body := `{"outbound_enabled":true,"web_search_enabled":false,"emergency_stop":false,` +
		`"search_endpoint":"","max_tool_rounds":2}`
	req := httptest.NewRequest("POST", "/api/ai/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.handleAISettingsSave(rec, req)
	if rec.Code != 200 {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		OK      bool `json:"ok"`
		Updated int  `json:"updated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatalf("解析保存响应失败: %v", err)
	}
	if !saved.OK || saved.Updated != 4 {
		t.Fatalf("保存结果 = %+v，期望 ok=true updated=4（旧 outbound 键被忽略）", saved)
	}

	ctx := context.Background()
	if got := db.AISetting(ctx, aiconfig.KeyOutboundEnabled, ""); got != "1" {
		t.Fatalf("旧 outbound_enabled 不应被整卡保存改写，得到 %q", got)
	}
	egress := aiconfig.Egress(func(key string) string { return db.AISetting(ctx, key, "") })
	if !egress.Allowed {
		t.Fatalf("整卡保存后也必须放行（只留急停），得到 %+v", egress)
	}
}

// TestAdminUINoLocalSecurityDefault 前端不得再自己实现「缺省即开/关」，
// 急停与去向只消费后端字段。
func TestAdminUINoLocalSecurityDefault(t *testing.T) {
	sources := readAIFrontendSources(t)
	for _, issue := range noLocalSecurityDefaultIssues(sources) {
		t.Error(issue)
	}
	js := sources["ai/ai.js"]
	for _, required := range []string{
		"function applyAIStatus",
		"S.settingStates = sec.keys || {};",
		"function applyEmergencyStatus(d) {",
		"const em = (d || {}).emergency_stop;",
		"setText('aiStatusDestination', destDisplay(d.destination));",
	} {
		if !strings.Contains(js, required) {
			t.Errorf("前端未消费后端状态，缺少 %q", required)
		}
	}
}
