package gameserver

// 闲时搭话的链路用例：能力/订阅/性格/静默都满足时，真的入队一条
// 受信事件任务，并带上 idle_chat 的能力证据。

import (
	"context"
	"strings"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

func TestT108IdleChatEnqueue(t *testing.T) {
	const channel = "t108_idle_chat"
	ctx := context.Background()
	db := openStaticTestDB(t)
	probe := newT013ChatProbeWithDB(t, channel, db)
	t108Agent(t, db, channel, "companion")
	grantCapabilities(t, db, "companion",
		aiconfig.CapabilityVirtualFishing, aiconfig.CapabilityIdleChat)
	t108Personality(t, db, "companion", 95, 90, 40, 50)
	if err := db.AISetSetting(ctx, "ai_idle_silence_min", "1"); err != nil {
		t.Fatal(err)
	}

	p, err := db.AIGetPersonality(ctx, "companion")
	if err != nil {
		t.Fatal(err)
	}
	seed := t108SeedForOutcome(t, p.Config, activityIdleFeature(), true, store.AIActorSeed(3, "companion"))
	t108SeedFixed(t, seed)

	// 重置包级节奏状态：静默已过一小时，本小时没说过话。
	aiIdleStates.Range(func(k, _ any) bool { aiIdleStates.Delete(k); return true })
	aiIdleGlobal.mu.Lock()
	aiIdleGlobal.hourStart = time.Time{}
	aiIdleGlobal.count = 0
	aiIdleGlobal.mu.Unlock()
	now := time.Now()
	st := aiIdleStateFor(channel, now)
	st.mu.Lock()
	st.lastHumanAt = now.Add(-time.Hour)
	st.lastTryAt = time.Time{}
	st.mu.Unlock()

	fishing := []aiFishingAgent{{
		agent: store.AIAgent{Key: "companion", Display: "伙伴", ContactUID: 2147480001, ActorEnabled: true},
		actor: &store.AIActor{Loc: "level_011_ladoga", Activity: "fishing",
			SinceTS: float64(now.Add(-20 * time.Minute).Unix())},
	}}
	if !probe.srv.maybeAIIdleChat(ctx, db, channel, t013SelfUID, fishing) {
		t.Fatal("静默够久且性格愿意开口，应入队")
	}
	tasks := t108Tasks(t, db)
	if len(tasks) != 1 {
		t.Fatalf("任务数 = %d，期望 1: %+v", len(tasks), tasks)
	}
	if tasks[0].Kind != "channel" || tasks[0].SourceKind != "system" || tasks[0].Agent != "companion" {
		t.Fatalf("任务形状不符: %+v", tasks[0])
	}
	if !strings.Contains(tasks[0].Text, "安静了一阵") {
		t.Fatalf("提示词不是闲时搭话: %q", tasks[0].Text)
	}
	var binding string
	if err := db.SQL.QueryRow(`SELECT binding_json FROM ai_task_evidence WHERE task_id=?`,
		tasks[0].ID).Scan(&binding); err != nil {
		t.Fatalf("读证据: %v", err)
	}
	if !strings.Contains(binding, `"category":"idle_chat"`) ||
		!strings.Contains(binding, `"capability":"idle_chat"`) {
		t.Fatalf("证据未绑定 idle_chat: %s", binding)
	}

	// 同一小时的第二次调用应被最小间隔挡住，不会连发。
	if probe.srv.maybeAIIdleChat(ctx, db, channel, t013SelfUID, fishing) {
		t.Fatal("距上次搭话太近，不应再次入队")
	}
	if n := len(t108Tasks(t, db)); n != 1 {
		t.Fatalf("第二次调用产生了额外任务: %d", n)
	}
}

// 圆桌人数从 3 放宽到 12 后，4 人证据必须能入队（旧上限会在 bind 时拒绝）。
func TestT108RoundtableFourParticipants(t *testing.T) {
	const channel = "t108_rt_four"
	ctx := context.Background()
	db := openStaticTestDB(t)
	keys := []string{"rt_a", "rt_b", "rt_c", "rt_d"}
	parts := make([]store.AITaskEvidenceParticipant, 0, len(keys))
	for _, key := range keys {
		if _, _, err := db.AICreateAgent(ctx, store.AIAgent{
			Key: key, Display: key, Role: "钓友", ReplyMode: "social",
		}, "[]"); err != nil {
			t.Fatalf("创建 %s: %v", key, err)
		}
		t108Agent(t, db, channel, key)
		grantCapabilities(t, db, key, aiconfig.CapabilityJoinRoundtable)
		parts = append(parts, store.AITaskEvidenceParticipant{
			Key: key, Capability: aiconfig.CapabilityJoinRoundtable,
		})
	}
	ev := store.AITaskEvidence{
		FormatVersion: store.AITaskEvidenceFormatV1,
		Category:      aiconfig.EventRoundtable,
		Participants:  parts,
	}
	taskID, err := db.AIEnqueueSystemRoundtable(ctx, channel, t013SelfUID, "聊聊今天", keys, &ev)
	if err != nil {
		t.Fatalf("4 人圆桌应能入队: %v", err)
	}
	var binding string
	if err := db.SQL.QueryRow(`SELECT binding_json FROM ai_task_evidence WHERE task_id=?`, taskID).Scan(&binding); err != nil {
		t.Fatalf("读证据: %v", err)
	}
	for _, key := range keys {
		if !strings.Contains(binding, `"key":"`+key+`"`) {
			t.Fatalf("证据缺少 %s: %s", key, binding)
		}
	}
}

// 这里把抽样固定为「升级」，验证任务形状与双方能力证据。
func TestT108IdleChatRoundtable(t *testing.T) {
	const channel = "t108_idle_rt"
	ctx := context.Background()
	db := openStaticTestDB(t)
	probe := newT013ChatProbeWithDB(t, channel, db)
	for _, key := range []string{"idle_a", "idle_b"} {
		if _, _, err := db.AICreateAgent(ctx, store.AIAgent{
			Key: key, Display: key, Role: "钓友", ReplyMode: "social", ActorEnabled: true,
		}, "[]"); err != nil {
			t.Fatalf("创建 %s: %v", key, err)
		}
		t108Agent(t, db, channel, key)
		grantCapabilities(t, db, key,
			aiconfig.CapabilityVirtualFishing, aiconfig.CapabilityIdleChat,
			aiconfig.CapabilityJoinRoundtable)
		t108Personality(t, db, key, 95, 90, 40, 50)
	}
	if err := db.AISetSetting(ctx, "ai_fishing_mood", "lively"); err != nil {
		t.Fatal(err)
	}
	p, err := db.AIGetPersonality(ctx, "idle_a")
	if err != nil {
		t.Fatal(err)
	}
	seed := t108SeedForOutcome(t, p.Config, activityIdleFeature(), true, store.AIActorSeed(3, "idle_a"))
	t108SeedFixed(t, seed)
	prevRoll := aiIdleRoundtableRollForTest
	aiIdleRoundtableRollForTest = func() float64 { return 0 }
	t.Cleanup(func() { aiIdleRoundtableRollForTest = prevRoll })

	aiIdleStates.Range(func(k, _ any) bool { aiIdleStates.Delete(k); return true })
	aiIdleGlobal.mu.Lock()
	aiIdleGlobal.hourStart = time.Time{}
	aiIdleGlobal.count = 0
	aiIdleGlobal.mu.Unlock()
	now := time.Now()
	st := aiIdleStateFor(channel, now)
	st.mu.Lock()
	st.lastHumanAt = now.Add(-time.Hour)
	st.lastTryAt = time.Time{}
	st.mu.Unlock()

	fishing := []aiFishingAgent{
		{agent: store.AIAgent{Key: "idle_a", Display: "A", ContactUID: 2147480001, ActorEnabled: true},
			actor: &store.AIActor{Loc: "level_011_ladoga", Activity: "fishing", SinceTS: float64(now.Add(-20 * time.Minute).Unix())}},
		{agent: store.AIAgent{Key: "idle_b", Display: "B", ContactUID: 2147480002, ActorEnabled: true},
			actor: &store.AIActor{Loc: "level_011_ladoga", Activity: "fishing", SinceTS: float64(now.Add(-10 * time.Minute).Unix())}},
	}
	if !probe.srv.maybeAIIdleChat(ctx, db, channel, t013SelfUID, fishing) {
		t.Fatal("应升级成两人对话")
	}
	tasks := t108Tasks(t, db)
	if len(tasks) != 1 || tasks[0].Kind != "roundtable" || tasks[0].SourceKind != "system" {
		t.Fatalf("应为一条圆桌任务: %+v", tasks)
	}
	var binding string
	if err := db.SQL.QueryRow(`SELECT binding_json FROM ai_task_evidence WHERE task_id=?`,
		tasks[0].ID).Scan(&binding); err != nil {
		t.Fatalf("读证据: %v", err)
	}
	for _, want := range []string{`"category":"idle_chat"`, `"key":"idle_a"`, `"key":"idle_b"`} {
		if !strings.Contains(binding, want) {
			t.Fatalf("证据缺少 %s: %s", want, binding)
		}
	}
}
