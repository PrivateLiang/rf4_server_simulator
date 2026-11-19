package admin

// V2（DEV-104）：部署出口主机名单不再参与放行；这里锁定
//   - 真正发送前仍强制 HTTPS（旧 http 配置在复测时被拒绝且零外发）；
//   - 后台保存 URL 不会改写旧名单，也不会因此获得/失去放行资格；
//   - 状态接口仍按原字段展示旧名单，但明确标注「不参与放行」。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
)

// TestAITestHTTPAddressDeniedBeforeConnect 旧配置里可能保存 http 地址：
// 保存不阻止，但复测在连接前按 HTTPS 拒绝，模拟厂商零请求。
func TestAITestHTTPAddressDeniedBeforeConnect(t *testing.T) {
	var hits int32
	httpVendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"不应到达"}}]}`))
	}))
	t.Cleanup(httpVendor.Close)
	srv, _ := newAITestFixtureWithHosts(t, httpVendor.URL, nil)
	rec, out := runAITest(t, srv, "", "", "")
	if out["ok"] != false {
		t.Fatalf("HTTP 地址应被拒绝: %s", rec.Body.String())
	}
	if got := fmt.Sprint(out["state"]); got != aiconfig.StateEgressSchemeDenied {
		t.Fatalf("state = %q，期望 %q", got, aiconfig.StateEgressSchemeDenied)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("被拒绝后 HTTP 端点收到 %d 次请求", got)
	}
	if !strings.Contains(rec.Body.String(), "HTTPS") {
		t.Fatalf("拒绝原因未说明 HTTPS: %s", rec.Body.String())
	}
}

// TestAIConfigSaveDoesNotChangeLegacyHostList 后台保存 URL 只刷新活动配置，
// 不改写旧部署名单；旧名单也不再是放行条件（未列出的主机可正常复测）。
func TestAIConfigSaveDoesNotChangeLegacyHostList(t *testing.T) {
	vendor := newAITestVendor(t, 200, `{"choices":[{"message":{"content":"正常"}}]}`)
	withAITestTransport(t, vendor.srv.Client().Transport)
	srv, _ := newAITestFixtureWithHosts(t, vendor.srv.URL, []string{"https://allowed.example"})
	before := aiconfig.DescribeEgressHosts(srv.aiEgressHostPolicy())
	saveProviderConfig(t, srv, vendor.srv.URL, "model-a")
	after := aiconfig.DescribeEgressHosts(srv.aiEgressHostPolicy())
	if before.Configured != after.Configured || srv.aiEgressHostPolicy().Allows("127.0.0.1", "443") {
		t.Fatalf("部署旧名单不应被后台保存改写: before=%+v after=%+v", before, after)
	}
	rec, out := runAITest(t, srv, "", "", "")
	if out["ok"] != true {
		t.Fatalf("旧名单未列出的主机也应能复测: %s", rec.Body.String())
	}
	if hits, _, _ := vendor.snapshot(); hits != 1 {
		t.Fatalf("模拟厂商请求次数 = %d，期望 1", hits)
	}
}

// TestAIStatusShowsLegacyHostListAsInformational 状态接口仍按原字段展示旧名单，
// 但明确标注不参与放行。
func TestAIStatusShowsLegacyHostListAsInformational(t *testing.T) {
	parse := func(t *testing.T, srv *Server) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/ai/status", nil)
		rec := httptest.NewRecorder()
		srv.handleAIStatus(rec, req)
		if rec.Code != 200 {
			t.Fatalf("状态接口返回 %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Security struct {
				EgressHosts struct {
					Configured int    `json:"configured"`
					Display    string `json:"display"`
				} `json:"egress_hosts"`
			} `json:"security"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析状态响应: %v: %s", err, rec.Body.String())
		}
		return body.Security.EgressHosts.Configured, body.Security.EgressHosts.Display
	}

	empty := NewServer(config.Admin{JWTSecret: "test"}, testDB(t), logging.Discard(), WebFS())
	if got, display := parse(t, empty); got != 0 || !strings.Contains(display, "不参与放行") {
		t.Fatalf("空旧名单状态 = (%d,%q)", got, display)
	}

	withHosts := NewServer(config.Admin{
		JWTSecret:            "test",
		AIEgressAllowedHosts: []string{"api.deepseek.com", "https://search.example.com:443"},
	}, testDB(t), logging.Discard(), WebFS())
	if got, display := parse(t, withHosts); got != 2 || !strings.Contains(display, "不参与放行") {
		t.Fatalf("两条旧名单状态 = (%d,%q)", got, display)
	}
}
