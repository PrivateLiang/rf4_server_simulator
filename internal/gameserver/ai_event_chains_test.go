package gameserver

// T-108 三条真实事件链的定向验收。
//
// 每条链都要求「一次确实发生 + 一次合法沉默」，并逐次记录候选、动作、
// 任务 ID 与投递结果。夹具固定：事件种子由 aiEventSeedForTest 给出、
// 性格与能力修订显式保存、时钟用真实秒但只做「已过硬驻留」的推前。

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/proto"
	"rf4server/internal/store"
)

// t108SeedFixed 固定主动事件的派生种子，并在用例结束时恢复。
func t108SeedFixed(t *testing.T, seed int64) {
	t.Helper()
	prev := aiEventSeedForTest
	aiEventSeedForTest = func() int64 { return seed }
	t.Cleanup(func() { aiEventSeedForTest = prev })
}

// t108SeedForOutcome 找一个让 want(personality, seed, feature) 等于期望值的种子。
func t108SeedForOutcome(t *testing.T, p aiconfig.PersonalityConfig,
	feature aiconfig.PersonalityFeature, want bool, offset int64) int64 {
	t.Helper()
	for seed := int64(1); seed <= 4096; seed++ {
		got, _ := activityWants(p, seed^offset, feature)
		if got == want {
			return seed
		}
	}
	t.Fatalf("找不到 want=%v 的种子", want)
	return 0
}

// t108Agent 把一个内置 Agent 接入游戏并订阅频道。
func t108Agent(t *testing.T, db *store.DB, channel, key string) {
	t.Helper()
	ctx := context.Background()
	if err := db.AISetAgentJoined(ctx, key, true, 0); err != nil {
		t.Fatalf("接入 %s: %v", key, err)
	}
	if err := db.AISetChannelAgent(ctx, channel, key, true); err != nil {
		t.Fatalf("订阅 %s: %v", key, err)
	}
}

func t108Personality(t *testing.T, db *store.DB, key string, soc, exp, per, reg int) {
	t.Helper()
	ctx := context.Background()
	cur, err := db.AIGetPersonality(ctx, key)
	if err != nil {
		t.Fatalf("读 %s 性格: %v", key, err)
	}
	if _, err := db.AISavePersonality(ctx, key, cur.Config.Revision,
		personality(soc, exp, per, reg)); err != nil {
		t.Fatalf("保存 %s 性格: %v", key, err)
	}
}

// t108Tasks 读全部任务的 (kind, source, agent, participants)。
type t108TaskRow struct {
	ID           int64
	Kind         string
	SourceKind   string
	Agent        string
	Participants string
	Text         string
}

func t108Tasks(t *testing.T, db *store.DB) []t108TaskRow {
	t.Helper()
	rows, err := db.SQL.Query(`SELECT id, kind, source_kind, agent, participants, text
		FROM ai_task ORDER BY id`)
	if err != nil {
		t.Fatalf("读任务: %v", err)
	}
	defer rows.Close()
	var out []t108TaskRow
	for rows.Next() {
		var r t108TaskRow
		if err := rows.Scan(&r.ID, &r.Kind, &r.SourceKind, &r.Agent, &r.Participants, &r.Text); err != nil {
			t.Fatalf("扫任务: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func t108Count(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.SQL.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("统计 %q: %v", query, err)
	}
	return n
}

// TestT108ChannelRoundtableChain 真人频道圆桌：一次确实发生 + 一次合法沉默。
func TestT108ChannelRoundtableChain(t *testing.T) {
	ctx := context.Background()

	t.Run("合资格两人起桌", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_rt_ok", db)
		if err := db.AISetSetting(ctx, "roundtable_enabled", "1"); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"companion", "fishguide"} {
			t108Agent(t, db, "t108_rt_ok", key)
			grantCapabilities(t, db, key, aiconfig.CapabilityJoinRoundtable)
			t108Personality(t, db, key, 90, 60, 40, 50)
		}
		// 找一个「至少一个人愿意起头」的种子：两名 Agent 各按自己的四维判断。
		seed := int64(0)
		for s := int64(1); s <= 4096 && seed == 0; s++ {
			for _, key := range []string{"companion", "fishguide"} {
				p, _ := db.AIGetPersonality(ctx, key)
				want, _ := activityWants(p.Config, s^store.AIActorSeed(0, key), activityGatherFeature())
				if want {
					seed = s
					break
				}
			}
		}
		if seed == 0 {
			t.Fatal("找不到愿意起桌的种子")
		}
		t108SeedFixed(t, seed)

		probe.send(t, "你们今天钓得怎么样")
		tasks := t108Tasks(t, db)
		if len(tasks) != 1 {
			t.Fatalf("应恰好 1 条任务，实得 %d：%+v", len(tasks), tasks)
		}
		got := tasks[0]
		if got.Kind != "roundtable" || got.SourceKind != "human" {
			t.Fatalf("任务形状 = %s/%s，期望 roundtable/human", got.Kind, got.SourceKind)
		}
		for _, key := range []string{"companion", "fishguide"} {
			if !strings.Contains(got.Participants, key) {
				t.Fatalf("参与者缺 %s：%s", key, got.Participants)
			}
		}
		t.Logf("圆桌入队 task=%d participants=%s（种子 %d）", got.ID, got.Participants, seed)
	})

	t.Run("全部撤权则沉默", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_rt_off", db)
		if err := db.AISetSetting(ctx, "roundtable_enabled", "1"); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"companion", "fishguide"} {
			t108Agent(t, db, "t108_rt_off", key)
			// 只有攻略能力：不参与任何主动事件。
			grantCapabilities(t, db, key, aiconfig.CapabilityGuideHelp)
			t108Personality(t, db, key, 90, 60, 40, 50)
		}
		t108SeedFixed(t, 7)
		probe.send(t, "你们今天钓得怎么样")
		if n := len(t108Tasks(t, db)); n != 0 {
			t.Fatalf("全部撤权后不得入队，实得 %d 条", n)
		}
	})

	t.Run("越界点名不冒头", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_rt_scope", db)
		if err := db.AISetSetting(ctx, "roundtable_enabled", "1"); err != nil {
			t.Fatal(err)
		}
		// companion 不在本频道范围；fishguide 在。
		t108Agent(t, db, "t108_rt_scope", "fishguide")
		grantCapabilities(t, db, "fishguide", aiconfig.CapabilityJoinRoundtable)
		if err := db.AISetAgentJoined(ctx, "companion", true, 0); err != nil {
			t.Fatal(err)
		}
		grantCapabilities(t, db, "companion", aiconfig.CapabilityJoinRoundtable)
		probe.send(t, "@邀请 AI 伙伴 你们都在钓什么") // 点名越界角色 + 群称
		for _, task := range t108Tasks(t, db) {
			if task.Kind == "roundtable" && strings.Contains(task.Participants, "companion") {
				t.Fatalf("越界角色进了圆桌：%+v", task)
			}
			if task.Agent == "companion" {
				t.Fatalf("越界角色被点名后仍应答：%+v", task)
			}
		}
	})
}

// TestT108PlayerCatchChain 玩家上星/上蓝：一次确实发生 + 一次无资格沉默。
func TestT108PlayerCatchChain(t *testing.T) {
	ctx := context.Background()
	fish := &RolledFish{Grade: 3, Species: "perch", Name: "鲈鱼", WeightG: 2400}

	t.Run("有资格且愿意接一句", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_star_ok", db)
		t108Agent(t, db, "t108_star_ok", "companion")
		grantCapabilities(t, db, "companion",
			aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		t108Personality(t, db, "companion", 95, 80, 30, 50)
		p, _ := db.AIGetPersonality(ctx, "companion")
		seed := t108SeedForOutcome(t, p.Config,
			activityReplyFeature(3), true, store.AIActorSeed(0, "companion"))
		t108SeedFixed(t, seed)

		probe.srv.maybeAIPlayerCatch(ctx, t013SelfUID, fish)
		tasks := t108Tasks(t, db)
		if len(tasks) != 1 {
			t.Fatalf("应恰好 1 条任务，实得 %d：%+v", len(tasks), tasks)
		}
		got := tasks[0]
		if got.Kind != "channel" || got.SourceKind != "system" || got.Agent != "companion" {
			t.Fatalf("任务形状 = %+v，期望 channel/system/companion", got)
		}
		if !strings.Contains(got.Text, "玩家刚上了一条") || !strings.Contains(got.Text, "上星了") {
			t.Fatalf("事件文案不符：%q", got.Text)
		}
		t.Logf("上星反应入队 task=%d（种子 %d）", got.ID, seed)
	})

	t.Run("无资格则零入队", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_star_off", db)
		t108Agent(t, db, "t108_star_off", "companion")
		grantCapabilities(t, db, "companion", aiconfig.CapabilityGuideHelp)
		t108SeedFixed(t, 3)
		probe.srv.maybeAIPlayerCatch(ctx, t013SelfUID, fish)
		if n := len(t108Tasks(t, db)); n != 0 {
			t.Fatalf("无资格不得入队，实得 %d 条", n)
		}
	})

	t.Run("急停则零入队", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_star_stop", db)
		t108Agent(t, db, "t108_star_stop", "companion")
		grantCapabilities(t, db, "companion",
			aiconfig.CapabilityReplyStar, aiconfig.CapabilityJoinRoundtable)
		t108Personality(t, db, "companion", 95, 80, 30, 50)
		t108SeedFixed(t, 5)
		if err := db.AISetSetting(ctx, aiconfig.KeyEmergencyStop, "1"); err != nil {
			t.Fatal(err)
		}
		probe.srv.maybeAIPlayerCatch(ctx, t013SelfUID, fish)
		if n := len(t108Tasks(t, db)); n != 0 {
			t.Fatalf("急停期间不得入队，实得 %d 条", n)
		}
	})
}

// TestT108FishingChainDev113A AI 自己上鱼播报：四段诊断、独立配置与「发言失败仍一条鱼」。
func TestT108FishingChainDev113A(t *testing.T) {
	ctx := context.Background()

	seedFishing := func(t *testing.T, db *store.DB) string {
		t.Helper()
		// 只留上星一档，让事件档位可复现。
		if err := db.AISetSettings(ctx, map[string]string{
			"ai_fishing_star_weight": "1", "ai_fishing_super_weight": "0",
			"ai_fishing_near_weight": "0", "ai_fishing_miss_weight": "0",
		}); err != nil {
			t.Fatal(err)
		}
		loc, _ := runtimeGSRoster(t, db)
		runtimeGSSeedActor(t, db, "companion", loc, "fishing")
		// 推前 since_ts 只用于换目标用例；钓鱼链要的是「正在钓鱼」。
		return loc
	}

	t.Run("四段可分辨且送达", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_fish_ok", db)
		loc := seedFishing(t, db)
		t108Agent(t, db, "t108_fish_ok", "companion")
		grantCapabilities(t, db, "companion",
			aiconfig.CapabilityVirtualFishing, aiconfig.CapabilityJoinRoundtable,
			aiconfig.CapabilityBroadcastCatch)
		t108Personality(t, db, "companion", 95, 90, 40, 50)
		p, _ := db.AIGetPersonality(ctx, "companion")
		seed := t108SeedForOutcome(t, p.Config, activityCatchFeature(evStar), true, 0)
		t108SeedFixed(t, seed)

		// 「四段」只在本用例里记录，用于回执逐段核对。
		var stages []string
		prevStage := aiFishingStageForTest
		aiFishingStageForTest = func(stage, agentKey string, taskID int64) {
			stages = append(stages, stage)
		}
		t.Cleanup(func() { aiFishingStageForTest = prevStage })

		probe.srv.aiFishingForChannel(ctx, db, "t108_fish_ok", t013SelfUID)
		if n := t108Count(t, db, `SELECT COUNT(*) FROM ai_catch WHERE agent_key='companion'`); n != 1 {
			t.Fatalf("渔获应正好 1 条，实得 %d", n)
		}
		tasks := t108Tasks(t, db)
		if len(tasks) != 1 || tasks[0].Kind != "channel" || tasks[0].SourceKind != "system" {
			t.Fatalf("播报任务形状不符：%+v", tasks)
		}
		if tasks[0].Agent != "companion" || !strings.Contains(tasks[0].Text, "你刚钓上") {
			t.Fatalf("播报任务归属/文案不符：%+v", tasks[0])
		}
		// 送达段：真实写结果 → pollAIResults 推 24/27。
		taskID := tasks[0].ID
		if _, err := db.SQL.Exec(`UPDATE ai_task SET state='processing' WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		if err := db.AIComplete(ctx, taskID, "done", "好家伙，这条星鱼真漂亮", "", ""); err != nil {
			t.Fatal(err)
		}
		probe.srv.pollAIResults(ctx, db)
		if rec := probe.next(t); !strings.Contains(rec.text, "真漂亮") {
			t.Fatalf("频道投递内容 = %q", rec.text)
		}
		joined := strings.Join(stages, ",")
		for _, want := range []string{"attempt", "quota", "enqueue", "delivery"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("四段缺 %q：%v", want, stages)
			}
		}
		t.Logf("四段=%v task=%d loc=%s", stages, taskID, loc)
	})

	t.Run("只开垂钓不播报：有鱼无声", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_fish_silent", db)
		seedFishing(t, db)
		t108Agent(t, db, "t108_fish_silent", "companion")
		grantCapabilities(t, db, "companion", aiconfig.CapabilityVirtualFishing)
		t108Personality(t, db, "companion", 95, 90, 40, 50)
		t108SeedFixed(t, 11)
		var stages []string
		prevStage := aiFishingStageForTest
		aiFishingStageForTest = func(stage, agentKey string, taskID int64) {
			stages = append(stages, stage)
		}
		t.Cleanup(func() { aiFishingStageForTest = prevStage })
		probe.srv.aiFishingForChannel(ctx, db, "t108_fish_silent", t013SelfUID)
		if n := t108Count(t, db, `SELECT COUNT(*) FROM ai_catch WHERE agent_key='companion'`); n != 1 {
			t.Fatalf("垂钓仍然产鱼，实得 %d", n)
		}
		if n := len(t108Tasks(t, db)); n != 0 {
			t.Fatalf("没有播报能力不得入队，实得 %d", n)
		}
		// 旧静默节拍仍在：没有播报角色的频道，每次抽鱼都占配额。
		joined := strings.Join(stages, ",")
		if !strings.Contains(joined, "attempt") || !strings.Contains(joined, "quota") {
			t.Fatalf("静默节拍应仍然占额：%v", stages)
		}
		if strings.Contains(joined, "enqueue") {
			t.Fatalf("没有播报能力不应有入队段：%v", stages)
		}
	})

	t.Run("不开垂钓：无鱼无任务", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_fish_off", db)
		seedFishing(t, db)
		t108Agent(t, db, "t108_fish_off", "companion")
		grantCapabilities(t, db, "companion",
			aiconfig.CapabilityJoinRoundtable, aiconfig.CapabilityBroadcastCatch)
		t108Personality(t, db, "companion", 95, 90, 40, 50)
		t108SeedFixed(t, 11)
		probe.srv.aiFishingForChannel(ctx, db, "t108_fish_off", t013SelfUID)
		if n := t108Count(t, db, `SELECT COUNT(*) FROM ai_catch`); n != 0 {
			t.Fatalf("无垂钓能力不得产鱼，实得 %d", n)
		}
		if n := len(t108Tasks(t, db)); n != 0 {
			t.Fatalf("无垂钓能力不得入队，实得 %d", n)
		}
	})

	t.Run("发言入队失败仍只有一条鱼", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_fish_fail", db)
		seedFishing(t, db)
		t108Agent(t, db, "t108_fish_fail", "companion")
		grantCapabilities(t, db, "companion",
			aiconfig.CapabilityVirtualFishing, aiconfig.CapabilityJoinRoundtable,
			aiconfig.CapabilityBroadcastCatch)
		t108Personality(t, db, "companion", 95, 90, 40, 50)
		p, _ := db.AIGetPersonality(ctx, "companion")
		seed := t108SeedForOutcome(t, p.Config, activityCatchFeature(evStar), true, 0)
		t108SeedFixed(t, seed)
		if _, err := db.SQL.Exec(`CREATE TRIGGER t108_block_task BEFORE INSERT ON ai_task
			BEGIN SELECT RAISE(ABORT, 'task blocked'); END`); err != nil {
			t.Fatalf("安装失败夹具: %v", err)
		}
		probe.srv.aiFishingForChannel(ctx, db, "t108_fish_fail", t013SelfUID)
		if n := t108Count(t, db, `SELECT COUNT(*) FROM ai_catch WHERE agent_key='companion'`); n != 1 {
			t.Fatalf("发言失败后已提交渔获应保持 1 条，实得 %d", n)
		}
		if n := len(t108Tasks(t, db)); n != 0 {
			t.Fatalf("入队失败不应留下任务，实得 %d", n)
		}
	})
}

// TestT108GuideOnlyAgent 仅攻略角色：能直接答攻略与基础私聊，主动事件不误入队。
func TestT108GuideOnlyAgent(t *testing.T) {
	ctx := context.Background()

	t.Run("被点名可直接答", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_guide_ask", db)
		if err := db.AISetSetting(ctx, "roundtable_enabled", "1"); err != nil {
			t.Fatal(err)
		}
		t108Agent(t, db, "t108_guide_ask", "fishguide")
		grantCapabilities(t, db, "fishguide", aiconfig.CapabilityGuideHelp)
		probe.send(t, "鱼情助手 鲈鱼用什么饵")
		tasks := t108Tasks(t, db)
		if len(tasks) != 1 || tasks[0].Agent != "fishguide" || tasks[0].Kind != "channel" {
			t.Fatalf("直接求助应得到 1 条单人回复任务：%+v", tasks)
		}
		if tasks[0].SourceKind != "human" {
			t.Fatalf("直接求助来源应为 human，实得 %s", tasks[0].SourceKind)
		}
	})

	t.Run("未被点名不当替补", func(t *testing.T) {
		db := openStaticTestDB(t)
		probe := newT013ChatProbeWithDB(t, "t108_guide_idle", db)
		if err := db.AISetSetting(ctx, "roundtable_enabled", "1"); err != nil {
			t.Fatal(err)
		}
		t108Agent(t, db, "t108_guide_idle", "fishguide")
		grantCapabilities(t, db, "fishguide", aiconfig.CapabilityGuideHelp)
		t108SeedFixed(t, 3)
		probe.send(t, "今天水面挺安静的")
		if n := len(t108Tasks(t, db)); n != 0 {
			t.Fatalf("仅攻略角色不得被当主动接话替补，实得 %d 条", n)
		}
	})

	t.Run("基础私聊不受事件能力限制", func(t *testing.T) {
		db := openStaticTestDB(t)
		srv := runtimeGSServer(t, db)
		if err := db.AISetAgentJoined(ctx, "fishguide", true, 0); err != nil {
			t.Fatal(err)
		}
		grantCapabilities(t, db, "fishguide", aiconfig.CapabilityGuideHelp)
		sess := &Session{ID: 99, Userid: t013SelfUID, Srv: srv}
		sess.SetState("channel", map[string]any{"name": "t108_guide_dm", "region": "CN", "users": 1})
		cctx := miscCtx(sess, 19, 1, proto.Args524(proto.StrVal("鲈鱼吃什么")))
		cctx.ExpectResponse = true
		body, ok := srv.receiveAIChat(cctx, 2147483644, "fishguide", "鲈鱼吃什么")
		if !ok || len(body) == 0 {
			t.Fatal("基础私聊应被受理")
		}
		tasks := t108Tasks(t, db)
		if len(tasks) != 1 || tasks[0].Kind != "dm" || tasks[0].SourceKind != "human" {
			t.Fatalf("私聊任务形状不符：%+v", tasks)
		}
	})
}

// TestT108TargetSuggestArbitration 目标建议的两道闸：硬驻留、W 阈值、邀请去重。
func TestT108TargetSuggestArbitration(t *testing.T) {
	ctx := context.Background()
	applyWith := func(t *testing.T, db *store.DB, srv *Server, reqID string,
		elapsed float64, personality aiconfig.PersonalityConfig) (*store.AIRuntimeCommand, *store.AIActor) {
		t.Helper()
		if _, err := db.SQL.Exec(`UPDATE ai_actor_state SET since_ts=?, switch_ts=0 WHERE agent_key=?`,
			float64(time.Now().Unix())-elapsed, runtimeGSAgent); err != nil {
			t.Fatal(err)
		}
		if cur, err := db.AIGetPersonality(ctx, runtimeGSAgent); err != nil {
			t.Fatalf("读性格: %v", err)
		} else if _, err := db.AISavePersonality(ctx, runtimeGSAgent,
			cur.Config.Revision, personality); err != nil {
			t.Fatalf("保存性格: %v", err)
		}
		loc, fish := runtimeGSRoster(t, db)
		rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityAcceptTarget)
		cmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
			RequestID: reqID, Source: store.AIRuntimeSourceBridge,
			Kind: store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
			ExpectedGeneration: 1, ExpectedRevision: 0,
			Payload: targetPayload(fish, loc, rev),
		})
		got := runtimeGSApply(t, srv, cmd.ID)
		return got, runtimeGSActorRow(t, db, runtimeGSAgent)
	}

	applyID := func(base int) string { return fmt.Sprintf("bridge:%d:companion:1", base) }

	t.Run("硬驻留拒绝", func(t *testing.T) {
		db := openStaticTestDB(t)
		srv := runtimeGSServer(t, db)
		runtimeGSSeedActor(t, db, runtimeGSAgent, "level_other_placeholder", "fishing")
		activityInvites.reset()
		got, actor := applyWith(t, db, srv, applyID(9201), 10,
			personality(100, 100, 0, 50))
		if got.Status != store.AIRuntimeStatusRejected || got.Reason != "hard_dwell" {
			t.Fatalf("未过硬驻留应 hard_dwell，实得 %s/%s", got.Status, got.Reason)
		}
		if actor.TargetFish == "" {
			t.Log("角色状态未被换目标（期望）")
		}
	})

	t.Run("高交往高尝新接受", func(t *testing.T) {
		db := openStaticTestDB(t)
		srv := runtimeGSServer(t, db)
		runtimeGSSeedActor(t, db, runtimeGSAgent, "level_other_placeholder", "fishing")
		activityInvites.reset()
		got, actor := applyWith(t, db, srv, applyID(9202), 1300,
			personality(100, 100, 0, 50))
		if got.Status != store.AIRuntimeStatusApplied {
			t.Fatalf("高交往高尝新应接受，实得 %s/%s", got.Status, got.Reason)
		}
		loc, fish := runtimeGSRoster(t, db)
		if actor.TargetFish != fish || actor.Loc != loc {
			t.Fatalf("建议未落到状态：%+v", actor)
		}
	})

	t.Run("高持续投入拒绝换目标", func(t *testing.T) {
		db := openStaticTestDB(t)
		srv := runtimeGSServer(t, db)
		runtimeGSSeedActor(t, db, runtimeGSAgent, "level_other_placeholder", "fishing")
		activityInvites.reset()
		got, _ := applyWith(t, db, srv, applyID(9203), 4000,
			personality(0, 0, 100, 50))
		if got.Status != store.AIRuntimeStatusRejected || got.Reason != "low_affinity" {
			t.Fatalf("高持续投入应 low_affinity，实得 %s/%s", got.Status, got.Reason)
		}
	})

	t.Run("同玩家同邀请第四次拒绝", func(t *testing.T) {
		db := openStaticTestDB(t)
		srv := runtimeGSServer(t, db)
		runtimeGSSeedActor(t, db, runtimeGSAgent, "level_other_placeholder", "fishing")
		// 高持续投入：前三次都被亲和度挡回（不加冷却），第四次算重复邀请。
		t108Personality(t, db, runtimeGSAgent, 0, 0, 100, 50)
		activityInvites.reset()
		loc, fish := runtimeGSRoster(t, db)
		rev := grantCapabilities(t, db, runtimeGSAgent, aiconfig.CapabilityAcceptTarget)
		if _, err := db.SQL.Exec(`UPDATE ai_actor_state SET since_ts=? WHERE agent_key=?`,
			float64(time.Now().Unix())-4000, runtimeGSAgent); err != nil {
			t.Fatal(err)
		}
		var last *store.AIRuntimeCommand
		for i := 1; i <= 4; i++ {
			// 每次重新读当前代次/版本：前一条应用成功会推进 revision。
			ctl, err := db.AIGetRuntimeControl(ctx, runtimeGSAgent)
			if err != nil {
				t.Fatalf("读控制行: %v", err)
			}
			cmd := runtimeGSEnqueue(t, db, store.AIRuntimeEnqueueRequest{
				RequestID: fmt.Sprintf("bridge:%d:companion:1", 9100+i),
				Source:    store.AIRuntimeSourceBridge,
				Kind:      store.AIRuntimeKindActorTargetSuggest, AgentKey: runtimeGSAgent,
				ExpectedGeneration: ctl.Generation, ExpectedRevision: ctl.Revision,
				Payload: targetPayload(fish, loc, rev),
			})
			last = runtimeGSApply(t, srv, cmd.ID)
		}
		if last.Status != store.AIRuntimeStatusRejected || last.Reason != "duplicate_invite" {
			t.Fatalf("第 4 次同邀请应 duplicate_invite，实得 %s/%s", last.Status, last.Reason)
		}
	})
}
