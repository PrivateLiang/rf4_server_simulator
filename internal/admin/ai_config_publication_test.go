package admin

// 配置发布（DEV-114 A / T-102）的可复核验收：
//
//   - A→B 整套切换：保存前两读端与页面仍是 A，提交后新读取均为 B；
//     「整套」由活动快照绑定（地址/模型/修订/凭据引用同一行），不会拼出跨版本组合；
//   - 修订冲突：两个写者（两个独立 DB 连接 + 两个后台实例）持相同修订竞发，
//     恰好一个提交、冲突方拿到 409 并刷新；
//   - 失败回滚：凭据文件写入失败、审计插入失败都保留上一套完整配置；
//     故障修复后重试一次即成功；
//   - 丢回包：提交成功但客户端重放旧修订时返回已发布结果或冲突，不叠加成功记录；
//   - 进程中断：在「准备件写入一半 / 对象安装完成 / 写 SQL 未提交 / 已提交未回 HTTP」
//     四处终止写进程（子进程 os.Exit 模拟），重启读者后分别看到 A / A / A / B；
//   - 页面状态区分已保存、已复测、失败沿用旧套、凭据锁定；
//   - 留空 Key 的同源沿用、换源拒绝，以及响应/审计/对象文件脱敏。
//
// 两个本地假厂商各自只接受一组地址、模型与随机测试凭据；观测器只记端点代号、
// 修订、凭据匹配布尔值与调用次数。所有数据都是合成值，不连接任何真实厂商。
// Runner 侧同一份快照的读取/发送证据见 internal/aibridge 的 TestProviderSnapshot。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"rf4server/internal/aibridge"
	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// ---- 假厂商与观测器 ----

type pubVendor struct {
	srv   *httptest.Server
	hits  int32
	mu    sync.Mutex
	auths []string
}

func newPubVendor(t *testing.T, label string) *pubVendor {
	t.Helper()
	v := &pubVendor{}
	v.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&v.hits, 1)
		v.mu.Lock()
		v.auths = append(v.auths, r.Header.Get("Authorization"))
		v.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + label + `"}}],"usage":{"total_tokens":3}}`))
	}))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *pubVendor) count() int { return int(atomic.LoadInt32(&v.hits)) }

func (v *pubVendor) lastAuth() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.auths) == 0 {
		return ""
	}
	return v.auths[len(v.auths)-1]
}

// hostDispatch 按请求主机分发到不同假厂商的 TLS Transport；只替换连接阶段，
// URL/重定向检查仍由共用实现执行。
type hostDispatch map[string]http.RoundTripper

func (d hostDispatch) RoundTrip(req *http.Request) (*http.Response, error) {
	tr, ok := d[req.URL.Host]
	if !ok {
		return nil, fmt.Errorf("未注册的测试主机 %s", req.URL.Host)
	}
	return tr.RoundTrip(req)
}

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		t.Fatalf("解析测试地址 %q: %v", rawURL, err)
	}
	return u.Host
}

// ---- 发布环境 ----

type pubEnv struct {
	dir     string
	dbPath  string
	keyPath string
	db      *store.DB
	srv     *Server
}

func newPubEnv(t *testing.T, baseURL string) *pubEnv {
	t.Helper()
	// 此夹具隔离发布逻辑，真实目录 I/O 另见 TestDR109WindowsPublication。
	t.Cleanup(aibridge.SetCredentialDirSyncForTest(func(string) error { return nil }))
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ai_master.key")
	t.Setenv("RF4_AI_MASTER_KEY_FILE", keyPath)
	dbPath := filepath.Join(dir, "pub.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	if baseURL != "" {
		if _, err := db.SQL.Exec(`UPDATE ai_provider SET base_url=? WHERE name='deepseek'`, baseURL); err != nil {
			t.Fatal(err)
		}
	}
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	return &pubEnv{dir: dir, dbPath: dbPath, keyPath: keyPath, db: db, srv: srv}
}

// secondHandle 打开同一个 SQLite 的第二个连接，模拟另一个进程/重启后的视角。
func (e *pubEnv) secondHandle(t *testing.T) *store.DB {
	t.Helper()
	d, err := store.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func (e *pubEnv) save(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postAs(t, e.srv, e.srv.handleAIConfigSave, "/api/ai/config", body, "tester")
}

func (e *pubEnv) saveBody(base, model, key string, revision int64) string {
	if revision > 0 {
		return fmt.Sprintf(`{"base_url":%q,"model_id":%q,"api_key":%q,"revision":%d}`, base, model, key, revision)
	}
	return fmt.Sprintf(`{"base_url":%q,"model_id":%q,"api_key":%q}`, base, model, key)
}

func (e *pubEnv) status(t *testing.T) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	e.srv.handleAIStatus(rec, httptest.NewRequest("GET", "/api/ai/status", nil))
	if rec.Code != 200 {
		t.Fatalf("状态接口 %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *pubEnv) snapshot(t *testing.T, d *store.DB) aibridge.Snapshot {
	t.Helper()
	key, err := aibridge.LoadMasterKey(e.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := aibridge.LoadSnapshot(context.Background(), d, e.keyPath, key)
	if err != nil {
		t.Fatalf("读取活动快照: %v", err)
	}
	return snap
}

func (e *pubEnv) revision(t *testing.T, d *store.DB) int64 {
	t.Helper()
	rev, err := aibridge.ActiveRevision(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

func (e *pubEnv) recordCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_setting WHERE key LIKE 'ai_config_record:%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *pubEnv) objectRefs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(aibridge.CredentialDir(e.keyPath))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]string, 0, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".json") {
			refs = append(refs, strings.TrimSuffix(ent.Name(), ".json"))
		}
	}
	return refs
}

// withDispatch 把后台复测的 Provider 换成按主机分发的本地 fixture Transport。
func (e *pubEnv) withDispatch(t *testing.T, dispatch hostDispatch) {
	t.Helper()
	old := newAITestProvider
	newAITestProvider = func(spec aibridge.ProviderSpec, timeout time.Duration) (*aibridge.OpenAICompatProvider, error) {
		return aibridge.NewProviderWithTransport(spec, timeout, dispatch)
	}
	t.Cleanup(func() { newAITestProvider = old })
}

// retest 走真实后台复测入口（只读活动快照），返回响应与解析后的 JSON。
func (e *pubEnv) retest(t *testing.T) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return runAITest(t, e.srv, "", "", "")
}

// ---- 主验收 ----

func TestAIConfigPublication(t *testing.T) {
	t.Run("A_to_B_whole_set", testPublicationWholeSet)
	t.Run("revision_conflict_single_winner", testPublicationConflict)
	t.Run("failures_keep_previous_set", testPublicationFailures)
	t.Run("lost_response_replay", testPublicationReplay)
	t.Run("process_crash_barriers", testPublicationCrashBarriers)
	t.Run("blank_key_rules", testPublicationBlankKey)
	t.Run("page_states_and_redaction", testPublicationPageStates)
}

func testPublicationWholeSet(t *testing.T) {
	ctx := context.Background()
	vA := newPubVendor(t, "A")
	vB := newPubVendor(t, "B")
	env := newPubEnv(t, vA.srv.URL)
	other := env.secondHandle(t)
	dispatch := hostDispatch{
		hostOf(t, vA.srv.URL): vA.srv.Client().Transport,
		hostOf(t, vB.srv.URL): vB.srv.Client().Transport,
	}
	env.withDispatch(t, dispatch)

	// 初始 A：保存一次形成活动修订 1。
	rec := env.save(t, env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-1111", 0))
	if rec.Code != 200 {
		t.Fatalf("保存 A 失败 %d: %s", rec.Code, rec.Body.String())
	}
	snapA1 := env.snapshot(t, env.db)
	snapA2 := env.snapshot(t, other)
	if snapA1.Revision() != 1 || snapA2.Revision() != 1 ||
		snapA1.Spec.BaseURL != vA.srv.URL || snapA2.Spec.BaseURL != vA.srv.URL {
		t.Fatalf("保存后两读端快照不一致: %+v / %+v", snapA1, snapA2)
	}
	if got := env.status(t)["revision"]; got != float64(1) {
		t.Fatalf("页面修订 = %v，期望 1", got)
	}

	// 保存 B（换域、换模型、换 Key）：提交后两读端与页面都只见 B。
	rec = env.save(t, env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-B-2222", 1))
	if rec.Code != 200 {
		t.Fatalf("保存 B 失败 %d: %s", rec.Code, rec.Body.String())
	}
	var saved map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(saved["revision"]) != "2" || !strings.Contains(fmt.Sprint(saved["message"]), "已保存") {
		t.Fatalf("保存响应 = %s", rec.Body.String())
	}
	snapB1 := env.snapshot(t, env.db)
	snapB2 := env.snapshot(t, other)
	if snapB1.Revision() != 2 || snapB2.Revision() != 2 ||
		snapB1.Spec.BaseURL != vB.srv.URL || snapB2.Spec.BaseURL != vB.srv.URL ||
		snapB1.Spec.ModelID != "model-b" || snapB2.Spec.ModelID != "model-b" {
		t.Fatalf("提交后两读端未见 B: %+v / %+v", snapB1, snapB2)
	}
	// 旧快照仍是整套 A（地址/模型/密钥同源），新快照整套 B；不存在跨版本拼接。
	if snapA1.Spec.BaseURL != vA.srv.URL || snapA1.Spec.ModelID != "model-a" ||
		!strings.Contains(snapA1.Spec.APIKey, "A-1111") {
		t.Fatalf("旧快照被改写: %+v", snapA1.Spec)
	}
	if strings.Contains(snapB1.Spec.APIKey, "A-1111") || !strings.Contains(snapB1.Spec.APIKey, "B-2222") {
		t.Fatalf("新快照凭据错配: %q", snapB1.Spec.APIKey)
	}
	// 旧修订的尝试作废；真实复测发到 B，A 一次都不再收到请求。
	if err := aibridge.Gate(ctx, env.db, snapA1.Revision()); err == nil {
		t.Fatal("旧修订的发送前检查应失败")
	}
	rec, out := env.retest(t)
	if out["ok"] != true {
		t.Fatalf("复测 B 失败: %s", rec.Body.String())
	}
	if vA.count() != 0 || vB.count() != 1 {
		t.Fatalf("出站计数 A=%d B=%d，期望 A=0 B=1", vA.count(), vB.count())
	}
	if !strings.Contains(vB.lastAuth(), "B-2222") {
		t.Fatalf("B 收到的凭据不匹配: %q", vB.lastAuth())
	}
}

func testPublicationConflict(t *testing.T) {
	vA := newPubVendor(t, "A")
	vB := newPubVendor(t, "B")
	envA := newPubEnv(t, vA.srv.URL)
	// 第二个后台实例（独立 DB 连接）共享同一文件与主密钥目录。
	dbB := envA.secondHandle(t)
	envB := &pubEnv{dir: envA.dir, dbPath: envA.dbPath, keyPath: envA.keyPath, db: dbB}
	envB.srv = NewServer(config.Admin{JWTSecret: "test"}, dbB, logging.Discard(), WebFS())

	if rec := envA.save(t, envA.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-1111", 0)); rec.Code != 200 {
		t.Fatalf("初始保存失败: %s", rec.Body.String())
	}
	start := make(chan struct{})
	type result struct {
		code int
		body string
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i, env := range []*pubEnv{envA, envB} {
		wg.Add(1)
		go func(i int, env *pubEnv) {
			defer wg.Done()
			<-start
			model, key := "model-b1", "sk-synthetic-B1-3333"
			if i == 1 {
				model, key = "model-b2", "sk-synthetic-B2-4444"
			}
			rec := env.save(t, env.saveBody(vB.srv.URL, model, key, 1))
			results <- result{code: rec.Code, body: rec.Body.String()}
		}(i, env)
	}
	close(start)
	wg.Wait()
	close(results)
	var ok, conflict int
	for r := range results {
		switch r.code {
		case 200:
			ok++
		case 409:
			conflict++
		default:
			t.Fatalf("并发写者返回 %d: %s", r.code, r.body)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("并发结果 ok=%d conflict=%d，期望恰好 1/1", ok, conflict)
	}
	// 有且只有一个新修订提交，记录不叠加。
	rev := envA.revision(t, envA.db)
	if rev != 2 {
		t.Fatalf("最终修订 = %d，期望 2", rev)
	}
	if n := envA.recordCount(t); n != 2 {
		t.Fatalf("发布记录 = %d 条，期望 2（无第二条成功记录）", n)
	}
	snap := envA.snapshot(t, envA.db)
	if snap.Spec.BaseURL != vB.srv.URL || (snap.Spec.ModelID != "model-b1" && snap.Spec.ModelID != "model-b2") {
		t.Fatalf("最终活动套 = %+v", snap.Spec)
	}
	// 凭据对象绑定厂商 ID：活动引用能解出与模型同一次发布的那把 Key。
	key := snap.Spec.APIKey
	if snap.Spec.ModelID == "model-b1" && !strings.Contains(key, "B1-3333") {
		t.Fatalf("活动凭据与模型不同源: model=%s key=%q", snap.Spec.ModelID, key)
	}
	if snap.Spec.ModelID == "model-b2" && !strings.Contains(key, "B2-4444") {
		t.Fatalf("活动凭据与模型不同源: model=%s key=%q", snap.Spec.ModelID, key)
	}
}

func testPublicationFailures(t *testing.T) {
	ctx := context.Background()
	vA := newPubVendor(t, "A")
	vB := newPubVendor(t, "B")
	env := newPubEnv(t, vA.srv.URL)
	dispatch := hostDispatch{
		hostOf(t, vA.srv.URL): vA.srv.Client().Transport,
		hostOf(t, vB.srv.URL): vB.srv.Client().Transport,
	}
	env.withDispatch(t, dispatch)

	if rec := env.save(t, env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-1111", 0)); rec.Code != 200 {
		t.Fatalf("初始保存失败: %s", rec.Body.String())
	}
	refs0 := len(env.objectRefs(t))
	if refs0 != 1 {
		t.Fatalf("初始对象数 = %d，期望 1", refs0)
	}
	// 1) 模型行在发布前被禁用（行冲突/提交失败）→ 整笔不发布，读者仍用 A。
	if _, err := env.db.SQL.Exec(`UPDATE ai_model SET enabled=0
		WHERE provider_id=(SELECT id FROM ai_provider WHERE name='deepseek')`); err != nil {
		t.Fatal(err)
	}
	rec := env.save(t, env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-B-2222", 1))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "刷新") {
		t.Fatalf("模型行冲突 = %d: %s", rec.Code, rec.Body.String())
	}
	if got := env.revision(t, env.db); got != 1 {
		t.Fatalf("失败后修订 = %d，期望仍为 1", got)
	}
	if snap := env.snapshot(t, env.db); snap.Spec.BaseURL != vA.srv.URL {
		t.Fatalf("失败后活动套 = %+v", snap.Spec)
	}
	// 准备阶段创建的对象保留但未被引用：读者不得使用它。
	if got := len(env.objectRefs(t)); got != refs0+1 {
		t.Fatalf("模型行冲突后对象数 = %d，期望 %d（未引用对象保留）", got, refs0+1)
	}
	// 两读端仍可用 A：真实复测发到 A，B 零请求。
	retestRec, out := env.retest(t)
	if out["ok"] != true || vB.count() != 0 || vA.count() != 1 {
		t.Fatalf("失败后复测 = %s（A=%d B=%d）", retestRec.Body.String(), vA.count(), vB.count())
	}
	if _, err := env.db.SQL.Exec(`UPDATE ai_model SET enabled=1
		WHERE provider_id=(SELECT id FROM ai_provider WHERE name='deepseek')`); err != nil {
		t.Fatal(err)
	}

	// 2) 审计插入失败（SQLite 触发器拒绝 ai_config）→ 同事务回滚。
	if _, err := env.db.SQL.ExecContext(ctx, `
		CREATE TRIGGER pub_audit_boom BEFORE INSERT ON audit_log
		WHEN NEW.action='ai_config'
		BEGIN SELECT RAISE(ABORT, 'audit boom'); END`); err != nil {
		t.Fatal(err)
	}
	rec = env.save(t, env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-B-2222", 1))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "保存失败，仍使用原配置") {
		t.Fatalf("审计失败 = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), aiconfig.DestinationHost(vA.srv.URL)) {
		t.Fatalf("失败响应未列原域: %s", rec.Body.String())
	}
	if got := env.revision(t, env.db); got != 1 || env.recordCount(t) != 1 {
		t.Fatalf("审计失败后修订/记录 = %d/%d，期望 1/1", got, env.recordCount(t))
	}
	if snap := env.snapshot(t, env.db); snap.Spec.BaseURL != vA.srv.URL || snap.Spec.ModelID != "model-a" {
		t.Fatalf("审计失败后活动套 = %+v", snap.Spec)
	}
	if got := len(env.objectRefs(t)); got != refs0+2 {
		t.Fatalf("审计失败后对象数 = %d，期望 %d（未引用对象保留）", got, refs0+2)
	}
	if _, err := env.db.SQL.ExecContext(ctx, `DROP TRIGGER pub_audit_boom`); err != nil {
		t.Fatal(err)
	}
	// 3) 修复后重试一次成功，只有一条新记录，复测发到 B。
	rec = env.save(t, env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-B-2222", 1))
	if rec.Code != 200 {
		t.Fatalf("修复后重试失败 %d: %s", rec.Code, rec.Body.String())
	}
	if got := env.revision(t, env.db); got != 2 || env.recordCount(t) != 2 {
		t.Fatalf("重试后修订/记录 = %d/%d，期望 2/2", got, env.recordCount(t))
	}
	retestRec, out = env.retest(t)
	if out["ok"] != true || vB.count() != 1 {
		t.Fatalf("重试成功后应发到 B：%s（B=%d）", retestRec.Body.String(), vB.count())
	}
}

func testPublicationReplay(t *testing.T) {
	vA := newPubVendor(t, "A")
	vB := newPubVendor(t, "B")
	env := newPubEnv(t, vA.srv.URL)
	if rec := env.save(t, env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-1111", 0)); rec.Code != 200 {
		t.Fatalf("初始保存失败: %s", rec.Body.String())
	}
	bodyB := env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-B-2222", 1)
	if rec := env.save(t, bodyB); rec.Code != 200 {
		t.Fatalf("保存 B 失败: %s", rec.Body.String())
	}
	rev := env.revision(t, env.db)
	records := env.recordCount(t)

	// 同一旧修订 + 相同字段/实际密钥：返回已发布结果，不新建修订/记录。
	rec := env.save(t, bodyB)
	if rec.Code != 200 {
		t.Fatalf("重放应返回已发布结果，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["replayed"] != true || fmt.Sprint(out["revision"]) != fmt.Sprint(rev) {
		t.Fatalf("重放响应 = %s", rec.Body.String())
	}
	if env.revision(t, env.db) != rev || env.recordCount(t) != records {
		t.Fatalf("重放叠加了新修订/记录: rev=%d records=%d", env.revision(t, env.db), env.recordCount(t))
	}

	// 旧调用者（不带修订）且字段与密钥完全一致：按无变更处理。
	rec = env.save(t, env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-B-2222", 0))
	if rec.Code != 200 {
		t.Fatalf("无修订无变更应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if env.revision(t, env.db) != rev || env.recordCount(t) != records {
		t.Fatal("无变更路径写入了新修订/记录")
	}

	// 同一旧修订但密钥不同：冲突，禁止盲目重放。
	rec = env.save(t, env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-B-9999", 1))
	if rec.Code != 409 {
		t.Fatalf("不同密钥的旧修订重放 = %d，期望 409", rec.Code)
	}
	if env.revision(t, env.db) != rev || env.recordCount(t) != records {
		t.Fatal("冲突重放改动了活动配置")
	}
}

func testPublicationBlankKey(t *testing.T) {
	vA := newPubVendor(t, "A")
	env := newPubEnv(t, vA.srv.URL)

	// 首次缺 Key：拒绝。
	rec := env.save(t, env.saveBody(vA.srv.URL, "model-a", "", 0))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "首次保存请填写 API Key") {
		t.Fatalf("首次留空 = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := env.save(t, env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-1111", 0)); rec.Code != 200 {
		t.Fatalf("首次保存失败: %s", rec.Body.String())
	}
	refsBefore := len(env.objectRefs(t))

	// 同源只改模型：允许留空，沿用同一凭据引用，产生新修订但不新建对象。
	rec = env.save(t, env.saveBody("", "model-a2", "", 1))
	if rec.Code != 200 {
		t.Fatalf("同源留空失败 %d: %s", rec.Code, rec.Body.String())
	}
	snap := env.snapshot(t, env.db)
	if snap.Revision() != 2 || snap.Spec.ModelID != "model-a2" || !strings.Contains(snap.Spec.APIKey, "A-1111") {
		t.Fatalf("同源保留后快照 = %+v", snap)
	}
	if len(env.objectRefs(t)) != refsBefore {
		t.Fatalf("同源留空不应新建凭据对象: %d → %d", refsBefore, len(env.objectRefs(t)))
	}

	// 换源留空：整笔拒绝，地址/模型/修订都不变。
	rec = env.save(t, env.saveBody("https://other.invalid/v1", "model-other", "", 2))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "必须重新填写 API Key") {
		t.Fatalf("换源留空 = %d: %s", rec.Code, rec.Body.String())
	}
	snap = env.snapshot(t, env.db)
	if snap.Revision() != 2 || snap.Spec.BaseURL != vA.srv.URL || snap.Spec.ModelID != "model-a2" {
		t.Fatalf("换源留空改动了活动套: %+v", snap)
	}

	// 同址单独换 Key：产生新修订与新对象。
	rec = env.save(t, env.saveBody(vA.srv.URL, "model-a2", "sk-synthetic-A-3333", 2))
	if rec.Code != 200 {
		t.Fatalf("同址换 Key 失败: %s", rec.Body.String())
	}
	snap = env.snapshot(t, env.db)
	if snap.Revision() != 3 || !strings.Contains(snap.Spec.APIKey, "A-3333") {
		t.Fatalf("换 Key 后快照 = %+v", snap)
	}
	if len(env.objectRefs(t)) != refsBefore+1 {
		t.Fatalf("换 Key 应新建一个对象: %d → %d", refsBefore, len(env.objectRefs(t)))
	}
}

func testPublicationPageStates(t *testing.T) {
	vA := newPubVendor(t, "A")
	vB := newPubVendor(t, "B")
	env := newPubEnv(t, vA.srv.URL)
	env.withDispatch(t, hostDispatch{
		hostOf(t, vA.srv.URL): vA.srv.Client().Transport,
		hostOf(t, vB.srv.URL): vB.srv.Client().Transport,
	})

	// 已保存、待复测。
	if rec := env.save(t, env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-1111", 0)); rec.Code != 200 {
		t.Fatalf("保存失败: %s", rec.Body.String())
	}
	st := env.status(t)
	if !strings.Contains(fmt.Sprint(st["saved_display"]), "已保存") ||
		!strings.Contains(fmt.Sprint(st["saved_display"]), "待复测") {
		t.Fatalf("已保存状态 = %v", st["saved_display"])
	}
	retest := st["retest"].(map[string]any)
	if retest["display"] != "待复测" || retest["current"] != false {
		t.Fatalf("复测状态 = %v", retest)
	}

	// 复测成功：此连接已通过复测（同修订）。
	rec, out := env.retest(t)
	if out["ok"] != true {
		t.Fatalf("复测失败: %s", rec.Body.String())
	}
	st = env.status(t)
	retest = st["retest"].(map[string]any)
	if retest["current"] != true || !strings.Contains(fmt.Sprint(st["saved_display"]), "已通过复测") {
		t.Fatalf("复测通过状态 = %v / %v", st["saved_display"], retest)
	}

	// 发布新套后复测状态回到待复测。
	if rec := env.save(t, env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-B-2222", 1)); rec.Code != 200 {
		t.Fatalf("发布 B 失败: %s", rec.Body.String())
	}
	st = env.status(t)
	if retest := st["retest"].(map[string]any); retest["current"] != false {
		t.Fatalf("新修订的复测状态 = %v", retest)
	}

	// 凭据锁定：删除活动对象后，状态与复测都显示锁定。
	raw, found, err := env.db.AIConfigActiveRaw(context.Background())
	if err != nil || !found {
		t.Fatalf("活动指针读取失败: found=%v err=%v", found, err)
	}
	active, ok := aiconfig.ParseActive(raw)
	if !ok || active.CredentialRef == "" {
		t.Fatalf("活动指针不可解析: %q", raw)
	}
	if err := os.Remove(filepath.Join(aibridge.CredentialDir(env.keyPath), active.CredentialRef+".json")); err != nil {
		t.Fatal(err)
	}
	st = env.status(t)
	cred := st["credential"].(map[string]any)
	if fmt.Sprint(cred["state"]) != "locked" || !strings.Contains(fmt.Sprint(cred["display"]), "锁定") {
		t.Fatalf("凭据锁定状态 = %v", cred)
	}
	rec, out = env.retest(t)
	if out["ok"] != false || fmt.Sprint(out["state"]) != "credential_locked" {
		t.Fatalf("锁定后复测 = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-synthetic") {
		t.Fatalf("锁定响应泄露 Key: %s", rec.Body.String())
	}

	// 脱敏：状态、审计、对象文件都不含明文 Key。
	statusRec := httptest.NewRecorder()
	env.srv.handleAIStatus(statusRec, httptest.NewRequest("GET", "/api/ai/status", nil))
	if strings.Contains(statusRec.Body.String(), "sk-synthetic") {
		t.Fatalf("状态泄露 Key: %s", statusRec.Body.String())
	}
	var auditDetail string
	if err := env.db.SQL.QueryRow(
		`SELECT detail FROM audit_log WHERE action='ai_config' ORDER BY id DESC LIMIT 1`).Scan(&auditDetail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(auditDetail, "sk-synthetic") {
		t.Fatalf("审计泄露 Key: %q", auditDetail)
	}
	for _, ref := range env.objectRefs(t) {
		data, err := os.ReadFile(filepath.Join(aibridge.CredentialDir(env.keyPath), ref+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("sk-synthetic")) {
			t.Fatalf("凭据对象泄露明文: %s", ref)
		}
	}
}

// ---- 进程中断：子进程在四个屏障终止，父进程重启读者验收 ----

const pubCrashHelperEnv = "PUB_CRASH_HELPER"

// TestAIConfigPublicationCrashHelper 只在被父用例以子进程方式拉起时执行。
func TestAIConfigPublicationCrashHelper(t *testing.T) {
	scenario := os.Getenv(pubCrashHelperEnv)
	if scenario == "" {
		t.Skip("仅作为配置发布中断用例的子进程入口")
	}
	if os.Getenv("PUB_TEST_DIR_SYNC_OK") == "1" {
		// 子进程同样模拟支持目录 fsync 的平台；真实平台拒绝分支由父用例覆盖。
		aibridge.SetCredentialDirSyncForTest(func(string) error { return nil })
	}
	dbPath := os.Getenv("PUB_CRASH_DB")
	keyPath := os.Getenv("PUB_CRASH_KEY")
	vendor := os.Getenv("PUB_CRASH_VENDOR")
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		fail(err)
	}
	if err := db.Migrate(); err != nil {
		fail(err)
	}
	ctx := context.Background()
	key, err := aibridge.LoadMasterKey(keyPath)
	if err != nil {
		fail(err)
	}
	var providerID, modelRowID int64
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT id FROM ai_provider WHERE name='deepseek'`).Scan(&providerID); err != nil {
		fail(err)
	}
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT id FROM ai_model WHERE provider_id=? ORDER BY id LIMIT 1`, providerID).Scan(&modelRowID); err != nil {
		fail(err)
	}
	dir := aibridge.CredentialDir(keyPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fail(err)
	}
	switch scenario {
	case "partial_temp":
		// 准备件写到一半：临时文件留下，未安装成对象。
		if err := os.WriteFile(filepath.Join(dir, ".ai_credential-crash.tmp"), []byte(`{"version":1,"ref":"`), 0o600); err != nil {
			fail(err)
		}
		os.Exit(0)
	case "object_only":
		if _, err := aibridge.CreateCredentialObject(dir, key, providerID, "deepseek", "sk-synthetic-crash-B"); err != nil {
			fail(err)
		}
		os.Exit(0)
	case "uncommitted_sql":
		// 模拟「写 SQL 未提交」：在独立连接上执行与发布相同的事务写入后直接退出。
		obj, err := aibridge.CreateCredentialObject(dir, key, providerID, "deepseek", "sk-synthetic-crash-B")
		if err != nil {
			fail(err)
		}
		conn, err := db.SQL.Conn(ctx)
		if err != nil {
			fail(err)
		}
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			fail(err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE ai_provider SET base_url=? WHERE id=?`, vendor, providerID); err != nil {
			fail(err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE ai_model SET model_id='model-crash' WHERE id=?`, modelRowID); err != nil {
			fail(err)
		}
		active := aiconfig.ActiveConfig{
			Schema: aiconfig.ConfigSchemaVersion, Revision: 2, ProviderID: providerID,
			ProviderName: "deepseek", Protocol: "openai_chat", AuthScheme: "bearer",
			BaseURL: vendor, ModelRowID: modelRowID, ModelID: "model-crash",
			CredentialRef: obj.Ref, Source: "admin_save", TS: time.Now().Unix(),
		}
		raw, _ := aiconfig.MarshalActive(active)
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,0)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, aiconfig.KeyConfigActive, raw); err != nil {
			fail(err)
		}
		rec := aiconfig.ConfigRecord{Schema: aiconfig.ConfigSchemaVersion, Revision: 2, Kind: "model",
			Actor: "helper", Source: "admin_save", TS: time.Now().Unix(),
			DestinationHost: aiconfig.DestinationHost(vendor), Model: "model-crash"}
		recRaw, _ := aiconfig.MarshalConfigRecord(rec)
		if _, err := conn.ExecContext(ctx, `INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,0)`,
			aiconfig.ConfigRecordKey(2), recRaw); err != nil {
			fail(err)
		}
		os.Exit(0) // 未 COMMIT：进程死亡等价于回滚
	case "committed":
		obj, err := aibridge.CreateCredentialObject(dir, key, providerID, "deepseek", "sk-synthetic-crash-B")
		if err != nil {
			fail(err)
		}
		if _, err := db.AIPublishConfig(ctx, store.AIPublishRequest{
			ProviderID: providerID, ModelRowID: modelRowID,
			BaseURL: vendor, ModelID: "model-crash",
			CredentialRef: obj.Ref, CredentialChanged: true, CredentialMasked: obj.Masked,
			Source: "admin_save", Actor: "helper",
		}); err != nil {
			fail(err)
		}
		os.Exit(0) // 已提交但未回 HTTP：直接退出
	default:
		fmt.Fprintf(os.Stderr, "未知场景 %q\n", scenario)
		os.Exit(2)
	}
}

func testPublicationCrashBarriers(t *testing.T) {
	vA := newPubVendor(t, "A")
	vB := newPubVendor(t, "B")
	env := newPubEnv(t, vA.srv.URL)
	if rec := env.save(t, env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-1111", 0)); rec.Code != 200 {
		t.Fatalf("初始保存失败: %s", rec.Body.String())
	}
	env.withDispatch(t, hostDispatch{
		hostOf(t, vA.srv.URL): vA.srv.Client().Transport,
		hostOf(t, vB.srv.URL): vB.srv.Client().Transport,
	})

	type crashCase struct {
		scenario string
		wantBase string
		wantRev  int64
		wantA    int
		wantB    int
	}
	cases := []crashCase{
		{scenario: "partial_temp", wantBase: vA.srv.URL, wantRev: 1, wantA: 1, wantB: 0},
		{scenario: "object_only", wantBase: vA.srv.URL, wantRev: 1, wantA: 1, wantB: 0},
		{scenario: "uncommitted_sql", wantBase: vA.srv.URL, wantRev: 1, wantA: 1, wantB: 0},
		{scenario: "committed", wantBase: vB.srv.URL, wantRev: 2, wantA: 0, wantB: 1},
	}
	prevA, prevB := 0, 0
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestAIConfigPublicationCrashHelper")
			cmd.Env = append(os.Environ(),
				pubCrashHelperEnv+"="+c.scenario,
				"PUB_CRASH_DB="+env.dbPath,
				"PUB_CRASH_KEY="+env.keyPath,
				"PUB_CRASH_VENDOR="+vB.srv.URL,
				"PUB_TEST_DIR_SYNC_OK=1",
			)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("子进程场景 %s 退出异常: %v\n%s", c.scenario, err, stderr.String())
			}
			// 「重启 AD 与 BR」：重新打开数据库并读取/复测。
			restarted := env.secondHandle(t)
			snap := env.snapshot(t, restarted)
			if snap.Spec.BaseURL != c.wantBase || snap.Revision() != c.wantRev {
				t.Fatalf("%s 重启后快照 = %+v（rev %d），期望 %s rev %d",
					c.scenario, snap.Spec, snap.Revision(), c.wantBase, c.wantRev)
			}
			beforeA, beforeB := vA.count(), vB.count()
			rec, out := env.retest(t)
			if out["ok"] != true {
				t.Fatalf("%s 重启后复测失败: %s", c.scenario, rec.Body.String())
			}
			if vA.count()-beforeA != c.wantA || vB.count()-beforeB != c.wantB {
				t.Fatalf("%s 出站增量 A=%d B=%d，期望 A=%d B=%d", c.scenario,
					vA.count()-beforeA, vB.count()-beforeB, c.wantA, c.wantB)
			}
			if snap.Revision() > 0 && env.recordCount(t) != int(snap.Revision()) {
				t.Fatalf("%s 记录数 = %d，活动修订 = %d", c.scenario, env.recordCount(t), snap.Revision())
			}
			prevA, prevB = vA.count(), vB.count()
		})
	}
	_ = prevA
	_ = prevB
}

// TestAIConfigPublicationFirstSaveSource 干净库首次显式保存：记录来源必须是
// admin_save（不是 migration），审计同源；第二次相同提交不新增记录。
func TestAIConfigPublicationFirstSaveSource(t *testing.T) {
	ctx := context.Background()
	env := newPubEnv(t, "")
	rec := env.save(t, env.saveBody("", "model-a", "sk-synthetic-first-1234", 0))
	if rec.Code != 200 {
		t.Fatalf("干净库首次保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	record, found, err := env.db.AIConfigRecord(ctx, 1)
	if err != nil || !found {
		t.Fatalf("rev1 记录缺失: found=%v err=%v", found, err)
	}
	if record.Source != "admin_save" {
		t.Fatalf("干净库首次保存来源 = %q，期望 admin_save", record.Source)
	}
	if !strings.Contains(record.DestinationHost, "api.deepseek.com") {
		t.Fatalf("记录目的域 = %q", record.DestinationHost)
	}
	var detail string
	if err := env.db.SQL.QueryRow(
		`SELECT detail FROM audit_log WHERE action='ai_config' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "source=admin_save") {
		t.Fatalf("审计来源 = %q，期望 source=admin_save", detail)
	}
	// 重复相同提交（旧调用者无修订）不新增成功记录。
	if rec := env.save(t, env.saveBody("", "model-a", "sk-synthetic-first-1234", 0)); rec.Code != 200 {
		t.Fatalf("重复保存失败: %s", rec.Body.String())
	}
	if got := env.recordCount(t); got != 1 {
		t.Fatalf("重复保存后记录数 = %d，期望 1", got)
	}
}

// TestAIConfigPublicationLegacyMigrationSource 真实旧凭据迁移：空白 Key 沿用旧库
// 密文时来源记 migration，审计同源；重复迁移不重复登记。
func TestAIConfigPublicationLegacyMigrationSource(t *testing.T) {
	ctx := context.Background()
	env := newPubEnv(t, "")
	key, err := aibridge.InitMasterKey(env.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	ct, nonce, err := aibridge.Encrypt(key, []byte("sk-synthetic-legacy-1111"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.SQL.ExecContext(ctx, `
		INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES((SELECT id FROM ai_provider WHERE name='deepseek'),?,?,'default','sk-****1111',0)`,
		ct, nonce); err != nil {
		t.Fatal(err)
	}
	// 留空 Key：同源沿用旧库密文，迁移为不可变对象。
	rec := env.save(t, env.saveBody("", "model-a", "", 0))
	if rec.Code != 200 {
		t.Fatalf("旧凭据迁移失败 %d: %s", rec.Code, rec.Body.String())
	}
	record, found, err := env.db.AIConfigRecord(ctx, 1)
	if err != nil || !found {
		t.Fatalf("rev1 记录缺失: found=%v err=%v", found, err)
	}
	if record.Source != "migration" {
		t.Fatalf("旧凭据迁移来源 = %q，期望 migration", record.Source)
	}
	var detail string
	if err := env.db.SQL.QueryRow(
		`SELECT detail FROM audit_log WHERE action='ai_config' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, "source=migration") {
		t.Fatalf("审计来源 = %q，期望 source=migration", detail)
	}
	// 第二次空白保存无变更：不新增记录。
	if rec := env.save(t, env.saveBody("", "model-a", "", 0)); rec.Code != 200 {
		t.Fatalf("重复迁移保存失败: %s", rec.Body.String())
	}
	if got := env.recordCount(t); got != 1 {
		t.Fatalf("重复迁移后记录数 = %d，期望 1", got)
	}
}

// TestAIConfigPublicationInstallDurability 验证持续同步失败和恢复后重试。
func TestAIConfigPublicationInstallDurability(t *testing.T) {
	ctx := context.Background()
	env := newPubEnv(t, "")
	// 注入同步失败：第一次不提交；移除故障后重试一次成功。
	aibridge.SetCredentialDirSyncForTest(func(string) error { return errors.New("sync boom") })
	rec := env.save(t, env.saveBody("", "model-a", "sk-synthetic-dur-2", 0))
	if rec.Code != 500 {
		t.Fatalf("注入同步失败后应拒绝发布，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "持久") || !strings.Contains(rec.Body.String(), "尚未保存，没有可用连接") {
		t.Fatalf("同步失败的拒绝原因未说明持久性: %s", rec.Body.String())
	}
	if _, found, _ := env.db.AIConfigActiveRaw(ctx); found {
		t.Fatal("同步失败后仍发布活动指针")
	}
	aibridge.SetCredentialDirSyncForTest(func(string) error { return nil })
	if rec := env.save(t, env.saveBody("", "model-a", "sk-synthetic-dur-2", 0)); rec.Code != 200 {
		t.Fatalf("故障移除后重试失败 %d: %s", rec.Code, rec.Body.String())
	}
	if got := env.revision(t, env.db); got != 1 || env.recordCount(t) != 1 {
		t.Fatalf("重试成功后修订/记录 = %d/%d，期望 1/1", got, env.recordCount(t))
	}
}

// ---- 独立 AD/BR 进程证据：两个 OS 进程共享同一个 SQLite 与密文目录 ----

const (
	pubBRHelperEnv = "PUB_BR_HELPER"
)

// TestAIConfigPublicationBRHelper 作为独立 BR 进程：按活动快照读取整套配置、
// 通过最后一跳 Gate，再真实发送到父进程的测试厂商。结果以单行 JSON 回传。
// 只在被父用例以子进程方式拉起时执行。
func TestAIConfigPublicationBRHelper(t *testing.T) {
	if os.Getenv(pubBRHelperEnv) == "" {
		t.Skip("仅作为独立 BR 进程入口")
	}
	type brOut struct {
		OK       bool   `json:"ok"`
		Error    string `json:"error,omitempty"`
		Revision int64  `json:"revision"`
		Base     string `json:"base,omitempty"`
		Model    string `json:"model,omitempty"`
		Reply    string `json:"reply,omitempty"`
	}
	write := func(o brOut) {
		b, _ := json.Marshal(o)
		fmt.Println("BR_RESULT " + string(b))
		os.Exit(0)
	}
	ctx := context.Background()
	dbPath, keyPath := os.Getenv("PUB_BR_DB"), os.Getenv("PUB_BR_KEY")
	db, err := store.Open(dbPath)
	if err != nil {
		write(brOut{Error: "open:" + err.Error()})
	}
	key, err := aibridge.LoadMasterKey(keyPath)
	if err != nil {
		write(brOut{Error: "master_key:" + err.Error()})
	}
	snap, err := aibridge.LoadSnapshot(ctx, db, keyPath, key)
	if err != nil {
		write(brOut{Error: "snapshot:" + err.Error()})
	}
	out := brOut{Revision: snap.Revision(), Base: snap.Spec.BaseURL, Model: snap.Spec.ModelID}
	if want := os.Getenv("PUB_BR_VENDOR"); snap.Spec.BaseURL != want {
		out.Error = "base_mismatch"
		write(out)
	}
	if err := aibridge.Gate(ctx, db, snap.Revision()); err != nil {
		out.Error = "gate:" + err.Error()
		write(out)
	}
	// 只信任父进程本地 fixture 的自签名证书；生产路径仍走安全 Transport。
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	p, err := aibridge.NewProviderWithTransport(snap.Spec, 10*time.Second, tr)
	if err != nil {
		out.Error = "provider:" + err.Error()
		write(out)
	}
	resp, err := p.Chat(ctx, aibridge.ChatRequest{
		Messages: []aibridge.Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		out.Error = "chat:" + err.Error()
		write(out)
	}
	out.OK, out.Reply = true, resp.Text
	write(out)
}

// runPubChild 以独立进程运行一个 helper 测试，返回 stdout。
func runPubChild(t *testing.T, testName string, extraEnv map[string]string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run="+testName)
	env := append(os.Environ(), "PUB_TEST_DIR_SYNC_OK=1")
	for k, v := range extraEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("子进程 %s 退出异常: %v\n%s", testName, err, stderr.String())
	}
	return string(out)
}

// TestAIConfigPublicationADBRProcesses 独立 AD 进程写入、独立 BR 进程读取并实际发送：
// 四个中断屏障后分别可见 A/A/A/B，父进程测试厂商只收到对应端点的一次调用。
func TestAIConfigPublicationADBRProcesses(t *testing.T) {
	vA := newPubVendor(t, "A")
	vB := newPubVendor(t, "B")
	env := newPubEnv(t, vA.srv.URL)
	if rec := env.save(t, env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-1111", 0)); rec.Code != 200 {
		t.Fatalf("初始保存失败: %s", rec.Body.String())
	}

	cases := []struct {
		scenario string
		wantBase string
		wantRev  int64
		wantA    int
		wantB    int
	}{
		{"partial_temp", vA.srv.URL, 1, 1, 0},
		{"object_only", vA.srv.URL, 1, 1, 0},
		{"uncommitted_sql", vA.srv.URL, 1, 1, 0},
		{"committed", vB.srv.URL, 2, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			// AD 进程：在命名屏障处退出（已提交/未提交/安装完成/准备件）。
			runPubChild(t, "TestAIConfigPublicationCrashHelper", map[string]string{
				pubCrashHelperEnv:  c.scenario,
				"PUB_CRASH_DB":     env.dbPath,
				"PUB_CRASH_KEY":    env.keyPath,
				"PUB_CRASH_VENDOR": vB.srv.URL,
			})
			// BR 进程：独立打开同一库与密文目录，复测实际发送。
			beforeA, beforeB := vA.count(), vB.count()
			stdout := runPubChild(t, "TestAIConfigPublicationBRHelper", map[string]string{
				pubBRHelperEnv:  "1",
				"PUB_BR_DB":     env.dbPath,
				"PUB_BR_KEY":    env.keyPath,
				"PUB_BR_VENDOR": c.wantBase,
			})
			line := ""
			for _, l := range strings.Split(stdout, "\n") {
				if strings.HasPrefix(l, "BR_RESULT ") {
					line = strings.TrimPrefix(l, "BR_RESULT ")
				}
			}
			if line == "" {
				t.Fatalf("%s 的 BR 进程没有回传结果: %q", c.scenario, stdout)
			}
			var out struct {
				OK       bool   `json:"ok"`
				Error    string `json:"error"`
				Revision int64  `json:"revision"`
				Base     string `json:"base"`
				Reply    string `json:"reply"`
			}
			if err := json.Unmarshal([]byte(line), &out); err != nil {
				t.Fatalf("解析 BR 结果: %v (%s)", err, line)
			}
			if !out.OK {
				t.Fatalf("%s 的独立 BR 进程失败: %s", c.scenario, line)
			}
			if out.Base != c.wantBase || out.Revision != c.wantRev {
				t.Fatalf("%s 的 BR 可见快照 = rev%d %s，期望 rev%d %s",
					c.scenario, out.Revision, out.Base, c.wantRev, c.wantBase)
			}
			if vA.count()-beforeA != c.wantA || vB.count()-beforeB != c.wantB {
				t.Fatalf("%s 出站增量 A=%d B=%d，期望 A=%d B=%d", c.scenario,
					vA.count()-beforeA, vB.count()-beforeB, c.wantA, c.wantB)
			}
			// 活动指针与记录保持一致，且父进程重启读取同一结果。
			restarted := env.secondHandle(t)
			snap := env.snapshot(t, restarted)
			if snap.Spec.BaseURL != c.wantBase || snap.Revision() != c.wantRev {
				t.Fatalf("%s 父进程重启后快照 = rev%d %s", c.scenario, snap.Revision(), snap.Spec.BaseURL)
			}
			if snap.Revision() > 0 && env.recordCount(t) != int(snap.Revision()) {
				t.Fatalf("%s 记录数 = %d，活动修订 = %d", c.scenario, env.recordCount(t), snap.Revision())
			}
		})
	}
}

// ---- 父目录持续同步失败：真实 HTTP 保存的重试不应绕过持久性确认 ----

// httpServer 启动真实后台 HTTP 路由（含鉴权中间件），返回服务器与签名 token。
func (e *pubEnv) httpServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts := httptest.NewServer(e.srv.Handler())
	t.Cleanup(ts.Close)
	return ts, signAdminToken(t, e.srv)
}

// signAdminToken 直接签一个管理 token：真实 HTTP 路由的 auth 中间件只验签。
func signAdminToken(t *testing.T, srv *Server) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Username: "http-tester",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString(srv.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// httpSave 走真实网络请求保存配置，返回状态码与响应正文。
func httpSave(t *testing.T, ts *httptest.Server, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+"/api/ai/config", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("保存请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestAIConfigPublicationParentSyncRetryHTTP 票 §2.1.1：
// 只让凭据版本目录的父目录持续同步失败、子目录同步可成功时，
// 首次保存 500 留下目录；原样重试与另一后台实例都必须继续拒绝，
// 活动指针与修订记录不变；故障移除后重试恰好发布一次。
func TestAIConfigPublicationParentSyncRetryHTTP(t *testing.T) {
	ctx := context.Background()
	vA := newPubVendor(t, "A")
	env := newPubEnv(t, vA.srv.URL)
	ts, token := env.httpServer(t)
	credDir := aibridge.CredentialDir(env.keyPath)

	var mu sync.Mutex
	parentCalls, childCalls := 0, 0
	restore := aibridge.SetCredentialDirSyncForTest(func(dir string) error {
		mu.Lock()
		defer mu.Unlock()
		if filepath.Clean(dir) == filepath.Clean(filepath.Dir(credDir)) {
			parentCalls++
			return errors.New("parent sync boom")
		}
		childCalls++
		return nil
	})
	defer restore()
	syncCalls := func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return parentCalls, childCalls
	}

	body := env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-parent-retry", 0)
	if code, resp := httpSave(t, ts, token, body); code != 500 {
		t.Fatalf("父目录持续失败时首次保存应 500，得到 %d: %s", code, resp)
	}
	if _, err := os.Stat(credDir); err != nil {
		t.Fatalf("首次失败后凭据版本目录应留下（复现前提）: %v", err)
	}
	if _, found, err := env.db.AIConfigActiveRaw(ctx); err != nil || found {
		t.Fatalf("首次失败不得发布活动指针: found=%v err=%v", found, err)
	}
	if n := env.recordCount(t); n != 0 {
		t.Fatalf("首次失败后发布记录 = %d，期望 0", n)
	}

	// 立即原样重试：目录已存在不能作为「父目录已确认」的证据。
	if code, resp := httpSave(t, ts, token, body); code != 500 {
		t.Fatalf("父目录持续失败时重试应继续拒绝（不得 200 发布），得到 %d: %s", code, resp)
	}
	if _, found, err := env.db.AIConfigActiveRaw(ctx); err != nil || found {
		t.Fatalf("重试被拒后活动指针仍不得出现: found=%v err=%v", found, err)
	}
	if n := env.recordCount(t); n != 0 {
		t.Fatalf("重试被拒后发布记录 = %d，期望 0", n)
	}
	if p, c := syncCalls(); p < 2 || c != 0 {
		t.Fatalf("父目录同步累计 = %d（应每次尝试都确认），子目录同步 = %d", p, c)
	}

	// 另一后台实例读到刚创建目录：独立 DB 连接 + 独立 Server，也必须拒绝。
	otherDB := env.secondHandle(t)
	otherSrv := NewServer(config.Admin{JWTSecret: "test"}, otherDB, logging.Discard(), WebFS())
	otherTS := httptest.NewServer(otherSrv.Handler())
	t.Cleanup(otherTS.Close)
	otherToken := signAdminToken(t, otherSrv)
	if code, resp := httpSave(t, otherTS, otherToken, body); code != 500 {
		t.Fatalf("另一后台实例读到刚创建目录时保存应拒绝，得到 %d: %s", code, resp)
	}
	if _, found, err := otherDB.AIConfigActiveRaw(ctx); err != nil || found {
		t.Fatalf("另一实例被拒后仍不得出现活动指针: found=%v err=%v", found, err)
	}
	if n := env.recordCount(t); n != 0 {
		t.Fatalf("另一实例被拒后发布记录 = %d，期望 0", n)
	}

	// 第三层证据：真正独立的第二个 AD 进程（另一个 OS 进程）读到刚创建目录，
	// 同样必须拒绝；它的父目录同步失败注入在自己的进程内。
	childURL, childCmd, childStdin := startADChild(t, map[string]string{
		pubE2EADHelperEnv: "1",
		pubE2EStageEnv:    "",
		pubE2ESyncEnv:     "parent_fail",
		pubE2EDBEnv:       env.dbPath,
		pubE2EKeyEnv:      env.keyPath,
	})
	code, resp, err := httpSaveRaw(t, childURL, token, body)
	if err != nil {
		t.Fatalf("独立 AD 进程保存请求异常: %v", err)
	}
	if code != 500 {
		t.Fatalf("独立 AD 进程读到刚创建目录时保存应拒绝，得到 %d: %s", code, resp)
	}
	_ = childStdin.Close()
	if werr := childCmd.Wait(); werr != nil {
		t.Fatalf("独立 AD 进程退出异常: %v", werr)
	}
	if _, found, err := env.db.AIConfigActiveRaw(ctx); err != nil || found {
		t.Fatalf("独立 AD 进程被拒后活动指针仍不得出现: found=%v err=%v", found, err)
	}
	if n := env.recordCount(t); n != 0 {
		t.Fatalf("独立 AD 进程被拒后发布记录 = %d，期望 0", n)
	}

	if p, c := syncCalls(); p != 3 || c != 0 {
		t.Fatalf("三次失败尝试的父目录同步累计 = %d（每次都要确认），子目录同步 = %d", p, c)
	}

	// 故障移除：重试恰好发布一次。
	restore()
	if code, resp := httpSave(t, otherTS, otherToken, body); code != 200 {
		t.Fatalf("故障移除后重试应成功，得到 %d: %s", code, resp)
	}
	if rev, err := aibridge.ActiveRevision(ctx, otherDB); err != nil || rev != 1 {
		t.Fatalf("故障移除后活动修订 = %d, %v，期望 1", rev, err)
	}
	if n := env.recordCount(t); n != 1 {
		t.Fatalf("故障移除后发布记录 = %d，期望恰好 1", n)
	}
}

// ---- §2.2 逐屏障端到端：真实保存 HTTP → 存储发布 → Runner → 复测 → 页面 ----

const (
	pubE2EADHelperEnv  = "PUB_E2E_AD_HELPER"
	pubRunnerHelperEnv = "PUB_RUNNER_HELPER"
	pubE2EStageEnv     = "PUB_E2E_STAGE"
	pubE2ESyncEnv      = "PUB_E2E_SYNC"
	pubE2EDBEnv        = "PUB_E2E_DB"
	pubE2EKeyEnv       = "PUB_E2E_KEY"
	pubE2ETaskEnv      = "PUB_E2E_TASK_ID"
)

// TestAIConfigPublicationE2EADHelper 独立 AD 进程：起真实后台 HTTP 路由，
// 在指定屏障处 os.Exit（进程中断），否则处理一次保存后等待 stdin 关闭退出。
func TestAIConfigPublicationE2EADHelper(t *testing.T) {
	if os.Getenv(pubE2EADHelperEnv) == "" {
		t.Skip("仅作为 §2.2 端到端 AD 子进程入口")
	}
	dbPath, keyPath := os.Getenv(pubE2EDBEnv), os.Getenv(pubE2EKeyEnv)
	stage := os.Getenv(pubE2EStageEnv)
	syncMode := os.Getenv(pubE2ESyncEnv)
	credDir := aibridge.CredentialDir(keyPath)
	parentDir := filepath.Dir(credDir)
	if syncMode == "parent_fail" {
		aibridge.SetCredentialDirSyncForTest(func(dir string) error {
			if filepath.Clean(dir) == filepath.Clean(parentDir) {
				return errors.New("parent sync boom")
			}
			return nil
		})
	} else if syncMode != "native" {
		aibridge.SetCredentialDirSyncForTest(func(string) error { return nil })
	}
	switch stage {
	case "partial_temp":
		aibridge.SetCredentialObjectStageHookForTest(func(st, _ string) {
			if st == "partial_temp" {
				os.Exit(0)
			}
		})
	case "object_only":
		aibridge.SetCredentialObjectStageHookForTest(func(st, dir string) {
			if st == "installed" {
				os.Exit(0)
			}
		})
	case "uncommitted_sql":
		store.SetAIPublishBarrierForTest(func(st string) {
			if st == "before_commit" {
				os.Exit(0)
			}
		})
	case "committed":
		store.SetAIPublishBarrierForTest(func(st string) {
			if st == "after_commit" {
				os.Exit(0)
			}
		})
	}
	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := db.Migrate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	ts := httptest.NewServer(srv.Handler())
	fmt.Println("AD_READY " + ts.URL)
	// 处理完一次请求（或屏障退出）后由父进程关闭 stdin。
	_, _ = io.Copy(io.Discard, os.Stdin)
	ts.Close()
	os.Exit(0)
}

// TestAIConfigPublicationRunnerHelper 独立 BR 进程：构造真实 Runner，
// 从共享库取一条待处理任务并按活动快照实际发送到父进程的假厂商。
func TestAIConfigPublicationRunnerHelper(t *testing.T) {
	if os.Getenv(pubRunnerHelperEnv) == "" {
		t.Skip("仅作为 §2.2 Runner 子进程入口")
	}
	dbPath, keyPath := os.Getenv(pubE2EDBEnv), os.Getenv(pubE2EKeyEnv)
	taskID := os.Getenv(pubE2ETaskEnv)
	db, err := store.Open(dbPath)
	if err != nil {
		fmt.Println("RUNNER_RESULT " + `{"ok":false,"error":"open"}`)
		os.Exit(0)
	}
	key, err := aibridge.LoadMasterKey(keyPath)
	if err != nil {
		fmt.Println("RUNNER_RESULT " + `{"ok":false,"error":"master_key"}`)
		os.Exit(0)
	}
	// 只信任父进程本地 fixture 的自签名证书；生产仍走安全 Transport。
	aibridge.SetRunnerProviderTransportForTest(&http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	})
	r := aibridge.NewRunner(db, nil, config.AI{
		Enabled: true, PollIntervalMS: 20, MaxConcurrent: 1, RequestTimeoutSec: 5,
		MasterKeyFile: keyPath,
	}, key, logging.Discard())
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go r.Loop(ctx)
	var taskN int64
	if _, err := fmt.Sscan(taskID, &taskN); err != nil {
		fmt.Println("RUNNER_RESULT " + `{"ok":false,"error":"task_id"}`)
		os.Exit(0)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status, reply string
		err := db.SQL.QueryRow(`SELECT status, reply_text FROM ai_result WHERE task_id=?`, taskN).
			Scan(&status, &reply)
		if err == nil {
			out, _ := json.Marshal(map[string]any{"ok": true, "status": status, "reply": reply})
			fmt.Println("RUNNER_RESULT " + string(out))
			os.Exit(0)
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Println("RUNNER_RESULT " + `{"ok":false,"error":"timeout"}`)
	os.Exit(0)
}

// startADChild 拉起独立 AD 进程并读回真实 HTTP 地址。
func startADChild(t *testing.T, envMap map[string]string) (string, *exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestAIConfigPublicationE2EADHelper")
	cmd.Env = append(os.Environ(), "PUB_TEST_DIR_SYNC_OK=1")
	for k, v := range envMap {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 AD 子进程: %v", err)
	}
	t.Cleanup(func() {
		// 用例中途失败时也要收掉子进程（正常路径已 Wait，重复调用仅返回错误）。
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	br := bufio.NewReader(stdout)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			_ = cmd.Wait()
			t.Fatalf("AD 子进程未就绪: %v\n%s", err, stderr.String())
		}
		if strings.HasPrefix(line, "AD_READY ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "AD_READY ")), cmd, stdin
		}
	}
	_ = cmd.Process.Kill()
	t.Fatalf("AD 子进程 15s 内未就绪\n%s", stderr.String())
	return "", nil, nil
}

// httpSaveRaw 发真实 HTTP 保存请求，返回状态码、正文与传输错误（中断时非 nil）。
func httpSaveRaw(t *testing.T, baseURL, token, body string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequest("POST", baseURL+"/api/ai/config", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}

// httpJSON 发真实 HTTP 请求并解析 JSON（GET 时 body 为空）。
func httpJSON(t *testing.T, baseURL, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, baseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s %s 响应不是 JSON: %v: %s", method, path, err, raw)
	}
	return resp.StatusCode, out
}

// seedRunnerTask 往共享库排一条真实 DM 任务，交给独立 Runner 进程消费。
func seedRunnerTask(t *testing.T, db *store.DB, text string) int64 {
	t.Helper()
	id, err := db.AIEnqueue(t.Context(), &store.AITask{
		Kind: "dm", Scope: "uid", ScopeKey: "9100100", UID: 9100100, Agent: "companion",
		ContactUID: 2147483645, SourceKind: "human", Text: text, DeadlineTS: float64(time.Now().Unix() + 60),
	})
	if err != nil {
		t.Fatalf("排 Runner 任务: %v", err)
	}
	return id
}

// TestAIConfigPublicationEndToEndHTTP 票 §2.2：四个进程中断屏障逐一贯通
// 真实保存 HTTP（独立 AD 进程）→ 存储发布 → 独立 Runner 进程实际发送 →
// 后台复测 HTTP → 页面状态 HTTP。进程边界：写进程、读/发送进程、复测与页面
// 服务进程均为独立 OS 进程或独立 Server 实例，只共享 SQLite 与密文目录。
func TestAIConfigPublicationEndToEndHTTP(t *testing.T) {
	testAIConfigPublicationEndToEndHTTP(t, "ok")
}

func testAIConfigPublicationEndToEndHTTP(t *testing.T, syncMode string) {
	vA := newPubVendor(t, "A")
	vB := newPubVendor(t, "B")
	env := newPubEnv(t, vA.srv.URL)
	if syncMode == "native" {
		defer aibridge.SetCredentialDirSyncForTest(nil)()
	}
	ts, token := env.httpServer(t)
	env.withDispatch(t, hostDispatch{
		hostOf(t, vA.srv.URL): vA.srv.Client().Transport,
		hostOf(t, vB.srv.URL): vB.srv.Client().Transport,
	})

	// 初始发布 rev1（真实 HTTP）。
	if code, body := httpSave(t, ts, token, env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-e2e", 0)); code != 200 {
		t.Fatalf("初始真实 HTTP 保存失败 %d: %s", code, body)
	}

	type e2eCase struct {
		stage       string
		body        string
		wantBase    string
		wantRev     int64
		wantA       int
		wantB       int
		wantObjects int
	}
	cases := []e2eCase{
		{"partial_temp", env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-p1", 1), vA.srv.URL, 1, 1, 0, 1},
		{"object_only", env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-p2", 1), vA.srv.URL, 1, 1, 0, 2},
		{"uncommitted_sql", env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-A-p3", 1), vA.srv.URL, 1, 1, 0, 3},
		{"committed", env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-B-e2e", 1), vB.srv.URL, 2, 0, 1, 4},
	}
	for _, c := range cases {
		c := c
		t.Run(c.stage, func(t *testing.T) {
			// 1) 独立 AD 进程：真实 HTTP 保存入口，在命名屏障处终止。
			url, cmd, stdin := startADChild(t, map[string]string{
				pubE2EADHelperEnv: "1",
				pubE2EStageEnv:    c.stage,
				pubE2ESyncEnv:     syncMode,
				pubE2EDBEnv:       env.dbPath,
				pubE2EKeyEnv:      env.keyPath,
			})
			code, body, err := httpSaveRaw(t, url, token, c.body)
			if c.stage == "committed" || c.stage == "uncommitted_sql" || c.stage == "object_only" || c.stage == "partial_temp" {
				if err == nil {
					t.Fatalf("%s 屏障应终止写进程（连接中断），实际返回 %d: %s", c.stage, code, body)
				}
			}
			_ = stdin.Close()
			if werr := cmd.Wait(); werr != nil {
				t.Fatalf("%s 的 AD 子进程退出异常: %v", c.stage, werr)
			}
			// 进程边界证据：请求确实进到了真实保存处理器，屏障位置符合预期。
			if c.stage == "partial_temp" {
				files, err := filepath.Glob(filepath.Join(aibridge.CredentialDir(env.keyPath), ".ai_credential-*.tmp"))
				if err != nil || len(files) != 1 {
					t.Fatalf("partial_temp 准备件数=%d: %v", len(files), err)
				}
				data, err := os.ReadFile(files[0])
				if err != nil || string(data) != "{" {
					t.Fatalf("真实准备件应只写入一个字节，实得长度=%d: %v", len(data), err)
				}
			}
			if n := len(env.objectRefs(t)); n != c.wantObjects {
				t.Fatalf("%s 后密文对象数 = %d，期望 %d（证明真实保存已走到该屏障）",
					c.stage, n, c.wantObjects)
			}

			// 2) 存储发布：活动修订/指针/记录数。
			db := env.secondHandle(t)
			snap := env.snapshot(t, db)
			if snap.Spec.BaseURL != c.wantBase || snap.Revision() != c.wantRev {
				t.Fatalf("%s 重启后快照 = rev%d %s，期望 rev%d %s",
					c.stage, snap.Revision(), snap.Spec.BaseURL, c.wantRev, c.wantBase)
			}
			if n := env.recordCount(t); n != int(c.wantRev) {
				t.Fatalf("%s 发布记录 = %d，期望 %d", c.stage, n, c.wantRev)
			}

			// 3) 独立 Runner 进程：真实 Runner 取任务、按快照发送。
			taskID := seedRunnerTask(t, db, "ping-"+c.stage)
			beforeA, beforeB := vA.count(), vB.count()
			stdout := runPubChild(t, "TestAIConfigPublicationRunnerHelper", map[string]string{
				pubRunnerHelperEnv: "1",
				pubE2EDBEnv:        env.dbPath,
				pubE2EKeyEnv:       env.keyPath,
				pubE2ETaskEnv:      fmt.Sprint(taskID),
			})
			line := ""
			for _, l := range strings.Split(stdout, "\n") {
				if strings.HasPrefix(l, "RUNNER_RESULT ") {
					line = strings.TrimPrefix(l, "RUNNER_RESULT ")
				}
			}
			if line == "" {
				t.Fatalf("%s 的 Runner 子进程没有回传结果: %q", c.stage, stdout)
			}
			var run struct {
				OK     bool   `json:"ok"`
				Error  string `json:"error"`
				Status string `json:"status"`
				Reply  string `json:"reply"`
			}
			if err := json.Unmarshal([]byte(line), &run); err != nil {
				t.Fatalf("%s 解析 Runner 结果: %v (%s)", c.stage, err, line)
			}
			if !run.OK || run.Status != "done" || strings.TrimSpace(run.Reply) == "" {
				t.Fatalf("%s Runner 未完成发送: %s", c.stage, line)
			}
			if vA.count()-beforeA != c.wantA || vB.count()-beforeB != c.wantB {
				t.Fatalf("%s Runner 出站增量 A=%d B=%d，期望 A=%d B=%d",
					c.stage, vA.count()-beforeA, vB.count()-beforeB, c.wantA, c.wantB)
			}

			// 4) 页面状态（真实 HTTP）。
			code, page := httpJSON(t, ts.URL, token, "GET", "/api/ai/status", "")
			if code != 200 {
				t.Fatalf("%s 状态接口 %d", c.stage, code)
			}
			if int64(page["revision"].(float64)) != c.wantRev {
				t.Fatalf("%s 页面修订 = %v，期望 %d", c.stage, page["revision"], c.wantRev)
			}
			dest := page["destination"].(map[string]any)
			if dest["host"] != hostOf(t, c.wantBase) {
				t.Fatalf("%s 页面目的地 = %v，期望 %s", c.stage, dest["host"], hostOf(t, c.wantBase))
			}
			cred := page["credential"].(map[string]any)
			if cred["state"] != "configured" {
				t.Fatalf("%s 页面凭据状态 = %v", c.stage, cred["state"])
			}

			// 5) 后台复测（真实 HTTP）：只读活动快照、只发固定合成输入。
			beforeA, beforeB = vA.count(), vB.count()
			code, retest := httpJSON(t, ts.URL, token, "POST", "/api/ai/test", `{}`)
			if code != 200 || retest["ok"] != true {
				t.Fatalf("%s 复测失败 %d: %v", c.stage, code, retest)
			}
			if vA.count()-beforeA != c.wantA || vB.count()-beforeB != c.wantB {
				t.Fatalf("%s 复测出站增量 A=%d B=%d，期望 A=%d B=%d",
					c.stage, vA.count()-beforeA, vB.count()-beforeB, c.wantA, c.wantB)
			}
			_, page = httpJSON(t, ts.URL, token, "GET", "/api/ai/status", "")
			retestView := page["retest"].(map[string]any)
			if retestView["ok"] != true || int64(retestView["revision"].(float64)) != c.wantRev {
				t.Fatalf("%s 页面复测状态 = %v，期望修订 %d 已通过", c.stage, retestView, c.wantRev)
			}
			if saved, _ := page["saved_display"].(string); !strings.Contains(saved, "已通过复测") {
				t.Fatalf("%s 页面保存文案 = %q", c.stage, saved)
			}
		})
	}
}

// TestAIConfigPublicationRealPlatformHTTP runs authenticated saves on the
// actual filesystem, including preparation of the first credential.
func TestAIConfigPublicationRealPlatformHTTP(t *testing.T) {
	ctx := context.Background()
	vA := newPubVendor(t, "A")
	vB := newPubVendor(t, "B")
	env := newPubEnv(t, vA.srv.URL)
	ts, token := env.httpServer(t)
	aibridge.SetCredentialDirSyncForTest(nil)
	if code, body := httpSave(t, ts, token, env.saveBody(vA.srv.URL, "model-a", "sk-synthetic-real-A", 0)); code != 200 {
		t.Fatalf("真实首次保存失败 %d: %s", code, body)
	}
	if rev := env.revision(t, env.db); rev != 1 || env.recordCount(t) != 1 {
		t.Fatalf("首次修订=%d，记录数=%d", rev, env.recordCount(t))
	}
	if snap := env.snapshot(t, env.secondHandle(t)); snap.Spec.APIKey != "sk-synthetic-real-A" {
		t.Fatal("首次发布后独立读者无法解密已保存凭据")
	}
	code, body := httpSave(t, ts, token, env.saveBody(vB.srv.URL, "model-b", "sk-synthetic-real-B", 1))
	if code != 200 {
		t.Fatalf("目标平台支持目录 fsync，换域保存应成功，得到 %d: %s", code, body)
	}
	if rev, err := aibridge.ActiveRevision(ctx, env.db); err != nil || rev != 2 {
		t.Fatalf("目标平台保存后修订 = %d, %v，期望 2", rev, err)
	}
	snap := env.snapshot(t, env.db)
	if snap.Spec.BaseURL != vB.srv.URL || snap.Spec.APIKey != "sk-synthetic-real-B" || snap.Spec.ModelID != "model-b" {
		t.Fatalf("目标平台保存后快照 = %+v", snap.Spec)
	}
}

func TestDR109WindowsPublication(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 原生凭据安装验收")
	}
	t.Run("真实首存和换域", TestAIConfigPublicationRealPlatformHTTP)
	t.Run("目录刷新失败仍保护旧套", TestAIConfigPublicationInstallDurability)
	t.Run("父目录持续失败和跨进程重试", TestAIConfigPublicationParentSyncRetryHTTP)
	t.Run("Windows AD和BR 发布中断", func(t *testing.T) { testAIConfigPublicationEndToEndHTTP(t, "native") })
}
