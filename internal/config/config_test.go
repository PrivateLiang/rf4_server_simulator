package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestStaticDatabasePathDefaultAndEnvOverride(t *testing.T) {
	if got := defaults().SQLite.StaticPath; got != "server_data/static.sqlite3" {
		t.Fatalf("默认静态库路径=%q", got)
	}
	if got := defaults().Launcher.GameRegion; got != "CN" {
		t.Fatalf("默认游戏区域=%q", got)
	}
	t.Setenv("RF4_SQLITE_STATIC_PATH", "custom/static.sqlite3")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.SQLite.StaticPath; got != "custom/static.sqlite3" {
		t.Fatalf("环境变量静态库路径=%q", got)
	}
}

func TestAdminPasswordEnvOverride(t *testing.T) {
	t.Setenv("RF4_ADMIN_PASSWORD", "compose-test-password")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Admin.Password != "compose-test-password" {
		t.Fatal("RF4_ADMIN_PASSWORD did not override the configured password")
	}
}

func TestLoginCompatEnvOverride(t *testing.T) {
	t.Setenv("RF4_GATEWAY_LOGIN_COMPAT_ADDR", ":7515")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Gateway.LoginCompatAddr; got != ":7515" {
		t.Fatalf("RF4_GATEWAY_LOGIN_COMPAT_ADDR=%q", got)
	}
}

// writeConfigFile 写一份最小配置（只覆盖被测字段，其余用默认值）。
func writeConfigFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写测试配置: %v", err)
	}
}

// TestMasterKeyFileRelativeToConfigDir 主密钥相对路径按「配置文件所在目录」解析：
// 换启动目录（CWD）不改变主密钥文件位置，否则旧密文会因换了一把主密钥而解不开。
func TestMasterKeyFileRelativeToConfigDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeConfigFile(t, cfgPath, "ai:\n  master_key_file: keys/ai_master.key\n")
	want := filepath.Join(dir, "keys", "ai_master.key")

	// 合成主密钥字面量，独立于被测代码。
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, key, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	if cfg.AI.MasterKeyFile != want {
		t.Fatalf("主密钥路径 = %q，期望 %q", cfg.AI.MasterKeyFile, want)
	}
	if got, err := os.ReadFile(cfg.AI.MasterKeyFile); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("按解析路径读取主密钥 = (%d 字节, %v)", len(got), err)
	}

	// 换启动目录后用相对配置文件路径加载：仍解析到同一把主密钥。
	t.Chdir(dir)
	cfg2, err := Load("config.yaml")
	if err != nil {
		t.Fatalf("换 CWD 后加载: %v", err)
	}
	if cfg2.AI.MasterKeyFile != want {
		t.Fatalf("换 CWD 后主密钥路径 = %q，期望 %q", cfg2.AI.MasterKeyFile, want)
	}
	got, err := os.ReadFile(cfg2.AI.MasterKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("换 CWD 后指向了不同的主密钥")
	}
}

// TestMasterKeyFileAbsolutePathKept 绝对路径只做 Clean，不拼配置目录。
func TestMasterKeyFileAbsolutePathKept(t *testing.T) {
	abs := filepath.ToSlash(filepath.Join(t.TempDir(), "secrets", "ai_master.key"))
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigFile(t, cfgPath, fmt.Sprintf("ai:\n  master_key_file: %q\n", abs))
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	if want := filepath.Clean(filepath.FromSlash(abs)); cfg.AI.MasterKeyFile != want {
		t.Fatalf("绝对主密钥路径 = %q，期望 %q", cfg.AI.MasterKeyFile, want)
	}
}

// TestMasterKeyFileDefaultRelativeWithoutConfigFile 没有配置文件时沿用默认相对
// 路径（本地开发默认值），不做隐式目录拼接。
func TestMasterKeyFileDefaultRelativeWithoutConfigFile(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("加载默认配置: %v", err)
	}
	if got := cfg.AI.MasterKeyFile; got != "data/ai_master.key" {
		t.Fatalf("无配置文件时主密钥路径 = %q", got)
	}
}
