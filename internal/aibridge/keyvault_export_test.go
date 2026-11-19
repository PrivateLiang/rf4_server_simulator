package aibridge

// 旧库只读导出与运行期凭据解析的独立真值：本地 SQLite + 合成密钥，
// 不连接任何真实厂商、不使用真实凭据。

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// newCredentialTestDB 建一个跑完迁移的本地库，并固定唯一启用厂商/模型。
func newCredentialTestDB(t *testing.T) (*store.DB, []byte, int64) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "ai-secret.db"))
	if err != nil {
		t.Fatalf("打开测试 SQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	// 迁移种子里模型 ID 为空（等后台填写）；本票测试统一写成确定值。
	if _, err := db.SQL.ExecContext(ctx,
		`UPDATE ai_model SET model_id='model-a' WHERE provider_id=(SELECT id FROM ai_provider WHERE name='deepseek')`); err != nil {
		t.Fatal(err)
	}
	var providerID int64
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT id FROM ai_provider WHERE name='deepseek'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	return db, []byte("0123456789abcdef0123456789abcdef"), providerID
}

// insertLegacySecret 模拟旧库形态：密文由测试指定的主密钥加密后写进 ai_secret。
func insertLegacySecret(t *testing.T, db *store.DB, providerID int64, encKey []byte, plain, masked string) ([]byte, []byte) {
	t.Helper()
	ct, nonce, err := Encrypt(encKey, []byte(plain))
	if err != nil {
		t.Fatalf("加密合成旧密文: %v", err)
	}
	if _, err := db.SQL.ExecContext(context.Background(), `
		INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES(?,?,?,'default',?,0)
		ON CONFLICT(provider_id) DO UPDATE SET
		  ciphertext=excluded.ciphertext, nonce=excluded.nonce,
		  masked=excluded.masked`, providerID, ct, nonce, masked); err != nil {
		t.Fatalf("写入合成旧密文: %v", err)
	}
	return ct, nonce
}

// TestExportLegacyCredentialsReadOnlyVerifiedAndIdempotent 旧库密文只读导出：
// 原样复制密文、验证可解密与厂商关联；旧库一行不动；重试不重复、不改文件。
func TestExportLegacyCredentialsReadOnlyVerifiedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	db, key, providerID := newCredentialTestDB(t)
	const legacy = "sk-synthetic-legacy-0123456789"
	ct, nonce := insertLegacySecret(t, db, providerID, key, legacy, "sk-****6789")
	credPath := filepath.Join(t.TempDir(), "keys", credentialFileName)

	report, err := ExportLegacyCredentials(ctx, db, credPath, key)
	if err != nil {
		t.Fatalf("导出: %v", err)
	}
	if report.Migrated != 1 || len(report.Skipped) != 0 {
		t.Fatalf("导出报告 = %+v，期望 1 条成功 0 条跳过", report)
	}
	f, err := LoadCredentialFile(credPath)
	if err != nil {
		t.Fatalf("读取导出文件: %v", err)
	}
	if len(f.Entries) != 1 {
		t.Fatalf("条目数 = %d，期望 1", len(f.Entries))
	}
	entry := f.Entries[0]
	if entry.Provider != "deepseek" || entry.ProviderID != providerID || entry.Masked != "sk-****6789" {
		t.Fatalf("条目关联信息 = %+v", entry)
	}
	// 原样导出：外部文件里的密文与旧库逐字节一致（不重新加密）。
	if entry.Ciphertext != base64.StdEncoding.EncodeToString(ct) ||
		entry.Nonce != base64.StdEncoding.EncodeToString(nonce) {
		t.Fatal("导出改写了旧密文，应为只读原样复制")
	}
	plain, found, err := f.APIKey("deepseek", key)
	if err != nil || !found || plain != legacy {
		t.Fatalf("导出后解密 = (%q, %v, %v)", plain, found, err)
	}
	// 旧库保留：行数、密文、nonce、masked 全部不变。
	rows, err := db.AIListSecrets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("导出后旧库行数 = %d，期望 1（不自动清理）", len(rows))
	}
	if !bytes.Equal(rows[0].Ciphertext, ct) || !bytes.Equal(rows[0].Nonce, nonce) || rows[0].Masked != "sk-****6789" {
		t.Fatal("导出改动了旧库密文行")
	}
	// 重试幂等：不再新增条目、文件字节不变。
	raw1, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	report2, err := ExportLegacyCredentials(ctx, db, credPath, key)
	if err != nil {
		t.Fatalf("重试导出: %v", err)
	}
	if report2.Migrated != 0 || len(report2.Skipped) != 0 {
		t.Fatalf("重试导出报告 = %+v，期望 0 条", report2)
	}
	raw2, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Fatal("重试导出改写了凭据文件")
	}
}

// TestExportLegacySecretsSkipsUndecryptable 没有原主密钥时旧密文导不出去：
// 不写空文件、不伪造恢复成功、旧库保留；随后重新提供凭据仍可写入。
func TestExportLegacySecretsSkipsUndecryptable(t *testing.T) {
	ctx := context.Background()
	db, key, providerID := newCredentialTestDB(t)
	otherKey := bytes.Repeat([]byte{0x33}, masterKeySize)
	const legacy = "sk-synthetic-lost-key-3333"
	insertLegacySecret(t, db, providerID, otherKey, legacy, "sk-****3333")
	credPath := filepath.Join(t.TempDir(), "keys", credentialFileName)

	report, err := ExportLegacyCredentials(ctx, db, credPath, key)
	if err != nil {
		t.Fatalf("导出: %v", err)
	}
	if report.Migrated != 0 || len(report.Skipped) != 1 {
		t.Fatalf("导出报告 = %+v，期望 0 成功 1 跳过", report)
	}
	if !strings.Contains(report.Skipped[0], "需重新提供凭据") {
		t.Fatalf("跳过原因 = %q", report.Skipped[0])
	}
	if _, err := os.Stat(credPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("没有可验证的旧密文时仍创建了凭据文件: %v", err)
	}
	rows, err := db.AIListSecrets(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("旧库行 = %d, %v", len(rows), err)
	}
	// 重新提供凭据：写新密钥成功，旧库行仍保留（不删数据）。
	const fresh = "sk-synthetic-reprovided-4444"
	if err := SaveCredential(credPath, key, providerID, "deepseek", fresh); err != nil {
		t.Fatalf("重新提供凭据: %v", err)
	}
	f, err := LoadCredentialFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if plain, _, err := f.APIKey("deepseek", key); err != nil || plain != fresh {
		t.Fatalf("重新提供后凭据 = (%q, %v)", plain, err)
	}
	if rows, err := db.AIListSecrets(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("重新提供后旧库行 = %d, %v", len(rows), err)
	}
}

// TestResolveProviderAPIKeyPrecedence 凭据文件优先、旧库只读兼容、
// 两处都没有时返回空（明确「未配置」，不伪造成功）。
func TestResolveProviderAPIKeyPrecedence(t *testing.T) {
	ctx := context.Background()
	db, key, providerID := newCredentialTestDB(t)
	const legacy = "sk-synthetic-db-only-5555"
	insertLegacySecret(t, db, providerID, key, legacy, "sk-****5555")
	credPath := filepath.Join(t.TempDir(), "keys", credentialFileName)

	plain, source, err := ResolveProviderAPIKey(ctx, db, credPath, key, "deepseek")
	if err != nil || plain != legacy || source != SourceLegacyDB {
		t.Fatalf("旧库解析 = (%q, %q, %v)", plain, source, err)
	}
	const fromFile = "sk-synthetic-file-only-6666"
	if err := SaveCredential(credPath, key, providerID, "deepseek", fromFile); err != nil {
		t.Fatal(err)
	}
	plain, source, err = ResolveProviderAPIKey(ctx, db, credPath, key, "deepseek")
	if err != nil || plain != fromFile || source != SourceCredentialFile {
		t.Fatalf("凭据文件解析 = (%q, %q, %v)", plain, source, err)
	}
}

// TestResolveProviderAPIKeyMissingCiphertext 缺密文：返回空 + 无错误，
// 由上层明确提示「未配置」，不能拿空 Key 去冒充可用凭据。
func TestResolveProviderAPIKeyMissingCiphertext(t *testing.T) {
	db, key, _ := newCredentialTestDB(t)
	plain, source, err := ResolveProviderAPIKey(context.Background(), db,
		filepath.Join(t.TempDir(), "keys", credentialFileName), key, "deepseek")
	if err != nil || plain != "" || source != "" {
		t.Fatalf("缺密文解析 = (%q, %q, %v)，期望空且无错误", plain, source, err)
	}
}

// TestCredentialErrorsDoNotLeakKeyMaterial 错误与跳过原因只带厂商名/原因，
// 不带明文 Key 或密文。
func TestCredentialErrorsDoNotLeakKeyMaterial(t *testing.T) {
	ctx := context.Background()
	db, key, providerID := newCredentialTestDB(t)
	otherKey := bytes.Repeat([]byte{0x44}, masterKeySize)
	const secret = "sk-synthetic-never-leak-7777"
	ct, _ := insertLegacySecret(t, db, providerID, otherKey, secret, "sk-****7777")
	credPath := filepath.Join(t.TempDir(), "keys", credentialFileName)

	report, err := ExportLegacyCredentials(ctx, db, credPath, key)
	if err != nil {
		t.Fatalf("导出: %v", err)
	}
	if len(report.Skipped) != 1 {
		t.Fatalf("跳过数 = %d，期望 1", len(report.Skipped))
	}
	leaks := []string{secret, base64.StdEncoding.EncodeToString(ct), "sk-synthetic"}
	for _, reason := range report.Skipped {
		for _, leak := range leaks {
			if strings.Contains(reason, leak) {
				t.Fatalf("跳过原因泄露凭据内容 %q: %s", leak, reason)
			}
		}
	}
	// 换一把主密钥读凭据文件：错误只说明不匹配。
	if err := SaveCredential(credPath, key, providerID, "deepseek", secret); err != nil {
		t.Fatal(err)
	}
	f, err := LoadCredentialFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	_, _, mismatchErr := f.APIKey("deepseek", otherKey)
	if !errors.Is(mismatchErr, ErrCredentialKeyMismatch) {
		t.Fatalf("错误主密钥 = %v", mismatchErr)
	}
	if strings.Contains(mismatchErr.Error(), secret) || strings.Contains(mismatchErr.Error(), "sk-synthetic") {
		t.Fatalf("错误信息泄露凭据内容: %v", mismatchErr)
	}
}

// TestRunnerLoadProviderPrefersExternalCredential 运行期凭据文件优先于旧库：
// 旧库留着旧 Key，Runner 取到的是外部凭据文件里的新 Key。
func TestRunnerLoadProviderPrefersExternalCredential(t *testing.T) {
	db, key, providerID := newCredentialTestDB(t)
	insertLegacySecret(t, db, providerID, key, "sk-synthetic-db-old-8888", "sk-****8888")
	keyPath := filepath.Join(t.TempDir(), "keys", "ai_master.key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	const fromFile = "sk-synthetic-file-new-9999"
	if err := SaveCredential(CredentialsPath(keyPath), key, providerID, "deepseek", fromFile); err != nil {
		t.Fatal(err)
	}
	r := NewRunner(db, nil, config.AI{Enabled: true, RequestTimeoutSec: 5, MasterKeyFile: keyPath}, key, logging.Discard())
	p, err := r.loadProvider(context.Background())
	if err != nil {
		t.Fatalf("加载 Provider: %v", err)
	}
	oc, ok := p.(*OpenAICompatProvider)
	if !ok {
		t.Fatalf("Provider 类型 = %T", p)
	}
	if oc.spec.APIKey != fromFile {
		t.Fatal("运行期没有优先使用外部凭据文件里的新 Key")
	}
}

// TestRunnerLoadProviderLegacyFallbackAndLock 没有凭据文件时回退旧库；
// 凭据文件与主密钥不匹配时锁定（即使旧库有密文也不静默回退）。
func TestRunnerLoadProviderLegacyFallbackAndLock(t *testing.T) {
	db, key, providerID := newCredentialTestDB(t)
	const legacy = "sk-synthetic-db-fallback-1010"
	insertLegacySecret(t, db, providerID, key, legacy, "sk-****1010")

	// 无凭据文件（MasterKeyFile 为空 → 不做文件查找）→ 旧库兼容。
	r := NewRunner(db, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, key, logging.Discard())
	p, err := r.loadProvider(context.Background())
	if err != nil {
		t.Fatalf("旧库兼容加载: %v", err)
	}
	if oc := p.(*OpenAICompatProvider); oc.spec.APIKey != legacy {
		t.Fatal("没有凭据文件时没有回退旧库")
	}

	// 凭据文件属于另一把主密钥 → 锁定。
	keyPath := filepath.Join(t.TempDir(), "keys", "ai_master.key")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	otherKey := bytes.Repeat([]byte{0x66}, masterKeySize)
	if err := SaveCredential(CredentialsPath(keyPath), otherKey, providerID, "deepseek", "sk-synthetic-other-2020"); err != nil {
		t.Fatal(err)
	}
	r = NewRunner(db, nil, config.AI{Enabled: true, RequestTimeoutSec: 5, MasterKeyFile: keyPath}, key, logging.Discard())
	if _, err := r.loadProvider(context.Background()); !errors.Is(err, ErrCredentialKeyMismatch) {
		t.Fatalf("主密钥不匹配时加载 = %v，期望 ErrCredentialKeyMismatch", err)
	}
}
