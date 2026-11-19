package aibridge

import (
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

func TestSimilarText(t *testing.T) {
	if !similarText("今天鱼口不错，可以去试试", "今天鱼口不错，可以去试试") {
		t.Fatal("完全相同应判复读")
	}
	if !similarText("河鲈在浅水区比较多，用亮片试试", "河鲈在浅水区比较多，用亮片也行") {
		t.Fatal("前 12 字相同应判复读")
	}
	if similarText("河鲈在浅水区", "鲤鱼在深水区") {
		t.Fatal("不同内容不应判复读")
	}
	if !similarText("好", "好") {
		t.Fatal("完全相同的短回复也算复读")
	}
	if similarText("好", "行") {
		t.Fatal("不同短回复不应判复读")
	}
}

func TestOtherNames(t *testing.T) {
	agents := []*store.AIAgent{
		{Key: "a", Display: "甲"},
		{Key: "b", Display: "乙"},
		{Key: "c", Display: "丙"},
	}
	if got := otherNames(agents, "a"); got != "乙、丙" {
		t.Fatalf("otherNames = %q", got)
	}
	if got := otherNames(agents[:1], "a"); got != "其他助手" {
		t.Fatalf("单人时应回兜底: %q", got)
	}
}

// AI 自己决定说不说：命中沉默标记就不投递，也不占条数。
func TestIsRoundtableStop(t *testing.T) {
	for _, s := range []string{"散了", "散了。", " 散了 ", "散了散了", "不语", "[不语]", "【不语】", "跳过", "PASS"} {
		if !isRoundtableStop(s) {
			t.Fatalf("%q 应判定为终止", s)
		}
	}
	for _, s := range []string{"", "确实。", "那就散了吧，我去换饵", "这轮聊得挺散的了，不过我还想说说昨天的鱼", "不是不语，是想说"} {
		if isRoundtableStop(s) {
			t.Fatalf("%q 不该判定为终止", s)
		}
	}
}

func TestRTMentionsOrder(t *testing.T) {
	agents := []*store.AIAgent{
		{Key: "a", Display: "甲"},
		{Key: "b", Display: "乙"},
		{Key: "c", Display: "丙"},
	}
	got := rtMentions("@丙 你刚说啥，@甲 也看看", agents)
	if len(got) != 2 || got[0].Key != "c" || got[1].Key != "a" {
		t.Fatalf("点名顺序不对: %+v", got)
	}
	if len(rtMentions("今天没叫人", agents)) != 0 {
		t.Fatal("没点名不应返回候选")
	}
}

// T-108：圆桌发言权来自 T-103 的四维派生，不再用 reply_mode / talkativeness。
func TestRTChooseSpeakerPrefersWillingPersonality(t *testing.T) {
	agents := []*store.AIAgent{
		{Key: "a", Display: "甲"},
		{Key: "b", Display: "乙"},
	}
	st := newRTSpeakerState()
	personality := func(key string) (aiconfig.PersonalityConfig, bool) {
		cfg := aiconfig.DefaultPersonality()
		if key == "b" {
			// 高交往 + 高尝新：更愿意主动开口。
			cfg = aiconfig.PersonalityConfig{SchemaVersion: 1, Sociability: 95,
				Exploration: 95, Persistence: 50, Regularity: 50}
		}
		return cfg, true
	}
	got := rtChooseSpeaker(agents, st, map[string]bool{}, rtConfig{maxStreak: 3}, false, 7, personality)
	if got == nil || got.Key != "b" {
		t.Fatalf("应选愿意开口的高交往角色，got=%+v", got)
	}
}

// 没人想开口时是合法冷场；首轮仍保证有人接住真人的话。
func TestRTChooseSpeakerSilenceAndFirstTurn(t *testing.T) {
	agents := []*store.AIAgent{{Key: "a", Display: "甲"}, {Key: "b", Display: "乙"}}
	personality := func(string) (aiconfig.PersonalityConfig, bool) {
		// 低交往 + 低尝新 + 高持续投入：主动开口的权重压到一半以下。
		return aiconfig.PersonalityConfig{SchemaVersion: 1, Sociability: 0, Exploration: 0,
			Persistence: 100, Regularity: 50}, true
	}
	if got := rtChooseSpeaker(agents, newRTSpeakerState(), map[string]bool{},
		rtConfig{maxStreak: 3}, false, 7, personality); got != nil {
		t.Fatalf("都不想开口时应冷场，got=%+v", got)
	}
	if got := rtChooseSpeaker(agents, newRTSpeakerState(), map[string]bool{},
		rtConfig{maxStreak: 3}, true, 7, personality); got == nil {
		t.Fatal("首轮至少要有一个角色接住真人的话")
	}
}

// 被点名的角色必须开口，不管自己的四维想不想主动说。
func TestRTChooseSpeakerHonorsMention(t *testing.T) {
	agents := []*store.AIAgent{{Key: "a", Display: "甲"}, {Key: "b", Display: "乙"}}
	st := newRTSpeakerState()
	st.pending = rtMentions("@甲 你看呢", agents)
	personality := func(string) (aiconfig.PersonalityConfig, bool) {
		return aiconfig.PersonalityConfig{SchemaVersion: 1}, true // 全中性
	}
	got := rtChooseSpeaker(agents, st, map[string]bool{}, rtConfig{maxStreak: 3}, false, 7, personality)
	if got == nil || got.Key != "a" {
		t.Fatalf("被点名的角色应该开口，got=%+v", got)
	}
}

// 频道历史里带【名字】前缀，模型偶尔会照抄，回复前要剥掉。
func TestStripSpeakerPrefix(t *testing.T) {
	if got := stripSpeakerPrefix("【Agent 1】星了！"); got != "星了！" {
		t.Fatalf("前缀未剥离: %q", got)
	}
	if got := stripSpeakerPrefix("星了！"); got != "星了！" {
		t.Fatalf("无前缀不应改动: %q", got)
	}
	if got := stripSpeakerPrefix("【这不是名字前缀，是一句很长很长的话】"); got == "" {
		t.Fatal("过长的括号不应被当成前缀整段吃掉")
	}
}

// 频道共享一份上下文，私聊按 Agent 分开。
func TestSessionKeySharedForChannel(t *testing.T) {
	ch := &store.AITask{Kind: "channel", Scope: "channel", ScopeKey: "nav1_03", Agent: "hutao"}
	other := &store.AITask{Kind: "channel", Scope: "channel", ScopeKey: "nav1_03", Agent: "furina"}
	if sessionKey(ch) != sessionKey(other) {
		t.Fatal("同一频道的不同 Agent 应共用一份上下文")
	}
	dm1 := &store.AITask{Kind: "dm", Scope: "uid", ScopeKey: "1000001", Agent: "hutao"}
	dm2 := &store.AITask{Kind: "dm", Scope: "uid", ScopeKey: "1000001", Agent: "furina"}
	if sessionKey(dm1) == sessionKey(dm2) {
		t.Fatal("私聊必须按 Agent 隔离")
	}
	rt := &store.AITask{Kind: "roundtable", Scope: "channel", ScopeKey: "nav1_03"}
	if sessionKey(rt) != sessionKey(ch) {
		t.Fatal("圆桌应与频道共用上下文")
	}
}
