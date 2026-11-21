package gameserver

// T-020 / DEV-004：GS 单写者命令消费者测试。
//
// 用真实迁移 SQLite（必要时只读挂载真实静态库）跑业务应用规则：
// 禁用/删除与 tick 竞争、旧建议晚到、重复 reroll 不再随机、
// 上鱼记忆与渔获同事务、公聊命令立即应用且只改一次。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

const runtimeGSAgent = "companion"

func runtimeGSServer(t *testing.T, db *store.DB) *Server {
	t.Helper()
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "runtime-commands"}}, logging.Discard(), nil)
	srv.SetDB(db)
	return srv
}

func runtimeGSEnqueue(t *testing.T, db *store.DB, req store.AIRuntimeEnqueueRequest) *store.AIRuntimeCommand {
	if req.Source == store.AIRuntimeSourceBridge && req.ParentTaskID == 0 {
		// Existing role generation tests use stable synthetic request IDs.
		var id int64
		if _, err := fmt.Sscanf(req.RequestID, "bridge:%d:", &id); err != nil {
			t.Fatal(err)
		}
		req.ParentTaskID = id
		if _, err := db.SQL.Exec(`INSERT INTO ai_task(id, uid, intent_epoch, created_ts, state)
			VALUES(?, 0, ?, 0, 'done') ON CONFLICT(id) DO NOTHING`, id, db.AIEmergencyState(t.Context()).Epoch); err != nil {
			t.Fatal(err)
		}
	}
	t.Helper()
	if req.RequestID == "" {
		t.Fatalf("测试必须显式给 request_id")
	}
	cmd, _, err := db.AIEnqueueRuntimeCommand(context.Background(), req)
	if err != nil {
		t.Fatalf("入队 %s: %v", req.Kind, err)
	}
	return cmd
}

func runtimeGSApply(t *testing.T, srv *Server, id int64) *store.AIRuntimeCommand {
	t.Helper()
	cmd, err := srv.DB.AIApplyRuntimeCommand(context.Background(), id, 0, srv.applyAIRuntimeCommand)
	if err != nil {
		t.Fatalf("应用命令 %d: %v", id, err)
	}
	return cmd
}

func runtimeGSActorPayload(t *testing.T, enabled bool) []byte {
	t.Helper()
	if enabled {
		return []byte(`{"enabled":true}`)
	}
	return []byte(`{"enabled":false}`)
}

// grantCapabilities 直接在真实库里保存一次能力集合，返回修订号。
func grantCapabilities(t *testing.T, db *store.DB, agentKey string, ids ...string) int64 {
	t.Helper()
	st, err := db.AISaveCapabilities(context.Background(), agentKey, 0, ids, nil)
	if err != nil {
		t.Fatalf("保存能力: %v", err)
	}
	return st.Revision
}

// targetPayload 是 T-104 写命令的规范载荷：能力与修订随命令携带，GS 在事务内复核。
func targetPayload(fish, loc string, rev int64) []byte {
	return []byte(fmt.Sprintf(`{"fish_key":%q,"loc_key":%q,"capability":%q,"cap_revision":%d}`,
		fish, loc, aiconfig.CapabilityAcceptTarget, rev))
}

func grantActorConsent(t *testing.T, db *store.DB) {
	t.Helper()
	rec, ok := aiconfig.NewConsentRecord(aiconfig.ConsentItemActor, "runtime-test", time.Now().Unix(), aiconfig.ConsentDestination{})
	if !ok {
		t.Fatal("构造 actor consent 失败")
	}
	raw, err := aiconfig.MarshalConsent(rec)
	if err != nil {
		t.Fatalf("序列化 consent: %v", err)
	}
	if err := db.AISetSettings(context.Background(), map[string]string{aiconfig.KeyConsentActor: raw}); err != nil {
		t.Fatalf("写 consent: %v", err)
	}
}

func runtimeGSSeedActor(t *testing.T, db *store.DB, agentKey, loc, activity string) {
	t.Helper()
	// 邀请去重是进程内状态：每个夹具从零开始，避免用例之间互相污染。
	activityInvites.reset()
	now := float64(time.Now().Unix())
	// T-108 硬驻留：一段活动至少 Bhard。夹具把这一段已经钓了一段时间，
	// 这样「换目标」用例测的是第二闸，而不是被第一闸挡在门外。
	since := now - 1300
	if _, err := db.SQL.Exec(`
		INSERT INTO ai_actor_state(agent_key, loc, loc_name, activity, target_fish, target_name,
		                           since_ts, loc_since_ts, switch_ts, activity_until_ts, energy, updated_ts)
		VALUES(?, ?, '测试湖', ?, 'perch', '鲈鱼', ?, ?, 0, ?, 55, ?)
		ON CONFLICT(agent_key) DO UPDATE SET loc=excluded.loc, activity=excluded.activity,
			since_ts=excluded.since_ts, activity_until_ts=excluded.activity_until_ts,
			switch_ts=excluded.switch_ts`,
		agentKey, loc, activity, since, since, now+3600, now); err != nil {
		t.Fatalf("seed 角色状态: %v", err)
	}
	if _, err := db.SQL.Exec(`UPDATE ai_agent SET actor_enabled=1 WHERE key=?`, agentKey); err != nil {
		t.Fatalf("打开角色开关: %v", err)
	}
	// T-108：换目标的接受度来自 T-103 的四维。测试夹具给一个「乐于交往 + 愿意尝新」
	// 的性格，让仲裁的第二闸可被满足；个性本身由 ai_activity_policy_test.go 定向验证。
	if _, err := db.AISavePersonality(context.Background(), agentKey, 0,
		aiconfig.PersonalityConfig{
			SchemaVersion: aiconfig.PersonalitySchemaVersion,
			Sociability:   100, Exploration: 100,
		}); err != nil {
		t.Fatalf("seed 性格: %v", err)
	}
}

func runtimeGSRoster(t *testing.T, db *store.DB) (loc, fish string) {
	t.Helper()
	if err := db.SQL.QueryRow(`
		SELECT loc, fish_key FROM map_fish_roster
		WHERE loc<>'' AND fish_key<>'' LIMIT 1`).Scan(&loc, &fish); err != nil {
		t.Fatalf("读静态 roster: %v", err)
	}
	return loc, fish
}

func runtimeGSActorRow(t *testing.T, db *store.DB, agentKey string) *store.AIActor {
	t.Helper()
	a, err := db.AIGetActor(context.Background(), agentKey)
	if err != nil {
		t.Fatalf("读角色状态: %v", err)
	}
	return a
}

func runtimeGSAgentFlag(t *testing.T, db *store.DB, agentKey string) (actorEnabled bool, deleted bool) {
	t.Helper()
	var actor int
	var deletedTS float64
	if err := db.SQL.QueryRow(`SELECT actor_enabled, deleted_ts FROM ai_agent WHERE key=?`, agentKey).
		Scan(&actor, &deletedTS); err != nil {
		t.Fatalf("读 Agent 开关: %v", err)
	}
	return actor != 0, deletedTS != 0
}

// TestAIRuntimeCommandChannelImmediateApply：公聊命令入队后立即在同一事务应用，
// 回执只发一次、官方广播仍在、命令行 applied。
func TestAIRuntimeCommandChannelImmediateApply(t *testing.T) {
	p := newT013ChatProbe(t, "runtime_channel")
	t013JoinAgent(t, p.db, "companion")
	msg := "@邀请 AI 伙伴"
	p.send(t, msg)
	t013AssertFeedback(t, p.next(t), "拉进")
	t013AssertBroadcast(t, p.next(t), msg)
	p.expectNoMore(t)
	if n := t013ChannelSubs(t, p.db, "runtime_channel", "companion"); n != 1 {
		t.Fatalf("频道订阅 = %d，期望 1", n)
	}
	var status, requestID, source, kind string
	if err := p.db.SQL.QueryRow(`
		SELECT status, request_id, source, kind FROM ai_runtime_command ORDER BY id DESC LIMIT 1`).
		Scan(&status, &requestID, &source, &kind); err != nil {
		t.Fatalf("读命令行: %v", err)
	}
	if status != store.AIRuntimeStatusApplied || !strings.HasPrefix(requestID, "gs:") ||
		source != store.AIRuntimeSourceGameServer || kind != store.AIRuntimeKindChannelSetMember {
		t.Fatalf("命令 = status:%s request:%s source:%s kind:%s", status, requestID, source, kind)
	}
}

// TestAIRuntimeCommandChannelReplaySameRequest：同一次客户端请求重放只改一次成员。
func TestAIRuntimeCommandChannelReplaySameRequest(t *testing.T) {
	p := newT013ChatProbe(t, "runtime_replay")
	t013JoinAgent(t, p.db, "companion")
	msg := "@邀请 AI 伙伴"
	for i := 0; i < 2; i++ {
		p.send(t, msg)
		t013AssertFeedback(t, p.next(t), "拉进")
		t013AssertBroadcast(t, p.next(t), msg)
	}
	if n := t013ChannelSubs(t, p.db, "runtime_replay", "companion"); n != 1 {
		t.Fatalf("重放后频道订阅 = %d，期望 1", n)
	}
	var rows int
	if err := p.db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("重放产生了 %d 行命令，期望 1", rows)
	}
}

// TestAIRuntimeCommandActorEnableOnlyEmergency V2 只留急停：
// 旧 actor consent 缺失不再阻止启用；急停期间的命令在写事务内拒绝并留终态；
// 停前排队、恢复后也不重放。
func TestAIRuntimeCommandActorEnableOnlyEmergency(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()

	// 1) 没有旧 consent：仍然允许启用。
	cmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "ad:enable-no-consent", Source: store.AIRuntimeSourceAdmin,
		Kind: store.AIRuntimeKindActorSetEnabled, AgentKey: runtimeGSAgent,
		Payload: runtimeGSActorPayload(t, true),
	})
	got := runtimeGSApply(t, srv, cmd.ID)
	if got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("无旧 consent 启用 = %s/%s，期望 applied", got.Status, got.Reason)
	}
	if enabled, _ := runtimeGSAgentFlag(t, db, runtimeGSAgent); !enabled {
		t.Fatal("启用后 actor_enabled 应为 1")
	}
	// 启用不重置代次，只推进版本。
	ctl, err := db.AIGetRuntimeControl(ctx, runtimeGSAgent)
	if err != nil {
		t.Fatal(err)
	}
	if ctl.Generation != 1 || ctl.Revision < 1 {
		t.Fatalf("启用后 control = gen%d rev%d", ctl.Generation, ctl.Revision)
	}

	// 3) 停前入队、恢复后执行：在事务内按代次拒绝。
	queued := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "ad:queued-before-resume", Source: store.AIRuntimeSourceAdmin,
		Kind: store.AIRuntimeKindActorSetEnabled, AgentKey: runtimeGSAgent,
		Payload: runtimeGSActorPayload(t, false),
	})
	// 2) 急停期间的命令：写事务内拒绝并留终态，不改运行态。
	if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
		t.Fatal(err)
	}
	stopped := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "ad:disable-while-stopped", Source: store.AIRuntimeSourceAdmin,
		Kind: store.AIRuntimeKindActorSetEnabled, AgentKey: runtimeGSAgent,
		Payload: runtimeGSActorPayload(t, false),
	})
	got = runtimeGSApply(t, srv, stopped.ID)
	if got.Status != store.AIRuntimeStatusRejected || got.Reason != "emergency_stop" {
		t.Fatalf("急停期间命令 = %s/%s，期望 rejected/emergency_stop", got.Status, got.Reason)
	}
	if enabled, _ := runtimeGSAgentFlag(t, db, runtimeGSAgent); !enabled {
		t.Fatal("被拒绝的禁用命令不得改动 actor_enabled")
	}

	if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "0"}); err != nil {
		t.Fatal(err)
	}
	got = runtimeGSApply(t, srv, queued.ID)
	if got.Status != store.AIRuntimeStatusRejected || got.Reason != "queued_before_resume" {
		t.Fatalf("停前排队命令 = %s/%s，期望 rejected/queued_before_resume", got.Status, got.Reason)
	}
	if enabled, _ := runtimeGSAgentFlag(t, db, runtimeGSAgent); !enabled {
		t.Fatal("停前排队的命令不得在恢复后生效")
	}
}

// TestT102MatrixBridgeCommandUsesParentIntent 复评矩阵「旧命令消费前」：
// BR 派生命令的创建时间可能是解除之后，但原意图属于停前任务——必须按
// request_id 里的父任务时点判定；独立管理命令沿用自身创建时点。
// 来源无法解析、父任务缺失、等时与解除记录损坏都保守拒绝。
func TestT102MatrixBridgeCommandUsesParentIntent(t *testing.T) {
	db := openMigratedTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	old, err := db.AIEnqueue(ctx, &store.AITask{Kind: "dm", SourceKind: "human"})
	if err != nil {
		t.Fatal(err)
	}
	oldEpoch := db.AIEmergencyState(ctx).Epoch
	suggest := func(task int64) *store.AIRuntimeCommand {
		return runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
			RequestID: fmt.Sprintf("bridge:%d:%s:1", task, runtimeGSAgent), Source: store.AIRuntimeSourceBridge,
			Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
			ParentTaskID: task, Payload: []byte(`{"fish_key":"x","loc_key":"y"}`),
		})
	}
	queued := suggest(old)
	for _, stop := range []string{"1", "0"} {
		if err := db.AISetSetting(ctx, aiconfig.KeyEmergencyStop, stop); err != nil {
			t.Fatal(err)
		}
	}
	if got := runtimeGSApply(t, srv, queued.ID); got.Status != store.AIRuntimeStatusRejected || got.Reason != aiconfig.IntentQueuedBeforeResume {
		t.Fatalf("cross-stop command: %+v", got)
	}
	fresh, err := db.AIEnqueue(ctx, &store.AITask{Kind: "dm", SourceKind: "human"})
	if err != nil {
		t.Fatal(err)
	}
	freshCmd := suggest(fresh)
	if freshCmd.Status != store.AIRuntimeStatusPending || freshCmd.IntentEpoch != oldEpoch+2 {
		t.Fatalf("fresh command did not inherit current parent: %+v", freshCmd)
	}
	if _, err := db.SQL.Exec(`DELETE FROM ai_task WHERE id=?`, fresh); err != nil {
		t.Fatal(err)
	}
	if got := runtimeGSApply(t, srv, freshCmd.ID); got.Status != store.AIRuntimeStatusRejected || got.Reason != aiconfig.IntentOriginUnknown {
		t.Fatalf("missing parent command: %+v", got)
	}
	if _, err := db.SQL.Exec(`UPDATE ai_setting SET value='{"version":2,"epoch":3}' WHERE key='emergency_epoch_record'`); err != nil {
		t.Fatal(err)
	}
	broken := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "ad:broken-epoch", Source: store.AIRuntimeSourceAdmin,
		Kind: store.AIRuntimeKindActorSetEnabled, AgentKey: runtimeGSAgent,
		Payload: runtimeGSActorPayload(t, false),
	})
	if broken.Status != store.AIRuntimeStatusRejected || broken.Reason != aiconfig.FaultEpochInvalid {
		t.Fatalf("invalid epoch command: %+v", broken)
	}
}

// TestAISingleWriterOldSuggestionAfterDisable：旧建议晚到时按代次拒绝，不复活状态。
func TestAISingleWriterOldSuggestionAfterDisable(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	loc, fish := runtimeGSRoster(t, db)
	runtimeGSSeedActor(t, db, runtimeGSAgent, loc, "fishing")
	rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityAcceptTarget)

	// BR 在禁用前读到 gen=1/rev=0，提交建议；随后 AD 的禁用先应用。
	suggestion := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:42:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: targetPayload(fish, loc, rev),
	})
	disable := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "ad:disable-first", Source: store.AIRuntimeSourceAdmin,
		Kind: store.AIRuntimeKindActorSetEnabled, AgentKey: runtimeGSAgent,
		Payload: runtimeGSActorPayload(t, false),
	})
	got := runtimeGSApply(t, srv, disable.ID)
	if got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("禁用 = %s/%s", got.Status, got.Reason)
	}
	ctl, err := db.AIGetRuntimeControl(ctx, runtimeGSAgent)
	if err != nil {
		t.Fatal(err)
	}
	if ctl.Generation != 2 {
		t.Fatalf("禁用后 generation = %d，期望 2", ctl.Generation)
	}
	if a := runtimeGSActorRow(t, db, runtimeGSAgent); a != nil {
		t.Fatalf("禁用后仍有角色状态: %+v", a)
	}

	late := runtimeGSApply(t, srv, suggestion.ID)
	if late.Status != store.AIRuntimeStatusRejected || late.Reason != "stale_generation" {
		t.Fatalf("旧建议 = %s/%s，期望 rejected/stale_generation", late.Status, late.Reason)
	}
	if a := runtimeGSActorRow(t, db, runtimeGSAgent); a != nil {
		t.Fatalf("旧建议复活了角色状态: %+v", a)
	}
	// tick 也不能复活：事务内重验 actor_enabled=0。
	if a := srv.actorTick(ctx, runtimeGSAgent, ""); a != nil {
		t.Fatalf("禁用后 tick 仍返回状态: %+v", a)
	}
}

// TestAISingleWriterDeleteVsTick：删除与 tick 竞争后不留下运行态。
func TestAISingleWriterDeleteVsTick(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	loc, _ := runtimeGSRoster(t, db)
	runtimeGSSeedActor(t, db, runtimeGSAgent, loc, "fishing")

	// 后台任务持续 tick，主线程删除；终态必须是无状态、无 actor 行。
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				srv.actorTick(ctx, runtimeGSAgent, "")
				time.Sleep(time.Millisecond)
			}
		}
	}()
	cmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "ad:delete-vs-tick", Source: store.AIRuntimeSourceAdmin,
		Kind: store.AIRuntimeKindAgentDelete, AgentKey: runtimeGSAgent,
	})
	got := runtimeGSApply(t, srv, cmd.ID)
	close(stop)
	<-done
	if got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("删除 = %s/%s", got.Status, got.Reason)
	}
	if a := runtimeGSActorRow(t, db, runtimeGSAgent); a != nil {
		t.Fatalf("删除后 tick 复活了状态: %+v", a)
	}
	if _, deleted := runtimeGSAgentFlag(t, db, runtimeGSAgent); !deleted {
		t.Fatal("Agent 未被逻辑删除")
	}
	ctl, err := db.AIGetRuntimeControl(ctx, runtimeGSAgent)
	if err != nil {
		t.Fatal(err)
	}
	if !ctl.Tombstoned || ctl.Generation < 2 {
		t.Fatalf("删除后 control = %+v，期望 tombstone 且 generation≥2", ctl)
	}
}

// TestAISingleWriterStaleSuggestionGeneration：Agent 仍启用但代次已推进时，
// 旧代次建议必须被拒绝，不能把旧目标写回状态。
func TestAISingleWriterStaleSuggestionGeneration(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	loc, fish := runtimeGSRoster(t, db)
	runtimeGSSeedActor(t, db, runtimeGSAgent, loc, "fishing")
	rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityAcceptTarget)
	if err := db.AISetSetting(ctx, "ai_actor_switch_chance", "1"); err != nil {
		t.Fatal(err)
	}
	suggestion := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:91:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: targetPayload(fish, loc, rev),
	})
	// 模拟一次禁用/重建后的代次推进：Agent 仍可用，但旧代次已作废。
	if _, err := db.SQL.Exec(`UPDATE ai_runtime_control SET generation=2 WHERE agent_key=?`, runtimeGSAgent); err != nil {
		t.Fatal(err)
	}
	before := runtimeGSActorRow(t, db, runtimeGSAgent)
	got := runtimeGSApply(t, srv, suggestion.ID)
	if got.Status != store.AIRuntimeStatusRejected || got.Reason != "stale_generation" {
		t.Fatalf("旧代次建议 = %s/%s，期望 rejected/stale_generation", got.Status, got.Reason)
	}
	after := runtimeGSActorRow(t, db, runtimeGSAgent)
	if before == nil || after == nil || before.TargetFish != after.TargetFish || before.Loc != after.Loc {
		t.Fatalf("旧代次建议改写了状态: %+v → %+v", before, after)
	}
}

// TestAIRuntimeCommandRerollAppliedOnce：同一次 reroll 只掷一次骰。
func TestAIRuntimeCommandRerollAppliedOnce(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	runtimeGSSeedActor(t, db, runtimeGSAgent, "level_001_lake", "fishing")
	cmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "ad:reroll-once", Source: store.AIRuntimeSourceAdmin,
		Kind: store.AIRuntimeKindActorReroll, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
	})
	got := runtimeGSApply(t, srv, cmd.ID)
	if got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("reroll = %s/%s", got.Status, got.Reason)
	}
	first := runtimeGSActorRow(t, db, runtimeGSAgent)
	if first == nil || first.Activity == "fishing" {
		t.Fatalf("reroll 未换活动: %+v", first)
	}
	// 进程重启后按 id 再应用：已终态，不再掷骰。
	again := runtimeGSApply(t, srv, cmd.ID)
	if again.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("重复应用状态 = %s", again.Status)
	}
	second := runtimeGSActorRow(t, db, runtimeGSAgent)
	if second == nil || second.Activity != first.Activity || second.ActivityUntilTS != first.ActivityUntilTS {
		t.Fatalf("重复 reroll 改变了状态: %+v → %+v", first, second)
	}
	var rows int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command WHERE kind='actor_reroll'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("reroll 命令行 = %d，期望 1", rows)
	}
}

// TestAIRuntimeCommandTargetSuggest：建议在 GS 事务里校验鱼/地图并应用。
func TestAIRuntimeCommandTargetSuggest(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	loc, fish := runtimeGSRoster(t, db)
	if err := db.AISetSetting(ctx, "ai_actor_switch_chance", "1"); err != nil {
		t.Fatal(err)
	}
	runtimeGSSeedActor(t, db, runtimeGSAgent, "level_other_placeholder", "fishing")
	rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityAcceptTarget)

	cmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:77:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: targetPayload(fish, loc, rev),
	})
	got := runtimeGSApply(t, srv, cmd.ID)
	if got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("建议 = %s/%s", got.Status, got.Reason)
	}
	a := runtimeGSActorRow(t, db, runtimeGSAgent)
	if a == nil || a.TargetFish != fish || a.Loc != loc || a.Activity != "fishing" {
		t.Fatalf("建议未应用: %+v", a)
	}
	// 同 request 重试：去重，不产生第二次建议。
	dup, isDup, err := db.AIEnqueueRuntimeCommand(ctx, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:77:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: targetPayload(fish, loc, rev),
	})
	if err != nil || !isDup || dup.ID != cmd.ID {
		t.Fatalf("建议重试未去重: dup=%v id=%d/%d err=%v", isDup, dup.ID, cmd.ID, err)
	}
	// 不存在的鱼：按真实 roster 拒绝。
	bad := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:78:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 1,
		Payload: targetPayload("definitely_not_a_fish", loc, rev),
	})
	got = runtimeGSApply(t, srv, bad.ID)
	if got.Status != store.AIRuntimeStatusRejected || got.Reason != "unknown_fish" {
		t.Fatalf("未知鱼 = %s/%s，期望 rejected/unknown_fish", got.Status, got.Reason)
	}
}

// TestAISingleWriterFishingMemorySameTx：渔获 + 状态 + 记忆同一事务，
// 记忆写入失败时渔获也回滚。
func TestAISingleWriterFishingMemorySameTx(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	loc, fish := runtimeGSRoster(t, db)
	runtimeGSSeedActor(t, db, runtimeGSAgent, loc, "fishing")
	agent, err := db.AIAgentByKey(ctx, runtimeGSAgent)
	if err != nil || agent == nil {
		t.Fatalf("读 Agent: %v", err)
	}
	if _, err := db.SQL.Exec(`
		CREATE TRIGGER runtime_fail_memory BEFORE INSERT ON ai_memory
		BEGIN SELECT RAISE(ABORT, 'memory blocked'); END`); err != nil {
		t.Fatalf("安装失败夹具: %v", err)
	}
	fishEvent := aiFishTemplate{FishKey: fish, Name: "测试鱼", WeightG: 1200, Event: evStar, Loc: loc}
	if ok := srv.writeAICatch(ctx, db, *agent, fishEvent, loc); ok {
		t.Fatal("记忆写入失败时渔获不应提交")
	}
	var catches int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_catch WHERE agent_key=?`, runtimeGSAgent).Scan(&catches); err != nil {
		t.Fatal(err)
	}
	if catches != 0 {
		t.Fatalf("事务回滚后渔获 = %d 行，期望 0", catches)
	}
	if _, err := db.SQL.Exec(`DROP TRIGGER runtime_fail_memory`); err != nil {
		t.Fatal(err)
	}
	if ok := srv.writeAICatch(ctx, db, *agent, fishEvent, loc); !ok {
		t.Fatal("正常渔获写入失败")
	}
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_catch WHERE agent_key=?`, runtimeGSAgent).Scan(&catches); err != nil {
		t.Fatal(err)
	}
	var memories int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_memory WHERE agent_key=?`, runtimeGSAgent).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if catches != 1 || memories != 1 {
		t.Fatalf("渔获/记忆 = %d/%d，期望 1/1", catches, memories)
	}
}

// TestAIRuntimeCommandDrainRecoversPending：GS 恢复后 drain 把停机期意图全部应用。
func TestAIRuntimeCommandDrainRecoversPending(t *testing.T) {
	db := openMigratedTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := db.AIAddMemory(ctx, runtimeGSAgent, "star", "perch", "鲈鱼", 1200, "x"); err != nil {
			t.Fatal(err)
		}
	}
	cmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "ad:drain-1", Source: store.AIRuntimeSourceAdmin,
		Kind: store.AIRuntimeKindMemoryClear, AgentKey: runtimeGSAgent,
	})
	if cmd.Status != store.AIRuntimeStatusPending {
		t.Fatalf("入队即非 pending: %s", cmd.Status)
	}
	srv.aiRuntimeCommandDrain(db)
	var memories int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_memory WHERE agent_key=?`, runtimeGSAgent).Scan(&memories); err != nil {
		t.Fatal(err)
	}
	if memories != 0 {
		t.Fatalf("drain 后记忆 = %d", memories)
	}
	cmdNow, err := db.AIGetRuntimeCommand(ctx, cmd.ID)
	if err != nil || cmdNow.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("drain 后命令 = %+v err=%v", cmdNow, err)
	}
	// 再 drain 不会重复应用（命令行仍只有一条）。
	srv.aiRuntimeCommandDrain(db)
	if cmdNow, _ := db.AIGetRuntimeCommand(ctx, cmd.ID); cmdNow.ResultRevision < 1 {
		t.Fatalf("result_revision 未记录: %+v", cmdNow)
	}
}

// TestAIRuntimeCommandTargetSuggestRevokedCapability：撤权后 GS 事务内拒绝旧建议。
func TestAIRuntimeCommandTargetSuggestRevokedCapability(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	loc, fish := runtimeGSRoster(t, db)
	runtimeGSSeedActor(t, db, runtimeGSAgent, loc, "fishing")
	if err := db.AISetSetting(ctx, "ai_actor_switch_chance", "1"); err != nil {
		t.Fatal(err)
	}
	rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityAcceptTarget)
	cmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:201:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: targetPayload(fish, loc, rev),
	})
	// 撤权：显式空集合，修订推进到 rev+1。
	if _, err := db.AISaveCapabilities(ctx, runtimeGSAgent, rev, []string{}, nil); err != nil {
		t.Fatalf("撤权: %v", err)
	}
	before := runtimeGSActorRow(t, db, runtimeGSAgent)
	got := runtimeGSApply(t, srv, cmd.ID)
	if got.Status != store.AIRuntimeStatusRejected || got.Reason != "capability_missing" {
		t.Fatalf("撤权后建议 = %s/%s，期望 rejected/capability_missing", got.Status, got.Reason)
	}
	after := runtimeGSActorRow(t, db, runtimeGSAgent)
	if before == nil || after == nil || before.TargetFish != after.TargetFish || before.Loc != after.Loc {
		t.Fatalf("撤权后仍改写了状态: %+v → %+v", before, after)
	}
}

// TestAIRuntimeCommandTargetSuggestStaleCapability：管理员已保存新修订，
// 旧修订的命令不能回写（旧意图不得覆盖新版本）。
func TestAIRuntimeCommandTargetSuggestStaleCapability(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	loc, fish := runtimeGSRoster(t, db)
	runtimeGSSeedActor(t, db, runtimeGSAgent, loc, "fishing")
	if err := db.AISetSetting(ctx, "ai_actor_switch_chance", "1"); err != nil {
		t.Fatal(err)
	}
	rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityAcceptTarget)
	cmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:202:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: targetPayload(fish, loc, rev),
	})
	// 再次保存：改为仍含该能力的新集合。同集合重复保存是幂等的（不再跳修订），
	// 这里必须真的改变集合才能制造「旧修订」。
	if _, err := db.AISaveCapabilities(ctx, runtimeGSAgent, rev,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil); err != nil {
		t.Fatalf("重复保存: %v", err)
	}
	got := runtimeGSApply(t, srv, cmd.ID)
	if got.Status != store.AIRuntimeStatusRejected || got.Reason != "stale_capability" {
		t.Fatalf("旧修订建议 = %s/%s，期望 rejected/stale_capability", got.Status, got.Reason)
	}
}

// 保存「虚拟垂钓」后由既有 GS 应用路径真正打开模拟；撤销后关闭并推进代次，
// 但同一份记录里的「攻略」能力不能因为这次代次推进而失效。
func TestT104CapabilityRuntimeRevokeKeepsGuide(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	agent := runtimeGSAgent

	res1, err := db.AISaveCapabilitiesWithRuntime(ctx, agent, 0,
		[]string{aiconfig.CapabilityVirtualFishing}, nil)
	if err != nil {
		t.Fatalf("保存虚拟垂钓: %v", err)
	}
	if res1.Revision != 1 || res1.Runtime.CommandID == 0 ||
		res1.Runtime.Availability != store.AICapabilityAvailabilityPending {
		t.Fatalf("保存回执 = %+v", res1)
	}
	applied := runtimeGSApply(t, srv, res1.Runtime.CommandID)
	if applied.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("启用命令 = %s/%s", applied.Status, applied.Reason)
	}
	if enabled, _ := runtimeGSAgentFlag(t, db, agent); !enabled {
		t.Fatal("启用命令应用后 actor_enabled 仍为 0")
	}

	// 保留攻略、撤销垂钓：禁用命令必须真正关闭模拟，并推进一次代次。
	res2, err := db.AISaveCapabilitiesWithRuntime(ctx, agent, 1,
		[]string{aiconfig.CapabilityGuideHelp}, nil)
	if err != nil {
		t.Fatalf("撤销垂钓: %v", err)
	}
	if res2.Revision != 2 || res2.Runtime.CommandID == 0 {
		t.Fatalf("撤权回执 = %+v", res2)
	}
	disabled := runtimeGSApply(t, srv, res2.Runtime.CommandID)
	if disabled.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("禁用命令 = %s/%s", disabled.Status, disabled.Reason)
	}
	if enabled, _ := runtimeGSAgentFlag(t, db, agent); enabled {
		t.Fatal("禁用命令应用后 actor_enabled 仍为 1")
	}
	ctl, err := db.AIGetRuntimeControl(ctx, agent)
	if err != nil || ctl.Generation != 2 {
		t.Fatalf("禁用后 generation = %+v/%v，期望 2", ctl, err)
	}
	view, err := db.AIGetCapabilities(ctx, agent)
	if err != nil {
		t.Fatal(err)
	}
	if view.State.State != aiconfig.CapabilityStateConfigured || view.State.Revision != 2 ||
		strings.Join(view.State.ReadTools, ",") != aiconfig.ToolMapVibe || view.State.Has(aiconfig.CapabilityVirtualFishing) {
		t.Fatalf("撤销垂钓后投影 = %+v", view.State)
	}
	rt, err := db.AICapabilityRuntimeStatus(ctx, agent, view.State)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Availability != store.AICapabilityAvailabilityAvailable || rt.Desired || rt.Actual {
		t.Fatalf("撤销后可用性 = %+v", rt)
	}
	actorAfter, _ := runtimeGSAgentFlag(t, db, agent)
	t.Logf("revoke：enable_id=%d disable_id=%d disable_request=%s gen=%d actor=%v read_tools=%d availability=%s",
		res1.Runtime.CommandID, res2.Runtime.CommandID, res2.Runtime.RequestID,
		ctl.Generation, actorAfter, len(view.State.ReadTools), rt.Availability)

	// 同一集合在当前修订上重复保存：不跳修订、不再补派。
	res3, err := db.AISaveCapabilitiesWithRuntime(ctx, agent, 2,
		[]string{aiconfig.CapabilityGuideHelp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Revision != 2 || res3.Changed ||
		res3.Runtime.CommandID != res2.Runtime.CommandID ||
		res3.Runtime.Availability != store.AICapabilityAvailabilityAvailable {
		t.Fatalf("重复撤销 = %+v", res3)
	}
}

// 能力命令携带修订：旧修订的启用/禁用都不能覆盖已经保存的新集合。
func TestT104CapabilityOldRevisionCommandCannotOverwrite(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	agent := runtimeGSAgent
	loc, _ := runtimeGSRoster(t, db)
	runtimeGSSeedActor(t, db, agent, loc, "fishing") // 模拟已由旧后台打开

	// rev1 授予垂钓（已开，无需命令）→ rev2 撤销（禁用 C2 待应用）
	if _, err := db.AISaveCapabilitiesWithRuntime(ctx, agent, 0,
		[]string{aiconfig.CapabilityVirtualFishing}, nil); err != nil {
		t.Fatal(err)
	}
	res2, err := db.AISaveCapabilitiesWithRuntime(ctx, agent, 1,
		[]string{aiconfig.CapabilityGuideHelp}, nil)
	if err != nil || res2.Runtime.CommandID == 0 {
		t.Fatalf("撤销垂钓 = %+v/%v", res2, err)
	}
	// rev3 重新授予：运行态仍是开启，不应补派新命令。
	res3, err := db.AISaveCapabilitiesWithRuntime(ctx, agent, 2,
		[]string{aiconfig.CapabilityVirtualFishing}, nil)
	if err != nil || res3.Revision != 3 || res3.Runtime.CommandID != 0 {
		t.Fatalf("重新授予 = %+v/%v", res3, err)
	}
	// 旧修订的禁用命令晚到：按能力修订拒绝，不改运行态。
	got := runtimeGSApply(t, srv, res2.Runtime.CommandID)
	if got.Status != store.AIRuntimeStatusRejected || got.Reason != "stale_capability" {
		t.Fatalf("旧禁用命令 = %s/%s，期望 rejected/stale_capability", got.Status, got.Reason)
	}
	if enabled, _ := runtimeGSAgentFlag(t, db, agent); !enabled {
		t.Fatal("旧禁用命令仍关闭了模拟")
	}
	view, err := db.AIGetCapabilities(ctx, agent)
	if err != nil || view.State.Revision != 3 ||
		!view.State.Has(aiconfig.CapabilityVirtualFishing) {
		t.Fatalf("新集合 = %+v/%v", view.State, err)
	}
	actorNow, _ := runtimeGSAgentFlag(t, db, agent)
	t.Logf("旧禁用命令：request=%s status=%s reason=%s；新集合 revision=%d actor=%v",
		res2.Runtime.RequestID, got.Status, got.Reason, view.State.Revision, actorNow)

	// 反向：rev4 清空（禁用 C4）→ rev5 授予（启用 C5 待应用）→ rev6 清空；
	// 旧启用命令晚到也必须拒绝。
	res4, err := db.AISaveCapabilitiesWithRuntime(ctx, agent, 3, []string{}, nil)
	if err != nil || res4.Runtime.CommandID == 0 {
		t.Fatalf("清空 = %+v/%v", res4, err)
	}
	if got := runtimeGSApply(t, srv, res4.Runtime.CommandID); got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("禁用命令 = %s/%s", got.Status, got.Reason)
	}
	res5, err := db.AISaveCapabilitiesWithRuntime(ctx, agent, 4,
		[]string{aiconfig.CapabilityVirtualFishing}, nil)
	if err != nil || res5.Runtime.CommandID == 0 {
		t.Fatalf("重新授予 = %+v/%v", res5, err)
	}
	res6, err := db.AISaveCapabilitiesWithRuntime(ctx, agent, 5, []string{}, nil)
	if err != nil || res6.Revision != 6 {
		t.Fatalf("再次清空 = %+v/%v", res6, err)
	}
	got = runtimeGSApply(t, srv, res5.Runtime.CommandID)
	if got.Status != store.AIRuntimeStatusRejected || got.Reason != "stale_capability" {
		t.Fatalf("旧启用命令 = %s/%s，期望 rejected/stale_capability", got.Status, got.Reason)
	}
	if enabled, _ := runtimeGSAgentFlag(t, db, agent); enabled {
		t.Fatal("旧启用命令仍打开了模拟")
	}
}

// 升级排空：升级前入队、没有 capability/cap_revision 的旧建议，由新 GS
// 以 capability_missing 终结，不产生运行态差量；终态行保留，同版本新建议正常。
func TestT104UpgradeRejectsPriorVersionSuggestion(t *testing.T) {
	db := openStaticTestDB(t)
	srv := runtimeGSServer(t, db)
	ctx := context.Background()
	loc, fish := runtimeGSRoster(t, db)
	runtimeGSSeedActor(t, db, runtimeGSAgent, "level_other_placeholder", "fishing")
	if err := db.AISetSetting(ctx, "ai_actor_switch_chance", "1"); err != nil {
		t.Fatal(err)
	}
	rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityAcceptTarget)

	oldCmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:700:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: []byte(fmt.Sprintf(`{"fish_key":%q,"loc_key":%q}`, fish, loc)),
	})
	before := runtimeGSActorRow(t, db, runtimeGSAgent)
	got := runtimeGSApply(t, srv, oldCmd.ID)
	if got.Status != store.AIRuntimeStatusRejected || got.Reason != "capability_missing" {
		t.Fatalf("旧版建议 = %s/%s，期望 rejected/capability_missing", got.Status, got.Reason)
	}
	after := runtimeGSActorRow(t, db, runtimeGSAgent)
	if before == nil || after == nil || before.TargetFish != after.TargetFish || before.Loc != after.Loc {
		t.Fatalf("旧版建议改写了运行态: %+v → %+v", before, after)
	}
	if again := runtimeGSApply(t, srv, oldCmd.ID); again.Status != store.AIRuntimeStatusRejected {
		t.Fatalf("终态重放 = %s", again.Status)
	}
	if cmd, err := db.AIGetRuntimeCommand(ctx, oldCmd.ID); err != nil ||
		cmd.Status != store.AIRuntimeStatusRejected || cmd.Reason != "capability_missing" {
		t.Fatalf("旧版终态未保留: %+v/%v", cmd, err)
	}

	// 同版本新建议：能力与修订齐全，仍走正常路径。
	newCmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:701:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: targetPayload(fish, loc, rev),
	})
	if got := runtimeGSApply(t, srv, newCmd.ID); got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("升级后新建议 = %s/%s", got.Status, got.Reason)
	}
	if after := runtimeGSActorRow(t, db, runtimeGSAgent); after == nil ||
		after.TargetFish != fish || after.Loc != loc {
		t.Fatalf("升级后新建议未应用: %+v", after)
	}
	t.Logf("升级：old=%s/%s new=%s；旧命令终态保留=%v",
		got.Status, got.Reason, store.AIRuntimeStatusApplied, oldCmd.ID > 0)
}

// 双向换版演练：新组排空跨版本工作后再切换；回撤沿用同一份库。
// 权限记录、旧 tools_json 快照与命令终态都保留，重开库后不重放。
func TestT104VersionSwitchDrainKeepsSnapshots(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switch.db")
	seedTestDBFile(t, path, testDBMigrated)
	db, err := store.OpenWithStatic(path, staticTestDBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.EnableStaticReadOnly(ctx); err != nil {
		t.Fatal(err)
	}
	srv := runtimeGSServer(t, db)
	loc, fish := runtimeGSRoster(t, db)
	runtimeGSSeedActor(t, db, runtimeGSAgent, "level_other_placeholder", "fishing")
	if err := db.AISetSetting(ctx, "ai_actor_switch_chance", "1"); err != nil {
		t.Fatal(err)
	}
	rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityAcceptTarget)

	capRaw := func(d *store.DB) string {
		var raw string
		if err := d.SQL.QueryRow(`SELECT value FROM ai_setting WHERE key=?`,
			store.AICapabilityKey(runtimeGSAgent)).Scan(&raw); err != nil {
			t.Fatalf("读能力快照: %v", err)
		}
		return raw
	}
	toolsRaw := func(d *store.DB) string {
		var raw string
		if err := d.SQL.QueryRow(`SELECT tools_json FROM ai_agent WHERE key=?`,
			runtimeGSAgent).Scan(&raw); err != nil {
			t.Fatalf("读旧 tools_json: %v", err)
		}
		return raw
	}
	capBefore, toolsBefore := capRaw(db), toolsRaw(db)

	oldCmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:710:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: []byte(fmt.Sprintf(`{"fish_key":%q,"loc_key":%q}`, fish, loc)),
	})
	newCmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
		RequestID: "bridge:711:companion:1", Source: store.AIRuntimeSourceBridge,
		Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
		ExpectedGeneration: 1, ExpectedRevision: 0,
		Payload: targetPayload(fish, loc, rev),
	})
	// 升级后的新 GS 排空：旧版拒绝，新版应用，队列无 pending。
	if got := runtimeGSApply(t, srv, oldCmd.ID); got.Status != store.AIRuntimeStatusRejected ||
		got.Reason != "capability_missing" {
		t.Fatalf("排空旧版 = %s/%s", got.Status, got.Reason)
	}
	if got := runtimeGSApply(t, srv, newCmd.ID); got.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("排空新版 = %s/%s", got.Status, got.Reason)
	}
	var pending int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command WHERE status='pending'`).
		Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("排空后仍有 %d 条 pending", pending)
	}
	if capRaw(db) != capBefore || toolsRaw(db) != toolsBefore {
		t.Fatal("排空改写了能力快照或旧 tools_json")
	}
	if offline, err := db.AIGetRuntimeCommand(ctx, oldCmd.ID); err != nil ||
		offline.Status != store.AIRuntimeStatusRejected {
		t.Fatalf("升级终态未保留: %+v/%v", offline, err)
	}

	// 回撤：旧组沿用同一份库重启。能力记录只是惰性 ai_setting，旧 tools_json
	// 仍是旧代码读取的那份快照；命令终态与无 pending 的状态原样保留。
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := store.OpenWithStatic(path, staticTestDBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	if err := db2.EnableStaticReadOnly(ctx); err != nil {
		t.Fatal(err)
	}
	if capRaw(db2) != capBefore || toolsRaw(db2) != toolsBefore {
		t.Fatal("回撤后权限快照或旧 tools_json 丢失")
	}
	if err := db2.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command WHERE status='pending'`).
		Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("回撤后出现 %d 条 pending", pending)
	}
	oldAfter, err := db2.AIGetRuntimeCommand(ctx, oldCmd.ID)
	if err != nil || oldAfter.Status != store.AIRuntimeStatusRejected ||
		oldAfter.Reason != "capability_missing" {
		t.Fatalf("回撤后旧终态 = %+v/%v", oldAfter, err)
	}
	newAfter, err := db2.AIGetRuntimeCommand(ctx, newCmd.ID)
	if err != nil || newAfter.Status != store.AIRuntimeStatusApplied {
		t.Fatalf("回撤后新终态 = %+v/%v", newAfter, err)
	}
	view, err := db2.AIGetCapabilities(ctx, runtimeGSAgent)
	if err != nil || !view.Present || view.Revision != rev {
		t.Fatalf("回撤后能力记录 = %+v/%v", view, err)
	}
	t.Logf("换版：old=%s/%s new=%s/%s revision=%d pending=%d cap_snapshot=%t tools_snapshot=%t",
		oldAfter.Status, oldAfter.Reason, newAfter.Status, newAfter.Reason,
		view.Revision, pending, capRaw(db2) == capBefore, toolsRaw(db2) == toolsBefore)
}
