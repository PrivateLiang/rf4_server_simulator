package admin

// T-107 后台四维性格与评分草稿：管理鉴权、修订保存、评分草稿与「已保存/已生效」分离。
//
// 全部使用真实 SQLite、真实 handler 与本地 TLS 模拟厂商；不连接真实厂商、
// 不使用真实凭据，也不改任何产品配置之外的状态。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"rf4server/internal/aibridge"
	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// ---- 夹具 ----

type personalityWireAgent struct {
	AgentKey      string  `json:"agent_key"`
	Display       string  `json:"display"`
	Role          string  `json:"role"`
	Description   string  `json:"description"`
	SchemaVersion int     `json:"schema_version"`
	Revision      int64   `json:"revision"`
	Present       bool    `json:"present"`
	Configured    bool    `json:"configured"`
	NeedsRepair   bool    `json:"needs_repair"`
	Inactive      string  `json:"inactive"`
	SavedAt       float64 `json:"saved_at"`
	SaveDisplay   string  `json:"save_display"`
	Values        struct {
		SchemaVersion int   `json:"schema_version"`
		Revision      int64 `json:"revision"`
		Generation    int64 `json:"generation"`
		Sociability   int   `json:"sociability"`
		Exploration   int   `json:"exploration"`
		Persistence   int   `json:"persistence"`
		Regularity    int   `json:"regularity"`
	} `json:"values"`
	Derived struct {
		DwellFloorSeconds   float64 `json:"dwell_floor_seconds"`
		RestIntervalSeconds float64 `json:"rest_interval_seconds"`
	} `json:"derived"`
	Effect struct {
		Effective      bool    `json:"effective"`
		Reason         string  `json:"reason"`
		Display        string  `json:"display"`
		TasksSinceSave int     `json:"tasks_since_save"`
		LastTaskAt     float64 `json:"last_task_at"`
	} `json:"effect"`
}

type personalityWire struct {
	SchemaVersion int `json:"schema_version"`
	Dimensions    []struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		LowEnd  string `json:"low_end"`
		HighEnd string `json:"high_end"`
	} `json:"dimensions"`
	Bands []struct {
		Band  string `json:"band"`
		Label string `json:"label"`
		Min   int    `json:"min"`
		Max   int    `json:"max"`
	} `json:"bands"`
	Destination struct {
		Host    string `json:"host"`
		Model   string `json:"model"`
		Display string `json:"display"`
	} `json:"destination"`
	Emergency struct {
		Stopped bool   `json:"stopped"`
		Blocked bool   `json:"blocked"`
		Fault   string `json:"fault"`
		Display string `json:"display"`
	} `json:"emergency"`
	Agents []personalityWireAgent `json:"agents"`
}

type personalityScoreWire struct {
	RequestID     string `json:"request_id"`
	BaseRevision  int64  `json:"base_revision"`
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	Values        *struct {
		Sociability int `json:"sociability"`
		Exploration int `json:"exploration"`
		Persistence int `json:"persistence"`
		Regularity  int `json:"regularity"`
	} `json:"values"`
	Ranges *struct {
		Sociability [2]int `json:"sociability"`
		Exploration [2]int `json:"exploration"`
		Persistence [2]int `json:"persistence"`
		Regularity  [2]int `json:"regularity"`
	} `json:"ranges"`
	Reasons *struct {
		Sociability string `json:"sociability"`
		Exploration string `json:"exploration"`
		Persistence string `json:"persistence"`
		Regularity  string `json:"regularity"`
	} `json:"reasons"`
	Reason string `json:"reason"`
}

func personalityServer(t *testing.T) (*httptest.Server, string, *store.DB) {
	t.Helper()
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, signAdminToken(t, srv), db
}

func createPersonalityAgent(t *testing.T, db *store.DB, key, display, prompt string) {
	t.Helper()
	if _, _, err := db.AICreateAgent(context.Background(), store.AIAgent{
		Key: key, Display: display, Role: "钓友", Prompt: prompt,
		ThinkingEffort: "off", ReplyMode: "social",
	}, "[]"); err != nil {
		t.Fatalf("建 Agent %s: %v", key, err)
	}
}

func personalityGet(t *testing.T, ts *httptest.Server, token string) (personalityWire, string) {
	t.Helper()
	code, body := capabilityRequest(t, ts, token, "GET", "/api/ai/personality", "")
	if code != 200 {
		t.Fatalf("GET /api/ai/personality = %d: %s", code, body)
	}
	var wire personalityWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("解析性格视图: %v: %s", err, body)
	}
	return wire, body
}

func personalityAgent(t *testing.T, wire personalityWire, key string) personalityWireAgent {
	t.Helper()
	for _, agent := range wire.Agents {
		if agent.AgentKey == key {
			return agent
		}
	}
	t.Fatalf("视图里没有角色 %s（共 %d 个）", key, len(wire.Agents))
	return personalityWireAgent{}
}

func personalitySave(t *testing.T, ts *httptest.Server, token, body string) (int, string) {
	t.Helper()
	return capabilityRequest(t, ts, token, "POST", "/api/ai/personality", body)
}

func personalitySaveBody(key string, base int64, schema int, s, e, p, r int) string {
	return `{"agent_key":` + strconv.Quote(key) + `,"base_revision":` + strconv.FormatInt(base, 10) +
		`,"schema_version":` + strconv.Itoa(schema) + `,"values":{"sociability":` + strconv.Itoa(s) +
		`,"exploration":` + strconv.Itoa(e) + `,"persistence":` + strconv.Itoa(p) +
		`,"regularity":` + strconv.Itoa(r) + `}}`
}

func personalitySaveOK(t *testing.T, ts *httptest.Server, token, key string, base int64, s, e, p, r int) int64 {
	t.Helper()
	code, body := personalitySave(t, ts, token, personalitySaveBody(key, base,
		aiconfig.PersonalitySchemaVersion, s, e, p, r))
	if code != 200 {
		t.Fatalf("保存性格 %s = %d: %s", key, code, body)
	}
	var out struct {
		Revision int64 `json:"revision"`
		Changed  bool  `json:"changed"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("解析保存回执: %v: %s", err, body)
	}
	return out.Revision
}

// ---- 读取与保存 ----

// TestAIPersonalityHTTPAuthAndSave 覆盖鉴权、四维定义、保存修订、同值不跳修订、
// 冲突不覆盖与刷新后回读同一修订。
func TestAIPersonalityHTTPAuthAndSave(t *testing.T) {
	ts, token, db := personalityServer(t)
	createPersonalityAgent(t, db, "hutao", "胡桃", "喜欢安静钓鱼，愿意尝试新水域，做事有耐心。")

	// 没有管理鉴权：读、存、评分都不放行。
	for _, tc := range []struct {
		method, path, body string
	}{
		{"GET", "/api/ai/personality", ""},
		{"POST", "/api/ai/personality", personalitySaveBody("hutao", 0, 1, 50, 50, 50, 50)},
		{"POST", "/api/ai/personality/score",
			`{"request_id":"ps-1","agent_key":"hutao","base_revision":0,"description":"x",` +
				`"dimension_schema_version":1,"current_values":{"sociability":50,"exploration":50,"persistence":50,"regularity":50}}`},
	} {
		if code, _ := capabilityRequest(t, ts, "", tc.method, tc.path, tc.body); code != 401 {
			t.Errorf("匿名 %s %s = %d，期望 401", tc.method, tc.path, code)
		}
	}

	wire, body := personalityGet(t, ts, token)
	if wire.SchemaVersion != aiconfig.PersonalitySchemaVersion {
		t.Fatalf("schema_version = %d", wire.SchemaVersion)
	}
	if len(wire.Dimensions) != 4 {
		t.Fatalf("四维定义 = %d 条", len(wire.Dimensions))
	}
	if len(wire.Bands) != 3 || wire.Bands[0].Max != 32 || wire.Bands[1].Min != 33 ||
		wire.Bands[2].Min != 68 {
		t.Fatalf("分段边界不符: %+v", wire.Bands)
	}
	if strings.Contains(body, "api_key") || strings.Contains(body, "sk-") {
		t.Fatalf("性格视图不得回显凭据: %s", body)
	}
	before := personalityAgent(t, wire, "hutao")
	if before.Present || before.Revision != 0 || before.Values.Sociability != 50 {
		t.Fatalf("未保存时应为缺值中性: %+v", before)
	}
	if !strings.Contains(before.Description, "喜欢安静钓鱼") {
		t.Fatalf("应读出同一角色的现有描述: %q", before.Description)
	}
	if before.SaveDisplay != "尚未保存" || before.Effect.Effective || before.Effect.Reason != "not_saved" {
		t.Fatalf("未保存时的保存/生效文案不符: %+v / %+v", before.SaveDisplay, before.Effect)
	}

	rev := personalitySaveOK(t, ts, token, "hutao", 0, 60, 40, 70, 55)
	if rev != 1 {
		t.Fatalf("首次保存修订 = %d，期望 1", rev)
	}
	// 刷新后回读同一修订：值、描述、派生预览都来自同一次读取。
	wire, _ = personalityGet(t, ts, token)
	after := personalityAgent(t, wire, "hutao")
	if !after.Present || after.Revision != 1 {
		t.Fatalf("回读修订不符: %+v", after)
	}
	if after.Values.Sociability != 60 || after.Values.Exploration != 40 ||
		after.Values.Persistence != 70 || after.Values.Regularity != 55 {
		t.Fatalf("回读四维不符: %+v", after.Values)
	}
	if !strings.Contains(after.SaveDisplay, "已保存") || !strings.Contains(after.SaveDisplay, "修订 1") {
		t.Fatalf("保存后应显示已保存修订: %q", after.SaveDisplay)
	}
	if after.SavedAt <= 0 {
		t.Fatalf("保存后应有保存时间证据: %+v", after)
	}
	// 「已保存」不等于「已生效」：没有同版运行消费证据时不得显示生效。
	if after.Effect.Effective || after.Effect.Reason != store.AITaskEvidenceReasonNoExpression {
		t.Fatalf("无同版消费证据时不得标记已生效: %+v", after.Effect)
	}
	if !strings.Contains(after.Effect.Display, "等待同版运行证据") ||
		strings.Contains(after.Effect.Display, "已生效") {
		t.Fatalf("未取证时必须显示等待而不是已生效: %q", after.Effect.Display)
	}
	if after.Derived.DwellFloorSeconds <= 0 || after.Derived.RestIntervalSeconds <= 0 {
		t.Fatalf("派生预览缺失: %+v", after.Derived)
	}

	// 同值重复保存：不跳修订（与能力保存同一口径）。
	code, body := personalitySave(t, ts, token, personalitySaveBody("hutao", 1,
		aiconfig.PersonalitySchemaVersion, 60, 40, 70, 55))
	if code != 200 {
		t.Fatalf("同值保存 = %d: %s", code, body)
	}
	var same struct {
		Revision int64 `json:"revision"`
		Changed  bool  `json:"changed"`
	}
	if err := json.Unmarshal([]byte(body), &same); err != nil {
		t.Fatal(err)
	}
	if same.Revision != 1 || same.Changed {
		t.Fatalf("同值保存应保持修订 1 且 changed=false: %+v", same)
	}

	// 另一个管理员先保存：提交旧 base_revision 必须 409，库里保持先到的值。
	if got := personalitySaveOK(t, ts, token, "hutao", 1, 10, 20, 30, 40); got != 2 {
		t.Fatalf("第二次保存修订 = %d，期望 2", got)
	}
	code, body = personalitySave(t, ts, token, personalitySaveBody("hutao", 1,
		aiconfig.PersonalitySchemaVersion, 90, 90, 90, 90))
	if code != 409 || !strings.Contains(body, "其他管理员") {
		t.Fatalf("陈旧修订应 409 且提示重载: %d %s", code, body)
	}
	wire, _ = personalityGet(t, ts, token)
	final := personalityAgent(t, wire, "hutao")
	if final.Revision != 2 || final.Values.Sociability != 10 {
		t.Fatalf("冲突后不得覆盖先到的值: %+v", final)
	}
}

// TestAIPersonalitySaveValidation 非法取值/版本/额外键/缺字段全部拒绝且不写库。
func TestAIPersonalitySaveValidation(t *testing.T) {
	ts, token, db := personalityServer(t)
	createPersonalityAgent(t, db, "hutao", "胡桃", "话不多的钓友。")

	cases := []struct {
		name string
		body string
	}{
		{"超域取值", personalitySaveBody("hutao", 0, 1, 120, 50, 50, 50)},
		{"负值", personalitySaveBody("hutao", 0, 1, -1, 50, 50, 50)},
		{"未知版本", personalitySaveBody("hutao", 0, 2, 50, 50, 50, 50)},
		{"缺 schema_version", `{"agent_key":"hutao","base_revision":0,"values":{"sociability":50,"exploration":50,"persistence":50,"regularity":50}}`},
		{"缺 base_revision", `{"agent_key":"hutao","schema_version":1,"values":{"sociability":50,"exploration":50,"persistence":50,"regularity":50}}`},
		{"缺 values", `{"agent_key":"hutao","base_revision":0,"schema_version":1}`},
		{"额外键", `{"agent_key":"hutao","base_revision":0,"schema_version":1,"capabilities":["guide_help"],"values":{"sociability":50,"exploration":50,"persistence":50,"regularity":50}}`},
		{"四维缺一维", `{"agent_key":"hutao","base_revision":0,"schema_version":1,"values":{"sociability":50,"exploration":50,"persistence":50}}`},
		{"四维额外键", `{"agent_key":"hutao","base_revision":0,"schema_version":1,"values":{"sociability":50,"exploration":50,"persistence":50,"regularity":50,"extraversion":70}}`},
		{"负修订", personalitySaveBody("hutao", -1, 1, 50, 50, 50, 50)},
	}
	for _, tc := range cases {
		code, body := personalitySave(t, ts, token, tc.body)
		if code != 400 {
			t.Errorf("%s 应 400，得到 %d: %s", tc.name, code, body)
		}
	}
	// 一个都没落库。
	wire, _ := personalityGet(t, ts, token)
	if got := personalityAgent(t, wire, "hutao"); got.Present || got.Revision != 0 {
		t.Fatalf("非法保存不应写库: %+v", got)
	}
	// 不存在的角色：404，不新建配置。
	if code, _ := personalitySave(t, ts, token, personalitySaveBody("ghost", 0, 1, 50, 50, 50, 50)); code != 404 {
		t.Errorf("不存在角色保存 = %d，期望 404", code)
	}
}

// TestAIPersonalityTwoRolesIndependent 两个角色的修订与取值互不影响；
// 性格保存也不得改动能力集合（能力仍只由 T-104 入口负责）。
func TestAIPersonalityTwoRolesIndependent(t *testing.T) {
	ts, token, db := personalityServer(t)
	createPersonalityAgent(t, db, "hutao", "胡桃", "喜欢安静。")
	createPersonalityAgent(t, db, "klee", "可莉", "喜欢热闹。")

	// 先给两个角色各存一份能力（只攻略 vs 已授权事件），作为「性格不碰能力」的对照。
	for key, caps := range map[string]string{
		"hutao": `["shop_knowledge"]`,
		"klee":  `["fishing_knowledge","virtual_fishing"]`,
	} {
		code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
			`{"agent_key":"`+key+`","base_revision":0,"capabilities":`+caps+`}`)
		if code != 200 {
			t.Fatalf("保存能力 %s = %d: %s", key, code, body)
		}
	}
	_, capsBefore := capabilityRequest(t, ts, token, "GET", "/api/ai/capabilities", "")

	if rev := personalitySaveOK(t, ts, token, "hutao", 0, 80, 20, 60, 40); rev != 1 {
		t.Fatalf("hutao 修订 = %d", rev)
	}
	if rev := personalitySaveOK(t, ts, token, "klee", 0, 10, 90, 30, 70); rev != 1 {
		t.Fatalf("klee 修订 = %d", rev)
	}
	if rev := personalitySaveOK(t, ts, token, "hutao", 1, 20, 30, 40, 50); rev != 2 {
		t.Fatalf("hutao 第二版修订 = %d", rev)
	}
	wire, _ := personalityGet(t, ts, token)
	hutao := personalityAgent(t, wire, "hutao")
	klee := personalityAgent(t, wire, "klee")
	if hutao.Revision != 2 || klee.Revision != 1 {
		t.Fatalf("两角色修订串了: hutao=%d klee=%d", hutao.Revision, klee.Revision)
	}
	if hutao.Values.Sociability != 20 || klee.Values.Sociability != 10 {
		t.Fatalf("两角色取值串了: %+v / %+v", hutao.Values, klee.Values)
	}
	if !strings.Contains(klee.Description, "热闹") || !strings.Contains(hutao.Description, "安静") {
		t.Fatalf("描述串了: %q / %q", hutao.Description, klee.Description)
	}
	// 能力集合逐字节不变：性格只写自己的 ai_setting 键。
	_, capsAfter := capabilityRequest(t, ts, token, "GET", "/api/ai/capabilities", "")
	if capsBefore != capsAfter {
		t.Fatalf("性格保存改动了能力集合：\n前 %s\n后 %s", capsBefore, capsAfter)
	}
}

// ---- 评分草稿 ----

func withPersonalityScoreTransport(t *testing.T, base http.RoundTripper) {
	t.Helper()
	old := newAIPersonalityScoreProvider
	newAIPersonalityScoreProvider = func(spec aibridge.ProviderSpec, timeout time.Duration) (*aibridge.OpenAICompatProvider, error) {
		return aibridge.NewProviderWithTransport(spec, timeout, base)
	}
	t.Cleanup(func() { newAIPersonalityScoreProvider = old })
}

func personalityScoreFixture(t *testing.T, reply string) (*httptest.Server, string, *store.DB, *aiTestVendor) {
	t.Helper()
	envelope := `{"choices":[{"message":{"role":"assistant","content":` + strconv.Quote(reply) + `}}]}`
	vendor := newAITestVendor(t, 200, envelope)
	srv, db := newAITestFixture(t, vendor.srv.URL)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	withPersonalityScoreTransport(t, vendor.srv.Client().Transport)
	return ts, signAdminToken(t, srv), db, vendor
}

const personalityScoreReplyOK = `{"status":"ok",` +
	`"values":{"sociability":60,"exploration":40,"persistence":70,"regularity":55},` +
	`"ranges":{"sociability":[45,70],"exploration":[30,55],"persistence":[55,80],"regularity":[40,70]},` +
	`"reasons":{"sociability":"描述里爱主动约人","exploration":"愿意尝试新水域",` +
	`"persistence":"做事有耐心","regularity":"提到固定作息"}}`

func personalityScoreBody(key string, base int64, schema int, description string, s, e, p, r int) string {
	return `{"request_id":"ps-test-1","agent_key":` + strconv.Quote(key) +
		`,"base_revision":` + strconv.FormatInt(base, 10) +
		`,"description":` + strconv.Quote(description) +
		`,"dimension_schema_version":` + strconv.Itoa(schema) +
		`,"current_values":{"sociability":` + strconv.Itoa(s) + `,"exploration":` + strconv.Itoa(e) +
		`,"persistence":` + strconv.Itoa(p) + `,"regularity":` + strconv.Itoa(r) + `}}`
}

// TestAIPersonalityScoreDraftOnly 合法输出只回草稿：不回显描述、不落库、不改修订。
func TestAIPersonalityScoreDraftOnly(t *testing.T) {
	ts, token, db, vendor := personalityScoreFixture(t, personalityScoreReplyOK)
	const prompt = "喜欢安静钓鱼，愿意尝试新水域，做事有耐心。"
	createPersonalityAgent(t, db, "hutao", "胡桃", prompt)
	personalitySaveOK(t, ts, token, "hutao", 0, 50, 50, 50, 50)

	code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/personality/score",
		personalityScoreBody("hutao", 1, aiconfig.PersonalitySchemaVersion, prompt, 50, 50, 50, 50))
	if code != 200 {
		t.Fatalf("评分 = %d: %s", code, body)
	}
	var got personalityScoreWire
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("解析评分回执: %v: %s", err, body)
	}
	if got.RequestID != "ps-test-1" || got.BaseRevision != 1 ||
		got.SchemaVersion != aiconfig.PersonalitySchemaVersion || got.Status != "ok" {
		t.Fatalf("回执未按请求回填: %+v", got)
	}
	if got.Values == nil || got.Ranges == nil || got.Reasons == nil {
		t.Fatalf("ok 必须带 values/ranges/reasons: %+v", got)
	}
	if got.Values.Sociability != 60 || got.Ranges.Sociability != [2]int{45, 70} ||
		got.Reasons.Persistence != "做事有耐心" {
		t.Fatalf("建议未按模型输出回填: %+v", got)
	}
	// 票面形状：ok 只允许这七个键，不多回 agent_key/description/current_values。
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"request_id": true, "base_revision": true, "schema_version": true,
		"status": true, "values": true, "ranges": true, "reasons": true}
	for key := range raw {
		if !want[key] {
			t.Errorf("评分回执多出键 %q", key)
		}
	}
	for key := range want {
		if _, ok := raw[key]; !ok {
			t.Errorf("评分回执缺键 %q", key)
		}
	}
	if strings.Contains(body, prompt) {
		t.Error("评分回执不应回显角色描述")
	}
	// 模型确实收到描述与当前四维；且只调用一次。
	hits, auth, vendorBody := vendor.snapshot()
	if hits != 1 {
		t.Fatalf("厂商调用 %d 次，期望 1 次", hits)
	}
	if auth != "Bearer "+aiTestSavedKey {
		t.Fatalf("厂商鉴权头 = %q", auth)
	}
	for _, wantText := range []string{prompt, "交往意愿 50", "只输出一个 JSON 对象"} {
		if !strings.Contains(vendorBody, wantText) {
			t.Errorf("厂商请求缺 %q", wantText)
		}
	}
	// 评分不落库：修订、值、描述都不变。
	wire, _ := personalityGet(t, ts, token)
	view := personalityAgent(t, wire, "hutao")
	if view.Revision != 1 || view.Values.Sociability != 50 || view.Description != prompt {
		t.Fatalf("评分不应改配置: %+v", view)
	}
}

// TestAIPersonalityScoreGuards 描述变更、修订过期、未知版本、额外键、急停都不发请求。
func TestAIPersonalityScoreGuards(t *testing.T) {
	ts, token, db, vendor := personalityScoreFixture(t, personalityScoreReplyOK)
	const prompt = "喜欢安静钓鱼。"
	createPersonalityAgent(t, db, "hutao", "胡桃", prompt)
	personalitySaveOK(t, ts, token, "hutao", 0, 50, 50, 50, 50)

	type tc struct {
		name   string
		body   string
		code   int
		expect string
	}
	cases := []tc{
		{"描述已改", personalityScoreBody("hutao", 1, aiconfig.PersonalitySchemaVersion, "旧描述", 50, 50, 50, 50),
			409, "描述已改变"},
		{"修订过期", personalityScoreBody("hutao", 0, aiconfig.PersonalitySchemaVersion, prompt, 50, 50, 50, 50),
			409, "已更新"},
		{"未知维度版本", personalityScoreBody("hutao", 1, 2, prompt, 50, 50, 50, 50),
			400, "dimension_schema_version"},
		{"当前值超域", personalityScoreBody("hutao", 1, aiconfig.PersonalitySchemaVersion, prompt, 120, 50, 50, 50),
			400, "不合法"},
		{"缺 request_id", `{"agent_key":"hutao","base_revision":1,"description":` + strconv.Quote(prompt) +
			`,"dimension_schema_version":1,"current_values":{"sociability":50,"exploration":50,"persistence":50,"regularity":50}}`,
			400, "request_id"},
		{"额外键", `{"request_id":"ps-2","agent_key":"hutao","base_revision":1,"description":` +
			strconv.Quote(prompt) + `,"dimension_schema_version":1,"capabilities":["guide_help"],` +
			`"current_values":{"sociability":50,"exploration":50,"persistence":50,"regularity":50}}`,
			400, "请求格式错误"},
		{"描述为空", personalityScoreBody("hutao", 1, aiconfig.PersonalitySchemaVersion, "   ", 50, 50, 50, 50),
			400, "描述为空"},
	}
	for _, c := range cases {
		code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/personality/score", c.body)
		if code != c.code {
			t.Errorf("%s = %d，期望 %d: %s", c.name, code, c.code, body)
		}
		if !strings.Contains(body, c.expect) {
			t.Errorf("%s 回执缺 %q: %s", c.name, c.expect, body)
		}
	}
	// 不存在的角色：404。
	if code, _ := capabilityRequest(t, ts, token, "POST", "/api/ai/personality/score",
		personalityScoreBody("ghost", 0, aiconfig.PersonalitySchemaVersion, prompt, 50, 50, 50, 50)); code != 404 {
		t.Errorf("不存在角色评分 = %d，期望 404", code)
	}
	// 上述全部拒绝都不得发外发请求。
	if hits, _, _ := vendor.snapshot(); hits != 0 {
		t.Fatalf("被拒绝的评分仍调用了厂商 %d 次", hits)
	}

	// 急停：拒绝评分、不发请求、给出既有急停文案；解除后可评分。
	if err := db.AISetSettings(context.Background(), map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/personality/score",
		personalityScoreBody("hutao", 1, aiconfig.PersonalitySchemaVersion, prompt, 50, 50, 50, 50))
	if code != 409 || !strings.Contains(body, "急停") {
		t.Fatalf("急停时应 409 且带急停文案: %d %s", code, body)
	}
	if hits, _, _ := vendor.snapshot(); hits != 0 {
		t.Fatalf("急停时仍调用了厂商 %d 次", hits)
	}
	wire, _ := personalityGet(t, ts, token)
	if !wire.Emergency.Stopped || !wire.Emergency.Blocked {
		t.Fatalf("急停状态未投影到性格视图: %+v", wire.Emergency)
	}
	if err := db.AISetSettings(context.Background(), map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	if code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/personality/score",
		personalityScoreBody("hutao", 1, aiconfig.PersonalitySchemaVersion, prompt, 50, 50, 50, 50)); code != 200 {
		t.Fatalf("解除急停后应可评分: %d %s", code, body)
	}
	if hits, _, _ := vendor.snapshot(); hits != 1 {
		t.Fatalf("解除急停后厂商调用 %d 次，期望 1 次", hits)
	}
}

// TestAIPersonalityScoreIllegalAndUnassessable 非法输出拒绝且不改草稿；无法评估只回固定文案。
func TestAIPersonalityScoreIllegalAndUnassessable(t *testing.T) {
	const prompt = "喜欢安静钓鱼。"
	for name, tc := range map[string]struct {
		reply string
		code  int
		check func(t *testing.T, body string, got personalityScoreWire)
	}{
		"自然语言": {reply: "这个角色交往意愿偏高。", code: 502, check: func(t *testing.T, body string, _ personalityScoreWire) {
			if !strings.Contains(body, "不符合约定") {
				t.Errorf("应提示输出不合约: %s", body)
			}
		}},
		"额外安全键": {reply: `{"status":"ok","values":{"sociability":60,"exploration":40,"persistence":70,"regularity":55},"ranges":{"sociability":[45,70],"exploration":[30,55],"persistence":[55,80],"regularity":[40,70]},"reasons":{"sociability":"a","exploration":"b","persistence":"c","regularity":"d"},"emergency_stop":1}`,
			code: 502},
		"区间不含建议值": {reply: `{"status":"ok","values":{"sociability":90,"exploration":40,"persistence":70,"regularity":55},"ranges":{"sociability":[45,70],"exploration":[30,55],"persistence":[55,80],"regularity":[40,70]},"reasons":{"sociability":"a","exploration":"b","persistence":"c","regularity":"d"}}`,
			code: 502},
		"无法评估": {reply: `{"status":"unassessable"}`, code: 200, check: func(t *testing.T, body string, got personalityScoreWire) {
			if got.Status != aiconfig.PersonalityScoreStatusUnassessable {
				t.Fatalf("status = %q", got.Status)
			}
			if got.Reason != aiconfig.PersonalityScoreUnassessableText {
				t.Fatalf("固定文案 = %q", got.Reason)
			}
			if got.Values != nil || got.Ranges != nil || got.Reasons != nil {
				t.Fatalf("无法评估不得带 values/ranges/reasons: %+v", got)
			}
			if strings.Contains(body, "values") || strings.Contains(body, "ranges") || strings.Contains(body, "reasons") {
				t.Fatalf("无法评估回执不应出现 values/ranges/reasons 键: %s", body)
			}
		}},
		"无法评估却带值": {reply: `{"status":"unassessable","values":{"sociability":50,"exploration":50,"persistence":50,"regularity":50}}`, code: 502},
	} {
		t.Run(name, func(t *testing.T) {
			ts, token, db, _ := personalityScoreFixture(t, tc.reply)
			createPersonalityAgent(t, db, "hutao", "胡桃", prompt)
			personalitySaveOK(t, ts, token, "hutao", 0, 50, 50, 50, 50)
			code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/personality/score",
				personalityScoreBody("hutao", 1, aiconfig.PersonalitySchemaVersion, prompt, 50, 50, 50, 50))
			if code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d: %s", code, tc.code, body)
			}
			var got personalityScoreWire
			_ = json.Unmarshal([]byte(body), &got)
			if tc.check != nil {
				tc.check(t, body, got)
			}
			// 无论合法与否，评分都不改配置。
			wire, _ := personalityGet(t, ts, token)
			view := personalityAgent(t, wire, "hutao")
			if view.Revision != 1 || view.Values.Sociability != 50 {
				t.Fatalf("评分改动了配置: %+v", view)
			}
		})
	}
}

// ---- 生效证据与错误映射 ----

// TestAIPersonalityEvidenceNoFakeEffective 保存后即使有任务结束，也不把「已运行」
// 说成「本修订已生效」：完成数只是诊断，生效必须来自同版 task/result 证据。
func TestAIPersonalityEvidenceNoFakeEffective(t *testing.T) {
	ts, token, db := personalityServer(t)
	createPersonalityAgent(t, db, "hutao", "胡桃", "话不多。")
	personalitySaveOK(t, ts, token, "hutao", 0, 60, 40, 70, 55)

	ctx := context.Background()
	res, err := db.SQL.ExecContext(ctx, `INSERT INTO ai_task(kind,agent,uid,created_ts) VALUES('dm','hutao',1,?)`,
		float64(time.Now().Unix()))
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := res.LastInsertId()
	if _, err := db.SQL.ExecContext(ctx,
		`INSERT INTO ai_result(task_id,status,reply_text,created_ts) VALUES(?, 'done', '收到', ?)`,
		taskID, float64(time.Now().Unix())+1); err != nil {
		t.Fatal(err)
	}

	wire, _ := personalityGet(t, ts, token)
	view := personalityAgent(t, wire, "hutao")
	if view.Effect.TasksSinceSave < 1 {
		t.Fatalf("应统计到保存后结束的任务: %+v", view.Effect)
	}
	if view.Effect.Effective || view.Effect.Reason != store.AITaskEvidenceReasonNoExpression {
		t.Fatalf("完成数不能当成同版证据: %+v", view.Effect)
	}
	if !strings.Contains(view.Effect.Display, "等待同版运行证据") {
		t.Fatalf("生效说明必须写明等待同版证据: %q", view.Effect.Display)
	}
	if strings.Contains(view.Effect.Display, "已生效") {
		t.Fatalf("生效说明不得出现「已生效」: %q", view.Effect.Display)
	}
	if view.Effect.LastTaskAt <= 0 {
		t.Fatalf("应记录最近任务时间: %+v", view.Effect)
	}
}

// TestAIPersonalityScoreErrorMapping 把评分失败的各类原因映射成页面可读状态码，
// 且不把厂商地址/密钥打进回执。
func TestAIPersonalityScoreErrorMapping(t *testing.T) {
	srv := NewServer(config.Admin{JWTSecret: "test"}, nil, logging.Discard(), nil)
	spec := aibridge.ProviderSpec{Name: "sim", Protocol: "openai_chat",
		BaseURL: "https://vendor.example", ModelID: "m1", APIKey: "sk-secret-0123456789"}

	cases := []struct {
		name string
		err  error
		code int
		want string
	}{
		{"超时", context.DeadlineExceeded, 504, "timeout"},
		{"急停", &aibridge.EgressDeniedError{State: aiconfig.StateEmergency, Display: "关（紧急停止）"}, 409, "emergency"},
		{"配置过期", &aibridge.StaleConfigError{Want: 1, Have: 2}, 409, "stale_config"},
		{"输出不合约", aibridge.ErrPersonalityScoreReply, 502, "illegal_output"},
		{"调用失败", errors.New("dial tcp 10.0.0.1:443: connection refused"), 502, "call_failed"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		srv.writePersonalityScoreError(rec, c.err, spec)
		if rec.Code != c.code {
			t.Errorf("%s 状态码 = %d，期望 %d: %s", c.name, rec.Code, c.code, rec.Body.String())
		}
		var body struct {
			Error string `json:"error"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 回执不是 JSON: %s", c.name, rec.Body.String())
		}
		if body.State != c.want {
			t.Errorf("%s state = %q，期望 %q", c.name, body.State, c.want)
		}
		if strings.Contains(body.Error, "sk-secret") || strings.Contains(body.Error, "vendor.example") {
			t.Errorf("%s 回执泄露敏感内容: %s", c.name, body.Error)
		}
	}
}

// ---- 前端静态契约 ----

// jsSectionBody 截取两段锚点之间的源码，用于只检查本票新增的性格区。
func jsSectionBody(t *testing.T, src, start, end string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("找不到段落起点 %q", start)
	}
	rest := src[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("找不到段落终点 %q", end)
	}
	return rest[:j]
}

// TestAIPersonalityFrontendContract 固化页面边界：四维与评分只走本票路由；
// 键盘可调、逐项/全部撤销、三选一切换、已保存/已生效分离；不重定义维度或能力。
func TestAIPersonalityFrontendContract(t *testing.T) {
	sources := readAIFrontendSources(t)
	fragment := sources["ai/index.html"]
	js := sources["ai/ai.js"]
	css := sources["ai/ai.css"]
	section := jsSectionBody(t, js, "// ---------- 四维性格与评分草稿（T-107） ----------",
		"// ---------- 路由注册 ----------")

	for _, want := range []string{
		"/api/ai/personality", "/api/ai/personality/score",
		"base_revision", "dimension_schema_version", "current_values", "request_id",
		"'data-action': 'suggest-personality'", "'data-action': 'save-personality'",
		"undo-personality-dim", "undo-personality-all",
		"personality-switch-save", "personality-switch-discard", "personality-switch-cancel",
		"type: 'range'", "type: 'number'",
		"已保存", "effect.effective",
		"描述已改变", "重新载入",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("ai.js 缺少 %q", want)
		}
	}

	// 「已生效」在性格区只允许出现在 effect.effective 为真的分支里。
	effectBody := jsFunctionBody(t, section, "function renderPersonalityEffect(view) {")
	if !strings.Contains(effectBody, "if (effect.effective)") ||
		!strings.Contains(effectBody, "已生效") {
		t.Fatalf("已生效必须由服务端证据控制: %s", effectBody)
	}
	if strings.Index(effectBody, "已生效") < strings.Index(effectBody, "if (effect.effective)") {
		t.Fatalf("「已生效」出现在证据判断之前: %s", effectBody)
	}
	// 生效分支必须渲染服务端文案：被消费的修订号与真实 task/result 只在服务端拼。
	if !strings.Contains(effectBody, "effect.display") {
		t.Fatalf("生效分支必须显示服务端 effect.display（含已消费修订号）: %s", effectBody)
	}
	rest := strings.Replace(section, effectBody, "", 1)
	if strings.Contains(rest, "已生效") {
		t.Errorf("性格区「已生效」只允许在证据分支出现")
	}

	// P6：T-108 已确认停读的旧概率/播报键不得再出现在页面控件、渲染或提交里。
	// 逐键读取与扰动证据见回执；这里固化页面边界，避免把无入口的旧开关又画回来。
	for _, banned := range []string{
		"ai_fishing_talk_enabled", "ai_fishing_talk_super_chance", "ai_fishing_talk_star_chance",
		"ai_fishing_talk_near_chance", "ai_fishing_talk_miss_chance", "ai_fishing_roundtable_chance",
		"roundtable_enabled", "roundtable_on_speech_chance", "roundtable_chime_chance",
		"ai_catch_star_chance", "ai_catch_super_chance", "ai_catch_roundtable_chance",
		"ai_catch_talk", "ai_actor_switch_chance", "ai_actor_rest_chance", "上鱼播报",
	} {
		if strings.Contains(js, banned) {
			t.Errorf("ai.js 仍引用已停用旧键/旧卡片 %q", banned)
		}
	}
	// 硬限、只读提示与 T-105 首句流程保留。
	for _, want := range []string{
		"'ai_fishing_enabled'", "'ai_fishing_hourly_limit'", "软目标不能大于硬上限",
		"ai_actor_enabled", "服务端运行开关：已关闭（只读）",
		"first_sentence", "复制首句",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("ai.js 丢失保留项 %q", want)
		}
	}
	// 旧概率键在注册表里仍可读写（Hidden），但页面不得再为它们建输入框。
	if !strings.Contains(js, "specsFor('fishing', opts.group)") ||
		!strings.Contains(js, "if (!spec) continue;") {
		t.Error("渔获卡片必须只渲染服务端下发的显式键")
	}

	// 说明卡写清四步，且不再出现 T-107 接线占位字样。
	for _, want := range []string{"①", "②", "③", "④", "写清角色描述", "保存性格"} {
		if !strings.Contains(section, want) {
			t.Errorf("初次说明缺少 %q", want)
		}
	}
	noteBody := jsFunctionBody(t, section, "function ensurePersonalityMount() {")
	for _, banned := range []string{"T-107", "接线点", "接线完成"} {
		if strings.Contains(noteBody, banned) {
			t.Errorf("性格说明仍含开发占位 %q", banned)
		}
	}
	for _, want := range []string{".ai-note", ".section-head small"} {
		if !strings.Contains(noteBody, want) {
			t.Errorf("说明替换缺少 %q", want)
		}
	}

	// 不重定义维度：页面不写死四维 ID、分段阈值或标签。
	for _, id := range []string{"sociability", "exploration", "persistence", "regularity"} {
		if strings.Contains(section, id) {
			t.Errorf("ai.js 不应另写一份维度 ID: %q", id)
		}
	}
	if !strings.Contains(section, "S.personality.bands") || !strings.Contains(section, "personalityBandOf") {
		t.Error("分段必须来自服务端 bands")
	}
	if !strings.Contains(section, "S.personality.dimensions") {
		t.Error("维度顺序必须来自服务端定义")
	}

	// 性格卡不承接能力/工具/事件权限：能力仍只在下方能力区。
	cardBody := jsFunctionBody(t, section, "function buildPersonalityCard(mount, view) {")
	for _, banned := range []string{"data-cap", "capabilities", "save-capabilities", "tools", "talkativeness", "reply_mode"} {
		if strings.Contains(cardBody, banned) {
			t.Errorf("性格卡不应出现能力/旧概率入口 %q", banned)
		}
	}

	// 样式：只服务 #view-ai，含有窄屏单列与建议区间带。
	for _, want := range []string{"#view-ai .ai-person-card", "ai-person-band-bar",
		"ai-person-band-mark", "@media(max-width:720px)"} {
		if !strings.Contains(css, want) {
			t.Errorf("ai.css 缺少 %q", want)
		}
	}
	// 片段仍然是唯一挂载点；四维卡片动态挂载，不改片段里的角色选择。
	if !strings.Contains(fragment, `id="aiPane-personality"`) {
		t.Error("片段缺少性格挂载面板")
	}
	if !strings.Contains(js, "aiPersonalityMount") {
		t.Error("ai.js 缺少动态挂载点")
	}
}

// ---- P6：旧键与既有流程 ----

// t107SettingRows 读整张 ai_setting（逐键原值），用于证明保存性格只写自己的键。
func t107SettingRows(t *testing.T, db *store.DB) map[string]string {
	t.Helper()
	rows, err := db.SQL.Query(`SELECT key, value FROM ai_setting ORDER BY key`)
	if err != nil {
		t.Fatalf("读 ai_setting: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatalf("扫 ai_setting: %v", err)
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 ai_setting: %v", err)
	}
	return out
}

// TestAIPersonalitySaveKeepsLegacySettingsCapsAndLimits 是 P6 的「保存性格后核旧设置、
// 能力权限与硬限值」：旧开关/旧概率/硬限全部先写成非默认值，连续两次真实管理保存
// 之后逐键比对，除性格键外一个字都不许变；能力集合也保持不变。
func TestAIPersonalitySaveKeepsLegacySettingsCapsAndLimits(t *testing.T) {
	ts, token, db := personalityServer(t)
	createPersonalityAgent(t, db, "hutao", "胡桃", "喜欢安静钓鱼。")
	ctx := context.Background()

	// 旧键覆盖：已停读的开关/概率、仍作硬限的圆桌参数、派生时长、休眠角色保护。
	legacy := map[string]string{
		"roundtable_enabled":           "1",
		"roundtable_on_speech_chance":  "0.85",
		"roundtable_chime_chance":      "0.15",
		"ai_catch_talk":                "1",
		"ai_catch_star_chance":         "0.11",
		"ai_catch_super_chance":        "0.12",
		"ai_catch_roundtable_chance":   "0.13",
		"ai_fishing_talk_enabled":      "1",
		"ai_fishing_talk_star_chance":  "0.14",
		"ai_fishing_talk_super_chance": "0.15",
		"ai_fishing_roundtable_chance": "0.16",
		"ai_actor_enabled":             "0",
		"ai_actor_switch_chance":       "0.17",
		"ai_actor_switch_cooldown_s":   "600",
		"roundtable_max_speakers":      "2",
		"roundtable_max_turns":         "4",
		"roundtable_max_ms":            "12000",
		"roundtable_cooldown_ms":       "60000",
		"roundtable_hourly_limit":      "5",
		"roundtable_gap_ms":            "1500",
		"roundtable_max_streak":        "2",
		"ai_fishing_hourly_limit":      "7",
	}
	if err := db.AISetSettings(ctx, legacy); err != nil {
		t.Fatalf("写入旧设置: %v", err)
	}
	if code, body := capabilityRequest(t, ts, token, "POST", "/api/ai/capabilities",
		`{"agent_key":"hutao","base_revision":0,"capabilities":["fishing_knowledge"]}`); code != 200 {
		t.Fatalf("保存能力 = %d: %s", code, body)
	}
	settingsBefore := t107SettingRows(t, db)
	_, capsBefore := capabilityRequest(t, ts, token, "GET", "/api/ai/capabilities", "")

	if rev := personalitySaveOK(t, ts, token, "hutao", 0, 60, 40, 70, 55); rev != 1 {
		t.Fatalf("第一次保存修订=%d", rev)
	}
	if rev := personalitySaveOK(t, ts, token, "hutao", 1, 20, 80, 30, 70); rev != 2 {
		t.Fatalf("第二次保存修订=%d", rev)
	}

	settingsAfter := t107SettingRows(t, db)
	for key, before := range settingsBefore {
		after, ok := settingsAfter[key]
		if !ok {
			t.Errorf("保存性格删除了旧设置键 %q", key)
			continue
		}
		if after != before {
			t.Errorf("保存性格改写了旧设置 %q: %q → %q", key, before, after)
		}
	}
	for key := range settingsAfter {
		if _, ok := settingsBefore[key]; !ok && !strings.HasPrefix(key, "ai_personality:") {
			t.Errorf("保存性格新增了非性格设置键 %q", key)
		}
	}
	// 硬限逐项点名：不能被性格保存顺手改掉或放宽。
	for _, key := range []string{
		"roundtable_max_speakers", "roundtable_max_turns", "roundtable_max_ms",
		"roundtable_cooldown_ms", "roundtable_hourly_limit", "roundtable_gap_ms",
		"roundtable_max_streak", "ai_fishing_hourly_limit",
	} {
		if got := db.AISetting(ctx, key, ""); got != legacy[key] {
			t.Errorf("硬限 %q = %q，期望 %q", key, got, legacy[key])
		}
	}
	// 能力权限不变（性格只写自己的 ai_setting 键）。
	_, capsAfter := capabilityRequest(t, ts, token, "GET", "/api/ai/capabilities", "")
	if capsBefore != capsAfter {
		t.Fatalf("保存性格改动了能力集合：\n前 %s\n后 %s", capsBefore, capsAfter)
	}
	if capsAfter == "" {
		t.Fatal("能力回读为空，无法证明未改动")
	}
	t.Logf("P6 保存性格后旧设置 %d 键、能力权限与 8 项硬限逐项不变；性格修订=2",
		len(settingsBefore))
}

// ---- P4 同副本联验：真实管理 GET + 真实 GS/BR 子进程夹具 ----

// t107DriverMarker 与 gameserver 夹具写下的 JSON 同形。
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

// t107Env 是一份「真实管理 Handler + 可与子进程共享的临时库」。
type t107Env struct {
	db    *store.DB
	path  string
	ts    *httptest.Server
	token string
}

func newT107Env(t *testing.T) *t107Env {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t107-effective.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("打开联验临时库: %v", err)
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		t.Fatalf("迁移联验临时库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &t107Env{db: db, path: path, ts: ts, token: signAdminToken(t, srv)}
}

// requireAgent 确认迁移内置的角色在库里：T-107 P4 不改角色定义，直接用它做管理保存。
func (e *t107Env) requireAgent(t *testing.T, key string) {
	t.Helper()
	agent, err := e.db.AIAgentByKey(context.Background(), key)
	if err != nil || agent == nil {
		t.Fatalf("迁移应已内置角色 %s: agent=%v err=%v", key, agent, err)
	}
}

func (e *t107Env) save(t *testing.T, key string, base int64, soc, exp, per, reg int) int64 {
	t.Helper()
	return personalitySaveOK(t, e.ts, e.token, key, base, soc, exp, per, reg)
}

func (e *t107Env) view(t *testing.T, key string) personalityWireAgent {
	t.Helper()
	wire, _ := personalityGet(t, e.ts, e.token)
	return personalityAgent(t, wire, key)
}

// t107RequireWaiting 断言「已保存但未生效」：不出现「已生效」，且文案给出当前修订。
func t107RequireWaiting(t *testing.T, view personalityWireAgent, wantRevision int64, wantReason string) {
	t.Helper()
	if view.Effect.Effective {
		t.Fatalf("未取得同版证据时不得已生效: %+v", view.Effect)
	}
	if wantReason != "" && view.Effect.Reason != wantReason {
		t.Fatalf("等待原因=%q，期望 %q: %+v", view.Effect.Reason, wantReason, view.Effect)
	}
	if strings.Contains(view.Effect.Display, "已生效") {
		t.Fatalf("等待文案不得出现「已生效」: %q", view.Effect.Display)
	}
	if wantRevision > 0 && !strings.Contains(view.Effect.Display, "修订 "+strconv.FormatInt(wantRevision, 10)) {
		t.Fatalf("等待文案必须带当前修订 %d: %q", wantRevision, view.Effect.Display)
	}
}

// t107RequireEffective 断言生效文案带被消费的修订号与真实 task/result。
func t107RequireEffective(t *testing.T, view personalityWireAgent, wantRevision, taskID, resultID int64) {
	t.Helper()
	if !view.Effect.Effective || view.Effect.Reason != "matched" {
		t.Fatalf("同版 GS 选择 + BR 表达后应已生效: %+v", view.Effect)
	}
	for _, want := range []string{"已生效", "修订 " + strconv.FormatInt(wantRevision, 10),
		"任务 " + strconv.FormatInt(taskID, 10), "结果 " + strconv.FormatInt(resultID, 10)} {
		if !strings.Contains(view.Effect.Display, want) {
			t.Fatalf("生效文案缺 %q: %q", want, view.Effect.Display)
		}
	}
}

func t107TaskGSRevision(t *testing.T, db *store.DB, taskID int64, agent string) int64 {
	t.Helper()
	ev, ok, err := db.AITaskEvidenceOf(context.Background(), taskID)
	if err != nil || !ok {
		t.Fatalf("读任务 %d 证据: ok=%v err=%v", taskID, ok, err)
	}
	for _, part := range ev.Participants {
		if part.Key != agent {
			continue
		}
		if part.GSPersonalityRevision == nil {
			t.Fatalf("任务 %d 没有绑定 %s 的 GS 四维修订: %+v", taskID, agent, ev.Participants)
		}
		return *part.GSPersonalityRevision
	}
	t.Fatalf("任务 %d 证据未包含 %s: %+v", taskID, agent, ev.Participants)
	return 0
}

func t107CountResults(t *testing.T, db *store.DB, taskID int64) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_result WHERE task_id=?`, taskID).Scan(&n); err != nil {
		t.Fatalf("统计任务 %d 结果: %v", taskID, err)
	}
	return n
}

func t107ResultOfTask(t *testing.T, db *store.DB, taskID int64) (resultID, brRevision int64, hasExpr, delivered bool, errText string) {
	t.Helper()
	var deliveredTS float64
	err := db.SQL.QueryRow(`
		SELECT r.id, COALESCE(r.delivered_ts,-999), COALESCE(r.err,''),
		       (e.result_id IS NOT NULL), COALESCE(e.br_personality_revision,0)
		FROM ai_result r LEFT JOIN ai_result_expression e ON e.result_id=r.id
		WHERE r.task_id=? ORDER BY r.id DESC LIMIT 1`, taskID).
		Scan(&resultID, &deliveredTS, &errText, &hasExpr, &brRevision)
	if err != nil {
		t.Fatalf("读任务 %d 结果: %v", taskID, err)
	}
	return resultID, brRevision, hasExpr, deliveredTS >= 0, errText
}

var (
	t107GameserverOnce sync.Once
	t107GameserverBin  string
	t107GameserverErr  error
	t107GameserverLog  string
)

// t107GameserverTestBinary 用当前 go 工具链把 gameserver 的测试二进制构建一次，
// 后续每个受控时点直接运行它；不引入 admin ↔ gameserver 测试包的互相导入。
func t107GameserverTestBinary(t *testing.T) string {
	t.Helper()
	t107GameserverOnce.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t107GameserverErr = err
			return
		}
		goTool, err := t107GoTool()
		if err != nil {
			t107GameserverErr = err
			return
		}
		dir, err := os.MkdirTemp("", "t107-gs-test-*")
		if err != nil {
			t107GameserverErr = err
			return
		}
		out := filepath.Join(dir, "gameserver.test")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		cmd := exec.Command(goTool, "test", "-c", "-o", out, "./internal/gameserver")
		cmd.Dir = root
		raw, err := cmd.CombinedOutput()
		t107GameserverLog = string(raw)
		if err != nil {
			t107GameserverErr = fmt.Errorf("构建 gameserver 测试二进制失败: %v", err)
			return
		}
		t107GameserverBin = out
	})
	if t107GameserverErr != nil {
		t.Fatalf("%v\n%s", t107GameserverErr, t107GameserverLog)
	}
	return t107GameserverBin
}

func t107GoTool() (string, error) {
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	var candidates []string
	if root := strings.TrimSpace(os.Getenv("GOROOT")); root != "" {
		candidates = append(candidates, filepath.Join(root, "bin", name))
	}
	if p, err := exec.LookPath("go"); err == nil {
		candidates = append(candidates, p)
	}
	if root := runtime.GOROOT(); root != "" {
		candidates = append(candidates, filepath.Join(root, "bin", name))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("找不到 go 工具链：P4 联验需要它构建并启动真实 GS/BR 夹具子进程（把 go 放进 PATH 或设置 GOROOT）")
}

// t107Driver 是一个受屏障控制的真实 GS/BR 子进程。
type t107Driver struct {
	t       *testing.T
	cmd     *exec.Cmd
	barrier string
	logPath string
	exited  chan struct{}
	waitErr error
}

func (e *t107Env) startDriver(t *testing.T, agent, channel, reply string, vendorOff bool) *t107Driver {
	t.Helper()
	bin := t107GameserverTestBinary(t)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("定位模块根: %v", err)
	}
	dir := t.TempDir()
	barrier := filepath.Join(dir, "barrier")
	if err := os.MkdirAll(barrier, 0o755); err != nil {
		t.Fatalf("建屏障目录: %v", err)
	}
	logPath := filepath.Join(dir, "driver.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("建夹具日志: %v", err)
	}
	cmd := exec.Command(bin, "-test.run", "^TestT107EffectiveRuntimeDriver$", "-test.v", "-test.timeout", "300s")
	cmd.Dir = filepath.Join(root, "internal", "gameserver")
	cmd.Env = append(os.Environ(),
		"T107_EFFECT_DB="+e.path,
		"T107_EFFECT_BARRIER="+barrier,
		"T107_EFFECT_AGENT="+agent,
		"T107_EFFECT_CHANNEL="+channel,
		"T107_EFFECT_REPLY="+reply,
	)
	if vendorOff {
		cmd.Env = append(cmd.Env, "T107_EFFECT_VENDOR=off")
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("启动 GS/BR 夹具: %v", err)
	}
	d := &t107Driver{t: t, cmd: cmd, barrier: barrier, logPath: logPath, exited: make(chan struct{})}
	go func() {
		d.waitErr = cmd.Wait()
		_ = logFile.Close()
		close(d.exited)
	}()
	t.Cleanup(d.stop)
	return d
}

func (d *t107Driver) logTail() string {
	data, err := os.ReadFile(d.logPath)
	if err != nil {
		return "（读不到夹具日志）"
	}
	if len(data) > 8000 {
		data = data[len(data)-8000:]
	}
	return string(data)
}

func (d *t107Driver) waitMarker(name string) t107DriverMarker {
	d.t.Helper()
	deadline := time.Now().Add(180 * time.Second)
	path := filepath.Join(d.barrier, name)
	for {
		if data, err := os.ReadFile(path); err == nil {
			var m t107DriverMarker
			if err := json.Unmarshal(data, &m); err != nil {
				d.t.Fatalf("标记 %s 不是合法 JSON: %v\n%s", name, err, d.logTail())
			}
			return m
		}
		select {
		case <-d.exited:
			d.t.Fatalf("GS/BR 夹具在写下 %s 之前退出\n%s", name, d.logTail())
		default:
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("等待夹具标记 %s 超时\n%s", name, d.logTail())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (d *t107Driver) release() {
	d.t.Helper()
	if err := os.WriteFile(filepath.Join(d.barrier, "release"), []byte("1"), 0o644); err != nil {
		d.t.Fatalf("写放行屏障失败: %v", err)
	}
}

func (d *t107Driver) finish(t *testing.T) {
	t.Helper()
	select {
	case <-d.exited:
	case <-time.After(60 * time.Second):
		t.Fatalf("GS/BR 夹具未在 60 秒内退出\n%s", d.logTail())
	}
	if d.waitErr != nil {
		t.Fatalf("GS/BR 夹具退出失败: %v\n%s", d.waitErr, d.logTail())
	}
}

func (d *t107Driver) stop() {
	select {
	case <-d.exited:
		return
	default:
	}
	if d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
	<-d.exited
}

// TestT107EffectiveFromRuntime 是 T-107 P4 的同副本联验：
// 真实管理 JWT/Handler 保存与 GET；真实 GS 选择（玩家上星链）与真实 BR 表达在
// 子进程中跑，两次真实管理 GET 分别发生在「GS 已入队、BR 尚未写表达」与「表达
// 落库」之后。断言「已保存」不冒充「已生效」，跨版、异角色同号、重建、只有
// fallback、证据不可读/已清理都不点亮当前版。
func TestT107EffectiveFromRuntime(t *testing.T) {
	bin := t107GameserverTestBinary(t)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(bin)) })

	t.Run("UnsavedOnlyDraft", func(t *testing.T) {
		e := newT107Env(t)
		e.requireAgent(t, "companion")
		view := e.view(t, "companion")
		if view.Present || view.Revision != 0 {
			t.Fatalf("未保存时应为缺值中性: %+v", view)
		}
		if view.Effect.Effective || view.Effect.Reason != "not_saved" {
			t.Fatalf("未保存不得显示已生效: %+v", view.Effect)
		}
		if !strings.Contains(view.Effect.Display, "尚未保存") {
			t.Fatalf("未保存文案不符: %q", view.Effect.Display)
		}
		t.Logf("P4 未保存 GET: save_display=%q effect={effective:%v reason:%q display:%q}",
			view.SaveDisplay, view.Effect.Effective, view.Effect.Reason, view.Effect.Display)
	})

	t.Run("SaveThenEnqueueThenExpression", func(t *testing.T) {
		e := newT107Env(t)
		e.requireAgent(t, "companion")
		rev := e.save(t, "companion", 0, 60, 40, 70, 55)
		if rev != 1 {
			t.Fatalf("首次保存修订=%d，期望 1", rev)
		}
		view := e.view(t, "companion")
		if !view.Present || view.Revision != rev {
			t.Fatalf("保存后回读不符: %+v", view)
		}
		t107RequireWaiting(t, view, rev, store.AITaskEvidenceReasonNoExpression)
		t.Logf("P4 保存 A/r 后等待: save_display=%q effect={reason:%q display:%q tasks_since_save:%d}",
			view.SaveDisplay, view.Effect.Reason, view.Effect.Display, view.Effect.TasksSinceSave)

		// 第一次真实管理 GET 时点：真实 GS 已按 r 选择并入队，BR 尚未写表达。
		d := e.startDriver(t, "companion", "t107_effect_main", "同版表达一", false)
		enq := d.waitMarker("enqueued.json")
		if enq.Stage != "enqueued" || enq.Agent != "companion" || enq.TaskID <= 0 {
			t.Fatalf("入队标记不符: %+v", enq)
		}
		if enq.Revision != rev {
			t.Fatalf("入队时管理端修订=%d，期望 %d", enq.Revision, rev)
		}
		if got := t107TaskGSRevision(t, e.db, enq.TaskID, "companion"); got != rev {
			t.Fatalf("任务 %d 的 GS 四维修订=%d，期望 %d", enq.TaskID, got, rev)
		}
		if n := t107CountResults(t, e.db, enq.TaskID); n != 0 {
			t.Fatalf("入队后 BR 尚未表达，不应有结果，实得 %d 条", n)
		}
		view = e.view(t, "companion")
		t107RequireWaiting(t, view, rev, store.AITaskEvidenceReasonNoExpression)
		t.Logf("P4 GS 已入队、BR 未表达时 GET: task=%d revision=%d effect={effective:%v reason:%q display:%q}",
			enq.TaskID, rev, view.Effect.Effective, view.Effect.Reason, view.Effect.Display)

		// 放行真实 BR 表达，再取第二次真实管理 GET。
		d.release()
		expr := d.waitMarker("expressed.json")
		d.finish(t)
		if expr.TaskID != enq.TaskID || expr.ResultID <= 0 || !expr.HasExpression {
			t.Fatalf("BR 表达标记不符: %+v（入队 task=%d）", expr, enq.TaskID)
		}
		resultID, brRev, hasExpr, delivered, resultErr := t107ResultOfTask(t, e.db, enq.TaskID)
		if resultID != expr.ResultID || !hasExpr || brRev != rev || !delivered || resultErr != "" {
			t.Fatalf("结果/表达凭据不符: id=%d want=%d has_expr=%v br_rev=%d want=%d delivered=%v err=%q",
				resultID, expr.ResultID, hasExpr, brRev, rev, delivered, resultErr)
		}
		if !expr.Matched {
			t.Fatalf("夹具自身的按角色报告未命中: %+v", expr)
		}
		view = e.view(t, "companion")
		t107RequireEffective(t, view, rev, enq.TaskID, resultID)
		t.Logf("P4 BR 表达落库后 GET: task=%d result=%d br_revision=%d delivered=%v effect={effective:%v reason:%q display:%q tasks_since_save:%d last_task_at:%.0f}",
			enq.TaskID, resultID, brRev, delivered, view.Effect.Effective, view.Effect.Reason,
			view.Effect.Display, view.Effect.TasksSinceSave, view.Effect.LastTaskAt)

		// 刷新回读同一证据，不因再次 GET 改口。
		refresh := e.view(t, "companion")
		t107RequireEffective(t, refresh, rev, enq.TaskID, resultID)
		if refresh.Effect.Display != view.Effect.Display {
			t.Fatalf("刷新后生效文案变了: %q → %q", view.Effect.Display, refresh.Effect.Display)
		}
		t.Logf("P4 刷新 GET: effect={effective:%v reason:%q display:%q}",
			refresh.Effect.Effective, refresh.Effect.Reason, refresh.Effect.Display)

		// 同角色保存 r+1：旧任务/旧结果属于 r，必须立即回到等待（同任务跨版不点亮）。
		rev2 := e.save(t, "companion", rev, 20, 80, 30, 70)
		if rev2 != rev+1 {
			t.Fatalf("第二次保存修订=%d，期望 %d", rev2, rev+1)
		}
		view = e.view(t, "companion")
		t107RequireWaiting(t, view, rev2, store.AITaskEvidenceReasonPersonaDiff)
		t.Logf("P4 保存 r+1 后立即等待: r=%d 旧 task=%d/result=%d effect={effective:%v reason:%q display:%q}",
			rev2, enq.TaskID, resultID, view.Effect.Effective, view.Effect.Reason, view.Effect.Display)

		// 第二次真实 GS 入队（仍等待）→ 放行 BR → 点亮 r+1。
		d2 := e.startDriver(t, "companion", "t107_effect_main", "同版表达二", false)
		enq2 := d2.waitMarker("enqueued.json")
		if enq2.TaskID <= enq.TaskID || enq2.Revision != rev2 {
			t.Fatalf("第二次入队标记不符: %+v（上次 task=%d，期望修订 %d）", enq2, enq.TaskID, rev2)
		}
		if got := t107TaskGSRevision(t, e.db, enq2.TaskID, "companion"); got != rev2 {
			t.Fatalf("新任务 %d 的 GS 四维修订=%d，期望 %d", enq2.TaskID, got, rev2)
		}
		view = e.view(t, "companion")
		t107RequireWaiting(t, view, rev2, store.AITaskEvidenceReasonPersonaDiff)
		t.Logf("P4 r+1 已入队、BR 未表达时 GET: task=%d revision=%d effect={effective:%v reason:%q display:%q}",
			enq2.TaskID, rev2, view.Effect.Effective, view.Effect.Reason, view.Effect.Display)
		d2.release()
		expr2 := d2.waitMarker("expressed.json")
		d2.finish(t)
		resultID2, brRev2, hasExpr2, delivered2, _ := t107ResultOfTask(t, e.db, enq2.TaskID)
		if expr2.ResultID != resultID2 || !hasExpr2 || brRev2 != rev2 || !delivered2 || resultID2 == resultID {
			t.Fatalf("r+1 结果/表达凭据不符: %+v（task=%d result=%d br_rev=%d want_rev=%d delivered=%v）",
				expr2, enq2.TaskID, resultID2, brRev2, rev2, delivered2)
		}
		view = e.view(t, "companion")
		t107RequireEffective(t, view, rev2, enq2.TaskID, resultID2)
		t.Logf("P4 r+1 表达落库后 GET: task=%d result=%d effect={effective:%v reason:%q display:%q}",
			enq2.TaskID, resultID2, view.Effect.Effective, view.Effect.Reason, view.Effect.Display)
	})

	t.Run("OtherRoleSameRevision", func(t *testing.T) {
		e := newT107Env(t)
		e.requireAgent(t, "companion")
		e.requireAgent(t, "fishguide")
		revA := e.save(t, "companion", 0, 88, 12, 64, 31)
		revB := e.save(t, "fishguide", 0, 12, 88, 30, 70)
		if revA != revB {
			t.Fatalf("两名角色应保存到同一修订号: companion=%d fishguide=%d", revA, revB)
		}
		d := e.startDriver(t, "fishguide", "t107_effect_other", "换个人说", false)
		enq := d.waitMarker("enqueued.json")
		if enq.Agent != "fishguide" || enq.Revision != revB {
			t.Fatalf("证据应由 fishguide 按 r=%d 产生: %+v", revB, enq)
		}
		d.release()
		expr := d.waitMarker("expressed.json")
		d.finish(t)
		if !expr.Matched || expr.ResultID <= 0 {
			t.Fatalf("fishguide 自身应命中: %+v", expr)
		}
		guide := e.view(t, "fishguide")
		t107RequireEffective(t, guide, revB, enq.TaskID, expr.ResultID)
		companion := e.view(t, "companion")
		t107RequireWaiting(t, companion, revA, store.AITaskEvidenceReasonNoExpression)
		t.Logf("P4 其他角色同号修订: companion r=%d effective=false reason=%q；fishguide r=%d task=%d result=%d effective=true",
			revA, companion.Effect.Reason, revB, enq.TaskID, expr.ResultID)
	})

	t.Run("RebuiltSameNameGeneration", func(t *testing.T) {
		e := newT107Env(t)
		e.requireAgent(t, "companion")
		rev := e.save(t, "companion", 0, 88, 12, 64, 31)
		d := e.startDriver(t, "companion", "t107_effect_rebuilt", "重建前的一句", false)
		enq := d.waitMarker("enqueued.json")
		d.release()
		expr := d.waitMarker("expressed.json")
		d.finish(t)
		view := e.view(t, "companion")
		t107RequireEffective(t, view, rev, enq.TaskID, expr.ResultID)

		// 逻辑删除 → 同 key 重建 → 代次 +1：旧代次证据不得点亮。
		ctx := context.Background()
		if err := e.db.AIDeleteAgent(ctx, "companion"); err != nil {
			t.Fatalf("删除角色: %v", err)
		}
		if _, _, err := e.db.AICreateAgent(ctx, store.AIAgent{
			Key: "companion", Display: "AI 伙伴", Role: "陪聊", ReplyMode: "social",
		}, "[]"); err != nil {
			t.Fatalf("重建角色: %v", err)
		}
		if _, err := e.db.SQL.Exec(
			`UPDATE ai_runtime_control SET generation=generation+1 WHERE agent_key='companion'`); err != nil {
			t.Fatalf("提升代次: %v", err)
		}
		view = e.view(t, "companion")
		if view.Effect.Effective || strings.Contains(view.Effect.Display, "已生效") {
			t.Fatalf("重建同名角色后不得保留旧代次的已生效: %+v", view.Effect)
		}
		t.Logf("P4 同名角色重建（未保存）: revision=%d inactive=%q effect={effective:%v reason:%q display:%q}",
			view.Revision, view.Inactive, view.Effect.Effective, view.Effect.Reason, view.Effect.Display)

		// 重新保存一次（T-103 的恢复路径）仍不点亮旧代次证据。
		rev2 := e.save(t, "companion", rev, 20, 80, 30, 70)
		view = e.view(t, "companion")
		t107RequireWaiting(t, view, rev2, store.AITaskEvidenceReasonGeneration)
		t.Logf("P4 重建后重新保存 r=%d: effect={effective:%v reason:%q display:%q}", rev2,
			view.Effect.Effective, view.Effect.Reason, view.Effect.Display)
	})

	t.Run("FallbackOnlyNoExpression", func(t *testing.T) {
		e := newT107Env(t)
		e.requireAgent(t, "companion")
		rev := e.save(t, "companion", 0, 60, 40, 70, 55)
		d := e.startDriver(t, "companion", "t107_effect_fallback", "不该出现的表达", true)
		enq := d.waitMarker("enqueued.json")
		d.release()
		expr := d.waitMarker("expressed.json")
		d.finish(t)
		if expr.HasExpression || expr.ResultID <= 0 || strings.TrimSpace(expr.ResultErr) == "" {
			t.Fatalf("厂商不可达时应落一条无表达凭据的降级结果: %+v", expr)
		}
		_, _, hasExpr, _, resultErr := t107ResultOfTask(t, e.db, enq.TaskID)
		if hasExpr {
			t.Fatalf("降级结果不得写表达凭据: %+v", resultErr)
		}
		view := e.view(t, "companion")
		t107RequireWaiting(t, view, rev, store.AITaskEvidenceReasonNoExpression)
		if view.Effect.TasksSinceSave < 1 {
			t.Fatalf("保存后应统计到已结束任务（仅诊断）: %+v", view.Effect)
		}
		t.Logf("P4 只有 fallback（无表达）: task=%d result=%d err=%q tasks_since_save=%d effective=false reason=%q display=%q",
			enq.TaskID, expr.ResultID, resultErr, view.Effect.TasksSinceSave, view.Effect.Reason,
			view.Effect.Display)
	})

	t.Run("UnreadableAndPrunedEvidence", func(t *testing.T) {
		e := newT107Env(t)
		e.requireAgent(t, "companion")
		rev := e.save(t, "companion", 0, 60, 40, 70, 55)
		d := e.startDriver(t, "companion", "t107_effect_evidence", "证据这一句", false)
		enq := d.waitMarker("enqueued.json")
		d.release()
		expr := d.waitMarker("expressed.json")
		d.finish(t)
		view := e.view(t, "companion")
		t107RequireEffective(t, view, rev, enq.TaskID, expr.ResultID)

		var binding string
		if err := e.db.SQL.QueryRow(`SELECT binding_json FROM ai_task_evidence WHERE task_id=?`, enq.TaskID).Scan(&binding); err != nil {
			t.Fatalf("读任务证据: %v", err)
		}

		// 改坏证据载荷：不可读 → 不点亮。
		if _, err := e.db.SQL.Exec(`UPDATE ai_task_evidence SET binding_json='{"format_version":99}' WHERE task_id=?`, enq.TaskID); err != nil {
			t.Fatalf("改坏任务证据: %v", err)
		}
		view = e.view(t, "companion")
		t107RequireWaiting(t, view, rev, store.AITaskEvidenceReasonUnreadable)
		t.Logf("P4 证据不可读: reason=%q display=%q", view.Effect.Reason, view.Effect.Display)
		// 还原后重新点亮：证明前一条是证据本身造成的。
		if _, err := e.db.SQL.Exec(`UPDATE ai_task_evidence SET binding_json=? WHERE task_id=?`, binding, enq.TaskID); err != nil {
			t.Fatalf("还原任务证据: %v", err)
		}
		view = e.view(t, "companion")
		t107RequireEffective(t, view, rev, enq.TaskID, expr.ResultID)

		// 已回收结果 → 不点亮。
		if _, err := e.db.SQL.Exec(
			`UPDATE ai_result SET delivered_ts=-1, err='attempt_reclaimed' WHERE id=?`, expr.ResultID); err != nil {
			t.Fatalf("回收结果: %v", err)
		}
		view = e.view(t, "companion")
		t107RequireWaiting(t, view, rev, store.AIEvidenceReasonAttemptReclaimed)
		t.Logf("P4 结果已回收: reason=%q display=%q", view.Effect.Reason, view.Effect.Display)
		if _, err := e.db.SQL.Exec(
			`UPDATE ai_result SET delivered_ts=NULL, err='' WHERE id=?`, expr.ResultID); err != nil {
			t.Fatalf("恢复结果: %v", err)
		}
		view = e.view(t, "companion")
		t107RequireEffective(t, view, rev, enq.TaskID, expr.ResultID)

		// 任务证据已清理 → 不点亮。
		if _, err := e.db.SQL.Exec(`DELETE FROM ai_task_evidence WHERE task_id=?`, enq.TaskID); err != nil {
			t.Fatalf("清理任务证据: %v", err)
		}
		view = e.view(t, "companion")
		t107RequireWaiting(t, view, rev, store.AITaskEvidenceReasonMissing)
		t.Logf("P4 任务证据已清理: reason=%q display=%q", view.Effect.Reason, view.Effect.Display)
		if _, err := e.db.SQL.Exec(`INSERT INTO ai_task_evidence(task_id, binding_json) VALUES(?,?)`, enq.TaskID, binding); err != nil {
			t.Fatalf("恢复任务证据: %v", err)
		}
		view = e.view(t, "companion")
		t107RequireEffective(t, view, rev, enq.TaskID, expr.ResultID)

		// 表达凭据已清理 → 不点亮。
		if _, err := e.db.SQL.Exec(`DELETE FROM ai_result_expression WHERE result_id=?`, expr.ResultID); err != nil {
			t.Fatalf("清理表达凭据: %v", err)
		}
		view = e.view(t, "companion")
		t107RequireWaiting(t, view, rev, store.AITaskEvidenceReasonNoExpression)
		t.Logf("P4 表达凭据已清理: reason=%q display=%q", view.Effect.Reason, view.Effect.Display)
	})

	if fixture := strings.TrimSpace(os.Getenv("T107_R158_FIXTURE_DB")); fixture != "" {
		t.Run("R158FixtureReuse", func(t *testing.T) {
			// R-158 的真实 GS/BR 夹具库：复制到临时目录后只用真实管理 GET 读，
			// 作为「R-158 夹具可在临时库中复用」的显示核对，不替代上面的新跑正例。
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "r158-fixture.db")
			for _, suffix := range []string{"", "-wal", "-shm"} {
				raw, err := os.ReadFile(fixture + suffix)
				if err != nil {
					if suffix == "" {
						t.Fatalf("读 R-158 夹具库: %v", err)
					}
					continue
				}
				if err := os.WriteFile(dbPath+suffix, raw, 0o644); err != nil {
					t.Fatalf("复制 R-158 夹具库: %v", err)
				}
			}
			db, err := store.OpenWithStatic(dbPath, filepath.Join("..", "..", "server_data", "static.sqlite3"))
			if err != nil {
				t.Fatalf("打开 R-158 夹具库: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			wire, _ := personalityGet(t, ts, signAdminToken(t, srv))
			view := personalityAgent(t, wire, "companion")
			t107RequireEffective(t, view, 1, 1, 1)
			t.Logf("P4 R-158 夹具复用: companion revision=%d effect=%q", view.Revision, view.Effect.Display)
		})
	}
}
