package aibridge

// 主密钥显式初始化的唯一判定点。
//
// 允许新建主密钥的前提必须同时成立：
//  1. 主密钥文件不存在（缺失）；
//  2. 旧库 ai_secret 没有任何密文行；
//  3. 主密钥同目录的外部凭据文件不存在；
//  4. 主密钥同目录的凭据版本目录里没有对象或孤立准备件（T-102 新增）。
//
// 任一不成立都拒绝创建并保留原文件：自动生成的新密钥会让既有密文永久无法解密，
// 等价于把主密钥「覆盖」掉。后台保存（internal/admin）与 CLI（cmd/aibridge -init-key）
// 都只能调用这里，禁止各写一套判定。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"rf4server/internal/store"
)

// MasterKeyInitResult 是一次显式初始化判定的结果。
type MasterKeyInitResult struct {
	// Key 是可用主密钥：本次新建，或已存在且合法的原文件内容。
	Key []byte
	// Created 为 true 表示本次新建；false 表示已存在且未改动。
	Created bool
}

// masterKeyInitRefused 是「已有密文/凭据文件，拒绝新建主密钥」的错误。
// 它同时满足 errors.Is(err, ErrMasterKeyMissing)：调用方既有的「缺主密钥」提示
// 与 fail-closed 分支不需要为了拒绝原因再分叉。
type masterKeyInitRefused struct{ reason string }

func (e *masterKeyInitRefused) Error() string { return e.reason }

func (e *masterKeyInitRefused) Unwrap() error { return ErrMasterKeyMissing }

var (
	// ErrMasterKeyInitLegacyCiphertext 旧库 ai_secret 已有密文而主密钥缺失：
	// 没有原主密钥就无法恢复，拒绝创建新主密钥。
	ErrMasterKeyInitLegacyCiphertext = &masterKeyInitRefused{
		reason: "ai: 旧库 ai_secret 已有密文，拒绝创建新主密钥；需恢复原主密钥或重新提供凭据",
	}
	// ErrMasterKeyInitCredentialFile 外部受保护凭据文件已存在而主密钥缺失：
	// 保留原文件，拒绝创建新主密钥。
	ErrMasterKeyInitCredentialFile = &masterKeyInitRefused{
		reason: "ai: 外部凭据文件已存在，拒绝创建新主密钥；请恢复原主密钥，不要删除或重建凭据文件",
	}
	// ErrMasterKeyInitCredentialObjects 凭据版本目录里已有密文对象（含写完但
	// 未发布的孤立准备件）而主密钥缺失：同样锁定，不新建主密钥。
	ErrMasterKeyInitCredentialObjects = &masterKeyInitRefused{
		reason: "ai: 凭据版本目录已存在密文对象，拒绝创建新主密钥；请恢复原主密钥",
	}
)

// EnsureMasterKeyForExplicitInit 返回显式初始化路径要用的主密钥。
//
// 只有「主密钥缺失 + 旧库无密文行 + 外部凭据文件不存在」三条同时成立才会创建；
// 主密钥已存在且合法时幂等返回（Created=false，文件字节不变）；主密钥损坏时返回
// ErrMasterKeyInvalid 并保留原文件。db 允许为 nil（主库文件尚未创建），等价于
// 「旧库没有任何密文行」。
func EnsureMasterKeyForExplicitInit(ctx context.Context, db *store.DB, keyPath string) (MasterKeyInitResult, error) {
	key, err := LoadMasterKey(keyPath)
	if err == nil {
		return MasterKeyInitResult{Key: key}, nil
	}
	if !errors.Is(err, ErrMasterKeyMissing) {
		// 损坏/长度异常/读取失败：保留原文件，不重建。
		return MasterKeyInitResult{}, err
	}
	if strings.TrimSpace(keyPath) == "" {
		return MasterKeyInitResult{}, err
	}
	hasLegacy, lerr := legacyCiphertextExists(ctx, db)
	if lerr != nil {
		return MasterKeyInitResult{}, lerr
	}
	if hasLegacy {
		return MasterKeyInitResult{}, ErrMasterKeyInitLegacyCiphertext
	}
	credPath := CredentialsPath(keyPath)
	if credPath != "" {
		if _, serr := os.Stat(credPath); serr == nil {
			return MasterKeyInitResult{}, ErrMasterKeyInitCredentialFile
		} else if !errors.Is(serr, os.ErrNotExist) {
			return MasterKeyInitResult{}, fmt.Errorf("ai: 检查外部凭据文件失败: %w", serr)
		}
	}
	// 版本目录里的对象（含孤立准备件）同样是「已有密文」的证据。
	if has, herr := CredentialDirHasCiphertext(CredentialDir(keyPath)); herr != nil {
		return MasterKeyInitResult{}, herr
	} else if has {
		return MasterKeyInitResult{}, ErrMasterKeyInitCredentialObjects
	}
	key, created, ierr := initMasterKeyOnce(keyPath)
	if ierr != nil {
		return MasterKeyInitResult{}, ierr
	}
	return MasterKeyInitResult{Key: key, Created: created}, nil
}

// legacyCiphertextExists 判断旧库 ai_secret 是否已有密文行。
// 表不存在（空库/未迁移）等价于没有密文行，不算失败。
func legacyCiphertextExists(ctx context.Context, db *store.DB) (bool, error) {
	if db == nil {
		return false, nil
	}
	var tables int
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ai_secret'`).Scan(&tables); err != nil {
		return false, fmt.Errorf("ai: 检查旧库密文表失败: %w", err)
	}
	if tables == 0 {
		return false, nil
	}
	rows, err := db.AIListSecrets(ctx)
	if err != nil {
		return false, fmt.Errorf("ai: 读取旧库密文失败: %w", err)
	}
	return len(rows) > 0, nil
}
