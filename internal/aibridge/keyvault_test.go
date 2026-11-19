package aibridge

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// 全部为合成凭据，只用于本地测试；不是任何真实厂商的 Key。
	testKeyPath = "sk-synthetic-t012-0123456789"
)

// TestLoadMasterKeyMissingNeverCreates 主密钥缺失时必须报错且不创建文件/目录：
// 自动新建会让既有密文永久无法解密（等价于覆盖主密钥）。
func TestLoadMasterKeyMissingNeverCreates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys", "ai_master.key")
	if _, err := LoadMasterKey(path); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("缺失主密钥错误 = %v，期望 ErrMasterKeyMissing", err)
	}
	if _, err := LoadOrCreateMasterKey(path); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("兼容入口错误 = %v，期望 ErrMasterKeyMissing", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺失主密钥被自动创建: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺失主密钥时创建了目录: %v", err)
	}
}

// TestInitMasterKeyExplicitAndIdempotent 首次初始化生成 32 字节密钥；重复初始化
// 返回同一把密钥且不改写文件。
func TestInitMasterKeyExplicitAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "ai_master.key")
	key1, err := InitMasterKey(path)
	if err != nil {
		t.Fatalf("初始化主密钥: %v", err)
	}
	if len(key1) != masterKeySize {
		t.Fatalf("主密钥长度 = %d，期望 %d", len(key1), masterKeySize)
	}
	raw1, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取主密钥文件: %v", err)
	}
	key2, err := InitMasterKey(path)
	if err != nil {
		t.Fatalf("再次初始化: %v", err)
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("再次读取主密钥文件: %v", err)
	}
	if !bytes.Equal(key1, key2) || !bytes.Equal(raw1, raw2) {
		t.Fatal("重复初始化改写了已有主密钥")
	}
	loaded, err := LoadMasterKey(path)
	if err != nil || !bytes.Equal(loaded, key1) {
		t.Fatalf("显式初始化后加载 = (%d 字节, %v)", len(loaded), err)
	}
	// 加密/解密仍复用 AES-GCM：同一把主密钥可往返。
	ct, nonce, err := Encrypt(key1, []byte(testKeyPath))
	if err != nil {
		t.Fatalf("加密: %v", err)
	}
	if bytes.Contains(ct, []byte(testKeyPath)) {
		t.Fatal("密文里出现明文 Key")
	}
	plain, err := Decrypt(loaded, ct, nonce)
	if err != nil || string(plain) != testKeyPath {
		t.Fatalf("解密 = (%q, %v)", plain, err)
	}
}

// TestInitMasterKeyNeverOverwritesCorruptFile 损坏的主密钥文件保留原样，
// 初始化返回错误而不是覆盖。
func TestInitMasterKeyNeverOverwritesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai_master.key")
	corrupt := []byte("short")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InitMasterKey(path); !errors.Is(err, ErrMasterKeyInvalid) {
		t.Fatalf("初始化损坏主密钥错误 = %v，期望 ErrMasterKeyInvalid", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, corrupt) {
		t.Fatalf("损坏的主密钥文件被改写为 %d 字节", len(raw))
	}
	if _, err := LoadMasterKey(path); !errors.Is(err, ErrMasterKeyInvalid) {
		t.Fatalf("加载损坏主密钥错误 = %v，期望 ErrMasterKeyInvalid", err)
	}
}

// TestCredentialFileRoundTripHidesPlaintext 新凭据写外部凭据文件：
// 文件里没有明文、可被同一把主密钥解开、遮蔽值保留尾四位。
func TestCredentialFileRoundTripHidesPlaintext(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "keys", "ai_master.key")
	key, err := InitMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	credPath := CredentialsPath(keyPath)
	if credPath != filepath.Join(filepath.Dir(keyPath), credentialFileName) {
		t.Fatalf("凭据文件路径 = %q", credPath)
	}
	if err := SaveCredential(credPath, key, 7, "deepseek", testKeyPath); err != nil {
		t.Fatalf("保存凭据: %v", err)
	}
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(testKeyPath)) {
		t.Fatal("凭据文件里出现明文 Key")
	}
	f, err := LoadCredentialFile(credPath)
	if err != nil {
		t.Fatalf("读取凭据文件: %v", err)
	}
	if got := f.MaskedFor("deepseek"); got != Masked(testKeyPath) {
		t.Fatalf("masked = %q，期望 %q", got, Masked(testKeyPath))
	}
	plain, found, err := f.APIKey("deepseek", key)
	if err != nil || !found || plain != testKeyPath {
		t.Fatalf("解析凭据 = (%q, %v, %v)", plain, found, err)
	}
	// 没有该厂商条目时 found=false，交给调用方回退旧库。
	if _, found, err := f.APIKey("other", key); err != nil || found {
		t.Fatalf("其他厂商 = (%v, %v)，期望 (false, nil)", found, err)
	}
}

// TestCredentialFileKeyMismatchLocks 主密钥换过以后：读不到明文、也写不进新凭据，
// 原文件保留（禁止自动生成覆盖）。
func TestCredentialFileKeyMismatchLocks(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "ai_master.key")
	keyA, err := InitMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	credPath := CredentialsPath(keyPath)
	if err := SaveCredential(credPath, keyA, 7, "deepseek", testKeyPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	keyB := bytes.Repeat([]byte{0x5a}, masterKeySize)
	f, err := LoadCredentialFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.APIKey("deepseek", keyB); !errors.Is(err, ErrCredentialKeyMismatch) {
		t.Fatalf("错误主密钥解析 = %v，期望 ErrCredentialKeyMismatch", err)
	}
	if err := SaveCredential(credPath, keyB, 7, "deepseek", "sk-synthetic-other-1111"); !errors.Is(err, ErrCredentialKeyMismatch) {
		t.Fatalf("错误主密钥写入 = %v，期望 ErrCredentialKeyMismatch", err)
	}
	after, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("主密钥不匹配时凭据文件被改写")
	}
	if err := SaveCredential(credPath, keyB, 7, "deepseek", "sk-synthetic-other-1111"); !errors.Is(err, ErrCredentialKeyMismatch) {
		t.Fatal("第二次错误写入未被锁定")
	}
}

// TestCredentialFileCorruptPreserved 损坏的凭据文件不被覆盖（里面可能还有
// 其他厂商的可用密文）；重试仍是明确错误。
func TestCredentialFileCorruptPreserved(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "ai_master.key")
	key, err := InitMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	credPath := CredentialsPath(keyPath)
	corrupt := []byte(`{"version":1,"key_id":"deadbeefdeadbeef","entries":[`)
	if err := os.WriteFile(credPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := SaveCredential(credPath, key, 7, "deepseek", testKeyPath); !errors.Is(err, ErrCredentialFileInvalid) {
			t.Fatalf("第 %d 次写入损坏文件 = %v，期望 ErrCredentialFileInvalid", i+1, err)
		}
	}
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, corrupt) {
		t.Fatal("损坏的凭据文件被覆盖")
	}
}

// TestPartialWriteFailureKeepsOldCredentials 部分写失败（临时文件已写、原子替换
// 未完成）时，旧凭据文件保持可用、临时文件不残留；修好后重试成功。
func TestPartialWriteFailureKeepsOldCredentials(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "keys", "ai_master.key")
	key, err := InitMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	credPath := CredentialsPath(keyPath)
	if err := SaveCredential(credPath, key, 7, "deepseek", testKeyPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	oldHook := credentialWriteHook
	credentialWriteHook = func(tmpPath string) error {
		// 模拟写到一半：临时文件只落了一小段，随后失败退出。
		if err := os.WriteFile(tmpPath, []byte(`{"version":1`), 0o600); err != nil {
			return err
		}
		return errors.New("模拟部分写失败")
	}
	err = SaveCredential(credPath, key, 7, "deepseek", "sk-synthetic-second-2222")
	credentialWriteHook = oldHook
	if err == nil {
		t.Fatal("部分写失败未返回错误")
	}
	after, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("部分写失败后旧凭据文件被破坏")
	}
	entries, err := os.ReadDir(filepath.Dir(credPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ai_credentials-") {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
	// 故障消失后重试幂等：旧条目被新条目替换，仍只有一条。
	if err := SaveCredential(credPath, key, 7, "deepseek", "sk-synthetic-second-2222"); err != nil {
		t.Fatalf("重试写入: %v", err)
	}
	f, err := LoadCredentialFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Entries) != 1 {
		t.Fatalf("条目数 = %d，期望 1", len(f.Entries))
	}
	plain, _, err := f.APIKey("deepseek", key)
	if err != nil || plain != "sk-synthetic-second-2222" {
		t.Fatalf("重试后凭据 = (%q, %v)", plain, err)
	}
}

// TestBackupRestoreIntoNewDirectory 备份 = 主密钥与凭据文件分别复制；
// 恢复到一个新目录后仍能解开原密文；只恢复密文、不恢复主密钥时锁定且不新建。
func TestBackupRestoreIntoNewDirectory(t *testing.T) {
	src := t.TempDir()
	keyPath := filepath.Join(src, "keys", "ai_master.key")
	key, err := InitMasterKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	credPath := CredentialsPath(keyPath)
	if err := SaveCredential(credPath, key, 7, "deepseek", testKeyPath); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	dstKey := filepath.Join(dst, "restore", "ai_master.key")
	dstCred := filepath.Join(dst, "restore", credentialFileName)
	copyFileForTest(t, keyPath, dstKey)
	copyFileForTest(t, credPath, dstCred)

	restoredKey, err := LoadMasterKey(dstKey)
	if err != nil {
		t.Fatalf("恢复后加载主密钥: %v", err)
	}
	f, err := LoadCredentialFile(dstCred)
	if err != nil {
		t.Fatalf("恢复后加载凭据文件: %v", err)
	}
	plain, found, err := f.APIKey("deepseek", restoredKey)
	if err != nil || !found || plain != testKeyPath {
		t.Fatalf("恢复后解密 = (%q, %v, %v)", plain, found, err)
	}

	// 只恢复密文、不恢复主密钥：不新建主密钥，文件保留，读取明确失败。
	onlyCred := t.TempDir()
	copyFileForTest(t, credPath, filepath.Join(onlyCred, credentialFileName))
	if _, err := LoadMasterKey(filepath.Join(onlyCred, "ai_master.key")); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("缺主密钥恢复 = %v，期望 ErrMasterKeyMissing", err)
	}
	if _, err := os.Stat(filepath.Join(onlyCred, "ai_master.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("恢复流程自动新建了主密钥")
	}
	if _, err := os.Stat(filepath.Join(onlyCred, credentialFileName)); err != nil {
		t.Fatalf("只恢复密文时凭据文件被删除: %v", err)
	}
}

func copyFileForTest(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取备份源 %s: %v", filepath.Base(src), err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("写入恢复目标 %s: %v", filepath.Base(dst), err)
	}
}

func TestMaskedHidesKey(t *testing.T) {
	got := Masked("sk-abcdefgh1234")
	if strings.Contains(got, "abcdefgh") {
		t.Fatalf("masked 泄露中段: %s", got)
	}
	if !strings.HasSuffix(got, "1234") {
		t.Fatalf("masked 应保留尾四位: %s", got)
	}
	if Masked("ab") != "****" {
		t.Fatal("短密钥应完全遮蔽")
	}
}
