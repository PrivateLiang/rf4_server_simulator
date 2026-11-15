// 后台 AI 参数的单一数据源。
//
// 以前新增一个参数要改 7 处：迁移默认值、handleAIStatus 清单、
// handleAISettingsSave 的 struct 字段、判空写入、前端 HTML、loadAI 赋值、
// 某个 saveAIXxx 的 body。漏一处就静默失效。
//
// 现在只在这里加一行 SettingSpec：
//   - 读取：handleAIStatus 遍历 Specs 取默认值
//   - 写入：handleAISettingsSave 通用循环按 Kind 解析 + 按 Min/Max clamp
//   - 界面：/api/ai/settings 把 Specs 发给前端，按 Module 分组渲染
//
// 约束：所有已有 key 名与取值语义保持不变，不改名、不迁移。

package admin

import (
	"fmt"
	"strconv"
	"strings"
)

type SettingKind string

const (
	KindBool   SettingKind = "bool"
	KindInt    SettingKind = "int"
	KindFloat  SettingKind = "float"
	KindString SettingKind = "string"
)

// SettingSpec 是一个 ai_setting 项的完整声明。
type SettingSpec struct {
	Key      string      `json:"key"`
	Module   string      `json:"module"`
	Label    string      `json:"label"`
	Hint     string      `json:"hint"`
	Kind     SettingKind `json:"kind"`
	Def      string      `json:"def"`
	Min      float64     `json:"min,omitempty"`
	Max      float64     `json:"max,omitempty"`
	Advanced bool        `json:"advanced"`
}

// AIModule 是界面上一张卡片，也是后端的一组参数。
type AIModule struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Hint  string `json:"hint"`
}

// AIModules 的顺序就是界面卡片顺序。
//
// 分组依据：先按「是不是基础能力」分两大层，再按功能划分。
//   - 核心层：连接、角色、说话与上下文（上下文 / 圆桌 / 玩家上星反应 / 记忆）
//     不管频道还是私聊都要用到，缺了 AI 就不会说话。
//   - 拓展层：AI 虚拟钓鱼（上鱼 / 播报 / 角色状态），关掉不影响基础聊天。
//
// 每个模块是一张卡片，也是后端一组独立参数；新增功能加一个 module 即可，
// 不用重排其它卡片。
var AIModules = []AIModule{
	{"conn", "连接与总开关", "出网是总闸；紧急停止优先于一切"},
	{"agents", "角色与订阅", "Agent 管理与范围订阅，不走 ai_setting"},
	{"budget", "上下文与回复上限", "频道、私聊、圆桌各自一份预算；最基础的一层"},
	{"talk", "圆桌", "被叫到的人先答，其余按活跃度决定接不接"},
	{"player_react", "玩家上星反应", "玩家上星/上蓝时，频道里的 AI 接一句"},
	{"memory", "记忆", "通用记忆能力，独立于对话历史；后续可扩展到玩家偏好、跨会话事实"},
	{"fishing", "虚拟上鱼", "拓展功能：抽什么鱼、多久抽一次"},
	{"fishing_talk", "上鱼播报", "拓展功能：AI 自己上鱼后主动开口；默认关"},
	{"actor", "角色状态", "拓展功能：角色在钓鱼/休息/闲逛之间自己切换"},
}

// AISettingSpecs 覆盖全部 ai_setting key。Min/Max 从旧 handleAISettingsSave
// 逐项搬来，硬上限不变：后台只能调小，不能突破代码 clamp。
var AISettingSpecs = []SettingSpec{
	// ---- conn：连接与总开关 ----
	{"outbound_enabled", "conn", "模型调用出网", "关闭后所有 AI 只走本地降级文案，不产生费用", KindBool, "1", 0, 0, false},
	{"web_search_enabled", "conn", "联网搜索", "默认关，只查内部资料；打开后需填检索端点", KindBool, "0", 0, 0, false},
	{"emergency_stop", "conn", "紧急停止", "切断所有模型调用，优先级最高", KindBool, "0", 0, 0, false},
	{"search_endpoint", "conn", "检索端点", "联网搜索打开时使用的外部检索地址", KindString, "", 0, 0, true},
	{"max_tool_rounds", "conn", "工具轮数", "单次请求最多几轮 function calling", KindInt, "2", 1, 4, true},

	// ---- talk：圆桌 ----
	{"roundtable_enabled", "talk", "允许互相对话", "关闭时只做单人回复", KindBool, "0", 0, 0, false},
	{"roundtable_max_speakers", "talk", "最多参与人数", "2–3 人", KindInt, "3", 2, 3, false},
	{"roundtable_max_turns", "talk", "最多发言条数", "2–6 条", KindInt, "6", 2, 6, true},
	{"roundtable_max_ms", "talk", "总时长上限 ms", "5000–30000", KindInt, "20000", 5000, 30000, true},
	{"roundtable_cooldown_ms", "talk", "圆桌冷却 ms", "30000–600000", KindInt, "120000", 30000, 600000, true},
	{"roundtable_hourly_limit", "talk", "每小时最多轮数", "1–10", KindInt, "3", 1, 10, true},
	{"roundtable_on_speech_chance", "talk", "自发开圆桌概率", "真人发言后按概率起圆桌，0–1", KindFloat, "0.2", 0, 1, true},
	{"roundtable_chime_chance", "talk", "接话概率", "候选者主动开口的基础概率，0–1", KindFloat, "0.55", 0, 1, true},
	{"roundtable_gap_ms", "talk", "发言间隔 ms", "500–10000", KindInt, "3000", 500, 10000, true},
	{"roundtable_max_streak", "talk", "连续 AI 发言上限", "1–6，超过后自然冷场", KindInt, "3", 1, 6, true},

	// ---- actor：角色状态机 ----
	{"ai_actor_enabled", "actor", "角色模拟总开关", "关闭后不注入位置和状态，角色不参与钓鱼", KindBool, "1", 0, 0, false},
	{"ai_actor_switch_chance", "actor", "换目标概率", "玩家安利某条鱼时切换目标的概率，0–1", KindFloat, "0.25", 0, 1, true},
	{"ai_actor_switch_cooldown_s", "actor", "换目标冷却 s", "60–21600，防止两句话就换地方", KindInt, "1800", 60, 21600, true},
	{"ai_actor_fishing_session_min_s", "actor", "一段钓鱼最短 s", "300–21600", KindInt, "1200", 300, 21600, true},
	{"ai_actor_fishing_session_max_s", "actor", "一段钓鱼最长 s", "600–43200", KindInt, "4800", 600, 43200, true},
	{"ai_actor_rest_min_s", "actor", "一段休息最短 s", "120–14400", KindInt, "600", 120, 14400, true},
	{"ai_actor_rest_max_s", "actor", "一段休息最长 s", "300–28800", KindInt, "2400", 300, 28800, true},
	{"ai_actor_rest_chance", "actor", "进入休息概率", "0–1", KindFloat, "0.35", 0, 1, true},

	// ---- fishing：虚拟上鱼（抽鱼本身，不含播报）----
	{"ai_fishing_enabled", "fishing", "虚拟上鱼总开关", "关闭后 AI 一条鱼都不上", KindBool, "0", 0, 0, false},
	{"ai_fishing_hourly_limit", "fishing", "每小时发言上限", "1–24。播报开启后计说出口的话，静默档不占配额", KindInt, "12", 1, 24, false},
	{"ai_fishing_base_interval_ms", "fishing", "基础节拍 ms", "60000–1800000，每次发言后 ×1.2", KindInt, "300000", 60000, 1800000, true},
	{"ai_fishing_max_interval_ms", "fishing", "节拍封顶 ms", "60000–3600000", KindInt, "600000", 60000, 3600000, true},
	{"ai_fishing_star_weight", "fishing", "上星占比", "0–1，构成比例不是发言概率", KindFloat, "0.02", 0, 1, true},
	{"ai_fishing_super_weight", "fishing", "上蓝占比", "0–1", KindFloat, "0.002", 0, 1, true},
	{"ai_fishing_miss_weight", "fishing", "未达标占比", "0–1", KindFloat, "0.33", 0, 1, true},
	{"ai_fishing_near_weight", "fishing", "下饭鱼占比", "0–1", KindFloat, "0.12", 0, 1, true},
	{"ai_fishing_miss_ratio", "fishing", "未达标分界", "0.05–0.6，相对星线", KindFloat, "0.35", 0.05, 0.6, true},
	{"ai_fishing_near_ratio", "fishing", "下饭分界", "0.6–0.99，相对星线", KindFloat, "0.9", 0.6, 0.99, true},

	// ---- fishing_talk：AI 自己上鱼后主动说话 ----
	{"ai_fishing_talk_enabled", "fishing_talk", "AI 上鱼主动说话", "默认关。打开后 AI 按自己的人设报一句", KindBool, "0", 0, 0, false},
	{"ai_fishing_talk_super_chance", "fishing_talk", "上蓝发言概率", "0–1，稀有档建议 1.0", KindFloat, "1", 0, 1, false},
	{"ai_fishing_talk_star_chance", "fishing_talk", "上星发言概率", "0–1", KindFloat, "0.9", 0, 1, false},
	{"ai_fishing_talk_near_chance", "fishing_talk", "下饭鱼发言概率", "0–1", KindFloat, "0.35", 0, 1, false},
	{"ai_fishing_talk_miss_chance", "fishing_talk", "未达标发言概率", "0–1", KindFloat, "0.25", 0, 1, false},
	{"ai_fishing_roundtable_chance", "fishing_talk", "上蓝升级圆桌概率", "0–1，复用圆桌冷却与每小时上限", KindFloat, "0.5", 0, 1, false},
	{"ai_catch_brief_inject", "fishing_talk", "知道自己上过鱼", "虚拟上鱼只写库不发消息；打开后每轮补一句「你确实上过鱼」，避免角色说自己空军", KindBool, "1", 0, 0, false},

	// ---- player_react：玩家自己上星/上蓝时 AI 接话 ----
	{"ai_catch_talk", "player_react", "玩家上星时 AI 接话", "关闭后玩家上星不再惊动 AI", KindBool, "1", 0, 0, false},
	{"ai_catch_star_chance", "player_react", "玩家上星接话概率", "0–1", KindFloat, "0.6", 0, 1, false},
	{"ai_catch_super_chance", "player_react", "玩家上蓝接话概率", "0–1", KindFloat, "0.9", 0, 1, false},
	{"ai_catch_roundtable_chance", "player_react", "玩家上蓝升级圆桌概率", "0–1", KindFloat, "0.5", 0, 1, true},

	// ---- budget：上下文与回复上限 ----
	{"ai_profile_internal_chars", "budget", "内部资料字数", "200–4000", KindInt, "800", 200, 4000, true},
	{"ai_profile_dm_context_tokens", "budget", "私聊上下文预算", "1000–30000", KindInt, "12000", 1000, 30000, true},
	{"ai_profile_dm_history_messages", "budget", "私聊历史条数", "0–300", KindInt, "200", 0, 300, true},
	{"ai_profile_dm_history_chars", "budget", "私聊历史字数", "0–20000", KindInt, "8000", 0, 20000, true},
	{"ai_profile_dm_reply_chars", "budget", "私聊回复硬上限", "100–2000", KindInt, "120", 100, 2000, true},
	{"ai_profile_dm_reply_soft_chars", "budget", "私聊回复软目标", "20–800", KindInt, "80", 20, 800, true},
	{"ai_profile_channel_context_tokens", "budget", "频道上下文预算", "800–30000", KindInt, "10000", 800, 30000, true},
	{"ai_profile_channel_history_messages", "budget", "频道历史条数", "0–300", KindInt, "150", 0, 300, true},
	{"ai_profile_channel_history_chars", "budget", "频道历史字数", "200–20000", KindInt, "6000", 200, 20000, true},
	{"ai_profile_channel_reply_chars", "budget", "频道回复硬上限", "20–500", KindInt, "45", 20, 500, false},
	{"ai_profile_channel_reply_soft_chars", "budget", "频道回复软目标", "10–200", KindInt, "25", 10, 200, false},
	{"ai_profile_roundtable_context_tokens", "budget", "圆桌上下文预算", "800–12000", KindInt, "4000", 800, 12000, true},
	{"ai_profile_roundtable_context_messages", "budget", "圆桌历史条数", "1–4", KindInt, "2", 1, 4, true},
	{"ai_profile_roundtable_reply_chars", "budget", "圆桌回复硬上限", "30–300", KindInt, "30", 30, 300, true},
	{"ai_profile_roundtable_reply_soft_chars", "budget", "圆桌回复软目标", "10–120", KindInt, "20", 10, 120, true},

	// ---- memory：记忆 ----
	{"ai_memory_inject", "memory", "注入记忆", "把结构化记忆注入上下文；当前数据源是星/蓝战绩", KindBool, "1", 0, 0, false},
	{"ai_memory_recent", "memory", "最近几条", "0–5", KindInt, "3", 0, 5, true},
	{"ai_memory_best", "memory", "最佳几条", "0–5", KindInt, "2", 0, 5, true},
	{"ai_memory_max_rows", "memory", "最多保留", "5–200 条", KindInt, "50", 5, 200, true},
}

var settingByKey = func() map[string]SettingSpec {
	m := make(map[string]SettingSpec, len(AISettingSpecs))
	for _, s := range AISettingSpecs {
		m[s.Key] = s
	}
	return m
}()

// NormalizeSettingValue 按 spec 把任意输入规范成可写入 ai_setting 的字符串。
// 非法输入返回 ok=false，调用方跳过该项（保持原值），与旧行为一致。
func NormalizeSettingValue(s SettingSpec, raw any) (string, bool) {
	switch s.Kind {
	case KindBool:
		v, ok := boolFromAny(raw)
		if !ok {
			return "", false
		}
		if v {
			return "1", true
		}
		return "0", true
	case KindString:
		v, ok := raw.(string)
		if !ok {
			return "", false
		}
		return strings.TrimSpace(v), true
	case KindInt, KindFloat:
		v, ok := numFromAny(raw)
		if !ok {
			return "", false
		}
		if v < s.Min {
			v = s.Min
		}
		if v > s.Max {
			v = s.Max
		}
		if s.Kind == KindInt {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'g', -1, 64), true
	}
	return "", false
}

func boolFromAny(raw any) (bool, bool) {
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "on", "yes":
			return true, true
		case "0", "false", "off", "no", "":
			return false, true
		}
	case float64:
		return v != 0, true
	}
	return false, false
}

func numFromAny(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

// SpecsJSON 给 /api/ai/settings 用，避免前端再手写一份模块清单。
func SpecsJSON() map[string]any {
	return map[string]any{
		"modules": AIModules,
		"specs":   AISettingSpecs,
		"default": fmt.Sprint(len(AISettingSpecs)),
	}
}
