package admin

import (
	"context"
	"encoding/json"
	"math"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// TestSettingSpecsComplete 确保注册表覆盖所有原有 key，且没有重复或未归属模块的项。
func TestSettingSpecsComplete(t *testing.T) {
	// 这份清单来自重构前 handleAIStatus 的历史 key（V2 起不再有 outbound_enabled）。
	legacy := []string{
		"web_search_enabled", "emergency_stop", "search_endpoint", "max_tool_rounds",
		"roundtable_enabled", "roundtable_max_speakers", "roundtable_max_turns", "roundtable_max_ms",
		"roundtable_cooldown_ms", "roundtable_hourly_limit", "roundtable_on_speech_chance",
		"roundtable_chime_chance", "roundtable_gap_ms", "roundtable_max_streak",
		"ai_actor_enabled", "ai_actor_switch_chance", "ai_actor_switch_cooldown_s",
		"ai_actor_fishing_session_min_s", "ai_actor_fishing_session_max_s",
		"ai_actor_rest_min_s", "ai_actor_rest_max_s", "ai_actor_rest_chance",
		"ai_fishing_enabled", "ai_fishing_hourly_limit", "ai_fishing_base_interval_ms",
		"ai_fishing_max_interval_ms", "ai_fishing_star_weight", "ai_fishing_super_weight",
		"ai_fishing_miss_weight", "ai_fishing_near_weight", "ai_fishing_miss_ratio", "ai_fishing_near_ratio",
		"ai_catch_talk", "ai_catch_star_chance", "ai_catch_super_chance", "ai_catch_roundtable_chance",
		"ai_profile_internal_chars", "ai_profile_dm_context_tokens", "ai_profile_dm_history_messages",
		"ai_profile_dm_history_chars", "ai_profile_dm_reply_chars", "ai_profile_dm_reply_soft_chars",
		"ai_profile_channel_context_tokens", "ai_profile_channel_history_messages",
		"ai_profile_channel_history_chars", "ai_profile_channel_reply_chars", "ai_profile_channel_reply_soft_chars",
		"ai_profile_roundtable_context_tokens", "ai_profile_roundtable_context_messages",
		"ai_profile_roundtable_reply_chars", "ai_profile_roundtable_reply_soft_chars",
		"ai_memory_inject", "ai_memory_recent", "ai_memory_best", "ai_memory_max_rows",
		"ai_catch_brief_inject",
	}
	for _, k := range legacy {
		if _, ok := settingByKey[k]; !ok {
			t.Errorf("注册表缺原有 key: %s", k)
		}
	}

	seen := map[string]bool{}
	moduleIDs := map[string]bool{}
	for _, m := range AIModules {
		moduleIDs[m.ID] = true
	}
	for _, s := range AISettingSpecs {
		if seen[s.Key] {
			t.Errorf("重复 key: %s", s.Key)
		}
		seen[s.Key] = true
		if !moduleIDs[s.Module] {
			t.Errorf("%s 的模块 %q 未在 AIModules 声明", s.Key, s.Module)
		}
		if s.Label == "" {
			t.Errorf("%s 缺 Label", s.Key)
		}
		if s.Def == "" && s.Kind != KindString {
			t.Errorf("%s 缺默认值", s.Key)
		}
		if (s.Kind == KindInt || s.Kind == KindFloat) && s.Max <= s.Min {
			t.Errorf("%s 的 Min/Max 不合法: %v ~ %v", s.Key, s.Min, s.Max)
		}
	}
	if len(AISettingSpecs) != len(settingByKey) {
		t.Fatalf("注册表长度与索引不一致: %d vs %d", len(AISettingSpecs), len(settingByKey))
	}

	// B-103 五模块：连接 / 角色与订阅 / 说话与上下文 / AI 虚拟钓鱼 / 性格与交互。
	// legacy 只承接仍保留读写的旧键，页面不再为它建标签页。
	wantModules := []string{"conn", "agents", "budget", "fishing", "personality", "legacy"}
	if len(AIModules) != len(wantModules) {
		t.Fatalf("模块数 = %d，期望 %d: %+v", len(AIModules), len(wantModules), AIModules)
	}
	for i, id := range wantModules {
		if AIModules[i].ID != id {
			t.Fatalf("第 %d 个模块 = %q，期望 %q", i+1, AIModules[i].ID, id)
		}
	}
	// 说话与上下文只放预算：四组都来自 ai_profile_*，且不再含圆桌/反应/记忆开关。
	for _, group := range []string{"channel", "dm", "roundtable", "profile"} {
		found := 0
		for _, spec := range AISettingSpecs {
			if spec.Module == "budget" && spec.Group == group && !spec.Hidden {
				found++
			}
		}
		if found == 0 {
			t.Errorf("budget/%s 组没有参数", group)
		}
	}
	for _, spec := range AISettingSpecs {
		if spec.Module == "budget" && !strings.HasPrefix(spec.Key, "ai_profile_") {
			t.Errorf("budget 混入了非预算参数: %s", spec.Key)
		}
		// 旧行为概率、角色模拟与圆桌细节保留读写，但不上页面。
		if spec.Module == "legacy" && !spec.Hidden {
			t.Errorf("legacy 参数 %s 必须 Hidden", spec.Key)
		}
	}
	// 钓鱼战绩注入归 fishing，不再留在通用记忆区。
	for _, key := range []string{"ai_memory_inject", "ai_memory_recent", "ai_memory_best", "ai_memory_max_rows", "ai_catch_brief_inject"} {
		spec, ok := settingByKey[key]
		if !ok {
			t.Fatalf("缺钓鱼注入参数 %s", key)
		}
		if spec.Module != "fishing" || spec.Group != "inject" || spec.Hidden {
			t.Errorf("%s 归属 = %s/%s hidden=%v，期望 fishing/inject 可见", key, spec.Module, spec.Group, spec.Hidden)
		}
	}
	// 频道软目标必须与硬上限同组，供页面同行校验。
	soft, hard := settingByKey["ai_profile_channel_reply_soft_chars"], settingByKey["ai_profile_channel_reply_chars"]
	if soft.Module != hard.Module || soft.Group != hard.Group || soft.Max > hard.Max {
		t.Errorf("频道软/硬上限分组不一致: %+v / %+v", soft, hard)
	}
}

// TestNormalizeSettingValue 逐项对齐旧代码的 clamp 行为。
func TestNormalizeSettingValue(t *testing.T) {
	cases := []struct {
		key  string
		raw  any
		want string
		ok   bool
	}{
		{"emergency_stop", true, "1", true},
		{"emergency_stop", false, "0", true},
		{"emergency_stop", "bad", "", false},
		{"search_endpoint", "  https://x.test  ", "https://x.test", true},
		{"max_tool_rounds", float64(9), "4", true}, // 上限 4
		{"max_tool_rounds", float64(0), "1", true}, // 下限 1
		{"roundtable_max_speakers", float64(1), "2", true},
		{"roundtable_hourly_limit", float64(99), "10", true},
		{"ai_fishing_miss_ratio", 0.01, "0.05", true},
		{"ai_fishing_near_ratio", 2.0, "0.99", true},
		{"ai_fishing_talk_super_chance", 5.0, "1", true},
		{"ai_profile_channel_reply_chars", float64(10), "20", true},
		{"ai_memory_max_rows", float64(500), "200", true},
		{"ai_memory_inject", 1.0, "1", true},
	}
	for _, c := range cases {
		s, ok := settingByKey[c.key]
		if !ok {
			t.Fatalf("测试用了不存在的 key: %s", c.key)
		}
		got, valid := NormalizeSettingValue(s, c.raw)
		if valid != c.ok || got != c.want {
			t.Errorf("%s(%v) = (%q,%v)，期望 (%q,%v)", c.key, c.raw, got, valid, c.want, c.ok)
		}
	}
}

// TestAISettingsSearchRecord 变更搜索去向时，同事务写设置 + 一条搜索记录
// （实际搜索端、用途、目的域）；记录写入失败时整笔回滚，设置不变。
func TestAISettingsSearchRecord(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	srv := NewServer(config.Admin{JWTSecret: "test"}, db, logging.Discard(), WebFS())
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/ai/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleAISettingsSave(rec, req)
		return rec
	}
	// 记录插入失败 → 同一事务回滚，search_endpoint 不落库。
	if _, err := db.SQL.ExecContext(ctx, `
		CREATE TRIGGER pub_search_boom BEFORE INSERT ON ai_setting
		WHEN NEW.key LIKE 'ai_config_search_record:%'
		BEGIN SELECT RAISE(ABORT, 'search record boom'); END`); err != nil {
		t.Fatal(err)
	}
	rec := post(`{"search_endpoint":"https://search.example.com/q","web_search_enabled":true}`)
	if rec.Code != 500 {
		t.Fatalf("搜索记录写入失败时应 500，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got := db.AISetting(ctx, aiconfig.KeySearchEndpoint, ""); got != "" {
		t.Fatalf("记录失败后设置仍写入: %q", got)
	}
	if _, err := db.SQL.ExecContext(ctx, `DROP TRIGGER pub_search_boom`); err != nil {
		t.Fatal(err)
	}
	// 修复后重试一次成功：设置与记录同事务写入。
	rec = post(`{"search_endpoint":"https://search.example.com/q","web_search_enabled":true}`)
	if rec.Code != 200 {
		t.Fatalf("重试保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	if got := db.AISetting(ctx, aiconfig.KeySearchEndpoint, ""); got != "https://search.example.com/q" {
		t.Fatalf("搜索端点 = %q", got)
	}
	var raw string
	if err := db.SQL.QueryRowContext(ctx, `
		SELECT value FROM ai_setting WHERE key LIKE 'ai_config_search_record:%' ORDER BY key DESC LIMIT 1`).
		Scan(&raw); err != nil {
		t.Fatalf("缺少搜索去向记录: %v", err)
	}
	rec2, err := aiconfig.ParseConfigRecord(raw)
	if err != nil {
		t.Fatalf("搜索记录格式: %v", err)
	}
	if rec2.Kind != "search" || rec2.SearchHost != "search.example.com" ||
		rec2.SearchEndpoint != "https://search.example.com/q" ||
		len(rec2.Purpose) != 1 || rec2.Purpose[0] != "web_search" {
		t.Fatalf("搜索记录 = %+v", rec2)
	}
	if strings.Contains(raw, "api_key") || strings.Contains(raw, "sk-") {
		t.Fatalf("搜索记录夹带凭据: %s", raw)
	}
}

// TestSettingSpecsJSON 确保发给前端的注册表能被正常序列化。
func TestSettingSpecsJSON(t *testing.T) {
	b, err := json.Marshal(SpecsJSON())
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 1000 {
		t.Fatalf("specs JSON 异常短: %d", len(b))
	}
	var decoded struct {
		Modules []AIModule    `json:"modules"`
		Specs   []SettingSpec `json:"specs"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Modules) != len(AIModules) || len(decoded.Specs) != len(AISettingSpecs) {
		t.Fatalf("序列化后条目数不符: modules=%d specs=%d", len(decoded.Modules), len(decoded.Specs))
	}
	// 首屏可见字段应该明显少于全部字段；Hidden 的兼容参数不算首屏。
	visible := 0
	for _, s := range AISettingSpecs {
		if !s.Advanced && !s.Hidden {
			visible++
		}
	}
	if visible > 25 {
		t.Errorf("首屏可见字段 %d 个，超过 25 个预算", visible)
	}
}

// ---- T-103 性格四维：管理端内部读取 ----

func openPersonalityAdminDB(t *testing.T, name string, keys ...string) *store.DB {
	t.Helper()
	d, err := store.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, key := range keys {
		if _, _, err := d.AICreateAgent(context.Background(), store.AIAgent{
			Key: key, Display: "钓友" + key, Role: "测试", ThinkingEffort: "off", ReplyMode: "social",
		}, "[]"); err != nil {
			t.Fatalf("建 Agent %s: %v", key, err)
		}
	}
	return d
}

func mustAdminPersonality(t *testing.T, s, e, p, r int) aiconfig.PersonalityConfig {
	t.Helper()
	cfg := aiconfig.PersonalityConfig{
		SchemaVersion: aiconfig.PersonalitySchemaVersion,
		Sociability:   s, Exploration: e, Persistence: p, Regularity: r,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("测试配置本身非法: %v", err)
	}
	return cfg
}

// TestAIPersonalitySnapshotDefinitionAndPreview 同一次读取给出定义、当前值、修订与派生预览。
func TestAIPersonalitySnapshotDefinitionAndPreview(t *testing.T) {
	d := openPersonalityAdminDB(t, "admin-personality.db", "hutao")
	ctx := context.Background()
	if _, err := d.AISavePersonality(ctx, "hutao", 0, mustAdminPersonality(t, 90, 10, 70, 30)); err != nil {
		t.Fatal(err)
	}
	view, err := aiPersonalitySnapshot(ctx, d, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if view.AgentKey != "hutao" || !view.Present || !view.Configured {
		t.Fatalf("视图状态不对: %+v", view)
	}
	if view.SchemaVersion != aiconfig.PersonalitySchemaVersion || view.Revision != 1 {
		t.Fatalf("版本/修订不对: %+v", view)
	}
	if len(view.Dimensions) != 4 || len(view.Bands) != 4 {
		t.Fatalf("应给四维定义与四维当前值: %d / %d", len(view.Dimensions), len(view.Bands))
	}
	// 端点与分段都来自同一份定义。
	for i, band := range view.Bands {
		if band.ID != view.Dimensions[i].ID {
			t.Fatalf("第 %d 维 ID 不一致: %s vs %s", i, band.ID, view.Dimensions[i].ID)
		}
		if band.Text == "" {
			t.Fatalf("%s 缺行为解释", band.ID)
		}
	}
	if view.Bands[0].Value != 90 || view.Bands[0].Band != "high" {
		t.Fatalf("交往意愿 90 应为高端: %+v", view.Bands[0])
	}
	if view.Bands[1].Value != 10 || view.Bands[1].Band != "low" {
		t.Fatalf("尝新意愿 10 应为低端: %+v", view.Bands[1])
	}
	// 派生预览与纯函数同版同值。
	if view.Derived.SchemaVersion != view.SchemaVersion || view.Derived.Revision != view.Revision {
		t.Fatalf("预览版本与当前修订不一致: %+v", view.Derived)
	}
	tun := aiconfig.PersonalityTunables{HardFloorSeconds: 1200, ExtraSeconds: 3600, RestBaseSeconds: 600}
	if got, want := view.Derived.DwellFloorSeconds, view.Values.DwellFloorSeconds(tun); math.Abs(got-want) > 1e-9 {
		t.Fatalf("预览底线 = %.2f，期望 %.2f", got, want)
	}
	if got, want := view.Derived.RestIntervalSeconds, view.Values.RestIntervalSeconds(tun); math.Abs(got-want) > 1e-9 {
		t.Fatalf("预览休整节拍 = %.2f，期望 %.2f", got, want)
	}
	// 行为派生也用同一个修订。
	choice := aiconfig.DecidePersonality(view.Values, []aiconfig.PersonalityCandidate{
		{ID: aiconfig.CandidateSilence, Feasible: true, Feature: aiconfig.PersonalityFeatureNeutral},
		{ID: "invite", Feasible: true, Feature: aiconfig.PersonalityFeature{Social: 1, Novelty: 0.2, Investment: 0.5, Rhythm: 0.4}},
	}, 2026)
	if choice.Revision != view.Revision || choice.SchemaVersion != view.SchemaVersion {
		t.Fatalf("派生预览与行为不是同一版本: %+v", choice)
	}
}

// TestAIPersonalitySnapshotNeedsRepairAndInactive 显式非法与已停用都要如实显示，不能装作生效。
func TestAIPersonalitySnapshotNeedsRepairAndInactive(t *testing.T) {
	d := openPersonalityAdminDB(t, "admin-personality-repair.db", "hutao", "klee")
	ctx := context.Background()
	// 非法：直接写一条超域值（保存接口会拒绝，这里模拟历史坏数据）。
	if _, err := d.SQL.Exec(`INSERT INTO ai_setting(key,value,updated_ts) VALUES(?,?,0)`,
		"ai_personality:hutao",
		`{"schema_version":1,"revision":2,"generation":1,"sociability":150,"exploration":50,"persistence":50,"regularity":50}`); err != nil {
		t.Fatal(err)
	}
	view, err := aiPersonalitySnapshot(ctx, d, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if !view.NeedsRepair || view.Configured || view.RepairReason == "" {
		t.Fatalf("非法值应显示需修复: %+v", view)
	}
	// 已删除：同样的读取要显示停用。
	if _, err := d.AISavePersonality(ctx, "klee", 0, mustAdminPersonality(t, 50, 50, 50, 50)); err != nil {
		t.Fatal(err)
	}
	if err := d.AIWithRuntimeTx(ctx, func(tx *store.AIRuntimeTx) error {
		if _, err := tx.DeleteAgent("klee"); err != nil {
			return err
		}
		_, err := tx.BumpControl("klee", 1, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	view, err = aiPersonalitySnapshot(ctx, d, "klee")
	if err != nil {
		t.Fatal(err)
	}
	if view.Inactive != store.AIPersonalityInactiveDeleted || view.Configured {
		t.Fatalf("已删除应显示停用: %+v", view)
	}
}

// TestAIPersonalitySnapshotTunablesInheritSettings 派生参数继承现有活动时长配置，不新增时长。
func TestAIPersonalitySnapshotTunablesInheritSettings(t *testing.T) {
	d := openPersonalityAdminDB(t, "admin-personality-tunables.db", "hutao")
	ctx := context.Background()
	if _, err := d.AISavePersonality(ctx, "hutao", 0, mustAdminPersonality(t, 50, 50, 100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{
		"ai_actor_fishing_session_min_s": "600",
		"ai_actor_fishing_session_max_s": "3600",
		"ai_actor_rest_min_s":            "300",
	}); err != nil {
		t.Fatal(err)
	}
	view, err := aiPersonalitySnapshot(ctx, d, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	// 硬底线 600 + 可延长 3000 × 1.0 × (0.5+0.5×0) = 2100；休整 300 × 1.0 = 300。
	if math.Abs(view.Derived.DwellFloorSeconds-2100) > 1e-9 {
		t.Fatalf("底线 = %.2f，期望 2100", view.Derived.DwellFloorSeconds)
	}
	if math.Abs(view.Derived.RestIntervalSeconds-300) > 1e-9 {
		t.Fatalf("休整节拍 = %.2f，期望 300", view.Derived.RestIntervalSeconds)
	}
	// 上限小于下限时按下限算，不出现负的可延长幅度。
	if err := d.AISetSettings(ctx, map[string]string{
		"ai_actor_fishing_session_max_s": "600",
	}); err != nil {
		t.Fatal(err)
	}
	view, err = aiPersonalitySnapshot(ctx, d, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(view.Derived.DwellFloorSeconds-600) > 1e-9 {
		t.Fatalf("上限=下限时底线应等于 600，实际 %.2f", view.Derived.DwellFloorSeconds)
	}
}

// TestAIPersonalitySnapshotIgnoresLegacyChances 旧概率与旧活跃度不参与派生预览。
func TestAIPersonalitySnapshotIgnoresLegacyChances(t *testing.T) {
	d := openPersonalityAdminDB(t, "admin-personality-legacy.db", "hutao")
	ctx := context.Background()
	if _, err := d.AISavePersonality(ctx, "hutao", 0, mustAdminPersonality(t, 60, 40, 55, 45)); err != nil {
		t.Fatal(err)
	}
	before, err := aiPersonalitySnapshot(ctx, d, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AISetAgentBehavior(ctx, "hutao", "silent", 0); err != nil {
		t.Fatal(err)
	}
	if err := d.AISetSettings(ctx, map[string]string{
		"roundtable_chime_chance":     "0.01",
		"ai_catch_super_chance":       "0.01",
		"ai_fishing_talk_star_chance": "0.01",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := aiPersonalitySnapshot(ctx, d, "hutao")
	if err != nil {
		t.Fatal(err)
	}
	if before.Values != after.Values || before.Derived != after.Derived {
		t.Fatalf("旧概率不应影响四维或派生的预览：\n%+v\n%+v", before.Derived, after.Derived)
	}
}
