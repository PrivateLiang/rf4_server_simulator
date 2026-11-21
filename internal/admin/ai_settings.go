// 后台 AI 参数的单一数据源。
//
// 以前新增一个参数要改 7 处：迁移默认值、handleAIStatus 清单、
// handleAISettingsSave 的 struct 字段、判空写入、前端 HTML、loadAI 赋值、
// 某个 saveAIXxx 的 body。漏一处就静默失效。
//
// 现在只在这里加一行 SettingSpec：
//   - 读取：handleAIStatus 遍历 Specs 取默认值
//   - 写入：handleAISettingsSave 通用循环按 Kind 解析 + 按 Min/Max clamp
//   - 界面：/api/ai/settings 把 Specs 发给前端，按 Module/Group 分组渲染
//
// 约束：所有已有 key 名与取值语义保持不变，不改名、不迁移。
// B-103 起页面只渲染五个设置模块（conn/budget/fishing + agents/personality 的自定义区）；
// 旧行为概率、角色模拟与圆桌细节仍保留读写口径（Hidden=true），但不再出现在新页面。

package admin

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

type SettingKind string

const (
	KindBool   SettingKind = "bool"
	KindInt    SettingKind = "int"
	KindFloat  SettingKind = "float"
	KindString SettingKind = "string"
)

// SettingSpec 是一个 ai_setting 项的完整声明。
//
// Hidden 的项仍参与读写与兼容（后台脚本、旧页面、服务端行为照旧），
// 但新页面不渲染：B-103 把行为概率/角色模拟/圆桌细节收进性格与事件侧，
// 不再让管理员在「说话与上下文」里逐项填。
type SettingSpec struct {
	Key      string      `json:"key"`
	Module   string      `json:"module"`
	Group    string      `json:"group,omitempty"`
	Label    string      `json:"label"`
	Hint     string      `json:"hint"`
	Kind     SettingKind `json:"kind"`
	Def      string      `json:"def"`
	Min      float64     `json:"min,omitempty"`
	Max      float64     `json:"max,omitempty"`
	Advanced bool        `json:"advanced"`
	Hidden   bool        `json:"hidden,omitempty"`
}

// AIModule 是界面上一张卡片，也是后端的一组参数。
type AIModule struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Hint  string `json:"hint"`
}

// AIModules 的顺序就是界面卡片顺序。
//
// 五个设置模块来自 B-103：连接、角色与订阅、说话与上下文、AI 虚拟钓鱼、
// 性格与交互。legacy 只用于给「仍然存在但不该出现在页面」的旧键一个归属，
// 前端不会为它建标签页。
var AIModules = []AIModule{
	{"conn", "连接", "厂商与密钥、复测、联网搜索、紧急停止；高级参数默认收起"},
	{"agents", "角色与订阅", "新增/修改角色与范围订阅，不走 ai_setting"},
	{"budget", "说话与上下文", "频道、私聊、圆桌各自一份预算；资料预算独立"},
	{"fishing", "AI 虚拟钓鱼", "模拟与状态、渔获与战绩注入"},
	{"personality", "性格与交互", "当前角色的能力目录、保存与生效状态"},
	{"legacy", "旧参数（保留兼容）", "行为概率与角色模拟已收口，页面不再提供"},
}

// AISettingSpecs 覆盖全部 ai_setting key。Min/Max 从旧 handleAISettingsSave
// 逐项搬来，硬上限不变：后台只能调小，不能突破代码 clamp。
//
// Group 只用于界面分组（budget: channel/dm/roundtable/profile；
// fishing: sim/rare/talk/inject），不改变存储与校验语义。
var AISettingSpecs = []SettingSpec{
	// ---- conn：连接与密钥 ----
	{Key: aiconfig.KeyWebSearchEnabled, Module: "conn", Group: "search", Label: "联网搜索", Hint: "全局允许选择的搜索方式；角色还需单独获准。", Kind: KindBool, Def: "0"},
	{Key: aiconfig.KeySearchMode, Module: "conn", Group: "search", Label: "搜索方式", Kind: KindString, Def: "", Hidden: true},
	{Key: "emergency_stop", Module: "conn", Group: "safety", Label: "紧急停止", Hint: "切断所有模型调用，优先级最高", Kind: KindBool, Def: aiconfig.DefaultEmergencyStop},
	{Key: "search_endpoint", Module: "conn", Group: "search", Label: "检索端点", Hint: "联网搜索打开时使用的外部检索地址", Kind: KindString, Def: "", Advanced: true},
	{Key: "max_tool_rounds", Module: "conn", Group: "search", Label: "工具轮数", Hint: "单次请求最多几轮 function calling", Kind: KindInt, Def: "2", Min: 1, Max: 4, Advanced: true},

	// ---- budget：说话与上下文（只放预算）----
	{Key: "ai_profile_channel_context_tokens", Module: "budget", Group: "channel", Label: "频道上下文 tokens", Hint: "800–30000；频道回复可参考的历史窗口", Kind: KindInt, Def: "10000", Min: 800, Max: 30000},
	{Key: "ai_profile_channel_history_messages", Module: "budget", Group: "channel", Label: "频道历史条数", Hint: "0–300；最近多少条频道消息进入上下文", Kind: KindInt, Def: "150", Min: 0, Max: 300},
	{Key: "ai_profile_channel_history_chars", Module: "budget", Group: "channel", Label: "频道历史字数", Hint: "200–20000；历史消息总字数上限", Kind: KindInt, Def: "6000", Min: 200, Max: 20000},
	{Key: "ai_profile_channel_reply_soft_chars", Module: "budget", Group: "channel", Label: "频道回复软目标", Hint: "10–200 字；提示词里的期望长度", Kind: KindInt, Def: "25", Min: 10, Max: 200},
	{Key: "ai_profile_channel_reply_chars", Module: "budget", Group: "channel", Label: "频道回复硬上限", Hint: "20–500 字；超过会被截断", Kind: KindInt, Def: "45", Min: 20, Max: 500},

	{Key: "ai_profile_dm_context_tokens", Module: "budget", Group: "dm", Label: "私聊上下文 tokens", Hint: "1000–30000；私聊回复可参考的历史窗口", Kind: KindInt, Def: "12000", Min: 1000, Max: 30000},
	{Key: "ai_profile_dm_history_messages", Module: "budget", Group: "dm", Label: "私聊历史条数", Hint: "0–300；最近多少条私聊进入上下文", Kind: KindInt, Def: "200", Min: 0, Max: 300},
	{Key: "ai_profile_dm_history_chars", Module: "budget", Group: "dm", Label: "私聊历史字数", Hint: "0–20000；历史消息总字数上限", Kind: KindInt, Def: "8000", Min: 0, Max: 20000},
	{Key: "ai_profile_dm_reply_soft_chars", Module: "budget", Group: "dm", Label: "私聊回复软目标", Hint: "20–800 字；提示词里的期望长度", Kind: KindInt, Def: "80", Min: 20, Max: 800},
	{Key: "ai_profile_dm_reply_chars", Module: "budget", Group: "dm", Label: "私聊回复硬上限", Hint: "100–2000 字；超过会被截断", Kind: KindInt, Def: "120", Min: 100, Max: 2000},

	{Key: "ai_profile_roundtable_context_tokens", Module: "budget", Group: "roundtable", Label: "圆桌上下文 tokens", Hint: "800–12000；圆形对话的上下文窗口", Kind: KindInt, Def: "4000", Min: 800, Max: 12000},
	{Key: "ai_profile_roundtable_context_messages", Module: "budget", Group: "roundtable", Label: "圆桌历史条数", Hint: "1–4；圆桌只看最近几条", Kind: KindInt, Def: "2", Min: 1, Max: 4},
	{Key: "ai_profile_roundtable_reply_soft_chars", Module: "budget", Group: "roundtable", Label: "圆桌回复软目标", Hint: "10–120 字；提示词里的期望长度", Kind: KindInt, Def: "20", Min: 10, Max: 120},
	{Key: "ai_profile_roundtable_reply_chars", Module: "budget", Group: "roundtable", Label: "圆桌回复硬上限", Hint: "30–300 字；超过会被截断", Kind: KindInt, Def: "30", Min: 30, Max: 300},

	{Key: "ai_profile_internal_chars", Module: "budget", Group: "profile", Label: "内部资料字数", Hint: "200–4000；攻略/鱼情等内部资料进入提示词的上限", Kind: KindInt, Def: "800", Min: 200, Max: 4000, Advanced: true},

	// ---- fishing：模拟与状态 ----
	{Key: "ai_fishing_enabled", Module: "fishing", Group: "sim", Label: "虚拟上鱼总开关", Hint: "关闭后 AI 一条鱼都不上；关闭不会删除已有渔获", Kind: KindBool, Def: "0"},
	// 旧配额：仍作为兜底读取（可播报档的小时尝试上限），页面不再暴露；说话总量由「整体氛围」控制。
	{Key: "ai_fishing_hourly_limit", Module: "fishing", Group: "sim", Label: "每小时播报尝试上限（旧）", Hint: "1–24；旧兜底参数，当前页面不展示", Kind: KindInt, Def: "12", Min: 1, Max: 24, Hidden: true},
	{Key: "ai_fishing_base_interval_ms", Module: "fishing", Group: "sim", Label: "基础节拍 ms", Hint: "60000–1800000，每次发言后 ×1.2", Kind: KindInt, Def: "300000", Min: 60000, Max: 1800000, Advanced: true},
	{Key: "ai_fishing_max_interval_ms", Module: "fishing", Group: "sim", Label: "节拍封顶 ms", Hint: "60000–3600000", Kind: KindInt, Def: "600000", Min: 60000, Max: 3600000, Advanced: true},

	// ---- fishing：稀有档与普通余量 ----
	{Key: "ai_fishing_star_weight", Module: "fishing", Group: "rare", Label: "上星占比", Hint: "0–1，构成比例不是发言概率", Kind: KindFloat, Def: "0.02", Min: 0, Max: 1, Advanced: true},
	{Key: "ai_fishing_super_weight", Module: "fishing", Group: "rare", Label: "上蓝占比", Hint: "0–1", Kind: KindFloat, Def: "0.002", Min: 0, Max: 1, Advanced: true},
	{Key: "ai_fishing_miss_weight", Module: "fishing", Group: "rare", Label: "未达标占比", Hint: "0–1", Kind: KindFloat, Def: "0.33", Min: 0, Max: 1, Advanced: true},
	{Key: "ai_fishing_near_weight", Module: "fishing", Group: "rare", Label: "下饭鱼占比", Hint: "0–1", Kind: KindFloat, Def: "0.12", Min: 0, Max: 1, Advanced: true},
	{Key: "ai_fishing_miss_ratio", Module: "fishing", Group: "rare", Label: "未达标分界", Hint: "0.05–0.6，相对星线", Kind: KindFloat, Def: "0.35", Min: 0.05, Max: 0.6, Advanced: true},
	{Key: "ai_fishing_near_ratio", Module: "fishing", Group: "rare", Label: "下饭分界", Hint: "0.6–0.99，相对星线", Kind: KindFloat, Def: "0.9", Min: 0.6, Max: 0.99, Advanced: true},

	// ---- fishing：上鱼播报（旧参数，行为已由能力 + 性格 + 说话预算接管）----
	{Key: "ai_fishing_talk_enabled", Module: "fishing", Group: "talk", Label: "AI 上鱼主动说话（旧）", Hint: "旧开关，不再读取", Kind: KindBool, Def: "0", Hidden: true},
	{Key: "ai_fishing_talk_super_chance", Module: "fishing", Group: "talk", Label: "上蓝发言概率（旧）", Hint: "旧概率，不再读取", Kind: KindFloat, Def: "1", Min: 0, Max: 1, Hidden: true},
	{Key: "ai_fishing_talk_star_chance", Module: "fishing", Group: "talk", Label: "上星发言概率（旧）", Hint: "旧概率，不再读取", Kind: KindFloat, Def: "0.9", Min: 0, Max: 1, Hidden: true},
	{Key: "ai_fishing_talk_near_chance", Module: "fishing", Group: "talk", Label: "下饭鱼发言概率（旧）", Hint: "旧概率，不再读取", Kind: KindFloat, Def: "0.35", Min: 0, Max: 1, Hidden: true},
	{Key: "ai_fishing_talk_miss_chance", Module: "fishing", Group: "talk", Label: "未达标发言概率（旧）", Hint: "旧概率，不再读取", Kind: KindFloat, Def: "0.25", Min: 0, Max: 1, Hidden: true},
	{Key: "ai_fishing_roundtable_chance", Module: "fishing", Group: "talk", Label: "上蓝升级圆桌概率（旧）", Hint: "旧概率，不再读取；是否升级由四维决定", Kind: KindFloat, Def: "0.5", Min: 0, Max: 1, Hidden: true},

	// ---- fishing：说话节奏（频道级总预算）----
	// 上鱼播报、闲时搭话、圆桌共用；mood 选预设，custom 才读下面五个参数。
	{Key: "ai_fishing_mood", Module: "fishing", Group: "talk", Label: "整体氛围", Hint: "冷清/正常/热闹/不限四档预设；自定义才读下面的参数", Kind: KindString, Def: "normal", Advanced: true},
	{Key: "ai_talk_hourly_limit", Module: "fishing", Group: "talk", Label: "自定义每小时上限", Hint: "1–24 句；所有角色、所有说话通道合计", Kind: KindInt, Def: "6", Min: 1, Max: 24, Advanced: true},
	{Key: "ai_talk_min_gap_s", Module: "fishing", Group: "talk", Label: "自定义最小间隔", Hint: "30–900 秒；闲时搭话遵守，上鱼播报可插队", Kind: KindInt, Def: "480", Min: 30, Max: 900, Advanced: true},
	{Key: "ai_talk_burst", Module: "fishing", Group: "talk", Label: "自定义连发上限", Hint: "1–4 句；小时开头或跨时段允许超前说几句", Kind: KindInt, Def: "2", Min: 1, Max: 4, Advanced: true},
	{Key: "ai_talk_roundtable_speakers", Module: "fishing", Group: "talk", Label: "自定义最多几人接话", Hint: "2–12 人；仅自定义档生效，条数按人数 +2", Kind: KindInt, Def: "5", Min: 2, Max: 12, Advanced: true},
	{Key: "ai_talk_weight_0", Module: "fishing", Group: "talk", Label: "0–15 分钟权重", Hint: "0–3；四段会归一化", Kind: KindFloat, Def: "1", Min: 0, Max: 3, Advanced: true},
	{Key: "ai_talk_weight_1", Module: "fishing", Group: "talk", Label: "15–30 分钟权重", Hint: "0–3；四段会归一化", Kind: KindFloat, Def: "1", Min: 0, Max: 3, Advanced: true},
	{Key: "ai_talk_weight_2", Module: "fishing", Group: "talk", Label: "30–45 分钟权重", Hint: "0–3；四段会归一化", Kind: KindFloat, Def: "1", Min: 0, Max: 3, Advanced: true},
	{Key: "ai_talk_weight_3", Module: "fishing", Group: "talk", Label: "45–60 分钟权重", Hint: "0–3；四段会归一化", Kind: KindFloat, Def: "1", Min: 0, Max: 3, Advanced: true},

	// ---- fishing：闲时搭话 ----
	// 独立于上鱼播报：角色要在能力里勾选「闲时搭话」，这里才可能生效。
	// Advanced 只影响通用表单的首屏预算；「虚拟钓鱼」页有专门的「闲时搭话」区块显式渲染。
	{Key: "ai_idle_silence_min", Module: "fishing", Group: "idle", Label: "安静多久后搭话", Hint: "1–15 分钟；频道里没人说话超过这个时间，AI 才可能自己找个话头", Kind: KindInt, Def: "3", Min: 1, Max: 15, Advanced: true},
	// 小时上限已并入频道级「说话节奏」，这一项只保留旧库兼容，不再读取。
	{Key: "ai_idle_hourly_limit", Module: "fishing", Group: "idle", Label: "每小时最多搭话（已并入说话节奏）", Hint: "旧参数；现在由「整体氛围」统一控制", Kind: KindInt, Def: "4", Min: 1, Max: 12, Hidden: true},

	// ---- fishing：渔获与战绩注入 ----
	{Key: "ai_memory_inject", Module: "fishing", Group: "inject", Label: "注入战绩摘要", Hint: "把星/蓝战绩摘要注入上下文；关闭不删除记录", Kind: KindBool, Def: aiconfig.DefaultMemoryInject},
	{Key: "ai_memory_recent", Module: "fishing", Group: "inject", Label: "最近几条", Hint: "0–5；摘要里列最近几条", Kind: KindInt, Def: "3", Min: 0, Max: 5},
	{Key: "ai_memory_best", Module: "fishing", Group: "inject", Label: "最佳几条", Hint: "0–5；摘要里列最大几条", Kind: KindInt, Def: "2", Min: 0, Max: 5},
	{Key: "ai_memory_max_rows", Module: "fishing", Group: "inject", Label: "最多保留", Hint: "5–200 条；超出的旧摘要会被裁剪", Kind: KindInt, Def: "50", Min: 5, Max: 200},
	{Key: "ai_catch_brief_inject", Module: "fishing", Group: "inject", Label: "知道自己上过鱼", Hint: "只按历史累计说「曾有渔获」，不推断今天；具体数字等玩家问", Kind: KindBool, Def: "1"},

	// ---- legacy：行为概率、圆桌细节与角色模拟（保留读写，不再上页面）----
	{Key: "roundtable_enabled", Module: "legacy", Label: "允许互相对话", Hint: "行为概率已收口到性格与事件侧", Kind: KindBool, Def: "0", Hidden: true},
	{Key: "roundtable_max_speakers", Module: "legacy", Label: "最多参与人数", Hint: "2–3 人", Kind: KindInt, Def: "3", Min: 2, Max: 3, Hidden: true},
	{Key: "roundtable_max_turns", Module: "legacy", Label: "最多发言条数", Hint: "2–6 条", Kind: KindInt, Def: "6", Min: 2, Max: 6, Hidden: true},
	{Key: "roundtable_max_ms", Module: "legacy", Label: "总时长上限 ms", Hint: "5000–30000", Kind: KindInt, Def: "20000", Min: 5000, Max: 30000, Hidden: true},
	{Key: "roundtable_cooldown_ms", Module: "legacy", Label: "圆桌冷却 ms", Hint: "30000–600000", Kind: KindInt, Def: "120000", Min: 30000, Max: 600000, Hidden: true},
	{Key: "roundtable_hourly_limit", Module: "legacy", Label: "每小时最多轮数", Hint: "1–10", Kind: KindInt, Def: "3", Min: 1, Max: 10, Hidden: true},
	{Key: "roundtable_on_speech_chance", Module: "legacy", Label: "自发开圆桌概率", Hint: "真人发言后按概率起圆桌，0–1", Kind: KindFloat, Def: "0.2", Min: 0, Max: 1, Hidden: true},
	{Key: "roundtable_chime_chance", Module: "legacy", Label: "接话概率", Hint: "候选者主动开口的基础概率，0–1", Kind: KindFloat, Def: "0.55", Min: 0, Max: 1, Hidden: true},
	{Key: "roundtable_gap_ms", Module: "legacy", Label: "发言间隔 ms", Hint: "500–10000", Kind: KindInt, Def: "3000", Min: 500, Max: 10000, Hidden: true},
	{Key: "roundtable_max_streak", Module: "legacy", Label: "连续 AI 发言上限", Hint: "1–6，超过后自然冷场", Kind: KindInt, Def: "3", Min: 1, Max: 6, Hidden: true},
	{Key: "ai_actor_enabled", Module: "legacy", Label: "角色模拟总开关", Hint: "由角色能力保存接管，页面不再单独提供", Kind: KindBool, Def: aiconfig.DefaultActorEnabled, Hidden: true},
	{Key: "ai_actor_switch_chance", Module: "legacy", Label: "换目标概率", Hint: "0–1", Kind: KindFloat, Def: "0.25", Min: 0, Max: 1, Hidden: true},
	{Key: "ai_actor_switch_cooldown_s", Module: "legacy", Label: "换目标冷却 s", Hint: "60–21600", Kind: KindInt, Def: "1800", Min: 60, Max: 21600, Hidden: true},
	{Key: "ai_actor_fishing_session_min_s", Module: "legacy", Label: "一段钓鱼最短 s", Hint: "300–21600", Kind: KindInt, Def: "1200", Min: 300, Max: 21600, Hidden: true},
	{Key: "ai_actor_fishing_session_max_s", Module: "legacy", Label: "一段钓鱼最长 s", Hint: "600–43200", Kind: KindInt, Def: "4800", Min: 600, Max: 43200, Hidden: true},
	{Key: "ai_actor_rest_min_s", Module: "legacy", Label: "一段休息最短 s", Hint: "120–14400", Kind: KindInt, Def: "600", Min: 120, Max: 14400, Hidden: true},
	{Key: "ai_actor_rest_max_s", Module: "legacy", Label: "一段休息最长 s", Hint: "300–28800", Kind: KindInt, Def: "2400", Min: 300, Max: 28800, Hidden: true},
	{Key: "ai_actor_rest_chance", Module: "legacy", Label: "进入休息概率", Hint: "0–1", Kind: KindFloat, Def: "0.35", Min: 0, Max: 1, Hidden: true},
	{Key: "ai_catch_talk", Module: "legacy", Label: "玩家上星时 AI 接话", Hint: "行为概率已收口，页面不再提供", Kind: KindBool, Def: aiconfig.DefaultCatchTalk, Hidden: true},
	{Key: "ai_catch_star_chance", Module: "legacy", Label: "玩家上星接话概率", Hint: "0–1", Kind: KindFloat, Def: "0.6", Min: 0, Max: 1, Hidden: true},
	{Key: "ai_catch_super_chance", Module: "legacy", Label: "玩家上蓝接话概率", Hint: "0–1", Kind: KindFloat, Def: "0.9", Min: 0, Max: 1, Hidden: true},
	{Key: "ai_catch_roundtable_chance", Module: "legacy", Label: "玩家上蓝升级圆桌概率", Hint: "0–1", Kind: KindFloat, Def: "0.5", Min: 0, Max: 1, Hidden: true},
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

// ---- 性格四维的管理端内部读取（T-103） ----
//
// 只读聚合：定义（ID/中文标签/两端/分段）、当前值、修订与派生预览。
// 不新增 HTTP 路由也不改 /api/ai/settings 的响应形状——页面与接口接线归 T-107；
// 它直接投影这里的结果，避免前端另抄一份端点、分段或公式。

// aiPersonalityDerived 是派生预览：这组旋钮在当前活动时长配置下的两个派生量。
type aiPersonalityDerived struct {
	SchemaVersion       int     `json:"schema_version"`
	Revision            int64   `json:"revision"`
	DwellFloorSeconds   float64 `json:"dwell_floor_seconds"`
	RestIntervalSeconds float64 `json:"rest_interval_seconds"`
}

// aiPersonalityView 是管理端读取一个角色性格配置的完整视图。
//
// Configured=false 时页面应显示「已保存但需修复」或「旧代次待确认」，
// 不得把 Config 当成生效值展示为已生效。
type aiPersonalityView struct {
	AgentKey          string                               `json:"agent_key"`
	SchemaVersion     int                                  `json:"schema_version"`
	Dimensions        []aiconfig.PersonalityDimension      `json:"dimensions"`
	Bands             []aiconfig.PersonalityDimensionValue `json:"bands"`
	Values            aiconfig.PersonalityConfig           `json:"values"`
	Present           bool                                 `json:"present"`
	Revision          int64                                `json:"revision"`
	CurrentGeneration int64                                `json:"current_generation"`
	Configured        bool                                 `json:"configured"`
	NeedsRepair       bool                                 `json:"needs_repair"`
	RepairReason      string                               `json:"repair_reason"`
	Inactive          string                               `json:"inactive"`
	Derived           aiPersonalityDerived                 `json:"derived"`
}

// aiPersonalitySnapshot 汇总管理端需要的性格视图。只读，不写库。
func aiPersonalitySnapshot(ctx context.Context, d *store.DB, agentKey string) (aiPersonalityView, error) {
	state, err := d.AIGetPersonality(ctx, agentKey)
	if err != nil {
		return aiPersonalityView{}, err
	}
	tunables := aiPersonalityTunables(ctx, d)
	view := aiPersonalityView{
		AgentKey:          state.AgentKey,
		SchemaVersion:     aiconfig.PersonalitySchemaVersion,
		Dimensions:        aiconfig.PersonalityDimensions(),
		Bands:             state.Config.DimensionValues(),
		Values:            state.Config,
		Present:           state.Present,
		Revision:          state.Config.Revision,
		CurrentGeneration: state.CurrentGeneration,
		Configured:        state.Derivable(),
		NeedsRepair:       state.NeedsRepair,
		RepairReason:      state.RepairReason,
		Inactive:          state.Inactive,
		Derived: aiPersonalityDerived{
			SchemaVersion:       state.Config.SchemaVersion,
			Revision:            state.Config.Revision,
			DwellFloorSeconds:   state.Config.DwellFloorSeconds(tunables),
			RestIntervalSeconds: state.Config.RestIntervalSeconds(tunables),
		},
	}
	return view, nil
}

// aiPersonalityTunables 把类型级派生参数接到现有活动时长配置上：
// 硬底线 = 一段钓鱼最短、可延长幅度 = 最长 − 最短、休整基数 = 一段休息最短。
//
// 这些键仍由原模块唯一写；本票不新增时长，也不做每角色覆盖。
func aiPersonalityTunables(ctx context.Context, d *store.DB) aiconfig.PersonalityTunables {
	floor := aiSettingInt(ctx, d, "ai_actor_fishing_session_min_s")
	ceiling := aiSettingInt(ctx, d, "ai_actor_fishing_session_max_s")
	rest := aiSettingInt(ctx, d, "ai_actor_rest_min_s")
	if ceiling < floor {
		ceiling = floor
	}
	return aiconfig.PersonalityTunables{
		HardFloorSeconds: float64(floor),
		ExtraSeconds:     float64(ceiling - floor),
		RestBaseSeconds:  float64(rest),
	}
}

// aiSettingInt 按注册表的默认值与硬界读一个整型设置。
// 与保存路径的 clamp 口径一致：越界值按端点读，不让坏值把预览算成负数或天文数字。
func aiSettingInt(ctx context.Context, d *store.DB, key string) int {
	spec, ok := settingByKey[key]
	def := 0
	if ok {
		if v, err := strconv.Atoi(strings.TrimSpace(spec.Def)); err == nil {
			def = v
		}
	}
	raw := d.AISetting(ctx, key, spec.Def)
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if ok && spec.Max > spec.Min {
		if float64(v) < spec.Min {
			v = int(spec.Min)
		}
		if float64(v) > spec.Max {
			v = int(spec.Max)
		}
	}
	return v
}
