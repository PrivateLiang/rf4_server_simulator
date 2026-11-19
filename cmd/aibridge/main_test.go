package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"rf4server/internal/aibridge"
	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/store"
)

// TestHealthSnapshotFailClosed /healthz 只按急停给出可外发判定；
// 历史 outbound 值仅作排查字段，不再决定放行。
func TestHealthSnapshotFailClosed(t *testing.T) {
	ctx := context.Background()
	d, err := store.Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}

	snap := healthSnapshot(ctx, d)
	if got := snap["outbound"]; got != "1" {
		t.Errorf("outbound 原始值 = %v，期望历史值 1（仅供排查）", got)
	}
	if got := snap["outbound_allowed"]; got != true {
		t.Errorf("outbound_allowed = %v，期望 true（未急停）", got)
	}
	if got := snap["outbound_state"]; got != aiconfig.StateAllowed {
		t.Errorf("outbound_state = %v，期望 %s", got, aiconfig.StateAllowed)
	}
	if got := snap["outbound_display"]; got != "开（未急停）" {
		t.Errorf("outbound_display = %v，期望 开（未急停）", got)
	}

	// 急停：拒绝；非法值同样保守停止。
	for _, value := range []string{"1", "yes"} {
		if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: value}); err != nil {
			t.Fatal(err)
		}
		snap = healthSnapshot(ctx, d)
		if got := snap["outbound_allowed"]; got != false {
			t.Errorf("急停 %q 时 outbound_allowed = %v，期望 false", value, got)
		}
		if got := snap["outbound_state"]; got != aiconfig.StateEmergency {
			t.Errorf("急停 %q 时 outbound_state = %v，期望 %s", value, got, aiconfig.StateEmergency)
		}
	}

	// 缺 outbound 键：旧闸缺失不影响放行。
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `DELETE FROM ai_setting WHERE key=?`, aiconfig.KeyOutboundEnabled); err != nil {
		t.Fatal(err)
	}
	snap = healthSnapshot(ctx, d)
	if got := snap["outbound"]; got != aiconfig.DefaultOutboundEnabled {
		t.Errorf("缺键 outbound = %v，期望默认 %s", got, aiconfig.DefaultOutboundEnabled)
	}
	if got := snap["outbound_state"]; got != aiconfig.StateAllowed {
		t.Errorf("缺键 outbound_state = %v，期望 %s", got, aiconfig.StateAllowed)
	}
}

// TestRunInitKeyEmptyInstall -init-key 的空安装路径：创建主密钥、不建主库、
// 不因缺少静态库而失败（证明没有打开静态库/Knowledge），重复执行幂等。
func TestRunInitKeyEmptyInstall(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "rf4server.db")
	keyPath := filepath.Join(dir, "cfg", "ai_master.key")
	cfg := &config.Config{
		SQLite: config.SQLite{Path: dbPath, StaticPath: filepath.Join(dir, "missing-static.sqlite3")},
		AI:     config.AI{Enabled: true, MasterKeyFile: keyPath},
	}

	res, err := runInitKey(cfg)
	if err != nil {
		t.Fatalf("-init-key 空安装失败: %v", err)
	}
	if !res.Created || len(res.Key) != 32 {
		t.Fatalf("结果 = (created=%v, len=%d)，期望 (true, 32)", res.Created, len(res.Key))
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil || len(raw) != 32 {
		t.Fatalf("主密钥文件 = %d 字节, %v", len(raw), err)
	}
	// 空安装没有主库时只读判断，不应顺手创建数据库或目录。
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("-init-key 不应创建主库: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dbPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("-init-key 不应创建主库目录: %v", err)
	}

	res2, err := runInitKey(cfg)
	if err != nil {
		t.Fatalf("重复 -init-key 失败: %v", err)
	}
	if res2.Created {
		t.Fatal("重复 -init-key 应返回已存在")
	}
	if after, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(raw, after) {
		t.Fatal("重复 -init-key 改动了主密钥文件")
	}
}

// TestRunInitKeyRefusesLegacyCiphertext 旧库已有密文：CLI 与后台走同一判定，
// 拒绝创建且不覆盖任何文件。
func TestRunInitKeyRefusesLegacyCiphertext(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "rf4server.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SQL.Exec(`
		INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
		VALUES((SELECT id FROM ai_provider WHERE name='deepseek'),x'00',x'00','default','sk-****0000',0)`); err != nil {
		t.Fatalf("写入旧库密文: %v", err)
	}
	keyPath := filepath.Join(dir, "ai_master.key")
	cfg := &config.Config{SQLite: config.SQLite{Path: dbPath}, AI: config.AI{Enabled: true, MasterKeyFile: keyPath}}
	if _, err := runInitKey(cfg); !errors.Is(err, aibridge.ErrMasterKeyInitLegacyCiphertext) {
		t.Fatalf("错误 = %v，期望拒绝创建（旧库有密文）", err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("旧库有密文时 -init-key 仍创建了主密钥")
	}
}

// TestRunInitKeyRefusesCredentialObjects 凭据版本目录里已有对象（含孤立准备件）：
// -init-key 与后台走同一判定，不得新建主密钥。
func TestRunInitKeyRefusesCredentialObjects(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ai_master.key")
	credDir := aibridge.CredentialDir(keyPath)
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "0123456789abcdef0123456789abcdef.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		SQLite: config.SQLite{Path: filepath.Join(dir, "rf4server.db")},
		AI:     config.AI{Enabled: true, MasterKeyFile: keyPath},
	}
	if _, err := runInitKey(cfg); !errors.Is(err, aibridge.ErrMasterKeyInitCredentialObjects) {
		t.Fatalf("错误 = %v，期望拒绝创建（版本目录已有密文）", err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("版本目录有密文时 -init-key 仍创建了主密钥")
	}
}

// TestRunInitKeyCorruptKeyNotOverwritten 主密钥损坏：-init-key 报错且保留原文件。
func TestRunInitKeyCorruptKeyNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ai_master.key")
	corrupt := []byte("broken")
	if err := os.WriteFile(keyPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		SQLite: config.SQLite{Path: filepath.Join(dir, "rf4server.db")},
		AI:     config.AI{Enabled: true, MasterKeyFile: keyPath},
	}
	if _, err := runInitKey(cfg); !errors.Is(err, aibridge.ErrMasterKeyInvalid) {
		t.Fatalf("错误 = %v，期望 ErrMasterKeyInvalid", err)
	}
	if raw, err := os.ReadFile(keyPath); err != nil || !bytes.Equal(raw, corrupt) {
		t.Fatal("损坏的主密钥文件被改写")
	}
}
