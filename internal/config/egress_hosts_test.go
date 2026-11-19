package config

// 部署配置里的出口白名单：YAML 解析、后台进程复制、非法条目启动即失败。
// 全部使用临时配置文件，不读写仓库内 config.yaml。

import (
	"os"
	"path/filepath"
	"testing"

	"rf4server/internal/aiconfig"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEgressAllowedHostsLoadedAndCopiedToAdmin(t *testing.T) {
	path := writeConfig(t, `
sqlite:
  path: "data/test.db"
ai:
  egress_allowed_hosts:
    - api.deepseek.com
    - "https://API.example.com:443"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AI.EgressAllowedHosts) != 2 {
		t.Fatalf("ai.egress_allowed_hosts = %v", cfg.AI.EgressAllowedHosts)
	}
	// 后台进程只拿到 config.Admin，必须在 Load 时同步同一份部署白名单。
	if len(cfg.Admin.AIEgressAllowedHosts) != 2 {
		t.Fatalf("admin 视图未复制出口白名单: %v", cfg.Admin.AIEgressAllowedHosts)
	}
	policy, err := aiconfig.NewEgressHostPolicy(cfg.AI.EgressAllowedHosts)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Allows("api.deepseek.com", "443") || !policy.Allows("api.example.com", "443") {
		t.Fatalf("白名单解析结果不符合预期: %v", cfg.AI.EgressAllowedHosts)
	}
}

func TestEgressAllowedHostsDefaultEmpty(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.AI.EgressAllowedHosts) != 0 || len(cfg.Admin.AIEgressAllowedHosts) != 0 {
		t.Fatalf("默认白名单应为空: ai=%v admin=%v",
			cfg.AI.EgressAllowedHosts, cfg.Admin.AIEgressAllowedHosts)
	}
}

// TestEgressAllowedHostsInvalidEntryNoLongerBlocksLoad 旧名单只为兼容继续解析：
// 条目写错（通配等）不再让进程起不来，也不再是发送前提（V2 只留急停）。
func TestEgressAllowedHostsInvalidEntryNoLongerBlocksLoad(t *testing.T) {
	path := writeConfig(t, `
sqlite:
  path: "data/test.db"
ai:
  egress_allowed_hosts:
    - "*.example.com"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("旧名单条目非法不应阻塞启动: %v", err)
	}
	if len(cfg.AI.EgressAllowedHosts) != 1 || cfg.AI.EgressAllowedHosts[0] != "*.example.com" {
		t.Fatalf("旧名单应按原样保留供排查: %v", cfg.AI.EgressAllowedHosts)
	}
	// 解析仍会报错（供展示/排查），但调用方不再据此拒绝发送。
	if _, err := aiconfig.NewEgressHostPolicy(cfg.AI.EgressAllowedHosts); err == nil {
		t.Fatal("通配条目本身仍应解析失败")
	}
}
