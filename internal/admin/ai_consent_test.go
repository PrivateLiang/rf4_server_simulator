package admin

// 旧 consent / 旧闸的兼容语义与「发布不影响在途传输」的独立真值。
// 全部为合成数据 + 本地 SQLite，不连接任何真实厂商。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// 手写授权记录：独立真值，不使用被测构造函数生成。
const grantedOutboundJSON = `{"schema":1,"item":"outbound","state":"granted","subject":"tester","ts":1700000000,` +
	`"scope":{"content_version":1,"contents":["chat_text","player_identity","fishing_records","memory_summary"],` +
	`"dest_base_url":"https://vendor.invalid/v1","dest_model":"model-a"}}`

// adminSaveKey 是测试用的合成 Key；只存在于临时目录的密文对象里。
const adminSaveKey = "sk-synthetic-admin-key-0001"

func consentServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	t.Cleanup(aibridge.SetCredentialDirSyncForTest(func(string) error { return nil }))
	t.Setenv("RF4_AI_MASTER_KEY_FILE", filepath.Join(t.TempDir(), "ai_master.key"))
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	return srv, db
}

// postAs 用管理员主体调用 handler。生产环境由 auth 中间件注入 ctxAdmin；
// 这里直接注入同名 context key，避免绕过真实 handler 逻辑。
func postAs(t *testing.T, srv *Server, handler func(w http.ResponseWriter, r *http.Request), target, body, subject string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxAdmin{}, subject))
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// saveProviderConfig 走真实保存入口；新 Key 先落不可变对象再发布。
func saveProviderConfig(t *testing.T, srv *Server, baseURL, modelID string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"base_url":%q,"model_id":%q,"api_key":%q}`, baseURL, modelID, adminSaveKey)
	rec := postAs(t, srv, srv.handleAIConfigSave, "/api/ai/config", body, "tester")
	if rec.Code != 200 {
		t.Fatalf("保存厂商配置失败 %d: %s", rec.Code, rec.Body.String())
	}
	return rec
}

func confirmConsent(t *testing.T, srv *Server, item string) *httptest.ResponseRecorder {
	t.Helper()
	return postAs(t, srv, srv.handleAIConsentConfirm, "/api/ai/consent/confirm",
		fmt.Sprintf(`{"item":%q}`, item), "tester")
}

func revokeConsent(t *testing.T, srv *Server, item string) *httptest.ResponseRecorder {
	t.Helper()
	return postAs(t, srv, srv.handleAIConsentRevoke, "/api/ai/consent/revoke",
		fmt.Sprintf(`{"item":%q}`, item), "tester")
}

func egressNow(db *store.DB) aiconfig.EgressStatus {
	ctx := context.Background()
	return aiconfig.Egress(func(key string) string { return db.AISetting(ctx, key, "") })
}

// TestLegacyConsentNoLongerGates 历史库形态（从未保存、整卡保存过、审计只有键名、
// 已撤销、伪造记录）都不再阻止外发；只有急停会。
func TestLegacyConsentNoLongerGates(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, srv *Server, db *store.DB)
	}{
		{"从未保存过", nil},
		{"曾整卡保存过", func(t *testing.T, srv *Server, db *store.DB) {
			body := `{"outbound_enabled":true,"web_search_enabled":false,"emergency_stop":false,` +
				`"search_endpoint":"","max_tool_rounds":2}`
			if rec := postAs(t, srv, srv.handleAISettingsSave, "/api/ai/settings", body, "tester"); rec.Code != 200 {
				t.Fatalf("整卡保存失败 %d: %s", rec.Code, rec.Body.String())
			}
		}},
		{"审计只有键名", func(t *testing.T, _ *Server, db *store.DB) {
			_ = db.AuditLogWrite(ctx, "tester", "ai_settings", "global", "keys=outbound_enabled,ai_actor_enabled")
		}},
		{"撤销的旧授权", func(t *testing.T, _ *Server, db *store.DB) {
			if err := db.AISetSettings(ctx, map[string]string{
				aiconfig.KeyConsentOutbound: `{"schema":1,"item":"outbound","state":"revoked","subject":"tester","ts":1700000001}`,
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"伪造记录", func(t *testing.T, _ *Server, db *store.DB) {
			if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyConsentOutbound: "1"}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, db := consentServer(t)
			if c.setup != nil {
				c.setup(t, srv, db)
			}
			if got := egressNow(db); !got.Allowed {
				t.Fatalf("旧闸状态不应阻止发送，得到 (%v,%q)", got.Allowed, got.State)
			}
			// 急停仍然有效。
			if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
				t.Fatal(err)
			}
			if got := egressNow(db); got.Allowed || got.State != aiconfig.StateEmergency {
				t.Fatalf("急停时得到 (%v,%q)，期望 (false,emergency)", got.Allowed, got.State)
			}
		})
	}
}

// TestConsentCompatEndpointsStillWork 旧 confirm/revoke 仍鉴权、仍写旧格式记录；
// 重复确认幂等；都不改变 V2 的运行资格。
func TestConsentCompatEndpointsStillWork(t *testing.T) {
	ctx := context.Background()
	srv, db := consentServer(t)
	saveProviderConfig(t, srv, "https://vendor.invalid/v1", "model-a")

	if rec := confirmConsent(t, srv, aiconfig.ConsentItemOutbound); rec.Code != 200 {
		t.Fatalf("确认失败 %d: %s", rec.Code, rec.Body.String())
	}
	raw := db.AISetting(ctx, aiconfig.KeyConsentOutbound, "")
	rec, err := aiconfig.ParseConsent(raw)
	if err != nil || rec.State != aiconfig.ConsentGranted || rec.Subject != "tester" || rec.TS <= 0 {
		t.Fatalf("旧格式记录 = %+v, err=%v", rec, err)
	}
	var before float64
	if err := db.SQL.QueryRow(`SELECT updated_ts FROM ai_setting WHERE key=?`, aiconfig.KeyConsentOutbound).Scan(&before); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if rec := confirmConsent(t, srv, aiconfig.ConsentItemOutbound); rec.Code != 200 {
		t.Fatalf("重复确认失败 %d", rec.Code)
	}
	var after float64
	if err := db.SQL.QueryRow(`SELECT updated_ts FROM ai_setting WHERE key=?`, aiconfig.KeyConsentOutbound).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("重复确认重写了记录: %v → %v", before, after)
	}
	if got := egressNow(db); !got.Allowed {
		t.Fatalf("确认旧授权不得改变 V2 资格: %+v", got)
	}

	if rec := revokeConsent(t, srv, aiconfig.ConsentItemOutbound); rec.Code != 200 {
		t.Fatalf("撤销失败 %d", rec.Code)
	}
	if rev, err := aiconfig.ParseConsent(db.AISetting(ctx, aiconfig.KeyConsentOutbound, "")); err != nil || rev.State != aiconfig.ConsentRevoked {
		t.Fatalf("撤销记录 = %+v, err=%v", rev, err)
	}
	if got := egressNow(db); !got.Allowed {
		t.Fatalf("撤销旧授权不得改变 V2 资格: %+v", got)
	}

	// 未鉴权主体仍被拒绝；未知项仍是 400。
	if rec := postAs(t, srv, srv.handleAIConsentConfirm, "/api/ai/consent/confirm", `{"item":"outbound"}`, ""); rec.Code != 403 {
		t.Fatalf("缺主体确认 = %d，期望 403", rec.Code)
	}
	if rec := confirmConsent(t, srv, "no_such_item"); rec.Code != 400 {
		t.Fatalf("未知授权项 = %d，期望 400", rec.Code)
	}
}

// TestSettingsSaveCannotForgeProtectedMetadata 普通整卡保存必须整笔拒收
// 授权/发布元数据，避免一次「保存卡片」伪造发布或授权。
func TestSettingsSaveCannotForgeProtectedMetadata(t *testing.T) {
	for _, key := range []string{
		aiconfig.KeyConsentOutbound,
		aiconfig.KeyEgressDestination,
		aiconfig.KeyConfigActive,
		aiconfig.KeyConfigRetest,
		aiconfig.KeyEmergencyStopLiftedTS,
		aiconfig.ConfigRecordKey(3),
	} {
		t.Run(key, func(t *testing.T) {
			srv, db := consentServer(t)
			body := fmt.Sprintf(`{"%s":"forged","emergency_stop":true}`, key)
			rec := postAs(t, srv, srv.handleAISettingsSave, "/api/ai/settings", body, "tester")
			if rec.Code != 400 {
				t.Fatalf("保存受保护键 %q 返回 %d，期望 400: %s", key, rec.Code, rec.Body.String())
			}
			if got := db.AISetting(context.Background(), key, ""); got != "" {
				t.Fatalf("受保护键 %q 被写入: %q", key, got)
			}
			// 整笔拒收：同请求里的普通键也不能落库。
			if got := db.AISetting(context.Background(), aiconfig.KeyEmergencyStop, ""); got == "1" {
				t.Fatalf("受保护键拒收时普通键仍被写入: emergency_stop=%q", got)
			}
		})
	}
}

// TestInFlightCompletesAfterPublication 旧套已读取的在途请求可以完成；
// 提交后的新发送（重试/续轮）用旧修订会被拒绝。
func TestInFlightCompletesAfterPublication(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var hits int32
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		<-release
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"旧套在途完成"}}]}`)
	}))
	defer vendor.Close()

	srv, db := consentServer(t)
	saveProviderConfig(t, srv, vendor.URL, "model-a")
	masterKey, err := aibridge.LoadMasterKey(aiMasterKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := aibridge.LoadSnapshot(ctx, db, aiMasterKeyPath(), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := aibridge.NewProviderWithTransport(snap.Spec, 5*time.Second, vendor.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	rev := snap.Revision()
	provider.SetEgressGate(func(gctx context.Context) error {
		return aibridge.Gate(gctx, db, rev)
	})
	done := make(chan error, 1)
	go func() {
		_, err := provider.Chat(ctx, aibridge.ChatRequest{
			Messages: []aibridge.Message{{Role: "user", Content: "ping"}},
		})
		done <- err
	}()
	<-started

	// 在途期间发布新修订（同域换 Key）。
	if rec := postAs(t, srv, srv.handleAIConfigSave, "/api/ai/config",
		fmt.Sprintf(`{"base_url":%q,"model_id":"model-a","api_key":"sk-synthetic-inflight-new","revision":%d}`, vendor.URL, rev), "tester"); rec.Code != 200 {
		t.Fatalf("在途发布失败 %d: %s", rec.Code, rec.Body.String())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("已读取的完整旧套应能完成在途传输: %v", err)
	}
	if hits != 1 {
		t.Fatalf("在途请求次数 = %d，期望 1", hits)
	}
	// 旧 provider 的下一次发送必须被「活动修订已变化」拦住。
	_, err = provider.Chat(ctx, aibridge.ChatRequest{
		Messages: []aibridge.Message{{Role: "user", Content: "retry"}},
	})
	var stale *aibridge.StaleConfigError
	if !errors.As(err, &stale) {
		t.Fatalf("旧修订的续轮应返回 stale，得到 %v", err)
	}
	if hits != 1 {
		t.Fatalf("陈旧续轮仍发出请求：累计 %d 次", hits)
	}
}
