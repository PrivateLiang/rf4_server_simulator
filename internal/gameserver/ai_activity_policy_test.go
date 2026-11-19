package gameserver

// T-108 活动仲裁策略的纯函数验收。
//
// 这里只测「决定本身」：硬驻留只加不减、第二闸的单调性、邀请去重的上限、
// 以及非法配置不产出决定。链路上的行为在 ai_event_chains_test.go。

import (
	"context"
	"testing"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

func t108Cfg() store.AIActorConfig {
	return store.AIActorConfig{FishingMin: 1200, FishingMax: 4800, RestMin: 600, RestMax: 2400}
}

func personality(soc, exp, per, reg int) aiconfig.PersonalityConfig {
	return aiconfig.PersonalityConfig{
		SchemaVersion: aiconfig.PersonalitySchemaVersion,
		Sociability:   soc, Exploration: exp, Persistence: per, Regularity: reg,
	}
}

// 硬驻留只加不减：p 越高底线越长，r 只放大或持平；任何组合都不低于 Bhard。
func TestActivityDwellFloorIsMonotonic(t *testing.T) {
	cfg := t108Cfg()
	hard := cfg.FishingMin
	base := activityDwellFloor(cfg, personality(50, 50, 0, 0))
	if base != hard {
		t.Fatalf("p=0 时 Dmin = %v，应等于硬底线 %v", base, hard)
	}
	prev := base
	for _, p := range []int{0, 25, 50, 75, 100} {
		for _, r := range []int{0, 50, 100} {
			got := activityDwellFloor(cfg, personality(50, 50, p, r))
			if got < hard {
				t.Fatalf("p=%d r=%d 的 Dmin=%v 低于硬底线 %v", p, r, got, hard)
			}
			if got > cfg.FishingMax {
				t.Fatalf("p=%d r=%d 的 Dmin=%v 超过 Bhard+Bextra=%v", p, r, got, cfg.FishingMax)
			}
		}
		if got := activityDwellFloor(cfg, personality(50, 50, p, 100)); got < prev {
			t.Fatalf("提高 p 不应降低底线：p=%d 得 %v < %v", p, got, prev)
		} else {
			prev = got
		}
	}
}

// 休息时长 Trest=Tbase×(1+0.5r)，夹在 RestMin..RestMax 内。
func TestActivityRestSeconds(t *testing.T) {
	cfg := t108Cfg()
	if got := activityRestSeconds(cfg, personality(50, 50, 50, 0)); got != cfg.RestMin {
		t.Fatalf("r=0 的 Trest=%v，期望 %v", got, cfg.RestMin)
	}
	if got := activityRestSeconds(cfg, personality(50, 50, 50, 100)); got != 900 {
		t.Fatalf("r=100 的 Trest=%v，期望 900", got)
	}
	tight := store.AIActorConfig{FishingMin: 1000, FishingMax: 2000, RestMin: 600, RestMax: 700}
	if got := activityRestSeconds(tight, personality(50, 50, 50, 100)); got != 700 {
		t.Fatalf("超出上界时应夹到 RestMax，得 %v", got)
	}
}

// 第一闸：证据 / 锁 / 硬驻留互相独立。
func TestActivityCanSwitchGates(t *testing.T) {
	if ok, why := activityCanSwitch(false, false, 9999, 0); ok || why != "evidence_invalid" {
		t.Fatalf("证据无效应拒绝，得 %v/%s", ok, why)
	}
	if ok, why := activityCanSwitch(true, true, 9999, 0); ok || why != "locked" {
		t.Fatalf("有锁应拒绝，得 %v/%s", ok, why)
	}
	if ok, why := activityCanSwitch(true, false, 100, 1200); ok || why != "hard_dwell" {
		t.Fatalf("未过硬驻留应拒绝，得 %v/%s", ok, why)
	}
	if ok, why := activityCanSwitch(true, false, 1200, 1200); !ok || why != "" {
		t.Fatalf("刚好到硬驻留应放行，得 %v/%s", ok, why)
	}
}

// 第二闸：W 随 u 与 n 单调，高交往/高尝新更容易接受，高持续投入更难。
func TestActivitySwitchWeight(t *testing.T) {
	cfg := t108Cfg()
	extra := activityTunables(cfg).ExtraSeconds
	dwell := activityDwellFloor(cfg, personality(50, 50, 50, 50))
	neutral := personality(50, 50, 50, 50)

	// u 从 0 涨到 1：普通消遣（d=+1）W 上升，已有投入（d=−1）W 下降。
	low := activitySwitchWeight(neutral, false, dwell, dwell, extra, 1)
	high := activitySwitchWeight(neutral, false, dwell+extra, dwell, extra, 1)
	if high <= low {
		t.Fatalf("普通消遣应随等待变久更愿意：W(%v) <= W(%v)", high, low)
	}
	invLow := activitySwitchWeight(neutral, true, dwell, dwell, extra, 1)
	invHigh := activitySwitchWeight(neutral, true, dwell+extra, dwell, extra, 1)
	if invHigh >= invLow {
		t.Fatalf("已有投入应随投入变久更不愿换：W(%v) >= W(%v)", invHigh, invLow)
	}
	// 重复邀请抬高 W，但上限 0.6。
	w1 := activitySwitchWeight(neutral, true, dwell, dwell, extra, 1)
	w3 := activitySwitchWeight(neutral, true, dwell, dwell, extra, 3)
	w9 := activitySwitchWeight(neutral, true, dwell, dwell, extra, 9)
	if !(w1 < w3 && w3 == activitySwitchWeight(neutral, true, dwell, dwell, extra, 4)) {
		t.Fatalf("邀请加成应在第 3 次封顶：w1=%v w3=%v w9=%v", w1, w3, w9)
	}
	// 高交往 + 高尝新 > 低交往 + 低尝新。
	social := activitySwitchWeight(personality(100, 100, 0, 50), true, dwell, dwell, extra, 1)
	shy := activitySwitchWeight(personality(0, 0, 100, 50), true, dwell, dwell, extra, 1)
	if social <= shy {
		t.Fatalf("高交往高尝新应更容易接受：%v <= %v", social, shy)
	}
	if !activitySwitchAccepted(social) || activitySwitchAccepted(shy) {
		t.Fatalf("阈值判定不符：social=%v shy=%v", social, shy)
	}
}

// 同玩家同邀请去重：60 秒窗口内最多 3 次，窗口外重新计数。
func TestActivityInviteDedupe(t *testing.T) {
	st := &activityInviteState{last: map[string]activityInviteEntry{}}
	key := activityInviteKey(1001, "perch", "loc")
	for i := 1; i <= 3; i++ {
		n, ok := st.note(key, 100)
		if !ok || n != i {
			t.Fatalf("第 %d 次应允许，得 n=%d ok=%v", i, n, ok)
		}
	}
	if n, ok := st.note(key, 100); ok || n != 4 {
		t.Fatalf("第 4 次应拒绝，得 n=%d ok=%v", n, ok)
	}
	// 超出 60 秒窗口后重新开始。
	if n, ok := st.note(key, 100+activityInviteInterval+1); !ok || n != 1 {
		t.Fatalf("新窗口应重新计数，得 n=%d ok=%v", n, ok)
	}
	// 不同玩家互不影响。
	if n, ok := st.note(activityInviteKey(1002, "perch", "loc"), 100); !ok || n != 1 {
		t.Fatalf("不同玩家应独立计数，得 n=%d ok=%v", n, ok)
	}
}

// 非法/缺值口径：缺值按中性参与，显式非法不产出决定。
func TestActivityDecideConfigStates(t *testing.T) {
	bad := aiconfig.PersonalityConfig{SchemaVersion: aiconfig.PersonalitySchemaVersion, Sociability: 500}
	if choice := activityDecide(bad, []aiconfig.PersonalityCandidate{
		{ID: "act", Feasible: true, Feature: activityGatherFeature()},
	}, 7); choice.Decided {
		t.Fatal("显式非法配置不得产出决定")
	}
	if choice := activityDecide(aiconfig.DefaultPersonality(), nil, 7); choice.Decided {
		t.Fatal("空候选不得产出决定")
	}
	// 同输入同种子结果稳定，且候选顺序不影响结果。
	cands := []aiconfig.PersonalityCandidate{
		{ID: "b", Feasible: true, Feature: aiconfig.PersonalityFeature{Social: 0.9}},
		{ID: "a", Feasible: true, Feature: aiconfig.PersonalityFeature{Social: 0.1}},
	}
	first := activityDecide(personality(80, 40, 60, 20), cands, 99)
	reversed := activityDecide(personality(80, 40, 60, 20),
		[]aiconfig.PersonalityCandidate{cands[1], cands[0]}, 99)
	if !first.Decided || first.ChosenID != reversed.ChosenID {
		t.Fatalf("候选顺序不应改变结果：%q vs %q", first.ChosenID, reversed.ChosenID)
	}
	if first.Revision != 0 || first.SchemaVersion != aiconfig.PersonalitySchemaVersion {
		t.Fatalf("结果应带 schema/修订：%+v", first)
	}
}

// 行为意愿是二选一：同一性格在固定种子上可得到两个分支，
// 权重的方向由四维决定（高交往高尝新 > 一半）。
func TestActivityWants(t *testing.T) {
	social := personality(100, 100, 0, 50)
	var acted, silent bool
	var actWeight float64
	for seed := int64(1); seed <= 64; seed++ {
		want, choice := activityWants(social, seed, activityReplyFeature(4))
		if !choice.Decided {
			t.Fatalf("合法配置应产出决定，seed=%d", seed)
		}
		actWeight = rtWeightOf(choice, "act")
		if want {
			acted = true
		} else {
			silent = true
		}
		if acted && silent {
			break
		}
	}
	if !acted || !silent {
		t.Fatalf("同一性格应能出现两个分支：acted=%v silent=%v", acted, silent)
	}
	if actWeight <= 0.5 {
		t.Fatalf("高交往高尝新对上蓝的开口权重应过半，得 %v", actWeight)
	}
	// 低交往低尝新：开口权重低于一半。
	shy, choice := activityWants(personality(0, 0, 100, 50), 3, activityReplyFeature(4))
	if w := rtWeightOf(choice, "act"); w >= 0.5 {
		t.Fatalf("低交往低尝新的开口权重应低于一半，得 %v", w)
	}
	_ = shy
	if bad, _ := activityWants(aiconfig.PersonalityConfig{
		SchemaVersion: aiconfig.PersonalitySchemaVersion, Persistence: 999}, 3,
		activityReplyFeature(4)); bad {
		t.Fatal("非法配置不得产愿意")
	}
}

// rtWeightOf 从派生结果里取某个候选的权重。
func rtWeightOf(choice aiconfig.PersonalityChoice, id string) float64 {
	for _, w := range choice.Weights {
		if w.ID == id {
			return w.Weight
		}
	}
	return 0
}

// 性格键前缀必须与 store 侧一致：事务内只能 tx.Setting 读同一行。
func TestPersonalitySettingKeyMatchesStore(t *testing.T) {
	db := openMigratedTestDB(t)
	ctx := context.Background()
	const key = "companion"
	if _, err := db.AISavePersonality(ctx, key, 0, personality(70, 60, 40, 30)); err != nil {
		t.Fatalf("保存性格: %v", err)
	}
	want, err := db.AIGetPersonality(ctx, key)
	if err != nil {
		t.Fatalf("读性格: %v", err)
	}
	var raw string
	var ok bool
	if err := db.AIWithRuntimeTx(ctx, func(tx *store.AIRuntimeTx) error {
		var rerr error
		raw, ok, rerr = tx.SettingRaw(personalitySettingKey(key))
		return rerr
	}); err != nil {
		t.Fatalf("事务内读性格: %v", err)
	}
	if !ok {
		t.Fatal("事务内没读到性格行：键前缀与 store 不一致")
	}
	got, derivable := parsePersonalitySetting(raw)
	if !derivable {
		t.Fatal("保存过的合法性格应可派生")
	}
	if got.Sociability != 70 || got.Exploration != 60 || got.Revision != want.Config.Revision {
		t.Fatalf("事务内解析结果 = %+v，期望 %+v", got, want.Config)
	}
}

// 缺值 ⇒ 中性且可派生；显式非法 ⇒ 不可派生。
func TestParsePersonalitySetting(t *testing.T) {
	if cfg, ok := parsePersonalitySetting(""); !ok || cfg.Sociability != aiconfig.PersonalityNeutral {
		t.Fatalf("缺值应按中性参与：%+v/%v", cfg, ok)
	}
	if _, ok := parsePersonalitySetting(`{"schema_version":1,"persistence":200}`); ok {
		t.Fatal("越界值应不可派生")
	}
	if _, ok := parsePersonalitySetting(`not-json`); ok {
		t.Fatal("坏 JSON 应不可派生")
	}
}

// 事件能力判定：只有显式配置且含事件能力才算「还能承载事件」。
func TestAIEventStateOK(t *testing.T) {
	if aiEventStateOK(aiconfig.CapabilityState{State: aiconfig.CapabilityStateMissing}) {
		t.Fatal("缺配置不得承载事件")
	}
	if aiEventStateOK(aiconfig.CapabilityState{
		State:        aiconfig.CapabilityStateConfigured,
		Capabilities: []string{aiconfig.CapabilityGuideHelp}}) {
		t.Fatal("只有攻略不得承载主动事件")
	}
	st := aiconfig.CapabilityState{State: aiconfig.CapabilityStateConfigured,
		Capabilities: []string{aiconfig.CapabilityReplyStar}}
	if !aiEventStateOK(st) || !aiEventCapabilityHeld(st, aiconfig.EventReplyStar) {
		t.Fatal("已配置的星蓝能力应承载对应事件")
	}
	if aiEventCapabilityHeld(st, aiconfig.EventBroadcastCatch) {
		t.Fatal("未授予的事件类别不得被当成已持有")
	}
}
