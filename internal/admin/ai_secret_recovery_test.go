package admin

// T-012 后台侧回归：新凭据只写外部受保护凭据文件、不落共享库；
// 主密钥缺失/损坏/不匹配时锁定并保留原文件；旧库密文只读导出。
// 全部为合成数据 + 本地 SQLite，不连接任何真实厂商。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/aibridge"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// aiSecretServer 用独立的临时主密钥目录构建后台，返回服务与解析后的两个密钥路径。
func aiSecretServer(t *testing.T) (*Server, *store.DB, string, string) {
	t.Helper()
	// 测试模拟支持目录 fsync 的平台；真实平台的拒绝发布见 ai_config_publication_test。
	t.Cleanup(aibridge.SetCredentialDirSyncForTest(func(string) error { return nil }))
	keyDir := t.TempDir()
	keyPath := filepath.Join(keyDir, "ai_master.key")
	t.Setenv("RF4_AI_MASTER_KEY_FILE", keyPath)
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	return srv, db, keyPath, aibridge.CredentialsPath(keyPath)
}

func saveAIConfig(t *testing.T, srv *Server, baseURL, modelID, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"base_url":%q,"model_id":%q,"api_key":%q}`, baseURL, modelID, apiKey)
	return postAs(t, srv, srv.handleAIConfigSave, "/api/ai/config", body, "tester")
}

func countAISecret(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_secret`).Scan(&n); err != nil {
		t.Fatalf("统计 ai_secret: %v", err)
	}
	return n
}

func countAuditAction(t *testing.T, db *store.DB, action string) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=?`, action).Scan(&n); err != nil {
		t.Fatalf("统计审计 %s: %v", action, err)
	}
	return n
}

// TestAIConfigSaveWritesExternalCredentialNotSharedDB 首次空安装的显式保存：
// 初始化主密钥 → 新凭据写成不可变密文对象；ai_secret 一行都不写；写入口保持停用。
func TestAIConfigSaveWritesExternalCredentialNotSharedDB(t *testing.T) {
	srv, db, keyPath, _ := aiSecretServer(t)
	const plain = "sk-synthetic-t012-save-0420"
	rec := saveAIConfig(t, srv, "https://vendor.invalid/v1", "model-a", plain)
	if rec.Code != 200 {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	// 共享库里没有新凭据（表还在，只是不再有新行）。
	if n := countAISecret(t, db); n != 0 {
		t.Fatalf("ai_secret 行数 = %d，期望 0（新凭据不落共享 DB）", n)
	}
	if err := db.AISaveSecret(context.Background(), 1, []byte{1}, []byte{2}, "sk-****0000"); !errors.Is(err, store.ErrAISecretWriteRetired) {
		t.Fatalf("旧写入口错误 = %v，期望 ErrAISecretWriteRetired", err)
	}
	if n := countAISecret(t, db); n != 0 {
		t.Fatalf("停用的写入口仍写入 ai_secret: %d 行", n)
	}
	// 主密钥与密文对象分开存在；对象文件里没有明文。
	key, err := aibridge.LoadMasterKey(keyPath)
	if err != nil {
		t.Fatalf("首次保存应显式初始化主密钥: %v", err)
	}
	refs := credentialObjectRefs(t, keyPath)
	if len(refs) != 1 {
		t.Fatalf("首次保存后密文对象 = %d 个，期望 1", len(refs))
	}
	raw, err := os.ReadFile(filepath.Join(aibridge.CredentialDir(keyPath), refs[0]+".json"))
	if err != nil {
		t.Fatalf("读取密文对象: %v", err)
	}
	if bytes.Contains(raw, []byte(plain)) {
		t.Fatal("密文对象里出现明文 Key")
	}
	obj, got, err := aibridge.ResolveCredentialObject(aibridge.CredentialDir(keyPath), refs[0], key, 0)
	if err != nil || got != plain {
		t.Fatalf("密文对象解密 = (%q, %v)", got, err)
	}
	if obj.ProviderID <= 0 || obj.KeyID != aibridge.MasterKeyID(key) {
		t.Fatalf("对象绑定字段不完整: %+v", obj)
	}
	// 首次初始化有独立审计；重复保存不再初始化。
	if n := countAuditAction(t, db, "ai_master_key"); n != 1 {
		t.Fatalf("ai_master_key 审计 = %d，期望 1", n)
	}
	if rec := saveAIConfig(t, srv, "", "", "sk-synthetic-t012-save-0421"); rec.Code != 200 {
		t.Fatalf("第二次保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	if n := countAuditAction(t, db, "ai_master_key"); n != 1 {
		t.Fatalf("第二次保存重复初始化主密钥: 审计 = %d", n)
	}
	if n := countAISecret(t, db); n != 0 {
		t.Fatalf("第二次保存后 ai_secret 行数 = %d", n)
	}
	if refs := credentialObjectRefs(t, keyPath); len(refs) != 2 {
		t.Fatalf("第二次保存后密文对象 = %d 个，期望 2（旧对象不回收）", len(refs))
	}
	// 状态接口仍给兼容的遮蔽值，不回显明文。
	status := httptest.NewRecorder()
	srv.handleAIStatus(status, httptest.NewRequest("GET", "/api/ai/status", nil))
	if status.Code != 200 {
		t.Fatalf("状态接口 %d: %s", status.Code, status.Body.String())
	}
	var body struct {
		APIKeyMasked string `json:"api_key_masked"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析状态: %v", err)
	}
	if body.APIKeyMasked != "sk-****0421" {
		t.Fatalf("api_key_masked = %q，期望 sk-****0421", body.APIKeyMasked)
	}
	if strings.Contains(status.Body.String(), plain) || strings.Contains(status.Body.String(), "sk-synthetic") {
		t.Fatal("状态接口泄露明文 Key")
	}
}

// credentialObjectRefs 列出密文版本目录里的对象引用（文件名去掉 .json）。
func credentialObjectRefs(t *testing.T, keyPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(aibridge.CredentialDir(keyPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("读取密文目录: %v", err)
	}
	refs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		refs = append(refs, strings.TrimSuffix(e.Name(), ".json"))
	}
	return refs
}

// TestAIConfigSaveLocksWithoutMasterKeyWhenLegacySecretExists 旧库已有密文而主密钥
// 缺失：不得自动新建主密钥，保存被锁定；旧密文原样保留。
func TestAIConfigSaveLocksWithoutMasterKeyWhenLegacySecretExists(t *testing.T) {
	srv, db, keyPath, credPath := aiSecretServer(t)
	ctx := context.Background()
	// 历史主密钥（不落盘）加密的旧密文：模拟换机器/丢主密钥。
	oldKey := bytes.Repeat([]byte{0x21}, 32)
	ct, nonce, err := aibridge.Encrypt(oldKey, []byte("sk-synthetic-legacy-9999"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES((SELECT id FROM ai_provider WHERE name='deepseek'),?,?,'default','sk-****9999',0)`,
		ct, nonce); err != nil {
		t.Fatal(err)
	}
	const fresh = "sk-synthetic-new-after-lost-1010"
	rec := saveAIConfig(t, srv, "https://vendor.invalid/v1", "model-a", fresh)
	if rec.Code != 500 {
		t.Fatalf("缺主密钥且已有密文时保存返回 %d，期望 500: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "主密钥") {
		t.Fatalf("拒绝原因未说明主密钥: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), fresh) || strings.Contains(rec.Body.String(), "sk-synthetic") {
		t.Fatalf("错误回显泄露凭据: %s", rec.Body.String())
	}
	// 没有自动生成主密钥，也没有创建凭据文件。
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺主密钥时被自动新建: %v", err)
	}
	if _, err := os.Stat(credPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("锁定时仍创建了凭据文件: %v", err)
	}
	// 旧密文与遮蔽值原样保留。
	rows, err := db.AIListSecrets(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("旧库行 = %d, %v", len(rows), err)
	}
	if !bytes.Equal(rows[0].Ciphertext, ct) || rows[0].Masked != "sk-****9999" {
		t.Fatal("锁定时旧密文被改动")
	}
}

// TestAIConfigSaveKeepsCorruptMasterKey 主密钥文件损坏：明确报错、保留原文件、
// 不新建凭据文件。
func TestAIConfigSaveKeepsCorruptMasterKey(t *testing.T) {
	srv, _, keyPath, credPath := aiSecretServer(t)
	corrupt := []byte("short-key")
	if err := os.WriteFile(keyPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := saveAIConfig(t, srv, "https://vendor.invalid/v1", "model-a", "sk-synthetic-corrupt-2020")
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "损坏") {
		t.Fatalf("损坏主密钥保存 = %d: %s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, corrupt) {
		t.Fatal("损坏的主密钥文件被改写")
	}
	if _, err := os.Stat(credPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("主密钥损坏时仍创建了凭据文件")
	}
}

// TestAIConfigSaveMigratesLegacySecretToImmutableObject 已有主密钥的升级形态：
// 旧库只读解析后生成不可变对象；旧文件不写、旧库一行不动、重复保存不重复登记。
func TestAIConfigSaveMigratesLegacySecretToImmutableObject(t *testing.T) {
	srv, db, keyPath, credPath := aiSecretServer(t)
	ctx := context.Background()
	key, err := aibridge.InitMasterKey(keyPath) // 升级场景：主密钥已存在
	if err != nil {
		t.Fatal(err)
	}
	// 第二个厂商只有旧库密文，不会被本次保存影响。
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO ai_provider(name,protocol,base_url) VALUES('vendor2','openai_chat','https://vendor2.invalid')`); err != nil {
		t.Fatal(err)
	}
	insertLegacy := func(provider string, plain, masked string) ([]byte, []byte) {
		t.Helper()
		ct, nonce, err := aibridge.Encrypt(key, []byte(plain))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.SQL.ExecContext(ctx, `
			INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
			VALUES((SELECT id FROM ai_provider WHERE name=?),?,?,'default',?,0)`, provider, ct, nonce, masked); err != nil {
			t.Fatal(err)
		}
		return ct, nonce
	}
	ct1, nonce1 := insertLegacy("deepseek", "sk-synthetic-legacy-v1-1111", "sk-****1111")
	ct2, nonce2 := insertLegacy("vendor2", "sk-synthetic-legacy-v2-2222", "sk-****2222")

	const fresh = "sk-synthetic-replaced-3333"
	if rec := saveAIConfig(t, srv, "https://vendor.invalid/v1", "model-a", fresh); rec.Code != 200 {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	// V2 不再写旧的多厂商凭据文件。
	if _, err := os.Stat(credPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("V2 保存不应再写旧凭据文件: %v", err)
	}
	refs := credentialObjectRefs(t, keyPath)
	if len(refs) != 1 {
		t.Fatalf("迁移后密文对象 = %d 个，期望 1", len(refs))
	}
	obj, got, err := aibridge.ResolveCredentialObject(aibridge.CredentialDir(keyPath), refs[0], key, 0)
	if err != nil || got != fresh {
		t.Fatalf("对象解密 = (%q, %v)", got, err)
	}
	if obj.Provider != "deepseek" {
		t.Fatalf("对象厂商 = %q，期望 deepseek", obj.Provider)
	}
	// 旧库两行都保留，密文逐字节不变（不删数据、不重新加密）。
	rows, err := db.AIListSecrets(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("旧库行 = %d, %v", len(rows), err)
	}
	byName := map[string]store.AISecretRow{}
	for _, r := range rows {
		byName[r.Provider] = r
	}
	if !bytes.Equal(byName["deepseek"].Ciphertext, ct1) || !bytes.Equal(byName["deepseek"].Nonce, nonce1) {
		t.Fatal("deepseek 旧密文被改动")
	}
	if !bytes.Equal(byName["vendor2"].Ciphertext, ct2) || !bytes.Equal(byName["vendor2"].Nonce, nonce2) {
		t.Fatal("vendor2 旧密文被改动")
	}
	// 再次留空保存（同源同字段）不新建对象、不重复登记。
	if rec := saveAIConfig(t, srv, "", "", ""); rec.Code != 200 {
		t.Fatalf("第二次保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	if refs := credentialObjectRefs(t, keyPath); len(refs) != 1 {
		t.Fatalf("第二次保存后密文对象 = %d 个，期望仍为 1", len(refs))
	}
	if rows, err := db.AIListSecrets(ctx); err != nil || len(rows) != 2 {
		t.Fatalf("第二次保存后旧库行 = %d, %v", len(rows), err)
	}
	var records int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_setting WHERE key LIKE 'ai_config_record:%'`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 1 {
		t.Fatalf("发布记录 = %d 条，期望 1（无变更不重复登记）", records)
	}
}

// TestAIConfigSaveLocksWhenOnlyCredentialFileExists 主密钥缺失但外部凭据文件已存在：
// 后台保存与 -init-key 走同一判定，拒绝自动创建主密钥并保留凭据文件。
func TestAIConfigSaveLocksWhenOnlyCredentialFileExists(t *testing.T) {
	srv, _, keyPath, credPath := aiSecretServer(t)
	credBytes := []byte(`{"version":1,"key_id":"deadbeef","entries":[]}`)
	if err := os.WriteFile(credPath, credBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := saveAIConfig(t, srv, "https://vendor.invalid/v1", "model-a", "sk-synthetic-t021-file-1010")
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "主密钥") {
		t.Fatalf("凭据文件已存在时保存 = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-synthetic") {
		t.Fatalf("错误回显泄露凭据: %s", rec.Body.String())
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("凭据文件存在时被自动新建主密钥: %v", err)
	}
	raw, err := os.ReadFile(credPath)
	if err != nil || !bytes.Equal(raw, credBytes) {
		t.Fatalf("外部凭据文件被改动: %v", err)
	}
}

// TestAITestLocksWhenMasterKeyRotated 主密钥被换掉后「测试连接」明确锁定：
// 不拿旧库/外部凭据里的 Key 硬发请求，零外发且回显不泄露凭据。
func TestAITestLocksWhenMasterKeyRotated(t *testing.T) {
	vendor := newAITestVendor(t, 200, `{"choices":[{"message":{"content":"正常"}}]}`)
	srv, _ := newAITestFixture(t, vendor.srv.URL)
	// 轮换主密钥（长度合法的另一把）：活动密文对象属于旧主密钥。
	if err := os.WriteFile(aiMasterKeyPath(), bytes.Repeat([]byte{0x7e}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, out := runAITest(t, srv, "", "", "")
	if out["ok"] != false {
		t.Fatalf("主密钥轮换后仍放行: %s", rec.Body.String())
	}
	if got := fmt.Sprint(out["state"]); got != "credential_locked" {
		t.Fatalf("state = %q，期望 credential_locked", got)
	}
	if hits, _, _ := vendor.snapshot(); hits != 0 {
		t.Fatalf("锁定时模拟厂商收到 %d 次请求", hits)
	}
	if strings.Contains(rec.Body.String(), aiTestSavedKey) || strings.Contains(rec.Body.String(), "sk-synthetic") {
		t.Fatalf("锁定回显泄露凭据: %s", rec.Body.String())
	}
}
