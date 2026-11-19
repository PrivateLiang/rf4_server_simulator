package admin

// 「复测已保存连接」的独立真值（V2）：真实 SQLite + 真实 handler；本地 httptest
// 模拟厂商，只断言请求次数与响应内容，不连接任何真实厂商、不使用真实凭据。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aibridge"
	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

const (
	aiTestSavedKey = "sk-t010-saved-0123456789"
	aiTestPageKey  = "page-key-t010-unsaved-9876"
)

type aiTestVendor struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits int
	auth string
	body string
}

func newAITestVendor(t *testing.T, status int, respBody string) *aiTestVendor {
	t.Helper()
	v := &aiTestVendor{}
	// TLS fixture：真正发送仍要求 HTTPS。
	v.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		v.mu.Lock()
		v.hits++
		v.auth = r.Header.Get("Authorization")
		v.body = string(raw)
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *aiTestVendor) snapshot() (hits int, auth, body string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.hits, v.auth, v.body
}

// newAITestFixture 保存一份合成 Key 与厂商配置（发布修订 1），返回可调用 handler 的后台。
func newAITestFixture(t *testing.T, vendorURL string) (*Server, *store.DB) {
	t.Helper()
	return newAITestFixtureWithHosts(t, vendorURL, nil)
}

// newAITestFixtureWithHosts 保留旧签名：部署旧名单已不参与放行，第二个参数被忽略。
func newAITestFixtureWithHosts(t *testing.T, vendorURL string, _ []string) (*Server, *store.DB) {
	t.Helper()
	t.Cleanup(aibridge.SetCredentialDirSyncForTest(func(string) error { return nil }))
	t.Setenv("RF4_AI_MASTER_KEY_FILE", filepath.Join(t.TempDir(), "ai_master.key"))
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	rec := postAs(t, srv, srv.handleAIConfigSave, "/api/ai/config",
		fmt.Sprintf(`{"base_url":%q,"model_id":"model-a","api_key":%q}`, vendorURL, aiTestSavedKey), "tester")
	if rec.Code != 200 {
		t.Fatalf("保存厂商配置失败 %d: %s", rec.Code, rec.Body.String())
	}
	return srv, db
}

// withAITestTransport 把「复测」的 Provider 换成注入本地 fixture Transport
// 的构造方式；URL 检查仍由真实 Provider 执行。
func withAITestTransport(t *testing.T, base http.RoundTripper) {
	t.Helper()
	old := newAITestProvider
	newAITestProvider = func(spec aibridge.ProviderSpec, timeout time.Duration) (*aibridge.OpenAICompatProvider, error) {
		return aibridge.NewProviderWithTransport(spec, timeout, base)
	}
	t.Cleanup(func() { newAITestProvider = old })
}

func runAITest(t *testing.T, srv *Server, baseURL, modelID, apiKey string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"base_url":%q,"model_id":%q,"api_key":%q}`, baseURL, modelID, apiKey)
	rec := postAs(t, srv, srv.handleAITest, "/api/ai/test", body, "tester")
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v: %s", err, rec.Body.String())
	}
	return rec, out
}

// assertNoEcho 断言后台回显不含明文 Key、完整厂商地址或主机。
func assertNoEcho(t *testing.T, body string, vendors ...*aiTestVendor) {
	t.Helper()
	secrets := []string{aiTestSavedKey, aiTestPageKey}
	for _, v := range vendors {
		secrets = append(secrets, v.srv.URL,
			strings.TrimPrefix(v.srv.URL, "http://"), strings.TrimPrefix(v.srv.URL, "https://"))
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(body, secret) {
			t.Fatalf("回显包含敏感内容 %q: %s", secret, body)
		}
	}
}

// TestAITestOldGatesNoLongerBlock 旧开关/旧 consent 不再挡住有效复测；
// 只有急停会拒绝，且拒绝时零外发。
func TestAITestOldGatesNoLongerBlock(t *testing.T) {
	ctx := context.Background()
	vendor := newAITestVendor(t, 200, `{"choices":[{"message":{"content":"正常"}}]}`)
	withAITestTransport(t, vendor.srv.Client().Transport)
	srv, db := newAITestFixture(t, vendor.srv.URL)

	// 旧闸全部写死为「关/撤销/伪造」，V2 都不得阻止复测。
	if err := db.AISetSettings(ctx, map[string]string{
		aiconfig.KeyOutboundEnabled:   "0",
		aiconfig.KeyConsentOutbound:   `{"schema":1,"item":"outbound","state":"revoked","subject":"tester","ts":1}`,
		aiconfig.KeyEgressDestination: `{"base_url":"https://stale.invalid/v1","model_id":"old"}`,
	}); err != nil {
		t.Fatal(err)
	}
	rec, out := runAITest(t, srv, "", "", "")
	if rec.Code != 200 || out["ok"] != true {
		t.Fatalf("旧闸不应阻止复测，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := fmt.Sprint(out["state"]); got != "retested" {
		t.Fatalf("state = %q，期望 retested", got)
	}
	if hits, _, _ := vendor.snapshot(); hits != 1 {
		t.Fatalf("复测请求次数 = %d，期望 1", hits)
	}

	// 急停：零外发，返回运行期同一套原因。
	if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	rec, out = runAITest(t, srv, "", "", "")
	if out["ok"] != false {
		t.Fatalf("急停时仍放行: %s", rec.Body.String())
	}
	if got := fmt.Sprint(out["state"]); got != aiconfig.StateEmergency {
		t.Fatalf("state = %q，期望 emergency", got)
	}
	if hits, _, _ := vendor.snapshot(); hits != 1 {
		t.Fatalf("急停后仍发出请求：累计 %d 次，期望 1", hits)
	}
	assertNoEcho(t, rec.Body.String(), vendor)
}

// TestAITestAllowedUsesSavedKeyAndSyntheticPrompt 放行时只用活动快照里的 Key，
// 请求体只带固定合成提示；响应不回显明文 Key，并记录所测修订。
func TestAITestAllowedUsesSavedKeyAndSyntheticPrompt(t *testing.T) {
	vendor := newAITestVendor(t, 200, `{"choices":[{"message":{"content":"正常"}}]}`)
	withAITestTransport(t, vendor.srv.Client().Transport)
	srv, _ := newAITestFixture(t, vendor.srv.URL)

	rec, out := runAITest(t, srv, "", "", "")
	if rec.Code != 200 || out["ok"] != true {
		t.Fatalf("放行场景应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if out["sample"] != "正常" {
		t.Fatalf("sample = %v，期望 正常", out["sample"])
	}
	if fmt.Sprint(out["revision"]) != "1" {
		t.Fatalf("复测修订 = %v，期望 1", out["revision"])
	}
	hits, auth, reqBody := vendor.snapshot()
	if hits != 1 {
		t.Fatalf("模拟厂商请求次数 = %d，期望 1", hits)
	}
	if auth != "Bearer "+aiTestSavedKey {
		t.Fatalf("请求鉴权头不是已保存的 Key: %q", auth)
	}
	if strings.Contains(reqBody, aiTestSavedKey) || strings.Contains(reqBody, aiTestPageKey) {
		t.Fatalf("请求体夹带明文 Key: %s", reqBody)
	}
	if !strings.Contains(reqBody, "请只回复两个字：正常") {
		t.Fatalf("请求体不是固定合成提示: %s", reqBody)
	}
	if strings.Contains(rec.Body.String(), aiTestSavedKey) || strings.Contains(rec.Body.String(), aiTestPageKey) {
		t.Fatalf("响应回显明文 Key: %s", rec.Body.String())
	}
	// 同一修订复测成功后，状态页显示「此连接已通过复测」。
	status := httptest.NewRecorder()
	srv.handleAIStatus(status, httptest.NewRequest("GET", "/api/ai/status", nil))
	if !strings.Contains(status.Body.String(), "此连接已通过复测") {
		t.Fatalf("状态未显示复测通过: %s", status.Body.String())
	}
}

// TestAITestRejectsUnsavedInput 页面上未保存的 Key / 地址 / 模型一律拒绝，
// 不得用它们扩大本次外发范围。
func TestAITestRejectsUnsavedInput(t *testing.T) {
	cases := []struct {
		name, baseURL, modelID, apiKey, wantSub string
	}{
		{"未保存的 Key", "", "", aiTestPageKey, "不一致"},
		{"未保存的地址", "https://unsaved.invalid/v1", "", "", "尚未保存"},
		{"未保存的模型 ID", "", "model-page", "", "尚未保存"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vendor := newAITestVendor(t, 200, `{"choices":[{"message":{"content":"正常"}}]}`)
			srv, _ := newAITestFixture(t, vendor.srv.URL)
			rec, out := runAITest(t, srv, c.baseURL, c.modelID, c.apiKey)
			if out["ok"] != false {
				t.Fatalf("未保存输入应被拒绝: %s", rec.Body.String())
			}
			if got := fmt.Sprint(out["state"]); got != "unsaved_input" {
				t.Fatalf("state = %q，期望 unsaved_input", got)
			}
			if !strings.Contains(rec.Body.String(), c.wantSub) {
				t.Fatalf("拒绝原因应包含 %q: %s", c.wantSub, rec.Body.String())
			}
			if hits, _, _ := vendor.snapshot(); hits != 0 {
				t.Fatalf("未保存输入仍发出 %d 次请求", hits)
			}
			assertNoEcho(t, rec.Body.String(), vendor)
		})
	}
}

// TestAITestUsesNewPublication 保存新目的地后，复测只发到新域（旧域零请求），
// 响应里的修订与去处都是新的。
func TestAITestUsesNewPublication(t *testing.T) {
	vendorA := newAITestVendor(t, 200, `{"choices":[{"message":{"content":"A"}}]}`)
	vendorB := newAITestVendor(t, 200, `{"choices":[{"message":{"content":"B"}}]}`)
	srv, _ := newAITestFixture(t, vendorA.srv.URL)
	// 新域必须先落新 Key（换源留空整笔拒绝）。
	rec := postAs(t, srv, srv.handleAIConfigSave, "/api/ai/config",
		fmt.Sprintf(`{"base_url":%q,"model_id":"model-b","api_key":"sk-t010-b-999999","revision":1}`, vendorB.srv.URL), "tester")
	if rec.Code != 200 {
		t.Fatalf("保存新目的地失败 %d: %s", rec.Code, rec.Body.String())
	}
	withAITestTransport(t, vendorB.srv.Client().Transport)
	rec2, out := runAITest(t, srv, "", "", "")
	if out["ok"] != true {
		t.Fatalf("新域复测应成功: %s", rec2.Body.String())
	}
	if fmt.Sprint(out["revision"]) != "2" {
		t.Fatalf("复测修订 = %v，期望 2", out["revision"])
	}
	dest, _ := out["destination"].(map[string]any)
	if dest["model"] != "model-b" {
		t.Fatalf("复测去向 = %v，期望 model-b", dest)
	}
	if hits, _, _ := vendorA.snapshot(); hits != 0 {
		t.Fatalf("旧域收到 %d 次请求，期望 0", hits)
	}
	if hits, _, _ := vendorB.snapshot(); hits != 1 {
		t.Fatalf("新域请求次数 = %d，期望 1", hits)
	}
}

// TestAITestLateResponseDoesNotMarkNewConfig 覆盖「迟到的旧响应不得把新套标成已测」：
// 复测 A 在途时发布 B；A 的响应回来后，复测记录仍是 A 的修订，状态显示待复测。
func TestAITestLateResponseDoesNotMarkNewConfig(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var hits atomic.Int32
	vendorA := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		<-release
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"A"}}]}`)
	}))
	defer vendorA.Close()
	withAITestTransport(t, vendorA.Client().Transport)
	srv, _ := newAITestFixture(t, vendorA.URL)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := postAs(t, srv, srv.handleAITest, "/api/ai/test", `{}`, "tester")
		done <- rec
	}()
	<-started
	// 在途期间发布 B（同域换 Key 也产生新修订）。
	rec := postAs(t, srv, srv.handleAIConfigSave, "/api/ai/config",
		fmt.Sprintf(`{"base_url":%q,"model_id":"model-a","api_key":"sk-t010-new-inflight","revision":1}`, vendorA.URL), "tester")
	if rec.Code != 200 {
		t.Fatalf("在途发布失败 %d: %s", rec.Code, rec.Body.String())
	}
	close(release)
	late := <-done
	var out map[string]any
	if err := json.Unmarshal(late.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != false || out["state"] != "stale_retest" {
		t.Fatalf("旧修订复测应失效: %s", late.Body.String())
	}
	if fmt.Sprint(out["revision"]) != "1" {
		t.Fatalf("在途复测记录修订 = %v，期望 1", out["revision"])
	}
	status := httptest.NewRecorder()
	srv.handleAIStatus(status, httptest.NewRequest("GET", "/api/ai/status", nil))
	var body struct {
		Revision int64 `json:"revision"`
		Retest   map[string]any
	}
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Revision != 2 {
		t.Fatalf("活动修订 = %d，期望 2", body.Revision)
	}
	if body.Retest["current"] == true || body.Retest["display"] == "此连接已通过复测" {
		t.Fatalf("迟到的旧响应被标成新套已测: %v", body.Retest)
	}
	if hits.Load() != 1 {
		t.Fatalf("模拟厂商请求次数 = %d，期望 1", hits.Load())
	}
}

// TestAITestFailureHidesKeyAndAddress 厂商错误回显与连接失败信息都不得带出
// 明文 Key、完整地址或主机。
func TestAITestFailureHidesKeyAndAddress(t *testing.T) {
	t.Run("厂商回显密钥与地址", func(t *testing.T) {
		vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			_, _ = io.WriteString(w, fmt.Sprintf(`{"error":"bad key %s at https://%s/v1"}`, aiTestSavedKey, r.Host))
		}))
		defer vendor.Close()

		withAITestTransport(t, vendor.Client().Transport)
		srv, _ := newAITestFixtureWithHosts(t, vendor.URL, nil)
		rec, out := runAITest(t, srv, "", "", "")
		if out["ok"] != false {
			t.Fatalf("500 应返回失败: %s", rec.Body.String())
		}
		// 页面必须能显示实际去向（域），但不允许回显完整 URL 或明文 Key。
		for _, secret := range []string{aiTestSavedKey, vendor.URL} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("失败回显包含 %q: %s", secret, rec.Body.String())
			}
		}
	})

	t.Run("连接失败", func(t *testing.T) {
		closed := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedURL := closed.URL
		closedTransport := closed.Client().Transport
		closed.Close()
		withAITestTransport(t, closedTransport)
		srv, _ := newAITestFixtureWithHosts(t, closedURL, nil)
		rec, out := runAITest(t, srv, "", "", "")
		if out["ok"] != false {
			t.Fatalf("连接失败应返回 ok:false: %s", rec.Body.String())
		}
		// 页面必须能显示实际去向（域），但不允许回显完整 URL 或明文 Key。
		for _, secret := range []string{aiTestSavedKey, closedURL} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("连接失败回显包含 %q: %s", secret, rec.Body.String())
			}
		}
	})
}

func TestSupervisorT102RetestStopAndResumeBeforeSend(t *testing.T) {
	ctx := context.Background()
	vendor := newAITestVendor(t, 200, `{"choices":[{"message":{"content":"normal"}}]}`)
	srv, db := newAITestFixture(t, vendor.srv.URL)
	old := newAITestProvider
	t.Cleanup(func() { newAITestProvider = old })
	newAITestProvider = func(spec aibridge.ProviderSpec, timeout time.Duration) (*aibridge.OpenAICompatProvider, error) {
		for _, stop := range []bool{true, false} {
			epoch := db.AIEmergencyState(ctx).Epoch
			rec := postAs(t, srv, srv.handleAISettingsSave, "/api/ai/settings",
				fmt.Sprintf(`{"emergency_stop":%t,"emergency_stop_epoch":"%d"}`, stop, epoch), "tester")
			if rec.Code != 200 {
				t.Fatalf("emergency HTTP %d", rec.Code)
			}
		}
		return aibridge.NewProviderWithTransport(spec, timeout, vendor.srv.Client().Transport)
	}
	_, out := runAITest(t, srv, "", "", "")
	if hits, _, _ := vendor.snapshot(); hits != 0 || out["ok"] != false {
		t.Fatalf("pre-stop retest sent %d request(s) after stop/resume, ok=%v; want 0 and false", hits, out["ok"])
	}
}

func TestT102EmergencySettingsCompareAndSwap(t *testing.T) {
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	ctx := t.Context()
	base := db.AIEmergencyState(ctx).Epoch
	save := func(body string, code int, epoch int64) {
		t.Helper()
		out := postAs(t, srv, srv.handleAISettingsSave, "/api/ai/settings", body, "tester")
		if got := db.AIEmergencyState(ctx).Epoch; out.Code != code || got != epoch {
			t.Fatalf("save %s: HTTP %d epoch=%d, want %d/%d", body, out.Code, got, code, epoch)
		}
		if code == 409 && !strings.Contains(out.Body.String(), "刷新") {
			t.Fatalf("conflict lacks refresh hint: %s", out.Body)
		}
	}
	save(`{"emergency_stop":false}`, 200, base)
	save(`{"emergency_stop":true,"max_tool_rounds":4}`, 409, base)
	if got := db.AISetting(ctx, "max_tool_rounds", "2"); got == "4" {
		t.Fatal("stale switch save committed its unrelated setting")
	}
	save(fmt.Sprintf(`{"emergency_stop":true,"emergency_stop_epoch":"%d"}`, base), 200, base+1)
	save(fmt.Sprintf(`{"emergency_stop":true,"emergency_stop_epoch":%d}`, base), 200, base+1)
	save(fmt.Sprintf(`{"emergency_stop":false,"emergency_stop_epoch":%d}`, base), 409, base+1)
	save(fmt.Sprintf(`{"emergency_stop":false,"emergency_stop_epoch":"%d"}`, base+1), 200, base+2)
	save(`{"emergency_stop":false,"web_search_enabled":true}`, 200, base+2)
	if _, err := db.SQL.Exec(`CREATE TRIGGER reject_emergency_audit BEFORE INSERT ON audit_log
		WHEN NEW.action='ai_emergency_stop' BEGIN SELECT RAISE(ABORT,'test'); END`); err != nil {
		t.Fatal(err)
	}
	save(fmt.Sprintf(`{"emergency_stop":true,"emergency_stop_epoch":"%d"}`, base+2), 500, base+2)
	if got := db.AIEmergencyState(ctx); got.StopRaw != "0" {
		t.Fatalf("audit rollback changed switch: %+v", got)
	}
	var count int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='ai_emergency_stop'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("transition audit count=%d err=%v, want 2", count, err)
	}
	t.Logf("switch epoch %d → %d; 2 transition audits, stale/missing requests HTTP 409, audit failure HTTP 500", base, base+2)
}

func TestT102RetestOrderingAndCrossStop(t *testing.T) {
	for _, crossStop := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross_stop_%t", crossStop), func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			var hits atomic.Int32
			var once sync.Once
			vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) == 1 {
					close(started)
					<-release
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"normal"}}]}`)
			}))
			defer vendor.Close()
			defer once.Do(func() { close(release) })
			withAITestTransport(t, vendor.Client().Transport)
			srv, db := newAITestFixture(t, vendor.URL)
			old := make(chan *httptest.ResponseRecorder, 1)
			go func() { old <- postAs(t, srv, srv.handleAITest, "/api/ai/test", "{}", "tester") }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("old retest failed to reach vendor")
			}
			if crossStop {
				for i, stop := range []bool{true, false} {
					epoch := db.AIEmergencyState(t.Context()).Epoch
					rec := postAs(t, srv, srv.handleAISettingsSave, "/api/ai/settings",
						fmt.Sprintf(`{"emergency_stop":%t,"emergency_stop_epoch":"%d"}`, stop, epoch), "tester")
					if rec.Code != 200 {
						t.Fatalf("change %d returned HTTP %d", i, rec.Code)
					}
				}
			}
			_, fresh := runAITest(t, srv, "", "", "")
			if fresh["ok"] != true {
				t.Fatalf("fresh retest: %v", fresh)
			}
			once.Do(func() { close(release) })
			late := <-old
			if !strings.Contains(late.Body.String(), "stale_retest") {
				t.Fatalf("late retest: %s", late.Body)
			}
			_, reading, latest, err := db.AIConnectionState(t.Context())
			if err != nil || !latest.OK || latest.Pending || latest.Attempt != fresh["attempt"] ||
				latest.Epoch != reading.Epoch || hits.Load() != 2 {
				t.Fatalf("latest result lost: %+v, requests=%d err=%v", latest, hits.Load(), err)
			}
			t.Logf("2 vendor requests; late attempt invalidated; epoch=%d revision=%d current success", latest.Epoch, latest.Revision)
		})
	}
}
