package gameserver

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
//
// 后台「发鱼测试」按钮的 GS 侧行为：
//   - 强前置全满足时，只新增 1 行 ai_task（kind=channel / source=system），
//     文案仍来自 aiFishingTalkPrompt，后续 ai_result 与 24/27 投递走真实路径；
//   - T-108：资格只认 T-104 的能力集合，旧全局开关与角色级报话开关不再放行/拒绝；
//   - 消费前如果能力被撤权、Agent 状态变了或急停，GS 重校验拒绝且不产生任何任务。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027Server(t *testing.T, db *store.DB) *Server {
	t.Helper()
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "t027-talk"}}, logging.Discard(), nil)
	srv.SetDB(db)
	return srv
}

// t027SeedTalkFixture 准备一条真实链路所需的最小状态：
// 全局播报开、Agent 报话开、已接入、订阅 nav1_03、外发放行。
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027SeedTalkFixture(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	dest := aiconfig.ConsentDestination{BaseURL: "https://vendor.invalid/v1", ModelID: "model-a"}
	rec, ok := aiconfig.NewConsentRecord(aiconfig.ConsentItemOutbound, "t027-test", time.Now().Unix(), dest)
	if !ok {
		t.Fatal("构造 outbound consent 失败")
	}
	raw, err := aiconfig.MarshalConsent(rec)
	if err != nil {
		t.Fatalf("序列化 consent: %v", err)
	}
	rawDest, err := aiconfig.MarshalDestination(dest)
	if err != nil {
		t.Fatalf("序列化目的地: %v", err)
	}
	if err := db.AISetSettings(ctx, map[string]string{
		aiconfig.KeyOutboundEnabled:   "1",
		aiconfig.KeyEmergencyStop:     "0",
		aiconfig.KeyConsentOutbound:   raw,
		aiconfig.KeyEgressDestination: rawDest,
	}); err != nil {
		t.Fatalf("写设置: %v", err)
	}
	// T-108：报鱼资格来自能力集合（播报自己渔获；按目录还需圆桌资格）。
	if _, err := db.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityJoinRoundtable, aiconfig.CapabilityBroadcastCatch}, nil); err != nil {
		t.Fatalf("保存能力: %v", err)
	}
	if err := db.AISetAgentFishingTalk(ctx, "companion", true); err != nil {
		t.Fatalf("打开上鱼报话: %v", err)
	}
	if err := db.AISetAgentJoined(ctx, "companion", true, 0); err != nil {
		t.Fatalf("接入 Agent: %v", err)
	}
	if err := db.AISetChannelAgent(ctx, "nav1_03", "companion", true); err != nil {
		t.Fatalf("订阅频道: %v", err)
	}
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027Count(t *testing.T, db *store.DB, table string) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("统计 %s: %v", table, err)
	}
	return n
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027ReadState(t *testing.T, db *store.DB, requestID string) *AIFishingTalkTestState {
	t.Helper()
	state, ok, err := GetAIFishingTalkTest(context.Background(), db, requestID)
	if err != nil {
		t.Fatalf("读测试结果: %v", err)
	}
	if !ok {
		t.Fatalf("没有读到 request_id=%s 的结果", requestID)
	}
	return state
}

// TestAIFishingTalkTestHookUsesRealChain 覆盖票 §4 的 DoD 2：一次触发只新增
// 1 行 ai_task；模拟 aibridge 写 ai_result 后，24/27 投递仍由既有 pollAIResults 完成。
func TestAIFishingTalkTestHookUsesRealChain(t *testing.T) {
	db := openStaticTestDB(t)
	t027SeedTalkFixture(t, db)
	probe := newT013ChatProbeWithDB(t, "nav1_03", db)
	ctx := context.Background()

	if got := aiEgress(ctx, db); !got.Allowed {
		t.Fatalf("测试前提：外发未放行 %+v", got)
	}
	state, rej := RequestAIFishingTalkTest(ctx, db, "companion", "t027-chain-1")
	if rej != nil {
		t.Fatalf("前置校验未通过: %s %s", rej.Code, rej.Message)
	}
	if state.Status != aiFishingTalkTestStatusPending {
		t.Fatalf("触发状态 = %q，期望 pending", state.Status)
	}
	if n := t027Count(t, db, "ai_task"); n != 0 {
		t.Fatalf("触发请求本身不得写 ai_task，实得 %d", n)
	}

	probe.srv.consumeAIFishingTalkTest(ctx, db)

	got := t027ReadState(t, db, "t027-chain-1")
	if got.Status != aiFishingTalkTestStatusEnqueued {
		t.Fatalf("消费结果 = %+v，期望 enqueued", got)
	}
	if got.Channel != "nav1_03" || got.TaskID <= 0 || got.FishName == "" || got.Event != string(evStar) {
		t.Fatalf("结果缺关键字段: %+v", got)
	}
	if n := t027Count(t, db, "ai_task"); n != 1 {
		t.Fatalf("ai_task 应恰好新增 1 行，实得 %d", n)
	}

	var kind, scope, scopeKey, agentKey, sourceKind, text, taskState string
	var uid, contactUID int64
	if err := db.SQL.QueryRow(`
		SELECT kind, scope, scope_key, uid, agent, contact_uid, source_kind, text, state
		FROM ai_task WHERE id=?`, got.TaskID).
		Scan(&kind, &scope, &scopeKey, &uid, &agentKey, &contactUID, &sourceKind, &text, &taskState); err != nil {
		t.Fatalf("读 ai_task: %v", err)
	}
	if kind != "channel" || scope != "channel" || scopeKey != "nav1_03" {
		t.Fatalf("任务形状 = (%s,%s,%s)，期望 channel/channel/nav1_03", kind, scope, scopeKey)
	}
	if agentKey != "companion" || contactUID != 2147483645 || uid != t013SelfUID {
		t.Fatalf("任务归属 = agent:%s contact:%d uid:%d", agentKey, contactUID, uid)
	}
	if sourceKind != "system" || taskState != "pending" {
		t.Fatalf("任务来源/状态 = (%s,%s)，期望 system/pending", sourceKind, taskState)
	}
	if !strings.Contains(text, "你刚钓上") || !strings.Contains(text, got.FishName) ||
		!strings.Contains(text, "上星") || strings.Contains(text, "kg") {
		t.Fatalf("文案不是既有上鱼提示词: %q", text)
	}

	// aibridge 侧的真实写入口：任务置 processing → AIComplete 写 1 行 ai_result。
	if _, err := db.SQL.Exec(`UPDATE ai_task SET state='processing' WHERE id=?`, got.TaskID); err != nil {
		t.Fatalf("置 processing: %v", err)
	}
	if err := db.AIComplete(ctx, got.TaskID, "done", "好家伙，这条星鱼真漂亮", "", ""); err != nil {
		t.Fatalf("AIComplete: %v", err)
	}
	if n := t027Count(t, db, "ai_result"); n != 1 {
		t.Fatalf("ai_result 应恰好 1 行，实得 %d", n)
	}
	// T-108：投递前按原任务证据复核。此处能力仍按入队时的绑定有效，
	// 因此这条已生成的结果应当真实推送；撤权后的「不得再入队」放在下一步。
	probe.srv.pollAIResults(ctx, db)
	rec := probe.next(t)
	if rec.main != 24 || rec.sub != 27 {
		t.Fatalf("投递必须走 24/27，实得 %d/%d", rec.main, rec.sub)
	}
	if rec.text != "好家伙，这条星鱼真漂亮" || rec.fromUID != 2147483645 {
		t.Fatalf("投递内容 = %q from=%d", rec.text, rec.fromUID)
	}
	var delivered float64
	if err := db.SQL.QueryRow(`SELECT COALESCE(delivered_ts,0) FROM ai_result`).Scan(&delivered); err != nil {
		t.Fatalf("读投递标记: %v", err)
	}
	if delivered <= 0 {
		t.Fatal("ai_result 未标记 delivered_ts")
	}

	// 钩子只跳过派生抽样，不改资格：普通达标档在任何性格下都静默，
	// 撤权后同一条真实路径同样不得再入队。
	agent, err := db.AIAgentByKey(ctx, "companion")
	if err != nil || agent == nil {
		t.Fatalf("读 Agent: %v", err)
	}
	fishOk := aiFishTemplate{Name: got.FishName, WeightG: 2500, Event: evOk}
	if _, spoken := probe.srv.maybeAIFishingTalk(ctx, db, "nav1_03", t013SelfUID,
		*agent, fishOk, 1); spoken {
		t.Fatal("普通达标档不得说话")
	}
	fish := aiFishTemplate{Name: got.FishName, WeightG: 2500, Event: evStar}
	if _, err := db.AISaveCapabilities(ctx, "companion", 1, []string{}, nil); err != nil {
		t.Fatalf("撤权: %v", err)
	}
	if _, spoken := probe.srv.maybeAIFishingTalk(ctx, db, "nav1_03", t013SelfUID,
		*agent, fish, 1); spoken {
		t.Fatal("撤权后正常播报不得再入队")
	}
	if n := t027Count(t, db, "ai_task"); n != 1 {
		t.Fatalf("正常路径不得追加任务，ai_task=%d", n)
	}
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func t027PendingState(t *testing.T, db *store.DB, requestID, agentKey string, created float64) {
	t.Helper()
	state := AIFishingTalkTestState{
		RequestID: requestID, AgentKey: agentKey, IntentEpoch: db.AIEmergencyState(t.Context()).Epoch,
		CreatedTS: created,
		Status:    aiFishingTalkTestStatusPending,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("序列化 pending: %v", err)
	}
	if err := db.KVSet(context.Background(), AIFishingTalkTestKVKey, string(raw)); err != nil {
		t.Fatalf("写 pending: %v", err)
	}
}

// TestAIFishingTalkTestHookRevalidates 消费时强制重校验：点击后开关/consent/
// Agent 状态被改，旧请求必须被拒绝且不产生任何 ai_task 投递。
func TestAIFishingTalkTestHookRevalidates(t *testing.T) {
	now := func() float64 { return float64(time.Now().Unix()) }
	cases := []struct {
		name   string
		mutate func(t *testing.T, ctx context.Context, db *store.DB)
		reason string
	}{
		{
			name: "Agent 删除",
			mutate: func(t *testing.T, ctx context.Context, db *store.DB) {
				if _, err := db.SQL.Exec(`UPDATE ai_agent SET deleted_ts=1 WHERE key='companion'`); err != nil {
					t.Fatal(err)
				}
			},
			reason: "agent_missing",
		},
		{
			name: "能力撤权",
			mutate: func(t *testing.T, ctx context.Context, db *store.DB) {
				if _, err := db.AISaveCapabilities(ctx, "companion", 1, []string{}, nil); err != nil {
					t.Fatal(err)
				}
			},
			reason: "capability_missing",
		},
		{
			name: "能力记录损坏",
			mutate: func(t *testing.T, ctx context.Context, db *store.DB) {
				if _, err := db.SQL.Exec(`UPDATE ai_setting SET value='{"schema_version":9}'
					WHERE key=?`, store.AICapabilityKey("companion")); err != nil {
					t.Fatal(err)
				}
			},
			reason: "capability_missing",
		},
		{
			name: "急停",
			mutate: func(t *testing.T, ctx context.Context, db *store.DB) {
				if err := db.AISetSettings(ctx, map[string]string{aiconfig.KeyEmergencyStop: "1"}); err != nil {
					t.Fatal(err)
				}
			},
			reason: "emergency_stop",
		},
		{
			name:   "没有在线玩家的频道",
			mutate: func(t *testing.T, ctx context.Context, db *store.DB) {},
			reason: "no_online_channel",
		},
		{
			name:   "超过消费时限",
			mutate: func(t *testing.T, ctx context.Context, db *store.DB) {},
			reason: "expired",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := openStaticTestDB(t)
			t027SeedTalkFixture(t, db)
			created := now()
			if c.reason == "expired" {
				created = now() - aiFishingTalkTestTTL.Seconds() - 5
			}
			t027PendingState(t, db, "t027-recheck-1", "companion", created)
			c.mutate(t, context.Background(), db)

			srv := t027Server(t, db)
			srv.consumeAIFishingTalkTest(context.Background(), db)

			got := t027ReadState(t, db, "t027-recheck-1")
			if got.Status != aiFishingTalkTestStatusRejected || got.Reason != c.reason {
				t.Fatalf("结果 = status:%s reason:%s message:%s，期望 rejected/%s",
					got.Status, got.Reason, got.Message, c.reason)
			}
			if got.Message == "" {
				t.Fatal("拒绝必须带可读原因，不得静默降级")
			}
			if n := t027Count(t, db, "ai_task"); n != 0 {
				t.Fatalf("拒绝时不得产生 ai_task，实得 %d", n)
			}
		})
	}
}

// TestAIFishingTalkTestHookIgnoresLegacyConsent V2 只留急停：旧 outbound 授权缺失
// 不再是前置拒绝原因；没有在线频道时才会以 no_online_channel 拒绝。
func TestAIFishingTalkTestHookIgnoresLegacyConsent(t *testing.T) {
	db := openStaticTestDB(t)
	t027SeedTalkFixture(t, db)
	if err := db.AISetSettings(context.Background(), map[string]string{
		aiconfig.KeyConsentOutbound: "",
		aiconfig.KeyOutboundEnabled: "0",
	}); err != nil {
		t.Fatal(err)
	}
	t027PendingState(t, db, "t027-consent-1", "companion", float64(time.Now().Unix()))
	srv := t027Server(t, db)
	srv.consumeAIFishingTalkTest(context.Background(), db)

	got := t027ReadState(t, db, "t027-consent-1")
	if got.Reason == "consent_not_allowed" {
		t.Fatalf("旧 consent 不应再作为拒绝原因: %+v", got)
	}
	if got.Status != aiFishingTalkTestStatusRejected || got.Reason != "no_online_channel" {
		t.Fatalf("结果 = status:%s reason:%s，期望 rejected/no_online_channel", got.Status, got.Reason)
	}
}
