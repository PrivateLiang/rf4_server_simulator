package aibridge

// T-020 / DEV-004：BR 侧边界测试。
//
// aibridge 不再是运行态写者：actorLine 只读已确认状态，状态缺失时省略位置句；
// 玩家安利鱼时只提交 actor_target_suggest，不推进、不初始化、不整行写回。

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

func bridgeTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func openBridgeRuntimeDB(t *testing.T) *store.DB {
	t.Helper()
	d, err := store.Open(filepath.Join(t.TempDir(), "bridge-runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(); err != nil {
		_ = d.Close()
		t.Fatalf("迁移: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func seedBridgeActor(t *testing.T, d *store.DB, agentKey, loc, locName, fishKey, fishName string, until float64) float64 {
	t.Helper()
	now := float64(time.Now().Unix())
	if _, err := d.SQL.Exec(`
		INSERT INTO ai_actor_state(agent_key, loc, loc_name, activity, target_fish, target_name,
		                           since_ts, loc_since_ts, switch_ts, activity_until_ts, energy, updated_ts)
		VALUES(?,?,?,'fishing',?,?,?,?,0,?,55,?)
		ON CONFLICT(agent_key) DO UPDATE SET loc=excluded.loc, activity='fishing',
			target_fish=excluded.target_fish, target_name=excluded.target_name,
			activity_until_ts=excluded.activity_until_ts, updated_ts=excluded.updated_ts`,
		agentKey, loc, locName, fishKey, fishName, now, now, until, now); err != nil {
		t.Fatalf("seed 角色状态: %v", err)
	}
	if _, err := d.SQL.Exec(`UPDATE ai_agent SET actor_enabled=1 WHERE key=?`, agentKey); err != nil {
		t.Fatalf("打开角色开关: %v", err)
	}
	// T-108：换目标的第二闸由 T-103 四维决定；夹具给「乐于交往 + 愿意尝新」，
	// 让 GS 的仲裁用例聚焦在代次/权限，而不是亲和度。
	if _, err := d.AISavePersonality(context.Background(), agentKey, 0,
		aiconfig.PersonalityConfig{
			SchemaVersion: aiconfig.PersonalitySchemaVersion,
			Sociability:   100, Exploration: 100,
		}); err != nil {
		t.Fatalf("seed 性格: %v", err)
	}
	// 第一闸要求已过硬驻留：把这一段开始时间推到 Dmin 之后。
	if _, err := d.SQL.Exec(`UPDATE ai_actor_state SET since_ts=since_ts-1300 WHERE agent_key=?`,
		agentKey); err != nil {
		t.Fatalf("推前 since_ts: %v", err)
	}
	return now
}

// TestAISingleWriterBridgeActorLineReadOnly：状态缺失不初始化、到期不推进。
func TestAISingleWriterBridgeActorLineReadOnly(t *testing.T) {
	d := openBridgeRuntimeDB(t)
	ctx := context.Background()
	r := &Runner{db: d}

	line, loc := r.actorLine(ctx, "companion", "level_001_lake")
	if line != "" || loc != "" {
		t.Fatalf("状态缺失时应省略位置句，实得 line=%q loc=%q", line, loc)
	}
	var rows int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_actor_state`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("BR 读取时擅自初始化了 %d 行角色状态", rows)
	}

	// 已到期（activity_until_ts 远过去）的状态：只读描述，不推进、不写回。
	seedBridgeActor(t, d, "companion", "level_001_lake", "测试湖", "perch", "鲈鱼",
		float64(time.Now().Unix())-10)
	// 固定一个远古 updated_ts 作为「BR 是否写过」的哨兵：同一秒内的 now 变化
	// 会让「写回当前时间」看不出来，哨兵值才能稳定抓到直接写入。
	if _, err := d.SQL.Exec(`UPDATE ai_actor_state SET updated_ts=1000 WHERE agent_key='companion'`); err != nil {
		t.Fatalf("设置哨兵 updated_ts: %v", err)
	}
	var activity string
	var until, updatedTS float64
	if err := d.SQL.QueryRow(`SELECT activity, activity_until_ts, updated_ts FROM ai_actor_state WHERE agent_key='companion'`).
		Scan(&activity, &until, &updatedTS); err != nil {
		t.Fatal(err)
	}
	line, loc = r.actorLine(ctx, "companion", "")
	if line == "" || loc != "level_001_lake" {
		t.Fatalf("已确认状态应注入: line=%q loc=%q", line, loc)
	}
	if !strings.Contains(line, "测试湖") {
		t.Fatalf("位置句缺中文水域名: %q", line)
	}
	var activity2 string
	var until2, updatedTS2 float64
	if err := d.SQL.QueryRow(`SELECT activity, activity_until_ts, updated_ts FROM ai_actor_state WHERE agent_key='companion'`).
		Scan(&activity2, &until2, &updatedTS2); err != nil {
		t.Fatal(err)
	}
	if activity2 != activity || until2 != until || updatedTS2 != updatedTS {
		t.Fatalf("BR 读取改写了状态: activity %s→%s until %.0f→%.0f updated %.0f→%.0f",
			activity, activity2, until, until2, updatedTS, updatedTS2)
	}
}

func bridgeStaticKnowledge(t *testing.T) *Knowledge {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "static.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE loc_fish(loc TEXT, fish_key TEXT, name_zh TEXT, catches INTEGER);
		CREATE TABLE loc_alias(loc TEXT, alias TEXT);
		INSERT INTO loc_fish VALUES('测试湖','perch','鲈鱼',10),
		                           ('测试湖','pike','狗鱼',5),
		                           ('测试湖','carp','鲤鱼',1);
		INSERT INTO loc_alias VALUES('测试湖','level_001_lake');`); err != nil {
		t.Fatalf("造静态夹具: %v", err)
	}
	return &Knowledge{static: db}
}

// TestAIRuntimeCommandBridgeSuggestion：目标建议只能由模型写工具提交，
// request_id 由服务端按工具调用生成，重试稳定、异载荷冲突、BR 不自行应用。
func TestAIRuntimeCommandBridgeSuggestion(t *testing.T) {
	d := openBridgeRuntimeDB(t)
	ctx := context.Background()
	seedBridgeActor(t, d, "companion", "level_002_river", "测试河", "pike", "狗鱼",
		float64(time.Now().Unix())+3600)
	r := &Runner{db: d, knowledge: bridgeStaticKnowledge(t), log: bridgeTestLogger()}
	st, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil)
	if err != nil {
		t.Fatalf("保存能力: %v", err)
	}
	if st.Revision != 1 || !st.Has(aiconfig.CapabilityAcceptTarget) {
		t.Fatalf("能力投影 = %+v", st)
	}

	id, err := d.AIEnqueue(ctx, &store.AITask{Kind: "channel", SourceKind: "human", Text: "鲈鱼今天口怎么样"})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok || task.ID != id {
		t.Fatalf("claim parent: %v/%v", ok, err)
	}
	ctx = withAITaskIntent(ctx, task)

	res, err := r.enqueueTargetSuggest(ctx, task, "companion", st.Revision, "鲈鱼", "", "call-1")
	if err != nil {
		t.Fatalf("提交建议: %v", err)
	}
	if res.Status != "accepted" || res.RequestID == "" || res.Data != "已提交，等待游戏服务确认。" {
		t.Fatalf("受理回执 = %+v", res)
	}
	if !strings.HasPrefix(res.RequestID, "ai-") || len(res.RequestID) != 35 {
		t.Fatalf("request_id 不是服务端 hash: %q", res.RequestID)
	}
	// 同一次工具调用重试：复用同一 request_id，只有一行。
	again, err := r.enqueueTargetSuggest(ctx, task, "companion", st.Revision, "鲈鱼", "", "call-1")
	if err != nil || again.RequestID != res.RequestID {
		t.Fatalf("重试应复用 request_id: %+v/%v", again, err)
	}
	// 同一任务换一个 call id 再提同一条建议：仍只有一条（一次任务一条建议）。
	third, err := r.enqueueTargetSuggest(ctx, task, "companion", st.Revision, "鲈鱼", "", "call-2")
	if err != nil || third.RequestID != res.RequestID {
		t.Fatalf("同任务应收敛到同一 request_id: %+v/%v", third, err)
	}

	var rows int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("建议命令 = %d 行，期望 1", rows)
	}
	var requestID, source, kind, agentKey, payload, status string
	var expectedGen, expectedRev int64
	if err := d.SQL.QueryRow(`SELECT request_id, source, kind, agent_key, payload,
		expected_generation, expected_revision, status FROM ai_runtime_command`).
		Scan(&requestID, &source, &kind, &agentKey, &payload, &expectedGen, &expectedRev, &status); err != nil {
		t.Fatal(err)
	}
	if requestID != res.RequestID || source != store.AIRuntimeSourceBridge ||
		kind != store.AIRuntimeKindActorTargetSuggest || agentKey != "companion" {
		t.Fatalf("建议命令 = %s/%s/%s/%s", requestID, source, kind, agentKey)
	}
	if expectedGen != 1 || expectedRev != 0 {
		t.Fatalf("建议期望代次/版本 = %d/%d，期望 1/0", expectedGen, expectedRev)
	}
	if !strings.Contains(payload, `"fish_key":"perch"`) ||
		!strings.Contains(payload, `"capability":"accept_target_suggest"`) ||
		!strings.Contains(payload, `"cap_revision":1`) {
		t.Fatalf("建议 payload = %s", payload)
	}
	if !strings.Contains(payload, `"loc_key":"level_001_lake"`) {
		t.Fatalf("建议应选渔获最多的地图: %s", payload)
	}
	if status != store.AIRuntimeStatusPending {
		t.Fatalf("BR 不应自行应用建议，status=%s", status)
	}
	// 状态行没有被 BR 改写。
	var target, loc string
	if err := d.SQL.QueryRow(`SELECT target_fish, loc FROM ai_actor_state WHERE agent_key='companion'`).
		Scan(&target, &loc); err != nil {
		t.Fatal(err)
	}
	if target != "pike" || loc != "level_002_river" {
		t.Fatalf("BR 直接改了运行态: target=%s loc=%s", target, loc)
	}

	// 同 request_id、不同载荷：拒绝，不产生第二行。
	if _, err := r.enqueueTargetSuggest(ctx, task, "companion", st.Revision, "鲤鱼", "", "call-1"); err == nil {
		t.Fatal("异载荷应被拒绝")
	}
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("异载荷产生了新命令: %d 行", rows)
	}

	// 已到当前目标：不再提交。
	if _, err := r.enqueueTargetSuggest(ctx, task, "companion", st.Revision, "狗鱼", "", "call-2"); err != nil {
		t.Fatalf("当前目标不应报错: %v", err)
	}

	// AI 角色总闸关闭时不提交建议。
	if err := d.AISetSetting(ctx, "ai_actor_enabled", "0"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.enqueueTargetSuggest(ctx, task, "companion", st.Revision, "鲈鱼", "", "call-3"); err == nil {
		t.Fatal("总闸关闭后仍提交了建议")
	}
}

// request_id 完全由服务端输入决定：同 (agent,gen,task,call,tool) 恒定，
// 换 call id 才不同；模型不能指定 ID 或身份。
func TestActorTargetRequestIDStable(t *testing.T) {
	a := actorTargetRequestID(actorTargetNamespace, "companion", 1, 42, "call-1", aiconfig.ToolActorTargetSuggest)
	b := actorTargetRequestID(actorTargetNamespace, "companion", 1, 42, "call-1", aiconfig.ToolActorTargetSuggest)
	if a != b {
		t.Fatalf("同输入 request_id 不稳定: %s / %s", a, b)
	}
	if a == actorTargetRequestID(actorTargetNamespace, "companion", 1, 42, "call-2", aiconfig.ToolActorTargetSuggest) {
		t.Fatal("不同 call id 应得到不同 request_id")
	}
	if len(a) != 35 || !strings.HasPrefix(a, "ai-") {
		t.Fatalf("request_id 形状 = %q", a)
	}
}

// 旧 actor.go 旁路同样受限：没有「接受目标建议」就完全不提交；
// 获准后同一任务重复触发仍只有一条命令。
func TestMaybeSwitchActorRequiresCapability(t *testing.T) {
	d := openBridgeRuntimeDB(t)
	ctx := context.Background()
	seedBridgeActor(t, d, "companion", "level_002_river", "测试河", "pike", "狗鱼",
		float64(time.Now().Unix())+3600)
	r := &Runner{db: d, knowledge: bridgeStaticKnowledge(t), log: bridgeTestLogger()}
	id, err := d.AIEnqueue(ctx, &store.AITask{Kind: "dm", SourceKind: "human", Agent: "companion",
		Text: "鲈鱼今天口怎么样"})
	if err != nil {
		t.Fatal(err)
	}
	task, ok, err := d.AIPopTask(ctx)
	if err != nil || !ok || task.ID != id {
		t.Fatalf("claim: %v/%v", ok, err)
	}
	ctx = withAITaskIntent(ctx, task)

	// 只有旧 tools_json（map_vibe）→ 无写能力，旁路不提交。
	r.maybeSwitchActor(ctx, task, "companion")
	var rows int
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("无能力时旁路提交了 %d 条", rows)
	}

	if _, err := d.AISaveCapabilities(ctx, "companion", 0,
		[]string{aiconfig.CapabilityGuideHelp, aiconfig.CapabilityAcceptTarget}, nil); err != nil {
		t.Fatal(err)
	}
	r.maybeSwitchActor(ctx, task, "companion")
	r.maybeSwitchActor(ctx, task, "companion")
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("获准后重复触发 = %d 条，期望 1", rows)
	}

	// 系统任务不是玩家建议。
	sys := &store.AITask{ID: task.ID + 1, Kind: "channel", SourceKind: "system", Agent: "companion",
		Text: "鲈鱼今天口怎么样"}
	r.maybeSwitchActor(ctx, sys, "companion")
	if err := d.SQL.QueryRow(`SELECT COUNT(*) FROM ai_runtime_command`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("系统任务触发了旁路: %d 条", rows)
	}
}
