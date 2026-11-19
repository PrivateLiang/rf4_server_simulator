package aibridge

// API Key 的加密存储：
//   - 主密钥：32 字节随机密钥的本地文件（AES-256-GCM），按「配置文件所在目录」
//     解析成稳定路径；进程只读加载，缺失不再自动生成，显式初始化只发生在
//     确认没有任何既有密文的时候。
//   - 密文：外部受保护凭据文件（与主密钥分文件、分别备份）；主密钥缺失/不匹配
//     时锁定并保留文件，禁止自动生成覆盖。
//   - 旧库 ai_secret 只作只读兼容与一次性导出，新凭据不再写入共享数据库。
// 明文 Key 不落库、不进日志、不进配置；错误信息只带厂商名/路径，不带密钥内容。

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rf4server/internal/store"
)

const (
	masterKeySize = 32

	// credentialFileName 是外部凭据文件名。它固定放在主密钥同目录：
	// 主密钥路径本身已按配置文件目录解析，所以凭据文件同样与启动目录无关。
	credentialFileName = "ai_credentials.json"
	credentialVersion  = 1

	// SourceCredentialFile / SourceLegacyDB 标记运行期解析到的 Key 来源。
	SourceCredentialFile = "credential_file"
	SourceLegacyDB       = "legacy_db"
)

var (
	// ErrMasterKeyMissing 主密钥文件不存在。缺它时绝不自动新建：
	// 自动新建会让既有密文永久无法解密（等价于覆盖主密钥）。
	ErrMasterKeyMissing = errors.New("ai: 主密钥文件不存在")
	// ErrMasterKeyInvalid 主密钥文件存在但长度/内容不可用；保留原文件，不覆盖。
	ErrMasterKeyInvalid = errors.New("ai: 主密钥文件损坏")
	// ErrCredentialFileMissing 外部凭据文件不存在（旧库/未迁移形态）。
	ErrCredentialFileMissing = errors.New("ai: 外部凭据文件不存在")
	// ErrCredentialFileInvalid 外部凭据文件损坏；保留原文件，不覆盖。
	ErrCredentialFileInvalid = errors.New("ai: 外部凭据文件损坏")
	// ErrCredentialKeyMismatch 凭据文件的指纹与当前主密钥不一致：锁定，不覆盖。
	ErrCredentialKeyMismatch = errors.New("ai: 外部凭据文件与当前主密钥不匹配")
)

// LoadMasterKey 只读加载主密钥；缺失返回 ErrMasterKeyMissing，长度异常返回
// ErrMasterKeyInvalid。两种错误都不创建/改写文件。
func LoadMasterKey(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: 未配置路径", ErrMasterKeyMissing)
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrMasterKeyMissing, path)
	}
	if err != nil {
		return nil, fmt.Errorf("ai: 读取主密钥失败: %w", err)
	}
	if len(b) != masterKeySize {
		return nil, fmt.Errorf("%w: 长度 %d，期望 %d", ErrMasterKeyInvalid, len(b), masterKeySize)
	}
	return b, nil
}

// InitMasterKey 首次初始化主密钥（0600，原子独占创建）。已存在时幂等返回现有密钥、
// 绝不覆盖；文件损坏时返回错误并保留原文件。只应在「确认没有任何既有密文」的
// 显式初始化路径调用。
func InitMasterKey(path string) ([]byte, error) {
	key, _, err := initMasterKeyOnce(path)
	return key, err
}

// initMasterKeyOnce 与 InitMasterKey 同义，但额外报告本次是否真的新建：
// 并发初始化时另一个调用可能已经建好文件，此时 created=false。
func initMasterKeyOnce(path string) (key []byte, created bool, err error) {
	if strings.TrimSpace(path) == "" {
		return nil, false, errors.New("ai: 主密钥路径未配置")
	}
	if existing, lerr := LoadMasterKey(path); lerr == nil {
		return existing, false, nil
	} else if !errors.Is(lerr, ErrMasterKeyMissing) {
		return nil, false, lerr
	}
	dir := filepath.Dir(path)
	if err := prepareMasterKeyDirectory(dir); err != nil {
		return nil, false, err
	}
	key = make([]byte, masterKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, false, err
	}
	// 先在同目录写完整临时文件，再用硬链接「仅当目标不存在才出现」地原子创建：
	// 并发初始化时另一个进程只会看到完整的 32 字节文件。若直接 O_EXCL 建空文件再写，
	// 对方会在写入完成前读到 0/半行而误判为损坏。
	tmp, err := os.CreateTemp(dir, ".ai_master-*.tmp")
	if err != nil {
		return nil, false, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(key); err != nil {
		_ = tmp.Close()
		return nil, false, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, false, err
	}
	if err := tmp.Close(); err != nil {
		return nil, false, err
	}
	if err := os.Link(tmpName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, lerr := LoadMasterKey(path)
			return existing, false, lerr
		}
		// 少数文件系统不支持硬链接：退回 O_EXCL 语义（仍只有一个写成功）。
		return writeMasterKeyExclusive(path, key)
	}
	return key, true, nil
}

// writeMasterKeyExclusive 是硬链接不可用时的兜底：O_EXCL 独占创建后写入。
func writeMasterKeyExclusive(path string, key []byte) ([]byte, bool, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, lerr := LoadMasterKey(path)
			return existing, false, lerr
		}
		return nil, false, err
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, false, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, false, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, false, err
	}
	return key, true, nil
}

// LoadOrCreateMasterKey 保留给既有调用点（cmd/aibridge、cmd/admin）。
//
// Deprecated: 语义已改为「只读加载」——主密钥缺失即返回 ErrMasterKeyMissing，
// 不再自动生成（自动生成会孤立已有密文）。显式初始化用 InitMasterKey。
func LoadOrCreateMasterKey(path string) ([]byte, error) {
	return LoadMasterKey(path)
}

// CredentialsPath 返回与主密钥同目录的外部凭据文件路径。
// masterKeyFile 已在 config.Load 里按配置文件目录解析成稳定路径；空路径返回空，
// 表示调用方没有配置主密钥路径（测试或未启用 AI），不做文件查找。
func CredentialsPath(masterKeyFile string) string {
	if strings.TrimSpace(masterKeyFile) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(masterKeyFile), credentialFileName)
}

// MasterKeyID 是主密钥指纹（SHA-256 前 8 字节十六进制），写进凭据文件用来判定
// 「这份密文属于哪把主密钥」。由 32 字节随机密钥派生，不能反推主密钥。
func MasterKeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// CredentialEntry 是外部凭据文件里的一条厂商密钥密文。
// Ciphertext/Nonce 是 AES-GCM 密文的 base64；Masked 只含尾四位，供后台展示。
type CredentialEntry struct {
	ProviderID int64  `json:"provider_id,omitempty"`
	Provider   string `json:"provider"`
	Masked     string `json:"masked"`
	Ciphertext string `json:"ciphertext"`
	Nonce      string `json:"nonce"`
	UpdatedTS  int64  `json:"updated_ts"`
}

// CredentialFile 是外部凭据文件。KeyID 不匹配即锁定，绝不覆盖。
type CredentialFile struct {
	Version int               `json:"version"`
	KeyID   string            `json:"key_id"`
	Entries []CredentialEntry `json:"entries"`
}

// LoadCredentialFile 读取并做结构校验（不解密）。文件缺失返回
// ErrCredentialFileMissing；格式/字段非法返回 ErrCredentialFileInvalid。
func LoadCredentialFile(path string) (*CredentialFile, error) {
	if strings.TrimSpace(path) == "" {
		return nil, ErrCredentialFileMissing
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrCredentialFileMissing, path)
	}
	if err != nil {
		return nil, fmt.Errorf("ai: 读取凭据文件失败: %w", err)
	}
	var f CredentialFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w: 不是合法 JSON: %v", ErrCredentialFileInvalid, err)
	}
	if f.Version != credentialVersion {
		return nil, fmt.Errorf("%w: 版本 %d 不受支持", ErrCredentialFileInvalid, f.Version)
	}
	if strings.TrimSpace(f.KeyID) == "" {
		return nil, fmt.Errorf("%w: 缺少 key_id", ErrCredentialFileInvalid)
	}
	for i, e := range f.Entries {
		if strings.TrimSpace(e.Provider) == "" {
			return nil, fmt.Errorf("%w: 第 %d 条缺少 provider", ErrCredentialFileInvalid, i+1)
		}
		if _, err := base64.StdEncoding.DecodeString(e.Ciphertext); err != nil {
			return nil, fmt.Errorf("%w: 第 %d 条 ciphertext 不是 base64", ErrCredentialFileInvalid, i+1)
		}
		nonce, err := base64.StdEncoding.DecodeString(e.Nonce)
		if err != nil || len(nonce) == 0 {
			return nil, fmt.Errorf("%w: 第 %d 条 nonce 不合法", ErrCredentialFileInvalid, i+1)
		}
	}
	return &f, nil
}

// MaskedFor 返回指定厂商的遮蔽值（不含明文）；没有条目时返回空。
func (f *CredentialFile) MaskedFor(provider string) string {
	if f == nil {
		return ""
	}
	for _, e := range f.Entries {
		if e.Provider == provider {
			return e.Masked
		}
	}
	return ""
}

// APIKey 解密指定厂商的 Key。found=false 表示文件里没有该厂商条目
// （调用方可回退旧库）。主密钥指纹不匹配或条目解密失败一律报错，不返回空值冒充。
func (f *CredentialFile) APIKey(provider string, masterKey []byte) (string, bool, error) {
	if err := checkMasterKey(masterKey); err != nil {
		return "", false, err
	}
	if f == nil {
		return "", false, nil
	}
	if f.KeyID != MasterKeyID(masterKey) {
		return "", false, ErrCredentialKeyMismatch
	}
	for _, e := range f.Entries {
		if e.Provider != provider {
			continue
		}
		plain, err := e.decrypt(masterKey)
		if err != nil {
			return "", true, fmt.Errorf("ai: 凭据条目解密失败（主密钥不匹配或文件损坏）: %w", err)
		}
		return plain, true, nil
	}
	return "", false, nil
}

func (e CredentialEntry) decrypt(masterKey []byte) (string, error) {
	ct, err := base64.StdEncoding.DecodeString(e.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("%w: ciphertext 不是 base64", ErrCredentialFileInvalid)
	}
	nonce, err := base64.StdEncoding.DecodeString(e.Nonce)
	if err != nil {
		return "", fmt.Errorf("%w: nonce 不是 base64", ErrCredentialFileInvalid)
	}
	plain, err := Decrypt(masterKey, ct, nonce)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(plain)), nil
}

// SaveCredential 把新凭据加密后写进外部凭据文件（其他厂商条目保留）。
// 文件不存在时新建；文件损坏或主密钥指纹不匹配时拒绝写入并保留原文件。
func SaveCredential(path string, masterKey []byte, providerID int64, providerName, plaintext string) error {
	plaintext = strings.TrimSpace(plaintext)
	providerName = strings.TrimSpace(providerName)
	if plaintext == "" {
		return errors.New("ai: API Key 为空")
	}
	if providerName == "" {
		return errors.New("ai: 厂商名为空")
	}
	if err := checkMasterKey(masterKey); err != nil {
		return err
	}
	keyID := MasterKeyID(masterKey)
	f, err := LoadCredentialFile(path)
	if errors.Is(err, ErrCredentialFileMissing) {
		f = &CredentialFile{Version: credentialVersion, KeyID: keyID}
	} else if err != nil {
		// 损坏/无法识别的文件不覆盖：里面可能是其他厂商的可用密文。
		return err
	} else if f.KeyID != keyID {
		return ErrCredentialKeyMismatch
	}
	ct, nonce, err := Encrypt(masterKey, []byte(plaintext))
	if err != nil {
		return err
	}
	entry := CredentialEntry{
		ProviderID: providerID,
		Provider:   providerName,
		Masked:     Masked(plaintext),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		UpdatedTS:  time.Now().Unix(),
	}
	replaced := false
	for i := range f.Entries {
		if f.Entries[i].Provider == providerName {
			f.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		f.Entries = append(f.Entries, entry)
	}
	f.Version = credentialVersion
	f.KeyID = keyID
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].Provider < f.Entries[j].Provider })
	// 原子切换前先验证整份文件都能用当前主密钥解开，避免写入不可恢复的内容。
	if err := verifyCredentialFile(f, masterKey); err != nil {
		return err
	}
	return writeCredentialFileAtomic(path, f)
}

// ExportReport 是一次旧库密文导出的结果。Skipped 只记厂商与原因，不记密文。
type ExportReport struct {
	Migrated int
	Skipped  []string
}

// ExportLegacyCredentials 把旧库 ai_secret 密文只读导出到外部凭据文件：
//   - 逐条用当前主密钥验证「可解密」且「厂商关联非空」才写入；旧密文原样保留，
//     不重新加密、不删库、不改库；
//   - 先写临时文件，整份校验通过后原子替换；已存在的厂商条目跳过（重试幂等）；
//   - 主密钥指纹与既有凭据文件不匹配时锁定，不覆盖；
//   - 一条都验证不过时不写空文件，由调用方要求重新提供凭据。
func ExportLegacyCredentials(ctx context.Context, db *store.DB, path string, masterKey []byte) (ExportReport, error) {
	var report ExportReport
	if err := checkMasterKey(masterKey); err != nil {
		return report, err
	}
	keyID := MasterKeyID(masterKey)
	f, err := LoadCredentialFile(path)
	if errors.Is(err, ErrCredentialFileMissing) {
		f = &CredentialFile{Version: credentialVersion, KeyID: keyID}
	} else if err != nil {
		return report, err
	} else if f.KeyID != keyID {
		return report, ErrCredentialKeyMismatch
	}
	rows, err := db.AIListSecrets(ctx)
	if err != nil {
		return report, err
	}
	have := make(map[string]bool, len(f.Entries))
	for _, e := range f.Entries {
		have[e.Provider] = true
	}
	for _, row := range rows {
		name := strings.TrimSpace(row.Provider)
		switch {
		case name == "":
			report.Skipped = append(report.Skipped, fmt.Sprintf("provider=%d（厂商名为空）", row.ProviderID))
			continue
		case have[name]:
			continue // 已导出过：重试不会重复写。
		case len(row.Ciphertext) == 0 || len(row.Nonce) == 0:
			report.Skipped = append(report.Skipped, name+"（旧密文为空）")
			continue
		}
		plain, err := Decrypt(masterKey, row.Ciphertext, row.Nonce)
		if err != nil {
			// 旧密文保留在库里；没有原主密钥就只能要求重新提供凭据。
			report.Skipped = append(report.Skipped, name+"（当前主密钥无法解密，需重新提供凭据）")
			continue
		}
		masked := strings.TrimSpace(row.Masked)
		if masked == "" {
			masked = Masked(string(plain))
		}
		f.Entries = append(f.Entries, CredentialEntry{
			ProviderID: row.ProviderID,
			Provider:   name,
			Masked:     masked,
			Ciphertext: base64.StdEncoding.EncodeToString(row.Ciphertext),
			Nonce:      base64.StdEncoding.EncodeToString(row.Nonce),
			UpdatedTS:  int64(row.UpdatedTS),
		})
		have[name] = true
		report.Migrated++
	}
	if report.Migrated == 0 {
		return report, nil
	}
	f.Version = credentialVersion
	f.KeyID = keyID
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].Provider < f.Entries[j].Provider })
	if err := verifyCredentialFile(f, masterKey); err != nil {
		return ExportReport{}, err
	}
	if err := writeCredentialFileAtomic(path, f); err != nil {
		return ExportReport{}, err
	}
	return report, nil
}

// ResolveProviderAPIKey 解析当前启用厂商的明文 Key：外部凭据文件（新凭据）优先，
// 其次旧库 ai_secret（只读兼容）。source 为空表示两处都没有配置。
// 返回的错误不含密钥内容，供上层降级或明确锁定。
func ResolveProviderAPIKey(ctx context.Context, db *store.DB, path string, masterKey []byte, providerName string) (string, string, error) {
	if path != "" {
		f, err := LoadCredentialFile(path)
		switch {
		case err == nil:
			plain, found, derr := f.APIKey(providerName, masterKey)
			if derr != nil {
				return "", "", derr
			}
			if found {
				return plain, SourceCredentialFile, nil
			}
		case errors.Is(err, ErrCredentialFileMissing):
			// 未迁移形态：继续看旧库。
		default:
			return "", "", err
		}
	}
	if strings.TrimSpace(providerName) == "" {
		return "", "", nil
	}
	ct, nonce, found, err := db.AISecretByName(ctx, providerName)
	if err != nil {
		return "", "", err
	}
	if !found || len(ct) == 0 || len(nonce) == 0 {
		return "", "", nil
	}
	plain, err := Decrypt(masterKey, ct, nonce)
	if err != nil {
		return "", "", fmt.Errorf("ai: 旧库密文无法用当前主密钥解密（主密钥缺失或不匹配，需重新提供凭据）: %w", err)
	}
	return strings.TrimSpace(string(plain)), SourceLegacyDB, nil
}

func checkMasterKey(masterKey []byte) error {
	if len(masterKey) != masterKeySize {
		return ErrMasterKeyInvalid
	}
	return nil
}

// verifyCredentialFile 解密整份文件，确认每条条目都能被当前主密钥解开。
func verifyCredentialFile(f *CredentialFile, masterKey []byte) error {
	if f.KeyID != MasterKeyID(masterKey) {
		return ErrCredentialKeyMismatch
	}
	for _, e := range f.Entries {
		if _, err := e.decrypt(masterKey); err != nil {
			return fmt.Errorf("ai: 凭据文件校验失败（provider=%s）: %w", e.Provider, err)
		}
	}
	return nil
}

// credentialWriteHook 仅测试用：在临时文件写完、原子替换之前注入失败，
// 验证部分写失败不会破坏已有凭据文件。生产路径恒为 nil。
var credentialWriteHook func(tmpPath string) error

// writeCredentialFileAtomic 写同目录临时文件 → fsync → （可选故障注入）→ rename。
// rename 失败或校验失败时删除临时文件，目标文件保持原样。
func writeCredentialFileAtomic(path string, f *CredentialFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".ai_credentials-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if credentialWriteHook != nil {
		if err := credentialWriteHook(tmpName); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func gcmFor(key []byte) (cipher.AEAD, error) {
	if len(key) != masterKeySize {
		return nil, errors.New("ai: 主密钥长度错误")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypt 返回密文与 nonce。
func Encrypt(key, plaintext []byte) ([]byte, []byte, error) {
	g, err := gcmFor(key)
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return g.Seal(nil, nonce, plaintext, nil), nonce, nil
}

// Decrypt 解密后台保存的 Key。
func Decrypt(key, ciphertext, nonce []byte) ([]byte, error) {
	g, err := gcmFor(key)
	if err != nil {
		return nil, err
	}
	if len(nonce) != g.NonceSize() {
		return nil, errors.New("ai: nonce 长度错误")
	}
	return g.Open(nil, nonce, ciphertext, nil)
}

// Masked 只保留尾四位用于后台展示。
func Masked(plain string) string {
	plain = strings.TrimSpace(plain)
	if len(plain) <= 4 {
		return "****"
	}
	return "sk-****" + plain[len(plain)-4:]
}

// ---- 不可变密文对象（DEV-114 A） ----
//
// 发布合同时，新 Key 不再覆盖同名文件，而是在主密钥同目录下的独立受保护
// 版本目录里按「随机、无秘密的引用」排他创建一个不可变对象：
//
//   - 文件名 = <ref>.json，ref 是 16 字节随机数的十六进制（不含任何秘密）；
//   - 对象内容绑定厂商 ID 与主密钥指纹；创建后不再改写（排他安装，无同名覆盖）；
//   - 先写同目录临时文件并 fsync，再用硬链接「仅当目标不存在才出现」地安装，
//     读回验证可解密后才进入发布；失败时目标不存在、旧对象逐字节不变；
//   - 本票不回收未引用对象：读者仍持有的引用不受影响。
const (
	credentialDirName     = "ai_credentials.d"
	credentialObjectVer   = 1
	credentialRefBytes    = 16
	credentialObjectPerm  = 0o600
	credentialTempPattern = ".ai_credential-*.tmp"
)

var (
	// ErrCredentialObjectMissing 活动引用的密文对象不存在：锁定，不回退同名文件。
	ErrCredentialObjectMissing = errors.New("ai: 凭据对象不存在")
	// ErrCredentialObjectInvalid 对象损坏/版本不符：锁定，不覆盖。
	ErrCredentialObjectInvalid = errors.New("ai: 凭据对象损坏")
	// ErrCredentialObjectMismatch 对象绑定的厂商 ID 与活动配置不符：锁定。
	ErrCredentialObjectMismatch = errors.New("ai: 凭据对象与厂商配置不匹配")
	// ErrCredentialInstallNotDurable 无法确认对象安装已持久（目录打开/同步失败，
	// 或平台不支持目录 fsync）：按合同停止发布，保留上一套完整配置。
	ErrCredentialInstallNotDurable = errors.New("ai: 凭据安装持久性无法验证")
	ErrCredentialVolumeUnsupported = errors.New("ai: 凭据目录不在可验证的本机固定 NTFS 卷")
	ErrCredentialDirectoryBusy     = errors.New("ai: 凭据目录正在被占用")
	ErrCredentialDiskFull          = errors.New("ai: 凭据目录磁盘空间不足")
)

// CredentialObject 是不可变的外部密文对象。Ref/ProviderID/KeyID 都是绑定字段：
// 读取时必须与活动配置、当前主密钥一致，否则按锁定处理。
type CredentialObject struct {
	Version    int    `json:"version"`
	Ref        string `json:"ref"`
	KeyID      string `json:"key_id"`
	ProviderID int64  `json:"provider_id"`
	Provider   string `json:"provider"`
	Masked     string `json:"masked"`
	Ciphertext string `json:"ciphertext"`
	Nonce      string `json:"nonce"`
	CreatedTS  int64  `json:"created_ts"`
}

// CredentialDir 返回与主密钥同目录的密文版本目录。空路径返回空串。
func CredentialDir(masterKeyFile string) string {
	if strings.TrimSpace(masterKeyFile) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(masterKeyFile), credentialDirName)
}

// ValidCredentialRef 校验引用形状：只允许固定长度的十六进制，
// 防止用 ../ 或绝对路径把读取引到目录外。
func ValidCredentialRef(ref string) bool {
	if len(ref) != credentialRefBytes*2 {
		return false
	}
	for _, r := range ref {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// NewCredentialRef 生成随机引用。引用本身不含任何密钥材料。
func NewCredentialRef() (string, error) {
	var b [credentialRefBytes]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func credentialObjectPath(dir, ref string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("ai: 凭据目录未配置")
	}
	if !ValidCredentialRef(ref) {
		return "", fmt.Errorf("%w: 引用形状不合法", ErrCredentialObjectInvalid)
	}
	return filepath.Join(dir, ref+".json"), nil
}

// credentialObjectHook 仅测试用：在写对象临时文件之前注入失败，
// 验证「外部凭据失败时上一套完整配置仍可用」。生产路径恒为 nil。
var credentialObjectHook func(dir string) error

// credentialObjectStageHook 仅测试用：在准备件写入前、写入中与对象确认
// 持久后插入中断屏障，供 §2.2 的
// 四处终止写进程用例走真实保存 HTTP。生产路径恒为 nil。
var credentialObjectStageHook func(stage, dir string)

// SetCredentialObjectStageHookForTest 仅测试用：设置对象阶段屏障，返回恢复函数。
func SetCredentialObjectStageHookForTest(fn func(stage, dir string)) (restore func()) {
	old := credentialObjectStageHook
	credentialObjectStageHook = fn
	return func() { credentialObjectStageHook = old }
}

func credentialObjectStage(stage, dir string) {
	if credentialObjectStageHook != nil {
		credentialObjectStageHook(stage, dir)
	}
}

// credentialDirSync 是目录持久化校验点。nil 使用平台的真实实现；
// 测试可用 SetCredentialDirSyncForTest 模拟支持目录 fsync 的平台或注入失败。
var credentialDirSync func(dir string) error

// SetCredentialDirSyncForTest 仅测试用：替换目录持久化校验，返回恢复函数。
// 生产代码不得调用；传入 nil 恢复真实实现。
func SetCredentialDirSyncForTest(fn func(dir string) error) func() {
	old := credentialDirSync
	credentialDirSync = fn
	return func() { credentialDirSync = old }
}

// syncDirDurable 确证目录项安装已持久：打开目录并 fsync。
// 目录打开或同步失败都返回错误（不忽略）——合同要求确认持久性后才发布。
func syncDirDurable(dir string) error {
	if credentialDirSync != nil {
		return credentialDirSync(dir)
	}
	return syncCredentialDirectory(dir)
}

// CreateCredentialObject 在版本目录里排他创建一个不可变密文对象，返回对象。
//
// 摘要：目录 → 每次安装前同步父目录 → 临时文件写完 → fsync → 硬链接安装
// （目标已存在即失败，绝不覆盖）→ 读回 + 解密校验 → fsync 目录。
// 任一步失败（含目录持久性无法验证）都返回错误，调用方不得发布。
func CreateCredentialObject(dir string, masterKey []byte, providerID int64, providerName, plaintext string) (CredentialObject, error) {
	var obj CredentialObject
	plaintext = strings.TrimSpace(plaintext)
	providerName = strings.TrimSpace(providerName)
	if plaintext == "" {
		return obj, errors.New("ai: API Key 为空")
	}
	if providerName == "" {
		return obj, errors.New("ai: 厂商名为空")
	}
	if providerID <= 0 {
		return obj, errors.New("ai: 厂商 ID 为空")
	}
	if err := checkMasterKey(masterKey); err != nil {
		return obj, err
	}
	if strings.TrimSpace(dir) == "" {
		return obj, errors.New("ai: 凭据目录未配置")
	}
	// 目录项本身的持久性来自父目录同步。不能把「目录已存在」当成已确认：
	// 上一次安装可能正是在父目录同步处失败并留下了目录，另一个后台实例也可能
	// 读到刚创建、尚未确认的目录。因此每次安装对象前都重新确认父目录，
	// 故障持续时每次都拒绝发布；故障移除后的重试才会首次成功。
	if err := prepareCredentialDirectory(dir); err != nil {
		return obj, fmt.Errorf("%w: 准备凭据版本目录失败: %w", ErrCredentialInstallNotDurable, err)
	}
	if err := syncDirDurable(filepath.Dir(dir)); err != nil {
		return obj, fmt.Errorf("%w: 同步凭据目录父目录失败: %w",
			ErrCredentialInstallNotDurable, err)
	}
	if credentialObjectHook != nil {
		if err := credentialObjectHook(dir); err != nil {
			return obj, fmt.Errorf("ai: 写入凭据对象失败: %w", err)
		}
	}
	credentialObjectStage("pre_install", dir)
	ct, nonce, err := Encrypt(masterKey, []byte(plaintext))
	if err != nil {
		return obj, err
	}
	ref, err := NewCredentialRef()
	if err != nil {
		return obj, err
	}
	obj = CredentialObject{
		Version: credentialObjectVer, Ref: ref, KeyID: MasterKeyID(masterKey),
		ProviderID: providerID, Provider: providerName, Masked: Masked(plaintext),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		CreatedTS:  time.Now().Unix(),
	}
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return CredentialObject{}, err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(dir, credentialTempPattern)
	if err != nil {
		return CredentialObject{}, err
	}
	tmpName := tmp.Name()
	installed := false
	defer func() {
		if !installed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(credentialObjectPerm); err != nil {
		_ = tmp.Close()
		return CredentialObject{}, err
	}
	// Let the crash test interrupt this actual preparation file mid-write.
	dataToWrite := data
	if credentialObjectStageHook != nil {
		if _, err := tmp.Write(data[:1]); err != nil {
			_ = tmp.Close()
			return CredentialObject{}, err
		}
		credentialObjectStage("partial_temp", dir)
		dataToWrite = data[1:]
	}
	if _, err := tmp.Write(dataToWrite); err != nil {
		_ = tmp.Close()
		return CredentialObject{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return CredentialObject{}, err
	}
	if err := tmp.Close(); err != nil {
		return CredentialObject{}, err
	}
	final, err := credentialObjectPath(dir, ref)
	if err != nil {
		return CredentialObject{}, err
	}
	// 硬链接：目标已存在时返回 ErrExist，绝不覆盖既有对象。
	if err := os.Link(tmpName, final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return CredentialObject{}, fmt.Errorf("%w: 引用冲突", ErrCredentialObjectInvalid)
		}
		// 少数文件系统不支持硬链接：退回 O_EXCL 独占创建（仍只有一个写成功）。
		if err := writeCredentialObjectExclusive(final, data); err != nil {
			return CredentialObject{}, err
		}
	}
	installed = true
	_ = os.Remove(tmpName)
	// 读回验证：文件完整、格式正确、能解密。任一步失败都按「安装未完成」返回错误，
	// 由调用方停止发布；旧对象与被引用对象逐字节不变。
	loaded, plain, err := ResolveCredentialObject(dir, ref, masterKey, providerID)
	if err != nil {
		return CredentialObject{}, err
	}
	if plain != plaintext {
		return CredentialObject{}, fmt.Errorf("%w: 读回校验不一致", ErrCredentialObjectInvalid)
	}
	// 目录项安装持久性：打开目录并 fsync。失败（含平台不支持目录 fsync）即
	// 无法确认安装已落盘，按合同停止发布并保留上一套完整配置；对象保留为未引用件。
	if err := syncDirDurable(dir); err != nil {
		return CredentialObject{}, fmt.Errorf("%w: 同步凭据目录失败: %w",
			ErrCredentialInstallNotDurable, err)
	}
	credentialObjectStage("installed", dir)
	return loaded, nil
}

func writeCredentialObjectExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, credentialObjectPerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: 引用冲突", ErrCredentialObjectInvalid)
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// LoadCredentialObject 只读加载一个密文对象（不解密）。
// 文件缺失 → ErrCredentialObjectMissing；格式/版本/引用不符 → ErrCredentialObjectInvalid。
func LoadCredentialObject(dir, ref string) (CredentialObject, error) {
	var obj CredentialObject
	path, err := credentialObjectPath(dir, ref)
	if err != nil {
		return obj, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return obj, fmt.Errorf("%w: %s", ErrCredentialObjectMissing, ref)
	}
	if err != nil {
		return obj, fmt.Errorf("ai: 读取凭据对象失败: %w", err)
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return obj, fmt.Errorf("%w: 不是合法 JSON", ErrCredentialObjectInvalid)
	}
	if obj.Version != credentialObjectVer || obj.Ref != ref || strings.TrimSpace(obj.KeyID) == "" {
		return obj, fmt.Errorf("%w: 版本/引用/指纹字段不符", ErrCredentialObjectInvalid)
	}
	if obj.ProviderID <= 0 || strings.TrimSpace(obj.Provider) == "" {
		return obj, fmt.Errorf("%w: 缺少厂商绑定", ErrCredentialObjectInvalid)
	}
	if _, err := base64.StdEncoding.DecodeString(obj.Ciphertext); err != nil {
		return obj, fmt.Errorf("%w: ciphertext 不是 base64", ErrCredentialObjectInvalid)
	}
	nonce, err := base64.StdEncoding.DecodeString(obj.Nonce)
	if err != nil || len(nonce) == 0 {
		return obj, fmt.Errorf("%w: nonce 不合法", ErrCredentialObjectInvalid)
	}
	return obj, nil
}

// Decrypt 解密对象内容；主密钥指纹不符按 mismatch 处理，不解密不返回空值冒充。
func (o CredentialObject) Decrypt(masterKey []byte) (string, error) {
	if err := checkMasterKey(masterKey); err != nil {
		return "", err
	}
	if o.KeyID != MasterKeyID(masterKey) {
		return "", ErrCredentialKeyMismatch
	}
	ct, err := base64.StdEncoding.DecodeString(o.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("%w: ciphertext 不是 base64", ErrCredentialObjectInvalid)
	}
	nonce, err := base64.StdEncoding.DecodeString(o.Nonce)
	if err != nil {
		return "", fmt.Errorf("%w: nonce 不是 base64", ErrCredentialObjectInvalid)
	}
	plain, err := Decrypt(masterKey, ct, nonce)
	if err != nil {
		return "", fmt.Errorf("ai: 凭据对象解密失败（主密钥不匹配或文件损坏）: %w", err)
	}
	return strings.TrimSpace(string(plain)), nil
}

// ResolveCredentialObject 是读者（Runner / 后台复测 / 状态回读）的唯一定位入口：
// 按活动引用取对象、核对厂商 ID 与主密钥指纹、解密。
// 任何一步失败都返回错误，绝不回退同名文件或旧库。
func ResolveCredentialObject(dir, ref string, masterKey []byte, providerID int64) (CredentialObject, string, error) {
	obj, err := LoadCredentialObject(dir, ref)
	if err != nil {
		return CredentialObject{}, "", err
	}
	if providerID > 0 && obj.ProviderID != providerID {
		return CredentialObject{}, "", fmt.Errorf("%w: 对象厂商 ID=%d，活动配置=%d",
			ErrCredentialObjectMismatch, obj.ProviderID, providerID)
	}
	plain, err := obj.Decrypt(masterKey)
	if err != nil {
		return CredentialObject{}, "", err
	}
	return obj, plain, nil
}

// CredentialDirHasCiphertext 报告版本目录里是否已经存在密文对象或准备件。
//
// 显式初始化主密钥用它作为额外前提：目录里只要有对象（包括写完但未发布的
// 孤立准备件），就说明已有密文依赖某把主密钥，不得自动新建主密钥。
func CredentialDirHasCiphertext(dir string) (bool, error) {
	if strings.TrimSpace(dir) == "" {
		return false, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ai: 检查凭据版本目录失败: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			return true, nil
		}
	}
	return false, nil
}
