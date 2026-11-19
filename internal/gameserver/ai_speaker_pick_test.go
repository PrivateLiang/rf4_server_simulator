package gameserver

// T-108-DoD-v3 / E6：派生选人、旧随机权重退场与 reply_mode 硬门。
//
// 验收口径（T-108 §9.2 E6、v6 §3.3）：
//   - aiPickSpeaker 不再用「10 + talkativeness」加权随机，改为稳定顺序逐人跑
//     T-103 共享派生（activityWants，种子由 aiEventSeed(channel, 原话) 与角色键派生）；
//     首个愿意者发言，无人愿意即合法沉默；
//   - last-speaker 去重、频道冷却、配额与既有硬限原样保留；
//   - reply_mode 只作硬门：silent 频道内一律静默、mention 不参与频道主动事件，
//     DM 与明确直接询答维持既有行为。
//
// 反证（§9.6）：恢复加权随机 → TestT108SpeakerPickIgnoresTalkativeness 红；
// 把 aiEventSeed 改回时间种子 → TestT108SpeakerPickDerivedStable 红。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/store"
)

// newSpeakerPickFixture 准备一份带静态库的 GS（频道范围靠 loc_alias 解析）。
func newSpeakerPickFixture(t *testing.T) (*store.DB, *Server) {
	t.Helper()
	db := openStaticTestDB(t)
	srv := NewServer(config.Game{}, logging.Discard(), nil)
	srv.SetDB(db)
	return db, srv
}

// addPickAgent 接入并订阅一个角色，返回保存后的能力修订。
func addPickAgent(t *testing.T, db *store.DB, channel, key, replyMode string, caps ...string) int64 {
	t.Helper()
	ctx := context.Background()
	// 只改这个角色的 reply_mode：旧写法兼容入口仍可用，但运行期只当硬门。
	if err := db.AISetAgentBehavior(ctx, key, replyMode, 50); err != nil {
		t.Fatalf("设置 %s reply_mode: %v", key, err)
	}
	if err := db.AISetAgentJoined(ctx, key, true, 0); err != nil {
		t.Fatalf("接入 %s: %v", key, err)
	}
	if err := db.AISetChannelAgent(ctx, channel, key, true); err != nil {
		t.Fatalf("订阅 %s: %v", key, err)
	}
	view, err := db.AIGetCapabilities(ctx, key)
	if err != nil {
		t.Fatalf("读 %s 能力: %v", key, err)
	}
	saved, err := db.AISaveCapabilities(ctx, key, view.Revision, caps, nil)
	if err != nil {
		t.Fatalf("保存 %s 能力: %v", key, err)
	}
	return saved.Revision
}

// setPickPersonality 保存一组四维并返回修订。
func setPickPersonality(t *testing.T, db *store.DB, key string, soc, exp, per, reg int) int64 {
	t.Helper()
	ctx := context.Background()
	cur, err := db.AIGetPersonality(ctx, key)
	if err != nil {
		t.Fatalf("读 %s 性格: %v", key, err)
	}
	rev, err := db.AISavePersonality(ctx, key, cur.Config.Revision, personality(soc, exp, per, reg))
	if err != nil {
		t.Fatalf("保存 %s 性格: %v", key, err)
	}
	return rev
}

// resetLastSpeaker 清掉频道 last-speaker 记忆，让同一频道的重复调用可比。
func resetLastSpeaker(channel string) { aiLastSpeaker.Delete(channel) }

// pickWithText 固定事件种子后取一次发言人。
func pickWithText(t *testing.T, srv *Server, channel, text string, seed int64) (string, bool) {
	t.Helper()
	t108SeedFixed(t, seed)
	key, _, _, ok := srv.aiPickSpeaker(context.Background(), channel, text)
	return key, ok
}

// TestT108SpeakerPickDerivedStable 同一输入两次派生必须换人失败：
// 同频道、同原话、同四维、同能力 → 两次得到同一个人（先清 last-speaker）。
func TestT108SpeakerPickDerivedStable(t *testing.T) {
	ctx := context.Background()
	const channel = "t108_pick_stable"
	db, srv := newSpeakerPickFixture(t)
	addPickAgent(t, db, channel, "companion", "social", aiconfig.CapabilityJoinRoundtable)
	addPickAgent(t, db, channel, "fishguide", "social", aiconfig.CapabilityJoinRoundtable)
	setPickPersonality(t, db, "companion", 90, 60, 30, 50)
	setPickPersonality(t, db, "fishguide", 20, 10, 90, 50)

	// 找一个让 companion 愿意、fishguide 不愿意的种子：结论可复跑。
	seed := int64(0)
	for s := int64(1); s <= 4096 && seed == 0; s++ {
		a, _ := db.AIGetPersonality(ctx, "companion")
		b, _ := db.AIGetPersonality(ctx, "fishguide")
		wantA, _ := activityWants(a.Config, s^store.AIActorSeed(0, "companion"), activitySpeakerFeature(0, 0, 0))
		wantB, _ := activityWants(b.Config, s^store.AIActorSeed(0, "fishguide"), activitySpeakerFeature(0, 0, 0))
		if wantA && !wantB {
			seed = s
		}
	}
	if seed == 0 {
		t.Fatal("找不到只让 companion 愿意接话的种子")
	}

	resetLastSpeaker(channel)
	first, ok := pickWithText(t, srv, channel, "你们今天钓得怎么样", seed)
	if !ok || first != "companion" {
		t.Fatalf("第一次派生 = (%q,%v)，期望 companion", first, ok)
	}
	resetLastSpeaker(channel)
	second, ok := pickWithText(t, srv, channel, "你们今天钓得怎么样", seed)
	if !ok || second != first {
		t.Fatalf("同一输入两次派生不得换人：%q -> %q（ok=%v）", first, second, ok)
	}
	// 换原话＝换种子：允许换人，但必须由派生决定而不是随机。
	resetLastSpeaker(channel)
	if _, ok := pickWithText(t, srv, channel, "有人吗", seed); !ok {
		t.Fatal("换原话后仍应有派生结果")
	}

	// G14 反证点：生产种子不得依赖时间。同一输入隔一段时间再派生必须换人不成立。
	prevHook := aiEventSeedForTest
	aiEventSeedForTest = nil
	t.Cleanup(func() { aiEventSeedForTest = prevHook })
	const probeText = "你们今天钓得怎么样"
	firstSeed := aiEventSeed(channel, probeText)
	time.Sleep(3 * time.Millisecond)
	secondSeed := aiEventSeed(channel, probeText)
	if firstSeed != secondSeed {
		t.Fatalf("同一输入两次派生不得换人：aiEventSeed 依赖时间（%d -> %d）", firstSeed, secondSeed)
	}
	var stablePicks []string
	for i := 0; i < 16; i++ {
		resetLastSpeaker(channel)
		key, _, _, ok := srv.aiPickSpeaker(ctx, channel, probeText)
		if ok {
			stablePicks = append(stablePicks, key)
		}
		time.Sleep(time.Millisecond)
	}
	if len(stablePicks) == 0 {
		t.Fatal("真实种子下应有可复跑的派生结果")
	}
	for _, key := range stablePicks[1:] {
		if key != stablePicks[0] {
			t.Fatalf("同一输入两次派生不得换人：%q -> %q", stablePicks[0], key)
		}
	}
	t.Logf("E6 稳定派生：seed=%d 两次均为 %s（同输入同结论）；真实种子连续 %d 次不换人",
		seed, first, len(stablePicks))
}

// TestT108SpeakerPickIgnoresTalkativeness talkativeness 0/100 不得改变本次发言者。
// 恢复「10+talkativeness」加权随机会让本用例变红。
func TestT108SpeakerPickIgnoresTalkativeness(t *testing.T) {
	ctx := context.Background()
	const channel = "t108_pick_talk"
	db, srv := newSpeakerPickFixture(t)
	addPickAgent(t, db, channel, "companion", "social", aiconfig.CapabilityJoinRoundtable)
	addPickAgent(t, db, channel, "fishguide", "social", aiconfig.CapabilityJoinRoundtable)
	// 两个人都愿意：稳定顺序的第一人（优先级、角色 id）发言。
	setPickPersonality(t, db, "companion", 100, 100, 10, 50)
	setPickPersonality(t, db, "fishguide", 100, 100, 10, 50)
	seed := int64(0)
	for s := int64(1); s <= 4096 && seed == 0; s++ {
		p, _ := db.AIGetPersonality(ctx, "companion")
		q, _ := db.AIGetPersonality(ctx, "fishguide")
		a, _ := activityWants(p.Config, s^store.AIActorSeed(0, "companion"), activitySpeakerFeature(0, 0, 0))
		b, _ := activityWants(q.Config, s^store.AIActorSeed(0, "fishguide"), activitySpeakerFeature(0, 0, 0))
		if a && b {
			seed = s
		}
	}
	if seed == 0 {
		t.Fatal("找不到两人都愿意的种子")
	}

	pick := func(talkCompanion, talkFishguide int) string {
		t.Helper()
		if err := db.AISetAgentBehavior(ctx, "companion", "social", talkCompanion); err != nil {
			t.Fatal(err)
		}
		if err := db.AISetAgentBehavior(ctx, "fishguide", "social", talkFishguide); err != nil {
			t.Fatal(err)
		}
		resetLastSpeaker(channel)
		key, ok := pickWithText(t, srv, channel, "你说这条怎么样", seed)
		if !ok {
			t.Fatalf("talkativeness=%d/%d 时应有发言人", talkCompanion, talkFishguide)
		}
		return key
	}
	first := pick(0, 100)
	if first != "companion" {
		t.Fatalf("稳定顺序首个愿意者应为 companion，实得 %q", first)
	}
	// 把两人活跃度互换：发言人不得改变（talkativeness 不是概率输入）。
	second := pick(100, 0)
	if second != first {
		t.Fatalf("talkativeness 0/100 不得改变本次发言者，实得 %q -> %q", first, second)
	}
	// 同配置连续多次也不得抖动（旧加权随机会抖动）。
	for i := 0; i < 12; i++ {
		if got := pick(0, 100); got != first {
			t.Fatalf("第 %d 次重复调用换人：%q -> %q", i+2, first, got)
		}
	}
	// 逐键审计：运行期不再读 talkativeness（生产代码里只允许 store/admin 显示）。
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "Talkativeness") {
			t.Errorf("%s 仍在运行期读 talkativeness（运行读取应为 0）", name)
		}
	}
	if src, err := os.ReadFile("ai_contacts.go"); err == nil {
		if strings.Contains(string(src), `"math/rand"`) {
			t.Error("ai_contacts.go 仍依赖 math/rand（加权随机应已删除）")
		}
	}
	t.Logf("E6 talkativeness 运行读取=0：0/100 与 100/0 两种配置均为 %s，连续 13 次不抖动", first)
}

// TestT108SpeakerPickMentionHardGate mention 只回应被点名/直接对话：
// 频道主动事件的候选里不许有它，虚拟钓鱼这类「不开口」的事件不受影响。
func TestT108SpeakerPickMentionHardGate(t *testing.T) {
	ctx := context.Background()
	const channel = "t108_pick_mention"
	db, srv := newSpeakerPickFixture(t)
	addPickAgent(t, db, channel, "companion", "mention",
		aiconfig.CapabilityJoinRoundtable, aiconfig.CapabilityVirtualFishing)
	addPickAgent(t, db, channel, "fishguide", "social",
		aiconfig.CapabilityJoinRoundtable, aiconfig.CapabilityVirtualFishing)
	setPickPersonality(t, db, "companion", 100, 100, 10, 50)
	setPickPersonality(t, db, "fishguide", 100, 100, 10, 50)

	// 无论换多少原话/种子，mention 角色都不会被选为主动接话人。
	for i := 0; i < 24; i++ {
		resetLastSpeaker(channel)
		key, ok := pickWithText(t, srv, channel, "你们看这条鱼"+strings.Repeat("啊", i), int64(i+1))
		if ok && key == "companion" {
			t.Fatalf("mention 角色不得参与频道主动事件（第 %d 次被选中）", i+1)
		}
	}
	// 候选清单：发言类事件排除 mention，虚拟钓鱼不受影响。
	speech, rejects := srv.aiEventCandidates(ctx, channel, aiconfig.EventRoundtable)
	if len(speech) != 1 || speech[0].Key != "fishguide" {
		t.Fatalf("圆桌候选应只剩 fishguide，实得 %+v", speech)
	}
	if !hasReject(rejects, "companion", "reply_mode_gate") {
		t.Fatalf("mention 应被 reply_mode 硬门拒绝，实得 %+v", rejects)
	}
	fishing, _ := srv.aiEventCandidates(ctx, channel, aiconfig.EventVirtualFishing)
	if !hasAgent(fishing, "companion") {
		t.Fatalf("虚拟钓鱼不是「开口」，mention 角色仍应可钓鱼，实得 %+v", fishing)
	}
	t.Logf("E6 mention 硬门：24 次主动事件零命中；圆桌候选=%v；虚拟钓鱼候选=%d 人",
		agentKeys(speech), len(fishing))
}

// TestT108SpeakerPickSilentHardGate silent 在频道内一律静默。
func TestT108SpeakerPickSilentHardGate(t *testing.T) {
	ctx := context.Background()
	const channel = "t108_pick_silent"
	db, srv := newSpeakerPickFixture(t)
	addPickAgent(t, db, channel, "companion", "silent",
		aiconfig.CapabilityJoinRoundtable, aiconfig.CapabilityVirtualFishing)
	addPickAgent(t, db, channel, "fishguide", "social",
		aiconfig.CapabilityJoinRoundtable, aiconfig.CapabilityVirtualFishing)
	setPickPersonality(t, db, "companion", 100, 100, 10, 50)
	setPickPersonality(t, db, "fishguide", 100, 100, 10, 50)

	for i := 0; i < 16; i++ {
		resetLastSpeaker(channel)
		key, ok := pickWithText(t, srv, channel, "随便聊聊"+strings.Repeat("呀", i), int64(i+1))
		if ok && key == "companion" {
			t.Fatalf("silent 角色不得被选为主动接话人（第 %d 次）", i+1)
		}
	}
	speech, rejects := srv.aiEventCandidates(ctx, channel, aiconfig.EventRoundtable)
	if len(speech) != 1 || speech[0].Key != "fishguide" {
		t.Fatalf("圆桌候选应只剩 fishguide，实得 %+v", speech)
	}
	if !hasReject(rejects, "companion", "reply_mode_gate") {
		t.Fatalf("silent 应被 reply_mode 硬门拒绝，实得 %+v", rejects)
	}
	// 被点名进圆桌时，silent 仍然不进（频道内一律静默）。
	group, groupRejects := srv.aiEventGroup(ctx, []store.AIAgent{
		{Key: "companion", ReplyMode: "silent"}, {Key: "fishguide", ReplyMode: "social"},
	}, aiconfig.EventRoundtable)
	if len(group) != 1 || group[0].Key != "fishguide" {
		t.Fatalf("点名组里 silent 仍应被剔除，实得 %+v", group)
	}
	if !hasReject(groupRejects, "companion", "reply_mode_gate") {
		t.Fatalf("点名组应记录 silent 拒绝原因，实得 %+v", groupRejects)
	}
	t.Logf("E6 silent 硬门：16 次主动事件零命中；点名组也剔除 silent")
}

// TestT108SpeakerPickAllDeclineIsSilence 无人愿意就是合法沉默，不硬凑一个人。
func TestT108SpeakerPickAllDeclineIsSilence(t *testing.T) {
	ctx := context.Background()
	const channel = "t108_pick_silence"
	db, srv := newSpeakerPickFixture(t)
	addPickAgent(t, db, channel, "companion", "social", aiconfig.CapabilityJoinRoundtable)
	addPickAgent(t, db, channel, "fishguide", "social", aiconfig.CapabilityJoinRoundtable)
	// 极不社交、极不尝新：两个人的派生都倾向于沉默。
	setPickPersonality(t, db, "companion", 0, 0, 100, 50)
	setPickPersonality(t, db, "fishguide", 0, 0, 100, 50)
	seed := int64(0)
	for s := int64(1); s <= 8192 && seed == 0; s++ {
		p, _ := db.AIGetPersonality(ctx, "companion")
		q, _ := db.AIGetPersonality(ctx, "fishguide")
		a, _ := activityWants(p.Config, s^store.AIActorSeed(0, "companion"), activitySpeakerFeature(0, 0, 0))
		b, _ := activityWants(q.Config, s^store.AIActorSeed(0, "fishguide"), activitySpeakerFeature(0, 0, 0))
		if !a && !b {
			seed = s
		}
	}
	if seed == 0 {
		t.Fatal("找不到两人都不愿意的种子")
	}
	resetLastSpeaker(channel)
	key, ok := pickWithText(t, srv, channel, "有人在吗", seed)
	if ok || key != "" {
		t.Fatalf("无人愿意时应为合法沉默，实得 (%q,%v)", key, ok)
	}
}

// hasReject 报告拒绝清单里是否有某角色某原因。
func hasReject(list []aiEventReject, key, reason string) bool {
	for _, r := range list {
		if r.AgentKey == key && r.Reason == reason {
			return true
		}
	}
	return false
}

// hasAgent 报告候选里是否有某角色。
func hasAgent(list []store.AIAgent, key string) bool {
	for _, a := range list {
		if a.Key == key {
			return true
		}
	}
	return false
}

// agentKeys 取候选角色键（日志用）。
func agentKeys(list []store.AIAgent) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.Key)
	}
	return out
}
