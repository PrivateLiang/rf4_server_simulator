package aibridge

// 出口前检查的独立真值（V2：只留急停）。模拟厂商用本地 httptest，
// 全部断言「请求次数」，不连接任何真实厂商。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

func gateLookup(extra map[string]string) aiconfig.Lookup {
	values := map[string]string{
		aiconfig.KeyOutboundEnabled: "0",
		// 旧的授权记录与目的地快照仍在库里，但不再参与放行。
		aiconfig.KeyConsentOutbound:   `{"schema":1,"item":"outbound","state":"revoked","subject":"tester","ts":1700000000}`,
		aiconfig.KeyEgressDestination: `{"base_url":"https://stale.invalid/v1","model_id":"old-model"}`,
	}
	for k, v := range extra {
		values[k] = v
	}
	return func(key string) string { return values[key] }
}

// TestCheckEgressBeforeSendOnlyEmergency 运行期与后台复测共用的判定：
// 只有急停拒绝；旧开关、撤销的 consent、过期的目的地快照都不再拒绝。
func TestCheckEgressBeforeSendOnlyEmergency(t *testing.T) {
	spec := ProviderSpec{BaseURL: "https://vendor.invalid/v1", ModelID: "model-a"}
	cases := []struct {
		name      string
		extra     map[string]string
		allowed   bool
		wantState string
	}{
		{"旧闸全关也放行", nil, true, ""},
		{"历史值1放行", map[string]string{aiconfig.KeyOutboundEnabled: "1"}, true, ""},
		{"急停拒绝", map[string]string{aiconfig.KeyEmergencyStop: "1"}, false, aiconfig.StateEmergency},
		{"急停非法值保守停止", map[string]string{aiconfig.KeyEmergencyStop: "yes"}, false, aiconfig.StateEmergency},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckEgressBeforeSend(spec, gateLookup(c.extra))
			if c.allowed {
				if err != nil {
					t.Fatalf("应放行，得到错误: %v", err)
				}
				return
			}
			var denied *EgressDeniedError
			if !errors.As(err, &denied) {
				t.Fatalf("应返回 *EgressDeniedError，得到 %v", err)
			}
			if denied.State != c.wantState {
				t.Fatalf("State = %q，期望 %q", denied.State, c.wantState)
			}
		})
	}
}

// TestProviderEgressGateBlocksBeforeSend 每次真正发送前都重查：
// 第一次放行后把判定改成拒绝，第二次调用不得再产生 HTTP 请求。
func TestProviderEgressGateBlocksBeforeSend(t *testing.T) {
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	p, err := NewProviderWithTransport(ProviderSpec{
		Name: "sim", Protocol: "openai_chat", BaseURL: srv.URL,
		ModelID: "model-a", APIKey: "sk-synthetic-gate",
	}, 2*time.Second, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	var calls int32
	p.SetEgressGate(func(context.Context) error {
		if atomic.AddInt32(&calls, 1) > 1 {
			return &EgressDeniedError{State: aiconfig.StateEmergency, Display: "关（紧急停止）"}
		}
		return nil
	})
	req := ChatRequest{Messages: []Message{{Role: "user", Content: "ping"}}}
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("第一次应放行: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("第一次请求次数 = %d，期望 1", got)
	}
	_, err = p.Chat(context.Background(), req)
	var denied *EgressDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("第二次应被出口检查拒绝，得到 %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("被拒绝后仍发出请求：累计 %d 次，期望 1", got)
	}
}

// newGateTestDB 准备一张可外发的旧安装形态库：本地厂商、合成密钥。
// 没有活动发布 → 走旧安装只读兼容路径。
func newGateTestDB(t *testing.T, vendorURL string) (*store.DB, []byte) {
	t.Helper()
	ctx := context.Background()
	d, err := store.Open(filepath.Join(t.TempDir(), "egress-gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_provider SET base_url=? WHERE name='deepseek'`, vendorURL); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx,
		`UPDATE ai_model SET model_id='model-a' WHERE provider_id=(SELECT id FROM ai_provider WHERE name='deepseek')`); err != nil {
		t.Fatal(err)
	}
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	ciphertext, nonce, err := Encrypt(masterKey, []byte("sk-synthetic-gate"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `
		INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES((SELECT id FROM ai_provider WHERE name='deepseek'),?,?,'default','****gate',0)
		ON CONFLICT(provider_id) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce`,
		ciphertext, nonce); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{
		aiconfig.KeyEmergencyStop: "0",
	}); err != nil {
		t.Fatal(err)
	}
	return d, masterKey
}

// TestRunnerRechecksEmergencyMidTask 任务开始时的判定不能替代出口前的重查：
// 第一次请求成功后触发急停，第二轮（工具续轮）真正发送前必须被拦住。
func TestRunnerRechecksEmergencyMidTask(t *testing.T) {
	ctx := context.Background()
	var hits int32
	var emergency func() error
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			// 第一轮先触发急停；第二轮必须被 Provider 的出口检查拦住。
			if emergency != nil {
				_ = emergency()
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"species_lookup","arguments":"{\"fish\":\"鲈鱼\"}"}}]}}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"不应到达"}}]}`)
	}))
	defer vendor.Close()

	d, masterKey := newGateTestDB(t, vendor.URL)
	emergency = func() error {
		return d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"})
	}

	taskID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
		Agent: "fishguide", ContactUID: 2147483644,
		SourceKind: "human", Text: "鲈鱼在哪钓",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	r.handle(ctx, task)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("模拟厂商请求次数 = %d，期望 1（第二轮应被急停拦住）", got)
	}
	var status, errText string
	if err := d.SQL.QueryRowContext(ctx,
		`SELECT status, err FROM ai_result WHERE task_id=?`, taskID).Scan(&status, &errText); err != nil {
		t.Fatalf("读取结果失败: %v", err)
	}
	if status != "fallback" {
		t.Fatalf("任务状态 = %q，期望 fallback", status)
	}
	// 在途回复返回后先按原任务复核，再处理工具调用：这比等第二轮发送前
	// 才被 Gate 拦住更早，工具作业也不会产生。
	if !strings.Contains(errText, "emergency_stop") {
		t.Fatalf("错误文本未记录急停原因: %q", errText)
	}
	var jobs int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_tool_job WHERE task_id=?`, taskID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("急停生效后仍排了 %d 个工具作业", jobs)
	}
	if strings.Contains(errText, "sk-synthetic-gate") {
		t.Fatalf("错误文本泄露密钥: %q", errText)
	}
}

// TestRunnerWiresSearchEgressGate 联网搜索出口接入同一份急停判定：
// 未急停时搜索发出请求；急停后同一实例的下一次搜索零请求。
func TestRunnerWiresSearchEgressGate(t *testing.T) {
	ctx := context.Background()
	var hits int32
	search := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"title":"t","snippet":"s"}]}`)
	}))
	defer search.Close()

	d, err := store.Open(filepath.Join(t.TempDir(), "search-gate.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSetting(ctx, "web_search_enabled", "1"); err != nil {
		t.Fatal(err)
	}
	ctx = store.AIIntentContext(ctx, d.AIEmergencyState(ctx).Epoch)
	k := &Knowledge{}
	// NewRunner 会把同一份急停判定装到搜索出口上。
	NewRunner(d, k, config.AI{}, nil, logging.Discard())
	// Transport 注入只替换连接阶段；URL 检查仍走共用实现。
	k.client = newEgressHTTPClient(time.Second, search.Client().Transport)

	if got := k.WebSearch(ctx, "鲈鱼 123456789", search.URL); got == "" {
		t.Fatal("未急停时搜索应返回模拟结果")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("未急停时搜索请求次数 = %d，期望 1", got)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if got := k.WebSearch(ctx, "鲈鱼", search.URL); got != "" {
		t.Fatalf("急停时搜索应返回空串，得到 %q", got)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("急停时搜索请求次数 = %d，期望仍为 1", got)
	}
}
