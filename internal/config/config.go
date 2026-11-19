// Package config 提供 YAML 配置加载和环境变量覆盖。
//
// 空配置使用开发默认值；敏感配置通过 config.yaml 或 RF4_ 环境变量注入，
// 仓库只保留示例配置。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 全量配置。每个进程只读自己关心的部分。
type Config struct {
	Env      string   `yaml:"env"` // dev | prod
	Log      Log      `yaml:"log"`
	SQLite   SQLite   `yaml:"sqlite"`
	Gateway  Gateway  `yaml:"gateway"`
	Game     Game     `yaml:"game"`
	Admin    Admin    `yaml:"admin"`
	Launcher Launcher `yaml:"launcher"`
	AI       AI       `yaml:"ai"`
}

// AI 是 aibridge 进程的本地配置。
// 出网、联网搜索、紧急停止属于运行期开关，统一由后台写入 ai_setting，
// 这里只保留进程级参数，避免配置文件和数据库两套真值。
type AI struct {
	Enabled           bool    `yaml:"enabled"`
	ObsAddr           string  `yaml:"obs_addr"`
	PollIntervalMS    int     `yaml:"poll_interval_ms"`
	RequestTimeoutSec float64 `yaml:"request_timeout_sec"`
	MaxConcurrent     int     `yaml:"max_concurrent"`
	MasterKeyFile     string  `yaml:"master_key_file"`

	// EgressAllowedHosts 是旧版部署级出口主机名单，仅为兼容旧配置继续解析。
	//
	// V2（DEV-104「只留急停」）不再用它做启动或发送前提：名单为空、条目非法
	// 都不影响启动与外发；传输层仍保留 HTTPS、解析地址、重定向与证书保护。
	// 后台只把它当部署记录展示（配了几条），不回显主机清单。
	EgressAllowedHosts []string `yaml:"egress_allowed_hosts"`
}

// SQLite 本地单文件数据库（多进程共享，WAL 并发）。
type SQLite struct {
	Path       string `yaml:"path"`        // 主库，例如 data/rf4server.db
	StaticPath string `yaml:"static_path"` // 只读静态配置库，例如 server_data/static.sqlite3
}

type Log struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // text | json
}

// Tunnel 网关↔主服内部隧道（gameserver 监听、gateway 拨号）。
type Tunnel struct {
	Addr string `yaml:"addr"` // gameserver 内部监听地址，如 127.0.0.1:9401
	PSK  string `yaml:"psk"`  // 预共享密钥，隧道握手认证
}

type Gateway struct {
	LoginAddr             string  `yaml:"login_addr"`        // HTTPS 登录监听，如 :443
	LoginCompatAddr       string  `yaml:"login_compat_addr"` // 可选：旧版 CN 客户端的明文 HTTP 登录兼容口，如 127.0.0.1:7515
	RealtimeAddr          string  `yaml:"realtime_addr"`     // realtime TCP 监听，如 :9341
	CertFile              string  `yaml:"cert_file"`
	KeyFile               string  `yaml:"key_file"`
	AdvertiseHost         string  `yaml:"advertise_host"`          // 写入 <logon><host> 的地址
	AdvertisePort         int     `yaml:"advertise_port"`          // 写入 <logon><port>
	Region                string  `yaml:"region"`                  // CN → 客户端中文界面
	ServerName            string  `yaml:"server_name"`             // 写入 <logon><server>
	LoginDomain           string  `yaml:"login_domain"`            // api.rf4game.ru（供日志/工具用）
	MaxConns              int     `yaml:"max_conns"`               // realtime 最大并发连接
	AckClientFrames       bool    `yaml:"ack_client_frames"`       // 先 ACK 后转发（协议要求，默认开启）
	TransportKeepaliveSec float64 `yaml:"transport_keepalive_sec"` // 4 字节零帧保活间隔

	Tunnel Tunnel `yaml:"tunnel"`

	ObsAddr string `yaml:"obs_addr"` // 观测端口 /healthz /readyz /metrics /debug/pprof

	TokenTTL          int     `yaml:"token_ttl_days"`         // 登录 token 有效期（天）
	UseridStart       int64   `yaml:"userid_start"`           // 自增 userid 起点
	BiteProbability   float64 `yaml:"bite_probability"`       // 起竿判出鱼概率（1.0=每杆必出）
	BiteDelayScale    float64 `yaml:"bite_delay_scale"`       // 咬钩延迟倍率
	BiteDelayFixed    float64 `yaml:"bite_delay_fixed"`       // 固定咬钩延迟秒（>0 时忽略分布）
	KeepaliveInterval float64 `yaml:"keepalive_interval_sec"` // 2/2 世界时钟推送间隔（秒）
}

type Game struct {
	Tunnel        Tunnel `yaml:"tunnel"` // 与 Gateway.Tunnel 相同配置
	ObsAddr       string `yaml:"obs_addr"`
	DefaultUserid int64  `yaml:"default_userid"` // 单账号模式下使用的固定 userid

	KeepaliveIntervalSec float64 `yaml:"keepalive_interval_sec"` // 2/2 世界时钟推送间隔（秒）
}

// KeepaliveInterval 2/2 保活间隔；必须小于客户端断开阈值（~10s）。
func (g *Game) KeepaliveInterval() time.Duration {
	if g.KeepaliveIntervalSec <= 0 {
		return 5 * time.Second
	}
	return time.Duration(g.KeepaliveIntervalSec * float64(time.Second))
}

type Admin struct {
	HTTPAddr  string `yaml:"http_addr"` // 仅 cmd/admin 独立调试监听，如 :8080
	JWTSecret string `yaml:"jwt_secret"`
	ObsAddr   string `yaml:"obs_addr"`
	Username  string `yaml:"username"` // 管理员账号（以配置为准，启动时同步）
	Password  string `yaml:"password"` // 管理员密码（明文进 yaml，注意文件权限）

	// AIEgressAllowedHosts 不在 admin 段单独配置：Load 会把
	// ai.egress_allowed_hosts 原样复制过来，让后台「测试连接」进程读到与
	// aibridge 相同的部署白名单；两处各配一份会漂移。进程只读，不回写 YAML。
	AIEgressAllowedHosts []string `yaml:"-"`
}

// Launcher 本地启动器设置。
type Launcher struct {
	GamePath    string `yaml:"game_path"`    // rf4_x64.exe 绝对路径
	GameRegion  string `yaml:"game_region"`  // 启动参数 -region 的值
	HostsSwitch bool   `yaml:"hosts_switch"` // 开启后：服务启动成功自动把登录域名指向 127.0.0.1，launcher 退出时还原（需管理员权限运行 launcher）
}

// Load 从 path 读取 YAML 并应用环境变量覆盖；path 为空时只用默认值+环境变量。
func Load(path string) (*Config, error) {
	cfg := defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取配置 %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("解析配置 %s: %w", path, err)
		}
	}
	cfg.applyEnvOverrides()
	// 主密钥路径按「配置文件所在目录」解析：相对 CWD 会让同一份配置从不同目录
	// 启动时指向不同的主密钥文件，已有密文将无法解密。
	cfg.resolveAISecretPaths(path)
	// 后台进程与 aibridge 分属两个进程、各自只读自己那部分配置；出口白名单
	// 属于同一份部署策略，这里在加载时复制给后台，避免两处配置漂移。
	cfg.Admin.AIEgressAllowedHosts = append([]string(nil), cfg.AI.EgressAllowedHosts...)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolveAISecretPaths 把 AI 主密钥路径解析成稳定路径：
// 绝对路径只做 Clean；相对路径相对「配置文件所在目录」，不是相对进程 CWD。
// 没有配置文件（默认值/测试）时保持相对路径，仅用于本地开发默认值。
func (c *Config) resolveAISecretPaths(configPath string) {
	raw := strings.TrimSpace(c.AI.MasterKeyFile)
	if raw == "" {
		return
	}
	if filepath.IsAbs(raw) {
		c.AI.MasterKeyFile = filepath.Clean(raw)
		return
	}
	if strings.TrimSpace(configPath) == "" {
		return
	}
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return
	}
	c.AI.MasterKeyFile = filepath.Join(filepath.Dir(absConfig), raw)
}

func defaults() *Config {
	return &Config{
		Env:      "dev",
		Log:      Log{Level: "info", Format: "text"},
		Launcher: Launcher{GameRegion: "CN"},
		SQLite: SQLite{
			Path:       "data/rf4server.db",
			StaticPath: "server_data/static.sqlite3",
		},
		Gateway: Gateway{
			LoginAddr:             ":443",
			RealtimeAddr:          ":9341",
			CertFile:              "certs/server.crt",
			KeyFile:               "certs/server.key",
			AdvertiseHost:         "127.0.0.1",
			AdvertisePort:         9341,
			Region:                "CN",
			ServerName:            "RF4_566",
			LoginDomain:           "api.rf4game.ru",
			MaxConns:              512,
			AckClientFrames:       true,
			TransportKeepaliveSec: 5.0,
			Tunnel:                Tunnel{Addr: "127.0.0.1:9401", PSK: "change-me"},
			ObsAddr:               ":9501",
			TokenTTL:              365,
			UseridStart:           1000001,
			BiteProbability:       1.0,
			BiteDelayScale:        1.0,
			BiteDelayFixed:        0,
			KeepaliveInterval:     5.0,
		},
		Game: Game{
			Tunnel:               Tunnel{Addr: "127.0.0.1:9401", PSK: "change-me"},
			ObsAddr:              ":9502",
			DefaultUserid:        1000001,
			KeepaliveIntervalSec: 5.0,
		},
		Admin: Admin{
			HTTPAddr:  ":8080",
			JWTSecret: "change-me",
			ObsAddr:   ":9503",
			Username:  "admin",
			Password:  "admin123",
		},
		AI: AI{
			Enabled:           true, // 默认拉起；未配 Key 时只走模板降级，不产生出网请求
			ObsAddr:           "127.0.0.1:9504",
			PollIntervalMS:    500,
			RequestTimeoutSec: 20,
			MaxConcurrent:     2,
			MasterKeyFile:     "data/ai_master.key",
		},
	}
}

// applyEnvOverrides 环境变量覆盖：只覆盖部署时最常注入的字段。
func (c *Config) applyEnvOverrides() {
	envStr := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	envFloat := func(key string, dst *float64) {
		if v, ok := os.LookupEnv(key); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				*dst = f
			}
		}
	}
	envInt := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				*dst = n
			}
		}
	}

	envStr("RF4_LOG_LEVEL", &c.Log.Level)
	envStr("RF4_LOG_FORMAT", &c.Log.Format)
	envStr("RF4_SQLITE_PATH", &c.SQLite.Path)
	envStr("RF4_SQLITE_STATIC_PATH", &c.SQLite.StaticPath)
	envStr("RF4_GATEWAY_LOGIN_ADDR", &c.Gateway.LoginAddr)
	envStr("RF4_GATEWAY_LOGIN_COMPAT_ADDR", &c.Gateway.LoginCompatAddr)
	envStr("RF4_GATEWAY_REALTIME_ADDR", &c.Gateway.RealtimeAddr)
	envStr("RF4_GATEWAY_ADVERTISE_HOST", &c.Gateway.AdvertiseHost)
	envStr("RF4_GATEWAY_CERT", &c.Gateway.CertFile)
	envStr("RF4_GATEWAY_KEY", &c.Gateway.KeyFile)
	envStr("RF4_TUNNEL_ADDR", &c.Gateway.Tunnel.Addr)
	envStr("RF4_TUNNEL_PSK", &c.Gateway.Tunnel.PSK)
	envStr("RF4_GAME_TUNNEL_PSK", &c.Game.Tunnel.PSK)
	envStr("RF4_ADMIN_HTTP_ADDR", &c.Admin.HTTPAddr)
	envStr("RF4_ADMIN_JWT_SECRET", &c.Admin.JWTSecret)
	envStr("RF4_ADMIN_PASSWORD", &c.Admin.Password)
	envStr("RF4_GAME_CLIENT_PATH", &c.Launcher.GamePath)
	envStr("RF4_GAME_CLIENT_REGION", &c.Launcher.GameRegion)
	envFloat("RF4_GATEWAY_KEEPALIVE_SEC", &c.Gateway.TransportKeepaliveSec)
	envFloat("RF4_KEEPALIVE_INTERVAL_SEC", &c.Gateway.KeepaliveInterval)
	envFloat("RF4_BITE_PROBABILITY", &c.Gateway.BiteProbability)
	envInt("RF4_GATEWAY_MAX_CONNS", &c.Gateway.MaxConns)
}

func (c *Config) validate() error {
	if c.SQLite.Path == "" {
		return fmt.Errorf("sqlite.path 未配置")
	}
	if c.Gateway.AdvertisePort == 0 {
		return fmt.Errorf("gateway.advertise_port 未配置")
	}
	if c.AI.Enabled && strings.TrimSpace(c.AI.MasterKeyFile) == "" {
		return fmt.Errorf("ai.master_key_file 未配置")
	}
	// 旧出口名单仅作兼容解析，不再校验、也不再作为启动前提（V2 只留急停）。
	// 条目写法错误不影响启动与外发；后台只显示「配了几条」。
	return nil
}
