package aiconfig

// 说话节奏的整体氛围：管理端只选一个档，具体预算、形状与圆桌规模由这里唯一给出。
//
// 这是「管理员策略」，不是角色行为：它只决定一个频道一小时最多能说几句、
// 几句之间至少隔多久、一次圆桌最多几人；「这一次想不想说」仍由四维性格派生决定。

const (
	// TalkMoodCustom 表示由自定义参数接管；预设表里没有这一档。
	TalkMoodCustom = "custom"
)

// RoundtableMaxParticipants 是证据表能承载的圆桌参与人数上限。
// store.AITaskEvidenceMaxParticipants 直接引用它，避免两边漂移。
const RoundtableMaxParticipants = 12

// TalkMood 是一档预设。MinGapSec 用秒而不是 Duration，便于直接下发到页面。
type TalkMood struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Budget    int        `json:"budget"`    // 句/小时（频道级、所有角色合计）
	MinGapSec int        `json:"min_gap_s"` // 两条 AI 消息的最小间隔
	Burst     int        `json:"burst"`     // 允许超前消费的句数
	Weights   [4]float64 `json:"weights"`   // 0–15/15–30/30–45/45–60 分钟的权重

	// 圆桌：一次事件最多几人、几条、冷却、每小时次数与单次时限。
	RoundtableSpeakers    int `json:"roundtable_speakers"`
	RoundtableTurns       int `json:"roundtable_turns"`
	RoundtableCooldownSec int `json:"roundtable_cooldown_s"`
	RoundtableHourly      int `json:"roundtable_hourly"`
	RoundtableMaxMS       int `json:"roundtable_max_ms"`
}

// talkMoods 是唯一的预设表，顺序即页面顺序。
var talkMoods = []TalkMood{
	{ID: "quiet", Label: "冷清", Budget: 3, MinGapSec: 15 * 60, Burst: 1, Weights: [4]float64{1, 1, 1, 1},
		RoundtableSpeakers: 3, RoundtableTurns: 4, RoundtableCooldownSec: 5 * 60, RoundtableHourly: 1, RoundtableMaxMS: 30000},
	{ID: "normal", Label: "正常", Budget: 6, MinGapSec: 8 * 60, Burst: 2, Weights: [4]float64{1, 1, 1, 1},
		RoundtableSpeakers: 5, RoundtableTurns: 6, RoundtableCooldownSec: 2 * 60, RoundtableHourly: 3, RoundtableMaxMS: 45000},
	{ID: "lively", Label: "热闹", Budget: 12, MinGapSec: 3 * 60, Burst: 3, Weights: [4]float64{1.5, 1, 0.75, 0.75},
		RoundtableSpeakers: 10, RoundtableTurns: 12, RoundtableCooldownSec: 45, RoundtableHourly: 6, RoundtableMaxMS: 90000},
	// 不限：不设每小时上限，只保留 60 秒的最小间隔，避免变成刷屏。
	{ID: "unlimited", Label: "不限", Budget: 0, MinGapSec: 60, Burst: 1, Weights: [4]float64{1, 1, 1, 1},
		RoundtableSpeakers: RoundtableMaxParticipants, RoundtableTurns: 16, RoundtableCooldownSec: 20, RoundtableHourly: 12, RoundtableMaxMS: 120000},
}

// RoundtableProfile 是某一档解析后的圆桌参数。
type RoundtableProfile struct {
	Speakers    int
	Turns       int
	CooldownSec int
	Hourly      int
	MaxMS       int
}

// TalkMoodRoundtable 按预设 ID 取圆桌参数；custom/未知返回 false。
func TalkMoodRoundtable(id string) (RoundtableProfile, bool) {
	m, ok := TalkMoodByID(id)
	if !ok {
		return RoundtableProfile{}, false
	}
	return RoundtableProfile{
		Speakers:    m.RoundtableSpeakers,
		Turns:       m.RoundtableTurns,
		CooldownSec: m.RoundtableCooldownSec,
		Hourly:      m.RoundtableHourly,
		MaxMS:       m.RoundtableMaxMS,
	}, true
}

// CustomRoundtableProfile 把「自定义人数」夹到 2..12，条数按人数 +2（最多 16）。
// 冷却/每小时/时限沿用正常档，避免自定义再长出一堆旋钮。
func CustomRoundtableProfile(speakers int) RoundtableProfile {
	if speakers < 2 {
		speakers = 2
	}
	if speakers > RoundtableMaxParticipants {
		speakers = RoundtableMaxParticipants
	}
	turns := speakers + 2
	if turns > 16 {
		turns = 16
	}
	return RoundtableProfile{Speakers: speakers, Turns: turns, CooldownSec: 2 * 60, Hourly: 3, MaxMS: 90000}
}

// TalkMoodPresets 返回预设副本，调用方不得改写包内状态。
func TalkMoodPresets() []TalkMood {
	out := make([]TalkMood, len(talkMoods))
	copy(out, talkMoods)
	return out
}

// TalkMoodByID 按 ID 取预设；custom 与未知 ID 都返回 false。
func TalkMoodByID(id string) (TalkMood, bool) {
	for _, m := range talkMoods {
		if m.ID == id {
			return m, true
		}
	}
	return TalkMood{}, false
}
