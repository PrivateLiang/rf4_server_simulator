package aibridge

// T-021 共享初始化判定：主密钥缺失 + ai_secret 无密文行 + 外部凭据文件不存在，
// 三条同时成立才允许创建；否则保留原文件并明确报错。全部为临时目录 + 合成数据。

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"rf4server/internal/store"
)

func initTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "rf4server.db"))
	if err != nil {
		t.Fatalf("打开测试 SQLite: %v", err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func readKeyFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取主密钥: %v", err)
	}
	return raw
}

func assertNoTempKeyFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("列目录: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ai_master-") {
			t.Fatalf("遗留临时主密钥文件: %s", e.Name())
		}
	}
}

// TestEnsureMasterKeyCreatesOnEmptyInstall 空安装（没有主库 / 空库 / 已迁移空库）
// 都满足三条件，应创建 32 字节主密钥且不留下临时文件。
func TestEnsureMasterKeyCreatesOnEmptyInstall(t *testing.T) {
	ctx := context.Background()
	t.Run("no_database", func(t *testing.T) {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "ai_master.key")
		res, err := EnsureMasterKeyForExplicitInit(ctx, nil, keyPath)
		if err != nil {
			t.Fatalf("空安装初始化失败: %v", err)
		}
		if !res.Created {
			t.Fatal("空安装应标记 Created=true")
		}
		if len(res.Key) != masterKeySize {
			t.Fatalf("返回密钥长度 = %d，期望 %d", len(res.Key), masterKeySize)
		}
		raw := readKeyFile(t, keyPath)
		if len(raw) != masterKeySize || !bytes.Equal(raw, res.Key) {
			t.Fatalf("落盘密钥 = %d 字节，与返回不一致", len(raw))
		}
		if _, err := os.Stat(CredentialsPath(keyPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("初始化不应创建凭据文件: %v", err)
		}
		assertNoTempKeyFiles(t, dir)
	})

	t.Run("empty_database_without_tables", func(t *testing.T) {
		dir := t.TempDir()
		db, err := store.Open(filepath.Join(dir, "empty.db"))
		if err != nil {
			t.Fatalf("打开空库: %v", err)
		}
		defer db.Close()
		keyPath := filepath.Join(dir, "ai_master.key")
		res, err := EnsureMasterKeyForExplicitInit(ctx, db, keyPath)
		if err != nil {
			t.Fatalf("空库初始化失败: %v", err)
		}
		if !res.Created {
			t.Fatal("空库应标记 Created=true")
		}
	})

	t.Run("migrated_empty_database", func(t *testing.T) {
		db := initTestDB(t)
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "ai_master.key")
		res, err := EnsureMasterKeyForExplicitInit(ctx, db, keyPath)
		if err != nil {
			t.Fatalf("已迁移空库初始化失败: %v", err)
		}
		if !res.Created {
			t.Fatal("已迁移空库应标记 Created=true")
		}
	})
}

// TestEnsureMasterKeyExistingIsIdempotent 已有合法主密钥：幂等成功、字节不变，
// 且不再查询数据库（传入 SQL 句柄为 nil 的 DB 也不应 panic）。
func TestEnsureMasterKeyExistingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ai_master.key")
	if _, err := InitMasterKey(keyPath); err != nil {
		t.Fatalf("预置主密钥: %v", err)
	}
	before := readKeyFile(t, keyPath)
	res, err := EnsureMasterKeyForExplicitInit(ctx, &store.DB{}, keyPath)
	if err != nil {
		t.Fatalf("已存在主密钥应成功: %v", err)
	}
	if res.Created {
		t.Fatal("已存在主密钥不应标记 Created")
	}
	if !bytes.Equal(res.Key, before) {
		t.Fatal("返回密钥与文件不一致")
	}
	if after := readKeyFile(t, keyPath); !bytes.Equal(before, after) {
		t.Fatal("幂等初始化改动了主密钥文件")
	}
}

// TestEnsureMasterKeyCorruptNotOverwritten 主密钥损坏/长度异常：报错、保留原文件、
// 不创建凭据文件。
func TestEnsureMasterKeyCorruptNotOverwritten(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ai_master.key")
	corrupt := []byte("short-key")
	if err := os.WriteFile(keyPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureMasterKeyForExplicitInit(ctx, nil, keyPath); !errors.Is(err, ErrMasterKeyInvalid) {
		t.Fatalf("损坏主密钥错误 = %v，期望 ErrMasterKeyInvalid", err)
	}
	if raw := readKeyFile(t, keyPath); !bytes.Equal(raw, corrupt) {
		t.Fatal("损坏的主密钥文件被改写")
	}
	if _, err := os.Stat(CredentialsPath(keyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("主密钥损坏时仍创建了凭据文件")
	}
}

// TestEnsureMasterKeyRefusesLegacyCiphertext 旧库已有密文而主密钥缺失：拒绝创建，
// 提示恢复原主密钥或重新提供凭据，且不落任何新文件。
func TestEnsureMasterKeyRefusesLegacyCiphertext(t *testing.T) {
	ctx := context.Background()
	db := initTestDB(t)
	oldKey := bytes.Repeat([]byte{0x21}, masterKeySize)
	ct, nonce, err := Encrypt(oldKey, []byte("sk-synthetic-t021-legacy"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL.ExecContext(ctx, `
		INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES((SELECT id FROM ai_provider WHERE name='deepseek'),?,?,'default','sk-****1111',0)`,
		ct, nonce); err != nil {
		t.Fatalf("写入旧库密文: %v", err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ai_master.key")
	_, err = EnsureMasterKeyForExplicitInit(ctx, db, keyPath)
	if !errors.Is(err, ErrMasterKeyInitLegacyCiphertext) {
		t.Fatalf("错误 = %v，期望 ErrMasterKeyInitLegacyCiphertext", err)
	}
	// 保留后台既有的「缺主密钥」判定：errors.Is(ErrMasterKeyMissing) 仍成立。
	if !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatal("拒绝错误应同时满足 ErrMasterKeyMissing")
	}
	if !strings.Contains(err.Error(), "需恢复原主密钥或重新提供凭据") {
		t.Fatalf("拒绝原因缺少恢复提示: %v", err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧库有密文时仍创建了主密钥")
	}
	if _, err := os.Stat(CredentialsPath(keyPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧库有密文时仍创建了凭据文件")
	}
}

// TestEnsureMasterKeyRefusesExistingCredentialFile 外部凭据文件已存在而主密钥缺失：
// 拒绝创建并保留原文件（即使凭据文件损坏，也只报「已存在」）。
func TestEnsureMasterKeyRefusesExistingCredentialFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ai_master.key")
	credPath := CredentialsPath(keyPath)
	credBytes := []byte(`{"version":1,"key_id":"deadbeef","entries":[]}`)
	if err := os.WriteFile(credPath, credBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureMasterKeyForExplicitInit(ctx, nil, keyPath)
	if !errors.Is(err, ErrMasterKeyInitCredentialFile) {
		t.Fatalf("错误 = %v，期望 ErrMasterKeyInitCredentialFile", err)
	}
	if !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatal("拒绝错误应同时满足 ErrMasterKeyMissing")
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("凭据文件存在时仍创建了主密钥")
	}
	raw, err := os.ReadFile(credPath)
	if err != nil || !bytes.Equal(raw, credBytes) {
		t.Fatalf("外部凭据文件被改写: %v", err)
	}
}

// TestEnsureMasterKeyConcurrentCreatesOnlyOneKey 并发两次初始化：一次创建、一次
// 读取既有文件，最终只有一把 32 字节密钥，不会出现两把或半写文件。
func TestEnsureMasterKeyConcurrentCreatesOnlyOneKey(t *testing.T) {
	for round := 0; round < 10; round++ {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "ai_master.key")
		start := make(chan struct{})
		results := make([]MasterKeyInitResult, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], errs[i] = EnsureMasterKeyForExplicitInit(context.Background(), nil, keyPath)
			}(i)
		}
		close(start)
		wg.Wait()

		created := 0
		for i := range results {
			if errs[i] != nil {
				t.Fatalf("第 %d 轮并发初始化 %d 失败: %v", round, i, errs[i])
			}
			if results[i].Created {
				created++
			}
			if len(results[i].Key) != masterKeySize {
				t.Fatalf("第 %d 轮返回密钥长度 = %d", round, len(results[i].Key))
			}
			if !bytes.Equal(results[i].Key, results[0].Key) {
				t.Fatalf("第 %d 轮并发得到两把不同密钥", round)
			}
		}
		if created != 1 {
			t.Fatalf("第 %d 轮 Created 次数 = %d，期望恰好 1", round, created)
		}
		raw := readKeyFile(t, keyPath)
		if len(raw) != masterKeySize {
			t.Fatalf("第 %d 轮落盘密钥长度 = %d，期望 %d", round, len(raw), masterKeySize)
		}
		assertNoTempKeyFiles(t, dir)
	}
}

// TestEnsureMasterKeyMissingPath 未配置路径：报缺路径，不创建文件。
func TestEnsureMasterKeyMissingPath(t *testing.T) {
	if _, err := EnsureMasterKeyForExplicitInit(context.Background(), nil, "  "); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("空路径错误 = %v，期望 ErrMasterKeyMissing", err)
	}
}
