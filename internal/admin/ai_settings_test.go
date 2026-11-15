package admin

import (
	"encoding/json"
	"testing"
)

// TestSettingSpecsComplete 确保注册表覆盖所有原有 key，且没有重复或未归属模块的项。
func TestSettingSpecsComplete(t *testing.T) {
	// 这份清单来自重构前 handleAIStatus 的 57 个 key，是兼容性基线。
	legacy := []string{
		"outbound_enabled", "web_search_enabled", "emergency_stop", "search_endpoint", "max_tool_rounds",
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
}

// TestNormalizeSettingValue 逐项对齐旧代码的 clamp 行为。
func TestNormalizeSettingValue(t *testing.T) {
	cases := []struct {
		key  string
		raw  any
		want string
		ok   bool
	}{
		{"outbound_enabled", true, "1", true},
		{"outbound_enabled", false, "0", true},
		{"outbound_enabled", "true", "1", true},
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
	// 首屏可见字段应该明显少于全部字段。
	visible := 0
	for _, s := range AISettingSpecs {
		if !s.Advanced {
			visible++
		}
	}
	if visible > 25 {
		t.Errorf("首屏可见字段 %d 个，超过 25 个预算", visible)
	}
}
