package aibridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// TestOutboundGateOnlyEmergency 用本地模拟厂商统计请求数：
// V2（DEV-104）只留急停——缺 outbound 键、非法值、显式 0、整卡保存都不得再拦住
// 发送；急停（含非法值保守停止）与「停前排队、恢复后」必须 0 请求并落终态。
func TestOutboundGateOnlyEmergency(t *testing.T) {
	type want struct {
		hits     int32
		status   string
		errMatch string
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, ctx context.Context, d *store.DB)
		want  want
	}{
		{
			name: "缺 outbound 键仍发送",
			setup: func(t *testing.T, ctx context.Context, d *store.DB) {
				if _, err := d.SQL.ExecContext(ctx, `DELETE FROM ai_setting WHERE key='outbound_enabled'`); err != nil {
					t.Fatal(err)
				}
			},
			want: want{hits: 1, status: "done"},
		},
		{
			name: "非法值仍发送",
			setup: func(t *testing.T, ctx context.Context, d *store.DB) {
				if _, err := d.SQL.ExecContext(ctx,
					`UPDATE ai_setting SET value='yes' WHERE key='outbound_enabled'`); err != nil {
					t.Fatal(err)
				}
			},
			want: want{hits: 1, status: "done"},
		},
		{
			name: "显式关仍发送",
			setup: func(t *testing.T, ctx context.Context, d *store.DB) {
				if err := d.AISetSettings(ctx, map[string]string{
					aiconfig.KeyOutboundEnabled: "0", "web_search_enabled": "0",
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: want{hits: 1, status: "done"},
		},
		{
			name: "后台整卡保存不构成闸",
			setup: func(t *testing.T, ctx context.Context, d *store.DB) {
				err := d.AISetSettings(ctx, map[string]string{
					"outbound_enabled":   "1",
					"web_search_enabled": "0",
					"emergency_stop":     "0",
					"search_endpoint":    "",
					"max_tool_rounds":    "2",
				})
				if err != nil {
					t.Fatal(err)
				}
			},
			want: want{hits: 1, status: "done"},
		},
		{
			name: "急停零出站",
			setup: func(t *testing.T, ctx context.Context, d *store.DB) {
				if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
					t.Fatal(err)
				}
			},
			want: want{hits: 0, status: "fallback", errMatch: "emergency_stop"},
		},
		{
			name: "急停非法值保守停止",
			setup: func(t *testing.T, ctx context.Context, d *store.DB) {
				if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "yes"}); err != nil {
					t.Fatal(err)
				}
			},
			want: want{hits: 0, status: "fallback", errMatch: "emergency_switch_invalid"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			d, err := store.Open(filepath.Join(t.TempDir(), "egress.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.Migrate(); err != nil {
				t.Fatal(err)
			}

			var hits int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"正常"}}]}`))
			}))
			defer srv.Close()

			if _, err := d.SQL.ExecContext(ctx, `
				UPDATE ai_provider SET base_url=?, enabled=1 WHERE name='deepseek'`, srv.URL); err != nil {
				t.Fatal(err)
			}
			if _, err := d.SQL.ExecContext(ctx, `
				UPDATE ai_model SET model_id='local-test' WHERE provider_id=(SELECT id FROM ai_provider WHERE name='deepseek')`); err != nil {
				t.Fatal(err)
			}
			// 合成密钥只用于让 Provider 可构造（旧安装兼容路径读 ai_secret）。
			masterKey := []byte("0123456789abcdef0123456789abcdef")
			ciphertext, nonce, err := Encrypt(masterKey, []byte("sk-synthetic-test"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.SQL.ExecContext(ctx, `
				INSERT INTO ai_secret(provider_id,ciphertext,nonce,key_id,masked,updated_ts)
				VALUES((SELECT id FROM ai_provider WHERE name='deepseek'),?,?,'default','****test',0)
				ON CONFLICT(provider_id) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce,masked=excluded.masked`,
				ciphertext, nonce); err != nil {
				t.Fatal(err)
			}

			taskID, err := d.AIEnqueue(ctx, &store.AITask{
				Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
				Agent: "fishguide", ContactUID: 2147483644,
				SourceKind: "human", Text: "你好",
			})
			if err != nil {
				t.Fatal(err)
			}
			task, ok, err := d.AIPopTask(ctx)
			if err != nil || !ok {
				t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
			}

			c.setup(t, ctx, d)

			r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
			r.providerTransport = srv.Client().Transport
			r.handle(ctx, task)

			var status, errText string
			if err := d.SQL.QueryRowContext(ctx,
				`SELECT status, err FROM ai_result WHERE task_id=?`, taskID).Scan(&status, &errText); err != nil {
				t.Fatalf("读取结果失败: %v", err)
			}
			if status != c.want.status {
				t.Errorf("任务状态 = %q，期望 %q（err=%q）", status, c.want.status, errText)
			}
			if c.want.errMatch != "" && !strings.Contains(errText, c.want.errMatch) {
				t.Errorf("错误文本 = %q，期望包含 %q", errText, c.want.errMatch)
			}
			if got := atomic.LoadInt32(&hits); got != c.want.hits {
				t.Errorf("模拟厂商被请求 %d 次，期望 %d 次", got, c.want.hits)
			}
			if strings.Contains(errText, "sk-synthetic-test") {
				t.Errorf("错误文本泄露密钥: %q", errText)
			}
		})
	}
}

// TestQueuedBeforeResumeIsDropped 覆盖「先入队后停、尚未被取走便恢复」：
// 任务创建时间不晚于最近一次解除急停时点，恢复后直接落终态、零出站。
func TestQueuedBeforeResumeIsDropped(t *testing.T) {
	ctx := context.Background()
	d, err := store.Open(filepath.Join(t.TempDir(), "queued.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}

	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"正常"}}]}`))
	}))
	defer srv.Close()
	masterKey := []byte("0123456789abcdef0123456789abcdef")
	if _, err := d.SQL.ExecContext(ctx, `UPDATE ai_provider SET base_url=? WHERE name='deepseek'`, srv.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SQL.ExecContext(ctx, `UPDATE ai_model SET model_id='local-test'
		WHERE provider_id=(SELECT id FROM ai_provider WHERE name='deepseek')`); err != nil {
		t.Fatal(err)
	}

	taskID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1, Agent: "fishguide",
		ContactUID: 2147483644, SourceKind: "human", Text: "停前的问题",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 停 → 恢复：解除时点写入 ai_setting。
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	if d.AISetting(ctx, aiconfig.KeyEmergencyStopLiftedTS, "") == "" {
		t.Fatal("解除急停没有记录时点")
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = srv.Client().Transport
	r.handle(ctx, task)

	var status, errText string
	if err := d.SQL.QueryRowContext(ctx, `SELECT status, err FROM ai_result WHERE task_id=?`, taskID).
		Scan(&status, &errText); err != nil {
		t.Fatal(err)
	}
	if status != "fallback" || !strings.Contains(errText, "queued_before_resume") {
		t.Fatalf("停前任务得到 (%q,%q)，期望 fallback+queued_before_resume", status, errText)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("停前任务请求厂商 %d 次，期望 0", got)
	}
}

// blockingVendor 在收到第一个 Chat 请求后暂停，直到 release 关闭才返回。
// 用于模拟「停前请求已发出、恢复后才返回」的迟到响应。
func blockingVendor(t *testing.T, reply string) (*httptest.Server, <-chan struct{}, chan<- struct{}, *int32) {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var hits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		once.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + reply + `"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, started, release, &hits
}

func aiResultOf(t *testing.T, d *store.DB, taskID int64) (status, reply, errText string) {
	t.Helper()
	if err := d.SQL.QueryRow(
		`SELECT status, reply_text, err FROM ai_result WHERE task_id=?`, taskID).
		Scan(&status, &reply, &errText); err != nil {
		t.Fatalf("读结果 task=%d: %v", taskID, err)
	}
	return status, reply, errText
}

func runTaskAsync(t *testing.T, r *Runner, task *store.AITask) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.handle(context.Background(), task)
	}()
	return done
}

// TestRunnerDropsLateReplyAfterResume 停前已发出的请求，在急停解除后才返回：
// Runner 必须按原任务跨停状态拒收，写成不可投递的终态（fallback/空正文）。
func TestRunnerDropsLateReplyAfterResume(t *testing.T) {
	ctx := context.Background()
	vendor, started, release, hits := blockingVendor(t, "late-reply")
	d, masterKey := newGateTestDB(t, vendor.URL)
	taskID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
		Agent: "fishguide", ContactUID: 2147483644, SourceKind: "human", Text: "停前的问题",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	done := runTaskAsync(t, r, task)
	<-started
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // 解除时点之后的一秒再释放响应
	close(release)
	<-done
	status, reply, errText := aiResultOf(t, d, taskID)
	if status != "fallback" || reply != "" {
		t.Fatalf("迟到回复被写成可投递结果: status=%q reply=%q err=%q", status, reply, errText)
	}
	if errText == "" {
		t.Fatal("迟到回复应记录拒收原因")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("厂商请求次数 = %d，期望 1", got)
	}
}

// TestRunnerDropsReplyWhileStopped 回复返回时急停仍在保持：同样不得写成可投递结果。
func TestRunnerDropsReplyWhileStopped(t *testing.T) {
	ctx := context.Background()
	vendor, started, release, _ := blockingVendor(t, "stopped-reply")
	d, masterKey := newGateTestDB(t, vendor.URL)
	taskID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
		Agent: "fishguide", ContactUID: 2147483644, SourceKind: "human", Text: "在吗",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	done := runTaskAsync(t, r, task)
	<-started
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	status, reply, _ := aiResultOf(t, d, taskID)
	if status != "fallback" || reply != "" {
		t.Fatalf("急停期间的回复被写成可投递结果: status=%q reply=%q", status, reply)
	}
}

// TestRunnerFreshIntentAfterResume 恢复后新意图正常执行：新任务（创建晚于解除时点）
// 正常发送并写成可投递结果。
func TestRunnerFreshIntentAfterResume(t *testing.T) {
	ctx := context.Background()
	var hits int32
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"fresh-reply"}}]}`))
	}))
	defer vendor.Close()
	d, masterKey := newGateTestDB(t, vendor.URL)
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // 保证新任务创建时间晚于解除时点
	taskID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
		Agent: "fishguide", ContactUID: 2147483644, SourceKind: "human", Text: "恢复后的问题",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	r.handle(ctx, task)
	status, reply, errText := aiResultOf(t, d, taskID)
	if status != "done" || !strings.Contains(reply, "fresh-reply") {
		t.Fatalf("恢复后新意图得到 (%q,%q,%q)，期望 done+fresh-reply", status, reply, errText)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("厂商请求次数 = %d，期望 1", got)
	}
}

// TestRunnerRoundtableDropsLateTurn 圆桌首轮请求在停前发出、恢复后返回：
// 不得写入频道结果，也不得继续后续轮次；任务落既有终态。
func TestRunnerRoundtableDropsLateTurn(t *testing.T) {
	ctx := context.Background()
	vendor, started, release, hits := blockingVendor(t, "迟到的一句话")
	d, masterKey := newGateTestDB(t, vendor.URL)
	for _, key := range []string{"companion", "fishguide"} {
		if err := d.AISetAgentJoined(ctx, key, true, 0); err != nil {
			t.Fatal(err)
		}
		// T-108-DoD-v3 §9.2 E2：频道事件在 BR 出站前要按任务频道复核当前范围，
		// 候选必须确实订阅了这个频道（范围 = 精确 ∪ 地图 ∪ 全局）。
		if err := d.AISetChannelAgent(ctx, "t102_rt", key, true); err != nil {
			t.Fatal(err)
		}
		// T-108：系统圆桌的参与资格来自 T-104 的能力集合；缺配置不参与事件。
		if _, err := d.AISaveCapabilities(ctx, key, 0,
			[]string{aiconfig.CapabilityJoinRoundtable}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// T-108：系统圆桌的参与资格来自任务绑定（类别 + 逐角色能力修订），
	// 与运行入口写入的是同一份载荷。
	ev := &store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventRoundtable,
		Participants: []store.AITaskEvidenceParticipant{
			{Key: "companion", Capability: aiconfig.CapabilityJoinRoundtable},
			{Key: "fishguide", Capability: aiconfig.CapabilityJoinRoundtable},
		},
	}
	taskID, err := d.AIEnqueueSystemRoundtable(ctx, "t102_rt", 1, "大家好", []string{"companion", "fishguide"}, ev)
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	done := runTaskAsync(t, r, task)
	<-started
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	var deliverable int
	if err := d.SQL.QueryRow(`
		SELECT COUNT(*) FROM ai_result WHERE task_id=? AND reply_text<>''`, taskID).
		Scan(&deliverable); err != nil {
		t.Fatal(err)
	}
	if deliverable != 0 {
		t.Fatalf("圆桌迟到的发言被写成可投递结果 %d 条", deliverable)
	}
	var rowCount int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_result WHERE task_id=?`, taskID).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount > 0 {
		status, reply, _ := aiResultOf(t, d, taskID)
		if status != "fallback" || reply != "" {
			t.Fatalf("圆桌任务结果 = (%q,%q)，期望空正文/fallback", status, reply)
		}
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("恢复后仍继续后续轮次：请求次数 = %d，期望 1", got)
	}
}

// TestSupervisorT102ToolContinuationAcrossStop 复评矩阵「工具续轮跨 1→0」：
// 首发请求已抵达本地 TLS 假厂商，厂商在返回合成工具请求前通过真实设置路径
// 连续保存 1、0。旧意图不得再发第二轮、不得执行工具（零作业、零副作用），
// 也不得写出可投递结果；厂商总请求数恰为 1。
func TestSupervisorT102ToolContinuationAcrossStop(t *testing.T) {
	ctx := context.Background()
	var hits int32
	var control func() error
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			if control != nil {
				if err := control(); err != nil {
					t.Errorf("厂商侧保存急停 1→0: %v", err)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"fish_query","arguments":"{\"fish\":\"鲈鱼\"}"}}]}}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"不应到达"}}]}`)
	}))
	defer vendor.Close()

	d, masterKey := newGateTestDB(t, vendor.URL)
	control = func() error {
		if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
			return err
		}
		return d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"})
	}

	taskID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
		Agent: "fishguide", ContactUID: 2147483644,
		SourceKind: "human", Text: "鲈鱼在哪钓",
	})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	r.handle(ctx, task)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("停前旧任务在 1→0 后仍续轮：厂商请求 %d 次，期望 1", got)
	}
	var jobs int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_tool_job WHERE task_id=?`, taskID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("停前旧任务在 1→0 后仍排了 %d 个工具作业", jobs)
	}
	status, reply, errText := aiResultOf(t, d, taskID)
	if status != "fallback" || reply != "" || errText == "" {
		t.Fatalf("旧任务结果 = (%q,%q,%q)，期望 fallback/空正文/带原因", status, reply, errText)
	}
}

// TestT102ProviderGateUsesTaskIntent 每次真正发送前（首发/重试/工具续轮）
// 都按原任务时点判定：同一份快照里完成 1→0 后，停前意图零出站，新意图放行。
func TestT102ProviderGateUsesTaskIntent(t *testing.T) {
	ctx := context.Background()
	var hits int32
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer vendor.Close()
	d, masterKey := newGateTestDB(t, vendor.URL)
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	epoch := d.AIEmergencyState(ctx).Epoch
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	p, err := r.loadProvider(store.AIIntentContext(ctx, epoch-2))
	if err != nil {
		t.Fatalf("loadProvider: %v", err)
	}
	req := ChatRequest{Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 10}
	oldCtx := withAITaskIntent(ctx, &store.AITask{ID: 1, IntentEpoch: epoch - 2})
	if _, err := p.Chat(oldCtx, req); err == nil {
		t.Fatal("停前意图在 1→0 后仍被放行")
	} else {
		var denied *EgressDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("期望 EgressDeniedError，得到 %v", err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("停前意图出站 %d 次，期望 0", got)
	}
	freshCtx := withAITaskIntent(ctx, &store.AITask{ID: 2, IntentEpoch: epoch})
	freshProvider, err := r.loadProvider(freshCtx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := freshProvider.Chat(freshCtx, req); err != nil {
		t.Fatalf("解除后新意图被误拦: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("新意图出站 %d 次，期望 1", got)
	}
}

// TestT102SearchGateUsesTaskIntent 任务触发的联网搜索与模型共用同一判定：
// 停前任务在 1→0 后零搜索请求，新任务可正常搜索。
func TestT102SearchGateUsesTaskIntent(t *testing.T) {
	ctx := context.Background()
	var hits int32
	search := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"title":"t","snippet":"s"}]}`)
	}))
	defer search.Close()
	d, _ := newGateTestDB(t, search.URL)
	if err := d.AISetSetting(ctx, "web_search_enabled", "1"); err != nil {
		t.Fatal(err)
	}
	k := &Knowledge{}
	NewRunner(d, k, config.AI{}, nil, logging.Discard())
	k.client = newEgressHTTPClient(time.Second, search.Client().Transport)
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	epoch := d.AIEmergencyState(ctx).Epoch
	oldCtx := withAITaskIntent(ctx, &store.AITask{ID: 1, IntentEpoch: epoch - 2})
	if got := k.WebSearch(oldCtx, "鲈鱼 123456789", search.URL); got != "" {
		t.Fatalf("停前任务的搜索被放行: %q", got)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("停前任务搜索请求 %d 次，期望 0", got)
	}
	freshCtx := withAITaskIntent(ctx, &store.AITask{ID: 2, IntentEpoch: epoch})
	if got := k.WebSearch(freshCtx, "鲈鱼 123456789", search.URL); got == "" {
		t.Fatal("新任务的搜索被误拦")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("新任务搜索请求 %d 次，期望 1", got)
	}
}

// TestT102RunnerRestartDropsOldPending 新进程（新 Runner 实例）只按持久状态
// 处理旧 pending：停前任务不重放，解除后的新意图正常执行。
func TestT102RunnerRestartDropsOldPending(t *testing.T) {
	ctx := context.Background()
	var hits int32
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"fresh"}}]}`)
	}))
	defer vendor.Close()
	d, masterKey := newGateTestDB(t, vendor.URL)
	oldTaskID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
		Agent: "fishguide", ContactUID: 2147483644, SourceKind: "human", Text: "停前",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}

	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	r.handle(ctx, task) // 新进程读取旧 pending
	if status, reply, errText := aiResultOf(t, d, oldTaskID); status != "fallback" || reply != "" ||
		!strings.Contains(errText, "queued_before_resume") {
		t.Fatalf("重启后旧任务 = (%q,%q,%q)，期望 fallback/空正文/queued_before_resume", status, reply, errText)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("重启后旧任务出站 %d 次，期望 0", got)
	}

	newTaskID, err := d.AIEnqueue(ctx, &store.AITask{
		Kind: "dm", Scope: "dm", ScopeKey: "u1", UID: 1,
		Agent: "fishguide", ContactUID: 2147483644, SourceKind: "human", Text: "解除后",
	})
	if err != nil {
		t.Fatal(err)
	}
	newTask, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取新任务失败: ok=%v err=%v", ok, err)
	}
	r2 := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r2.providerTransport = vendor.Client().Transport
	r2.handle(ctx, newTask)
	if status, reply, _ := aiResultOf(t, d, newTaskID); status != "done" || !strings.Contains(reply, "fresh") {
		t.Fatalf("解除后新意图 = (%q,%q)，期望 done/fresh", status, reply)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("新意图出站 %d 次，期望 1", got)
	}
}

func TestT102RetryReusesOriginalProviderBinding(t *testing.T) {
	ctx := t.Context()
	var hits atomic.Int32
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer vendor.Close()
	db, master := newGateTestDB(t, vendor.URL)
	epoch := db.AIEmergencyState(ctx).Epoch
	bound := store.AIIntentContext(ctx, epoch)
	r := NewRunner(db, nil, config.AI{Enabled: true, RequestTimeoutSec: 3}, master, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	provider, err := r.loadProvider(bound)
	if err != nil {
		t.Fatal(err)
	}
	req := ChatRequest{Messages: []Message{{Role: "user", Content: "synthetic"}}, MaxTokens: 10}
	if _, err := provider.Chat(bound, req); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("first response should be retryable status 503: %v", err)
	}
	for _, stop := range []string{"1", "0"} {
		if err := db.AISetSetting(ctx, aiconfig.KeyEmergencyStop, stop); err != nil {
			t.Fatal(err)
		}
	}
	var denied *EgressDeniedError
	if _, err := provider.Chat(bound, req); !errors.As(err, &denied) || hits.Load() != 1 {
		t.Fatalf("retry: err=%v, vendor count=%d, want deny/1", err, hits.Load())
	}
	t.Logf("original epoch %d, current %d, 503 received once; retry released zero requests", epoch, db.AIEmergencyState(ctx).Epoch)
}
