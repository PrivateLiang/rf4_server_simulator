package main

// aibridge 是可选子进程，刻意不进 supervisor 的固定进程列表：
// 它挂了不该让 gateway/gameserver 停机，也不该改变 supervisor 的进程数断言。

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type aiBridgeProc struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	logFile  *os.File
	stopping bool
	restarts int
	backoff  time.Duration
	started  time.Time
	lastErr  string
}

var aiProc aiBridgeProc

// startAIBridge 在 ai.enabled 时拉起同目录的 aibridge.exe。
// 未配置 Key 时进程仍可运行，只是走模板降级，不产生出网请求。
func (a *app) startAIBridge() {
	if !a.cfg.AI.Enabled {
		return
	}
	aiProc.mu.Lock()
	defer aiProc.mu.Unlock()
	if aiProc.cmd != nil && aiProc.cmd.Process != nil {
		return
	}
	exe := filepath.Join(filepath.Dir(mustExePath()), "aibridge.exe")
	if _, err := os.Stat(exe); err != nil {
		aiProc.lastErr = "aibridge.exe 不存在"
		a.log.Warn("aibridge.exe 不存在，跳过 AI 进程")
		return
	}
	if err := os.MkdirAll("logs", 0o755); err != nil {
		aiProc.lastErr = "AI 日志目录创建失败: " + err.Error()
		a.log.Warn("AI 日志目录创建失败: " + err.Error())
		return
	}
	f, err := os.OpenFile(filepath.Join("logs", "aibridge.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		aiProc.lastErr = "AI 日志打开失败: " + err.Error()
		a.log.Warn("AI 日志打开失败: " + err.Error())
		return
	}
	cmd := exec.Command(exe, "-config", a.cfgPath)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		aiProc.lastErr = "aibridge 启动失败: " + err.Error()
		a.log.Warn("aibridge 启动失败: " + err.Error())
		return
	}
	aiProc.cmd = cmd
	aiProc.logFile = f
	aiProc.stopping = false
	aiProc.started = time.Now()
	aiProc.lastErr = ""
	started := aiProc.started
	a.log.Info("aibridge 已启动（可选进程）")
	go func() {
		_ = cmd.Wait()
		aiProc.mu.Lock()
		restart := !aiProc.stopping && a.ctx.Err() == nil
		if aiProc.cmd == cmd {
			aiProc.cmd = nil
			aiProc.logFile = nil
			aiProc.started = time.Time{}
			_ = f.Close()
		}
		// 稳定运行超过一分钟视为恢复；否则指数退避，最多 30 秒，
		// 避免缺少数据表或端口被占用时每 2 秒刷一次日志。
		if time.Since(started) > time.Minute {
			aiProc.restarts = 0
			aiProc.backoff = 2 * time.Second
		} else {
			aiProc.restarts++
			if aiProc.backoff <= 0 {
				aiProc.backoff = 2 * time.Second
			} else {
				aiProc.backoff *= 2
			}
			if aiProc.backoff > 30*time.Second {
				aiProc.backoff = 30 * time.Second
			}
		}
		delay := aiProc.backoff
		restarts := aiProc.restarts
		aiProc.mu.Unlock()
		if !restart || !a.cfg.AI.Enabled {
			return
		}
		a.log.Warn(fmt.Sprintf("aibridge 退出，%s 后重启（第 %d 次）", delay, restarts))
		time.Sleep(delay)
		aiProc.mu.Lock()
		stopped := aiProc.stopping || a.ctx.Err() != nil
		aiProc.mu.Unlock()
		if !stopped {
			a.startAIBridge()
		}
	}()
}

// aiBridgeStatus 给控制中心面板用：进程状态 + aibridge /healthz。
// aibridge 不在 supervisor 固定列表里，所以单独给一份状态。
func (p *app) aiBridgeStatus() map[string]any {
	aiProc.mu.Lock()
	running := aiProc.cmd != nil && aiProc.cmd.Process != nil
	pid := 0
	if running {
		pid = aiProc.cmd.Process.Pid
	}
	restarts := aiProc.restarts
	started := aiProc.started
	lastErr := aiProc.lastErr
	aiProc.mu.Unlock()

	state := "stopped"
	if !p.cfg.AI.Enabled {
		state = "disabled"
	}
	if running {
		state = "running"
	}
	healthy := false
	if running {
		healthy = probeAIHealth(p.cfg.AI.ObsAddr)
	}
	uptime := 0
	if running && !started.IsZero() {
		uptime = int(time.Since(started).Seconds())
	}
	return map[string]any{
		"name": "aibridge", "state": state, "pid": pid, "restarts": restarts,
		"uptime_s": uptime, "healthy": healthy, "last_error": lastErr,
		"obs_addr": p.cfg.AI.ObsAddr,
	}
}

func probeAIHealth(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	} else if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	client := &http.Client{Timeout: 700 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// stopAIBridge 停止可选 AI 进程；显式停止不触发看门狗重启。
func (a *app) stopAIBridge() {
	aiProc.mu.Lock()
	defer aiProc.mu.Unlock()
	aiProc.stopping = true
	if aiProc.cmd == nil || aiProc.cmd.Process == nil {
		aiProc.started = time.Time{}
		aiProc.lastErr = ""
		return
	}
	_ = aiProc.cmd.Process.Kill()
	aiProc.cmd = nil
	aiProc.started = time.Time{}
	aiProc.lastErr = ""
	if aiProc.logFile != nil {
		_ = aiProc.logFile.Close()
		aiProc.logFile = nil
	}
	a.log.Info("aibridge 已停止")
}
