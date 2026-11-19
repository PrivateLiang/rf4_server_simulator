package store_test

// 本文件从 internal/store 的外部测试包走真实后台 HTTP 路由，
// 复现 2026-11-16 的「保存厂商与密钥」500（T-016）。
// 只使用合成数据；不连接真实厂商、不使用真实凭据。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/admin"
	"rf4server/internal/aibridge"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

func TestAdminProviderSaveOnTwoModelRows(t *testing.T) {
	// V2：换源保存必须带目标 Key（新 Key 先落不可变密文对象）。
	t.Setenv("RF4_AI_MASTER_KEY_FILE", filepath.Join(t.TempDir(), "ai_master.key"))
	// 测试模拟支持目录 fsync 的平台；真实平台的拒绝发布由 admin 包覆盖。
	t.Cleanup(aibridge.SetCredentialDirSyncForTest(func(string) error { return nil }))
	db, err := store.Open(filepath.Join(t.TempDir(), "admin-dirty.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	ctx := context.Background()

	// 真实库形态：有效模型行 + 迁移种子遗留的空行。
	var providerID, modelRowID, blankRowID int64
	if err := db.SQL.QueryRow(
		`SELECT id FROM ai_provider WHERE enabled=1 ORDER BY id LIMIT 1`).Scan(&providerID); err != nil {
		t.Fatalf("读取 provider: %v", err)
	}
	if _, err := db.SQL.Exec(`
		UPDATE ai_model SET model_id='deepseek-flash'
		WHERE id=(SELECT MIN(id) FROM ai_model WHERE provider_id=? AND model_id='')`,
		providerID); err != nil {
		t.Fatalf("构造有效模型行: %v", err)
	}
	res, err := db.SQL.Exec(`
		INSERT INTO ai_model(provider_id, model_id, display, context_limit, supports_tools, supports_thinking, enabled)
		VALUES(?, '', '历史种子遗留（空模型）', 0, 1, 1, 1)`, providerID)
	if err != nil {
		t.Fatalf("构造空模型行: %v", err)
	}
	if blankRowID, err = res.LastInsertId(); err != nil {
		t.Fatalf("空模型行 id: %v", err)
	}
	if err := db.SQL.QueryRow(`
		SELECT id FROM ai_model WHERE provider_id=? AND model_id='deepseek-flash'`,
		providerID).Scan(&modelRowID); err != nil {
		t.Fatalf("有效模型行 id: %v", err)
	}

	if err := db.EnsureAdmin(ctx, "admin", "admin123"); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	srv := admin.NewServer(config.Admin{HTTPAddr: "127.0.0.1:0", JWTSecret: "test"},
		db, logging.Discard(), admin.WebFS())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// 登录拿 token。
	loginBody := bytes.NewBufferString(`{"username":"admin","password":"admin123"}`)
	loginResp, err := http.Post(ts.URL+"/api/login", "application/json", loginBody)
	if err != nil {
		t.Fatalf("登录请求: %v", err)
	}
	defer loginResp.Body.Close()
	if loginResp.StatusCode != 200 {
		t.Fatalf("登录返回 %d", loginResp.StatusCode)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(loginResp.Body).Decode(&login); err != nil || login.Token == "" {
		t.Fatalf("登录响应无 token: %v", err)
	}

	// 真实后台入口：保存厂商地址、模型与目标 Key。
	req, _ := http.NewRequest("POST", ts.URL+"/api/ai/config",
		strings.NewReader(`{"base_url":"https://vendor.invalid/v1","model_id":"model-new","api_key":"sk-synthetic-http-0001"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+login.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("保存请求: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("两行 ai_model 的库上保存返回 %d: %s", resp.StatusCode, buf.String())
	}

	// 独立真值：只改后台选中的行，空行原样保留。
	type row struct {
		id      int64
		model   string
		enabled int
	}
	rows, err := db.SQL.Query(`SELECT id, model_id, enabled FROM ai_model
		WHERE provider_id=? ORDER BY id`, providerID)
	if err != nil {
		t.Fatalf("读取模型行: %v", err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.model, &r.enabled); err != nil {
			t.Fatalf("扫描模型行: %v", err)
		}
		got = append(got, r)
	}
	want := []row{
		{id: modelRowID, model: "model-new", enabled: 1},
		{id: blankRowID, model: "", enabled: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("模型行 = %+v，期望 %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 行 = %+v，期望 %+v", i, got[i], want[i])
		}
	}
	if got := db.AISetting(ctx, "ai_egress_destination", ""); got == "" {
		t.Fatal("保存成功后没有写入目的地快照")
	}

	// 换源留空 Key：整笔拒绝，配置与修订都不变（保存失败仍用旧套）。
	before := db.AISetting(ctx, "ai_config_active", "")
	req2, _ := http.NewRequest("POST", ts.URL+"/api/ai/config",
		strings.NewReader(`{"base_url":"https://other.invalid/v1","model_id":"model-other"}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+login.Token)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("换源留空请求: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("换源留空 Key 返回 %d，期望 400", resp2.StatusCode)
	}
	if after := db.AISetting(ctx, "ai_config_active", ""); after != before {
		t.Fatalf("被拒绝的保存改变了活动修订: %q → %q", before, after)
	}
	var curBase string
	if err := db.SQL.QueryRow(`SELECT base_url FROM ai_provider WHERE id=?`, providerID).Scan(&curBase); err != nil {
		t.Fatal(err)
	}
	if curBase != "https://vendor.invalid/v1" {
		t.Fatalf("被拒绝的保存改写了厂商地址: %q", curBase)
	}
}
