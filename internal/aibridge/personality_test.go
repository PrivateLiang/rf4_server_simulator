package aibridge

// T-103 表达编译接线：普通回复与圆桌都从「同一任务固定的一份四维快照」取约束，
// 没有配置时 system 与改造前逐字相同（关掉派生即回到原表达）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

func openPersonalityBridgeDB(t *testing.T, name, agentKey string) *Runner {
	t.Helper()
	d, err := store.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if agentKey != "" {
		if _, _, err := d.AICreateAgent(context.Background(), store.AIAgent{
			Key: agentKey, Display: "钓友", Role: "测试", ThinkingEffort: "off", ReplyMode: "social",
		}, "[]"); err != nil {
			t.Fatalf("建 Agent: %v", err)
		}
	}
	return &Runner{db: d}
}

func saveTestPersonality(t *testing.T, r *Runner, key string, base int64, s, e, p, rg int) int64 {
	t.Helper()
	cfg := aiconfig.PersonalityConfig{
		SchemaVersion: aiconfig.PersonalitySchemaVersion,
		Sociability:   s, Exploration: e, Persistence: p, Regularity: rg,
	}
	rev, err := r.db.AISavePersonality(context.Background(), key, base, cfg)
	if err != nil {
		t.Fatalf("保存性格: %v", err)
	}
	return rev
}

// TestExpressionsLineSameRevisionBothScenes 普通回复（600 字人设）与圆桌（400 字人设）
// 拿到同一修订的四句约束。
func TestExpressionsLineSameRevisionBothScenes(t *testing.T) {
	ctx := context.Background()
	r := openPersonalityBridgeDB(t, "bridge-personality.db", "hutao")
	saveTestPersonality(t, r, "hutao", 0, 85, 15, 70, 30)

	channel := r.expressionsLine(ctx, "hutao", "你是一位话不多的钓友。", 600)
	roundtable := r.expressionsLine(ctx, "hutao", "你是一位话不多的钓友。", 400)
	for name, got := range map[string]string{"channel": channel, "roundtable": roundtable} {
		if !strings.HasPrefix(got, "你是一位话不多的钓友。") {
			t.Fatalf("%s 丢了人设背景：%q", name, got)
		}
		for _, want := range []string{"性格约束", "schema v1", "修订 1", "交往意愿 85/100（偏高）", "尝新意愿 15/100（偏低）"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s 表达缺 %q：\n%s", name, want, got)
			}
		}
	}
	if channel != roundtable {
		t.Fatalf("同一修订两个场景应得到同一段约束：\n%s\n---\n%s", channel, roundtable)
	}
}

// TestExpressionsLineWithoutConfigIsUnchanged 没有四维配置时只返回原提示词：
// 关掉派生（切回旧读取路径）就回到改造前的 system。
func TestExpressionsLineWithoutConfigIsUnchanged(t *testing.T) {
	ctx := context.Background()
	r := openPersonalityBridgeDB(t, "bridge-personality-none.db", "hutao")
	prompt := "你是一位话不多的钓友，喜欢在湖边坐着。"
	got := r.expressionsLine(ctx, "hutao", prompt, 600)
	if got != trimToTokens(prompt, 600) {
		t.Fatalf("缺值时不应改动 system：\n%q\n%q", got, trimToTokens(prompt, 600))
	}
	if strings.Contains(got, "性格约束") {
		t.Fatalf("缺值不应凭空加约束：%q", got)
	}
	// 空人设时也不会变成纯空串。
	if got := r.expressionsLine(ctx, "hutao", "", 600); got != "" {
		t.Fatalf("空人设 + 缺值应保持空: %q", got)
	}
}

// TestExpressionsLineSkippedForRepair 显式非法配置不产出新的派生表达。
func TestExpressionsLineSkippedForRepair(t *testing.T) {
	ctx := context.Background()
	r := openPersonalityBridgeDB(t, "bridge-personality-bad.db", "hutao")
	if _, err := r.db.SQL.Exec(`INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,0)`,
		"ai_personality:hutao",
		`{"schema_version":1,"revision":3,"generation":1,"sociability":-40,"exploration":50,"persistence":50,"regularity":50}`); err != nil {
		t.Fatal(err)
	}
	prompt := "你是一位话不多的钓友。"
	got := r.expressionsLine(ctx, "hutao", prompt, 600)
	if got != trimToTokens(prompt, 600) {
		t.Fatalf("非法配置应停止派生表达：%q", got)
	}
	state, err := r.db.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if state.Derivable() || !state.NeedsRepair {
		t.Fatalf("应标记需修复且不可派生: %+v", state)
	}
}

// TestPersonalitySnapshotHeldPerTask 已开始的任务读到的修订不变，下一任务才看到新版本。
func TestPersonalitySnapshotHeldPerTask(t *testing.T) {
	ctx := context.Background()
	r := openPersonalityBridgeDB(t, "bridge-personality-rev.db", "hutao")
	first := saveTestPersonality(t, r, "hutao", 0, 80, 20, 60, 40)

	// 第一个任务开始时取快照。
	inFlight, ok := r.personalityConfig(ctx, "hutao")
	if !ok || inFlight.Revision != first {
		t.Fatalf("任务开始应取到修订 %d: %+v", first, inFlight)
	}
	// 任务进行中管理员又存了一版。
	second := saveTestPersonality(t, r, "hutao", first, 20, 80, 40, 60)
	if second <= first {
		t.Fatalf("新保存应推进修订: %d -> %d", first, second)
	}
	// 已经开始的任务手里的快照不变。
	if inFlight.Revision != first || inFlight.Sociability != 80 {
		t.Fatalf("已开始任务的快照被改了: %+v", inFlight)
	}
	// 下一个任务读到新版本。
	next, ok := r.personalityConfig(ctx, "hutao")
	if !ok || next.Revision != second || next.Sociability != 20 {
		t.Fatalf("下一任务应看到新修订 %d: %+v", second, next)
	}
}

// TestExpressionsLineIgnoresLegacyProbabilities 旧活跃度与旧概率不是表达输入。
func TestExpressionsLineIgnoresLegacyProbabilities(t *testing.T) {
	ctx := context.Background()
	r := openPersonalityBridgeDB(t, "bridge-personality-legacy.db", "hutao")
	saveTestPersonality(t, r, "hutao", 0, 50, 50, 50, 50)
	before := r.expressionsLine(ctx, "hutao", "你是钓友。", 600)

	if err := r.db.AISetAgentBehavior(ctx, "hutao", "silent", 0); err != nil {
		t.Fatal(err)
	}
	if err := r.db.AISetSettings(ctx, map[string]string{
		"roundtable_chime_chance":     "0.99",
		"roundtable_on_speech_chance": "0.99",
		"ai_fishing_talk_star_chance": "0",
		"ai_catch_star_chance":        "0",
	}); err != nil {
		t.Fatal(err)
	}
	after := r.expressionsLine(ctx, "hutao", "你是钓友。", 600)
	if before != after {
		t.Fatalf("旧概率不应影响表达：\n%s\n---\n%s", before, after)
	}
	// 配置本身也没被旧字段改动。
	state, err := r.db.AIGetPersonality(ctx, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if state.Config.Sociability != 50 || state.Config.Revision != 1 {
		t.Fatalf("旧字段改动影响了四维: %+v", state.Config)
	}
}

// TestRunnerExpressionWiringIsShared 静态断言：普通回复与圆桌都走同一条表达装配，
// 且装配用的是调用方已经取好的同一份四维快照（expressionsLineOf），没有人绕开它
// 直接用旧的人设裁剪。改回旧读取路径会让这条用例变红。
func TestRunnerExpressionWiringIsShared(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	if err != nil {
		t.Fatalf("读取 runner.go: %v", err)
	}
	text := string(src)
	must := []string{
		"system := r.expressionsLineOf(agent.Prompt, 600, persona, hasPersona)",
		"exprLines[a.Key] = r.expressionsLineOf(a.Prompt, 400, persona, hasPersona)",
		"system := \"你是「\" + a.Display + \"」。\" + exprLines[a.Key]",
		// 编译进提示词的修订与写进表达凭据的修订必须来自同一次快照。
		"persona, hasPersona, personaReason := r.personalitySnapshot(ctx, agent.Key, binding)",
	}
	for _, want := range must {
		if !strings.Contains(text, want) {
			t.Errorf("runner.go 缺接线：%s", want)
		}
	}
	mustNot := []string{
		"system := trimToTokens(agent.Prompt, 600)",
		"system := \"你是「\" + a.Display + \"」。\" + trimToTokens(a.Prompt, 400)",
	}
	for _, banned := range mustNot {
		if strings.Contains(text, banned) {
			t.Errorf("runner.go 仍在走旧人设路径（绕开同版表达）：%s", banned)
		}
	}
	// 兼容包装 1 处 + 两处真实调用 + 一处定义 = 4 次出现；多出来说明有人另加了路径。
	if n := strings.Count(text, "expressionsLineOf("); n != 4 {
		t.Fatalf("expressionsLineOf 出现 %d 次，期望 2 处调用 + 兼容包装 + 1 处定义", n)
	}
}
