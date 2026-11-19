package aibridge

// 三个出口共用的传输层防护与旧部署名单兼容的独立真值（V2 / DEV-104）：
//   - 旧部署主机名单不再参与放行：任意 HTTPS 主机都能发送；
//   - HTTPS、无 userinfo、合法主机名仍然强制；
//   - 重定向逐跳重验 URL，且不向另一主机转发鉴权头；
//   - 连接阶段仍只拨校验过的公网 IP，并禁用环境变量代理。
//
// 所有 fixture 都是本地 httptest，不连接任何真实主机。

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
)

// egressFixtureTransport 只记录请求，不建立任何连接；用于证明「被拒绝时
// 底层 Transport 没有被调用」。
type egressFixtureTransport struct {
	mu       sync.Mutex
	hits     int
	lastAuth string
	lastXKey string
	reqURLs  []string
}

func (f *egressFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.hits++
	f.lastAuth = req.Header.Get("Authorization")
	f.lastXKey = req.Header.Get("x-api-key")
	f.reqURLs = append(f.reqURLs, req.URL.String())
	f.mu.Unlock()
	return &http.Response{
		StatusCode: 200, Status: "200 OK",
		Header:  http.Header{"Content-Type": []string{"application/json"}},
		Body:    io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"}}]}`)),
		Request: req,
	}, nil
}

func (f *egressFixtureTransport) snapshot() (hits int, auth, xkey string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, f.lastAuth, f.lastXKey
}

func chatOnce(t *testing.T, p *OpenAICompatProvider) error {
	t.Helper()
	_, err := p.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Content: "ping"}},
	})
	return err
}

// TestProviderURLShapeDeniedBeforeTransport 旧名单不再拒绝任意 HTTPS 主机；
// 但 HTTP 明文与 userinfo 仍在发送前被拒绝，底层 Transport 一次都不调用。
func TestProviderURLShapeDeniedBeforeTransport(t *testing.T) {
	cases := []struct {
		name      string
		baseURL   string
		wantState string
		wantHits  int
	}{
		{"任意 HTTPS 主机放行", "https://api.never-listed.example/v1", "", 1},
		{"HTTP 明文", "http://api.example.com/v1", aiconfig.StateEgressSchemeDenied, 0},
		{"userinfo", "https://user:pass@api.example.com/v1", aiconfig.StateEgressUserinfoDenied, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := &egressFixtureTransport{}
			p, err := NewProviderWithTransport(ProviderSpec{
				Name: "sim", Protocol: "openai_chat", BaseURL: c.baseURL,
				ModelID: "model-a", APIKey: "sk-synthetic-host",
			}, time.Second, base)
			if err != nil {
				t.Fatal(err)
			}
			err = chatOnce(t, p)
			if c.wantState == "" {
				if err != nil {
					t.Fatalf("应放行，得到 %v", err)
				}
			} else {
				var denied *aiconfig.HostPolicyError
				if !errors.As(err, &denied) {
					t.Fatalf("应返回 *aiconfig.HostPolicyError，得到 %v", err)
				}
				if denied.State != c.wantState {
					t.Fatalf("state = %q，期望 %q", denied.State, c.wantState)
				}
			}
			if hits, _, _ := base.snapshot(); hits != c.wantHits {
				t.Fatalf("底层 Transport 请求次数 = %d，期望 %d", hits, c.wantHits)
			}
		})
	}
}

// tlsTransportFor 构造一个信任给定 fixture 证书的 Transport，供重定向用例
// 在两个本地 TLS fixture 之间跳转。
func tlsTransportFor(t *testing.T, servers ...*httptest.Server) *http.Transport {
	t.Helper()
	pool := x509.NewCertPool()
	for _, srv := range servers {
		pool.AddCert(srv.Certificate())
	}
	return &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
}

type redirectRecorder struct {
	mu    sync.Mutex
	hits  int
	auths []string
	xkeys []string
}

func (r *redirectRecorder) handler(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits++
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.xkeys = append(r.xkeys, req.Header.Get("x-api-key"))
		r.mu.Unlock()
		if target != "" {
			http.Redirect(w, req, target, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}
}

func (r *redirectRecorder) snapshot() (hits int, auths, xkeys []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits, append([]string(nil), r.auths...), append([]string(nil), r.xkeys...)
}

// TestProviderRedirectStripsAuthAcrossHosts 跨主机重定向允许发生（旧名单不再是闸），
// 但鉴权只属于最初的主机：第二跳不得带上 Authorization / x-api-key。
func TestProviderRedirectStripsAuthAcrossHosts(t *testing.T) {
	auths := []struct{ name, scheme string }{
		{"bearer", "bearer"},
		{"x-api-key", "x-api-key"},
	}
	for _, auth := range auths {
		t.Run(auth.name, func(t *testing.T) {
			target := &redirectRecorder{}
			targetSrv := httptest.NewTLSServer(target.handler(""))
			defer targetSrv.Close()
			source := &redirectRecorder{}
			sourceSrv := httptest.NewTLSServer(source.handler(targetSrv.URL + "/next"))
			defer sourceSrv.Close()

			p, err := NewProviderWithTransport(ProviderSpec{
				Name: "sim", Protocol: "openai_chat", BaseURL: sourceSrv.URL,
				ModelID: "model-a", APIKey: "sk-synthetic-host", AuthScheme: auth.scheme,
			}, 2*time.Second, tlsTransportFor(t, sourceSrv, targetSrv))
			if err != nil {
				t.Fatal(err)
			}
			if err := chatOnce(t, p); err != nil {
				t.Fatalf("重定向到另一主机应放行: %v", err)
			}
			if hits, _, _ := source.snapshot(); hits != 1 {
				t.Fatalf("第一跳请求次数 = %d，期望 1", hits)
			}
			hits, authsSeen, xkeysSeen := target.snapshot()
			if hits != 1 {
				t.Fatalf("第二跳请求次数 = %d，期望 1", hits)
			}
			if authsSeen[0] != "" || xkeysSeen[0] != "" {
				t.Fatalf("跨主机重定向泄露鉴权头: auth=%q xkey=%q", authsSeen[0], xkeysSeen[0])
			}
		})
	}
}

// TestProviderRedirectToHTTPSRejected 重定向到非 HTTPS 地址时，第二跳不得发出。
func TestProviderRedirectToHTTPSRejected(t *testing.T) {
	target := &redirectRecorder{}
	targetSrv := httptest.NewServer(target.handler(""))
	defer targetSrv.Close()
	source := &redirectRecorder{}
	sourceSrv := httptest.NewTLSServer(source.handler(targetSrv.URL + "/next"))
	defer sourceSrv.Close()

	p, err := NewProviderWithTransport(ProviderSpec{
		Name: "sim", Protocol: "openai_chat", BaseURL: sourceSrv.URL,
		ModelID: "model-a", APIKey: "sk-synthetic-host",
	}, 2*time.Second, tlsTransportFor(t, sourceSrv))
	if err != nil {
		t.Fatal(err)
	}
	if err := chatOnce(t, p); err == nil {
		t.Fatal("重定向到 HTTP 明文应被拒绝")
	}
	if hits, _, _ := source.snapshot(); hits != 1 {
		t.Fatalf("第一跳请求次数 = %d，期望 1", hits)
	}
	if hits, _, _ := target.snapshot(); hits != 0 {
		t.Fatalf("非 HTTPS 重定向目标被请求 %d 次，期望 0", hits)
	}
}

// ---- 连接阶段地址检查 ----

type resolverSpy struct {
	mu    sync.Mutex
	calls int
	addrs []netip.Addr
}

func (r *resolverSpy) resolve(_ context.Context, _ string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return append([]netip.Addr(nil), r.addrs...), nil
}

func (r *resolverSpy) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type dialerSpy struct {
	mu    sync.Mutex
	calls int
	addrs []string
	err   error
}

func (d *dialerSpy) dial(_ context.Context, _ string, addr string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.addrs = append(d.addrs, addr)
	if d.err != nil {
		return nil, d.err
	}
	return nil, errors.New("测试拨号器不建立真实连接")
}

func (d *dialerSpy) snapshot() (int, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls, append([]string(nil), d.addrs...)
}

func addrsOf(t *testing.T, raw ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, 0, len(raw))
	for _, s := range raw {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ip)
	}
	return out
}

// TestSafeTransportRejectsNonPublicResolution 连接阶段检查实际解析到的地址：
// 内网/环回/链路本地在拨号前拒绝，底层拨号器一次都不调用。
func TestSafeTransportRejectsNonPublicResolution(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		addrs []string
	}{
		{"解析到内网", "https://api.example.test/", []string{"10.1.2.3"}},
		{"解析到环回", "https://api.example.test/", []string{"127.0.0.1"}},
		{"解析到链路本地", "https://api.example.test/", []string{"169.254.10.1"}},
		{"IPv6 私网", "https://api.example.test/", []string{"fd00::1"}},
		{"混合公网与内网", "https://api.example.test/", []string{"8.8.8.8", "192.168.1.5"}},
		{"IP 字面量环回", "https://127.0.0.1/", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resolver := &resolverSpy{addrs: addrsOf(t, c.addrs...)}
			dialer := &dialerSpy{}
			client := newEgressHTTPClient(time.Second, newSafeTransportWith(resolver.resolve, dialer.dial))
			_, err := client.Get(c.url)
			var denied *aiconfig.HostPolicyError
			if !errors.As(err, &denied) || denied.State != aiconfig.StateEgressAddrDenied {
				t.Fatalf("应返回 %q，得到 %v", aiconfig.StateEgressAddrDenied, err)
			}
			if calls, _ := dialer.snapshot(); calls != 0 {
				t.Fatalf("解析到非公网地址后仍调用了拨号器 %d 次", calls)
			}
			if c.addrs != nil && resolver.count() != 1 {
				t.Fatalf("解析次数 = %d，期望 1", resolver.count())
			}
		})
	}
}

// TestSafeTransportDialsValidatedIPOnly 只拨校验过的 IP（不把主机名交给
// 第二次解析），这是防 DNS 重绑定的关键。
func TestSafeTransportDialsValidatedIPOnly(t *testing.T) {
	resolver := &resolverSpy{addrs: addrsOf(t, "1.2.3.4", "5.6.7.8")}
	dialer := &dialerSpy{}
	client := newEgressHTTPClient(time.Second, newSafeTransportWith(resolver.resolve, dialer.dial))
	if _, err := client.Get("https://api.example.test/"); err == nil {
		t.Fatal("拨号器返回错误时应得到失败")
	}
	if resolver.count() != 1 {
		t.Fatalf("解析次数 = %d，期望 1", resolver.count())
	}
	calls, addrs := dialer.snapshot()
	if calls != 2 {
		t.Fatalf("拨号次数 = %d，期望按解析结果逐个尝试 2 次", calls)
	}
	for i, want := range []string{"1.2.3.4:443", "5.6.7.8:443"} {
		if addrs[i] != want {
			t.Fatalf("拨号地址[%d] = %q，期望 %q", i, addrs[i], want)
		}
	}
	if strings.Contains(strings.Join(addrs, ","), "api.example.test") {
		t.Fatalf("拨号器收到了未校验的主机名: %v", addrs)
	}
}

// TestSafeTransportDisablesEnvProxy 显式禁用代理，不允许 HTTP(S)_PROXY
// 把已校验的连接改道。
func TestSafeTransportDisablesEnvProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	resolver := &resolverSpy{addrs: addrsOf(t, "1.1.1.1")}
	dialer := &dialerSpy{}
	tr := newSafeTransportWith(resolver.resolve, dialer.dial)
	if tr.Proxy != nil {
		t.Fatal("安全 Transport 不应使用代理")
	}
	client := newEgressHTTPClient(time.Second, tr)
	if _, err := client.Get("https://api.example.test/"); err == nil {
		t.Fatal("拨号器返回错误时应得到失败")
	}
	_, addrs := dialer.snapshot()
	if len(addrs) != 1 || addrs[0] != "1.1.1.1:443" {
		t.Fatalf("拨号地址 = %v，期望直连校验过的 1.1.1.1:443", addrs)
	}
}

// ---- 搜索出口 ----

// TestKnowledgeSearchURLShape 搜索出口与模型出口共用同一套传输防护：
// 任意 HTTPS 主机可搜索；HTTP/userinfo 被拒绝时零请求；急停时零请求。
func TestKnowledgeSearchURLShape(t *testing.T) {
	ctx := context.Background()
	var allowedHits int32
	allowed := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&allowedHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"title":"t","snippet":"s"}]}`)
	}))
	defer allowed.Close()

	k := &Knowledge{}
	k.SetEgressGate(func(context.Context) error { return nil })
	k.client = newEgressHTTPClient(time.Second, allowed.Client().Transport)
	if got := k.WebSearch(ctx, "鲈鱼 123456789", allowed.URL); got == "" {
		t.Fatal("任意 HTTPS 端点都应可搜索（旧名单不再是闸）")
	}
	if got := atomic.LoadInt32(&allowedHits); got != 1 {
		t.Fatalf("本地 fixture 请求次数 = %d，期望 1", got)
	}

	base := &egressFixtureTransport{}
	k2 := &Knowledge{}
	k2.SetEgressGate(func(context.Context) error { return nil })
	k2.client = newEgressHTTPClient(time.Second, base)
	for _, endpoint := range []string{
		"http://127.0.0.1:1/search",           // 非 HTTPS
		"https://allowed.invalid@evil.test/x", // userinfo
	} {
		if got := k2.WebSearch(ctx, "鲈鱼", endpoint); got != "" {
			t.Fatalf("被拒绝的搜索端点 %q 仍返回结果 %q", endpoint, got)
		}
	}
	if hits, _, _ := base.snapshot(); hits != 0 {
		t.Fatalf("被拒绝的搜索仍发出 %d 次请求", hits)
	}

	k3 := &Knowledge{}
	k3.SetEgressGate(func(context.Context) error {
		return &EgressDeniedError{State: aiconfig.StateEmergency, Display: "关（紧急停止）"}
	})
	base3 := &egressFixtureTransport{}
	k3.client = newEgressHTTPClient(time.Second, base3)
	if got := k3.WebSearch(ctx, "鲈鱼", allowed.URL); got != "" {
		t.Fatalf("急停时搜索应返回空串，得到 %q", got)
	}
	if hits, _, _ := base3.snapshot(); hits != 0 {
		t.Fatalf("急停后搜索仍发出 %d 次请求", hits)
	}
}
