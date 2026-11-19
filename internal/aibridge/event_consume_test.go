package aibridge

// T-108：aibridge 侧的事件消费复核。
//
// 系统事件跨队列只带 source_kind 与 kind（类别与能力修订没有载体，见回执 DR-121），
// 所以这里能证明两件事：
//   1. 系统单频道事件要求角色此刻仍持有至少一个事件能力；
//   2. 系统圆桌按参与者逐个复核「参加圆桌」，撤权者被剔出，不足两人则落终态。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// TestRunnerRejectsSystemChannelWithoutEventCapability 系统频道事件在撤掉原事件能力后
// 不写任何可投递结果：认领前的复核按任务绑定（类别 + 修订）一比就终止。
func TestRunnerRejectsSystemChannelWithoutEventCapability(t *testing.T) {
	ctx := context.Background()
	d := openBridgeRuntimeDB(t)
	if err := d.AISetAgentJoined(ctx, "companion", true, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityReplyStar}, nil); err != nil {
		t.Fatal(err)
	}
	ev := &store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventReplyStar,
		Participants: []store.AITaskEvidenceParticipant{
			{Key: "companion", Capability: aiconfig.CapabilityReplyStar},
		},
	}
	taskID, err := d.AIEnqueueSystemChannel(ctx, "t108_consume", 1, "系统事件", "companion", 2147483645, ev)
	if err != nil {
		t.Fatal(err)
	}
	// 入队后撤掉该事件能力：旧意图不得再调用模型，也不得写可投递结果。
	if _, err := d.AISaveCapabilities(ctx, "companion", 1,
		[]string{aiconfig.CapabilityGuideHelp}, nil); err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, nil, logging.Discard())
	r.handle(ctx, task)

	status, reply, errText := aiResultOf(t, d, taskID)
	if status != "fallback" || reply != "" {
		t.Fatalf("撤权后的系统事件应落不可投递终态，实得 (%q,%q)", status, reply)
	}
	if errText != store.AITaskEvidenceReasonCapability {
		t.Fatalf("终态原因 = %q，期望 %q", errText, store.AITaskEvidenceReasonCapability)
	}
}

// TestRunnerRoundtableDropsRevokedParticipants 系统圆桌按参与者逐个复核，
// 撤权后不足两人即落终态，不会调用模型。
func TestRunnerRoundtableDropsRevokedParticipants(t *testing.T) {
	ctx := context.Background()
	var hits int32
	vendor := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"不该被叫到"}}]}`))
	}))
	defer vendor.Close()

	d, masterKey := newGateTestDB(t, vendor.URL)
	for _, key := range []string{"companion", "fishguide"} {
		if err := d.AISetAgentJoined(ctx, key, true, 0); err != nil {
			t.Fatal(err)
		}
	}
	// 两人先按同一份圆桌资格入队，再只撤掉 fishguide：旧任务在认领后的
	// 复核里必须按原修订识别撤权，不足两人落终态。
	for _, key := range []string{"companion", "fishguide"} {
		if _, err := d.AISaveCapabilities(ctx, key, 0,
			[]string{aiconfig.CapabilityJoinRoundtable}, nil); err != nil {
			t.Fatal(err)
		}
	}
	ev := &store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventRoundtable,
		Participants: []store.AITaskEvidenceParticipant{
			{Key: "companion", Capability: aiconfig.CapabilityJoinRoundtable},
			{Key: "fishguide", Capability: aiconfig.CapabilityJoinRoundtable},
		},
	}
	taskID, err := d.AIEnqueueSystemRoundtable(ctx, "t108_rt", 1, "大家好",
		[]string{"companion", "fishguide"}, ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AISaveCapabilities(ctx, "fishguide", 1, []string{}, nil); err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok {
		t.Fatalf("取任务失败: ok=%v err=%v", ok, err)
	}
	r := NewRunner(d, nil, config.AI{Enabled: true, RequestTimeoutSec: 5}, masterKey, logging.Discard())
	r.providerTransport = vendor.Client().Transport
	r.handle(ctx, task)

	var state string
	if err := d.SQL.QueryRow(`SELECT state FROM ai_task WHERE id=?`, taskID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "fallback" {
		t.Fatalf("撤权后参与者不足两人应落 fallback，实得 %q", state)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("撤权后不应再调用模型，实得 %d 次", got)
	}
}

// TestActivityFactSnapshotIsHonest 只读快照必须写出来源、采样时间、陈旧标志、
// 实际投影行与「无参赛身份」：不许把本地投影说成官方已结束或正在比赛。
//
// T-108 §9.3 / E5：competition 只经 store.AIActivityFacts 读；输出保留
// source/sampled_at/stale/registered=false/capability=unavailable/资格=unknown，
// 并带至少一条实际 id/状态/开始与预计结束；event_snapshot 固定
// unknown(source_unavailable) 并给负证据。本用例同时覆盖三条对照：
// 报名中、进行中（已过本地预计结束）、无赛事。
func TestActivityFactSnapshotIsHonest(t *testing.T) {
	ctx := context.Background()
	d := openBridgeRuntimeDB(t)
	// 进行中且已过本地预计结束（stale）的一条。
	if _, err := d.SQL.Exec(`INSERT INTO competition(id, name, state, owner, map_key,
		duration_sec, started_ts, created_ts) VALUES('c1','测试赛',2,1,'level_001_lake',
		60, ?, ?)`, float64(1), float64(1)); err != nil {
		t.Fatalf("插入赛事: %v", err)
	}
	r := &Runner{db: d, log: bridgeTestLogger()}
	line := r.activityFactSnapshot(ctx, "companion")
	for _, want := range []string{
		"source=official-db(本地投影)", "sampled_at=", "stale=true",
		"registered=false", "capability=unavailable", "资格=unknown",
		"event_snapshot=unknown(source_unavailable)",
		"不调用懒结算", "不注册事件", "不写官方表",
		"id=c1",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("快照缺 %q：%s", want, line)
		}
	}
	// 实际行必须带状态与时刻（开始 + 预计结束），且不含玩家数据字段。
	for _, want := range []string{"状态=", "开始=", "预计结束="} {
		if !strings.Contains(line, want) {
			t.Fatalf("快照缺实际投影字段 %q：%s", want, line)
		}
	}
	// 过期的本地投影只能说 unknown，不能宣称官方已结束（英文 finished 也不许出现）。
	if strings.Contains(line, "finished") {
		t.Fatalf("快照出现 finished 字样：%s", line)
	}
	if !strings.Contains(line, "unknown") {
		t.Fatalf("过期投影的阶段应为 unknown：%s", line)
	}

	// 报名中不计入「进行中」。
	d2 := openBridgeRuntimeDB(t)
	if _, err := d2.SQL.Exec(`INSERT INTO competition(id, name, state, owner, map_key,
		duration_sec, started_ts, created_ts) VALUES('c2','报名赛',1,1,'level_001_lake',
		3600, ?, ?)`, float64(time.Now().Unix()), float64(time.Now().Unix())); err != nil {
		t.Fatalf("插入报名赛事: %v", err)
	}
	r2 := &Runner{db: d2, log: bridgeTestLogger()}
	signup := r2.activityFactSnapshot(ctx, "companion")
	if !strings.Contains(signup, "本地赛事 1 条，进行中 0") {
		t.Fatalf("报名中不应计入进行中：%s", signup)
	}
	if !strings.Contains(signup, "id=c2") {
		t.Fatalf("报名赛应出现在实际投影行里：%s", signup)
	}

	// 无赛事：仍然是完整形状（来源/采样/资格/unknown），不编造。
	d3 := openBridgeRuntimeDB(t)
	r3 := &Runner{db: d3, log: bridgeTestLogger()}
	empty := r3.activityFactSnapshot(ctx, "")
	for _, want := range []string{
		"source=official-db(本地投影)", "sampled_at=",
		"本地赛事 0 条", "registered=false",
		"event_snapshot=unknown(source_unavailable)", "unknown",
	} {
		if !strings.Contains(empty, want) {
			t.Fatalf("无赛事快照缺 %q：%s", want, empty)
		}
	}
	t.Logf("E5 快照形状：stale=%v；报名中不计进行中；无赛事仍给完整 unknown",
		strings.Contains(line, "stale=true"))
}
