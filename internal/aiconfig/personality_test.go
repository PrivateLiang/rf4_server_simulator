package aiconfig

// 四维定义、共享派生与同版表达编译的定向用例。
// 期望值全部是手写字面量：不拿被测公式再算一遍期望值。

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestPersonalityDefinitionSingleSource 定义只有一份：四个固定 ID、非空标签与端点、
// 三段文案齐全。加第五维、改 ID 或漏文案都会在这里变红。
func TestPersonalityDefinitionSingleSource(t *testing.T) {
	dims := PersonalityDimensions()
	if len(dims) != 4 {
		t.Fatalf("四维定义应为 4 条，实际 %d", len(dims))
	}
	wantIDs := []string{DimensionSociability, DimensionExploration, DimensionPersistence, DimensionRegularity}
	wantLabels := []string{"交往意愿", "尝新意愿", "持续投入", "作息规律"}
	for i, d := range dims {
		if d.ID != wantIDs[i] {
			t.Errorf("第 %d 维 ID = %q，期望 %q", i, d.ID, wantIDs[i])
		}
		if d.Label != wantLabels[i] {
			t.Errorf("%s 标签 = %q，期望 %q", d.ID, d.Label, wantLabels[i])
		}
		if d.LowEnd == "" || d.HighEnd == "" {
			t.Errorf("%s 缺端点：low=%q high=%q", d.ID, d.LowEnd, d.HighEnd)
		}
		for name, text := range map[string]string{
			"low": d.LowBand, "mid": d.MidBand, "high": d.HighBand,
		} {
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s 缺 %s 段文案", d.ID, name)
			}
		}
	}
	if _, ok := PersonalityDimensionByID(DimensionPersistence); !ok {
		t.Error("按 ID 取定义失败")
	}
	if _, ok := PersonalityDimensionByID("extraversion"); ok {
		t.Error("不接受第二套维度 ID（外向度）")
	}
	// 返回值是副本：调用方改它不能污染真源。
	dims[0].Label = "被改了"
	if again, _ := PersonalityDimensionByID(DimensionSociability); again.Label != "交往意愿" {
		t.Fatalf("定义被外部改动: %q", again.Label)
	}
}

// TestPersonalityConfigValidate 显式非法一律报错，不钳制。
func TestPersonalityConfigValidate(t *testing.T) {
	def := DefaultPersonality()
	if def.Sociability != 50 || def.Exploration != 50 || def.Persistence != 50 || def.Regularity != 50 {
		t.Fatalf("缺值默认应为四项 50: %+v", def)
	}
	if def.SchemaVersion != 1 || def.Revision != 0 {
		t.Fatalf("默认版本/修订不对: %+v", def)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("默认配置应合法: %v", err)
	}

	bad := []struct {
		name string
		cfg  PersonalityConfig
	}{
		{"schema 版本", PersonalityConfig{SchemaVersion: 2, Sociability: 50, Exploration: 50, Persistence: 50, Regularity: 50}},
		{"负修订", PersonalityConfig{SchemaVersion: 1, Revision: -1, Sociability: 50, Exploration: 50, Persistence: 50, Regularity: 50}},
		{"上越界", PersonalityConfig{SchemaVersion: 1, Sociability: 101, Exploration: 50, Persistence: 50, Regularity: 50}},
		{"下越界", PersonalityConfig{SchemaVersion: 1, Sociability: 50, Exploration: -1, Persistence: 50, Regularity: 50}},
	}
	for _, c := range bad {
		err := c.cfg.Validate()
		if err == nil {
			t.Errorf("%s 应被判非法", c.name)
			continue
		}
		if !errors.Is(err, ErrPersonalityInvalid) {
			t.Errorf("%s 错误应可 errors.Is 到 ErrPersonalityInvalid: %v", c.name, err)
		}
	}
	// 边界值 0 与 100 合法。
	edge := PersonalityConfig{SchemaVersion: 1, Sociability: 0, Exploration: 100, Persistence: 0, Regularity: 100}
	if err := edge.Validate(); err != nil {
		t.Fatalf("端点 0/100 应合法: %v", err)
	}
}

// TestPersonalityJSONShape 存储形状就是票面约定：schema_version + 修订 + 代次 + 四维，
// 且不多出第二套字段。将来塞进「外向度」或第二组权重会让这里变红。
func TestPersonalityJSONShape(t *testing.T) {
	blob, err := MarshalPersonality(DefaultPersonality())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schema_version", "revision", "generation",
		"sociability", "exploration", "persistence", "regularity",
	}
	if len(fields) != len(want) {
		t.Fatalf("JSON 字段数 = %d，期望 %d：%s", len(fields), len(want), blob)
	}
	for _, k := range want {
		if _, ok := fields[k]; !ok {
			t.Errorf("缺字段 %s：%s", k, blob)
		}
	}
	// 解析回来逐项相同。
	back, present, err := ParsePersonality(blob)
	if err != nil || !present {
		t.Fatalf("回读失败: present=%v err=%v", present, err)
	}
	if back != DefaultPersonality() {
		t.Fatalf("回读不一致: %+v", back)
	}
	// 空串是缺值，不是错误。
	if cfg, present, err := ParsePersonality("  "); err != nil || present || cfg != DefaultPersonality() {
		t.Fatalf("缺值处理不对: present=%v err=%v cfg=%+v", present, err, cfg)
	}
	// 坏 JSON：报非法，且仍给出可用默认（界面据此提示修复）。
	if _, present, err := ParsePersonality("{oops"); err == nil || !present {
		t.Fatalf("坏 JSON 应报非法: present=%v err=%v", present, err)
	}
	// 超域 JSON：报非法，但把越界值原样带出来，管理员能看到是哪一项。
	cfg, present, err := ParsePersonality(
		`{"schema_version":1,"revision":3,"generation":1,"sociability":120,"exploration":50,"persistence":50,"regularity":50}`)
	if err == nil || !present {
		t.Fatalf("超域应报非法: present=%v err=%v", present, err)
	}
	if cfg.Sociability != 120 || cfg.Revision != 3 {
		t.Fatalf("越界值应原样返回供修复: %+v", cfg)
	}
}

// TestPersonalityBandEndpoints 分段端点：0..32 偏低、33..67 适中、68..100 偏高。
func TestPersonalityBandEndpoints(t *testing.T) {
	cases := []struct {
		value int
		want  string
	}{
		{0, PersonalityBandLow}, {32, PersonalityBandLow},
		{33, PersonalityBandMid}, {67, PersonalityBandMid},
		{68, PersonalityBandHigh}, {100, PersonalityBandHigh},
	}
	for _, c := range cases {
		if got := PersonalityBandOf(c.value); got != c.want {
			t.Errorf("取值 %d 分段 = %q，期望 %q", c.value, got, c.want)
		}
	}
	// 同一份定义编译出的四句约束按分段取文案。
	low := PersonalityConfig{SchemaVersion: 1, Sociability: 0, Exploration: 0, Persistence: 0, Regularity: 0}
	text := CompilePersonalityExpression(low)
	for _, want := range []string{"交往意愿 0/100（偏低）", "一个人待着", "守在熟悉的", "做到够就收", "作息灵活"} {
		if !strings.Contains(text, want) {
			t.Errorf("低端表达缺 %q：\n%s", want, text)
		}
	}
	high := PersonalityConfig{SchemaVersion: 1, Sociability: 100, Exploration: 100, Persistence: 100, Regularity: 100}
	text = CompilePersonalityExpression(high)
	for _, want := range []string{"交往意愿 100/100（偏高）", "一起行动", "新钓点", "多留一会", "固定"} {
		if !strings.Contains(text, want) {
			t.Errorf("高端表达缺 %q：\n%s", want, text)
		}
	}
	// 每句都带对应当前行为的解释，不是只有标签。
	if n := strings.Count(text, "\n- "); n != 4 {
		t.Errorf("表达约束应为 4 句，实际 %d 句：\n%s", n, text)
	}
}

// TestExpressionAndBehaviourSameRevision 表达编译与行为派生读出同一个版本。
func TestExpressionAndBehaviourSameRevision(t *testing.T) {
	cfg := PersonalityConfig{
		SchemaVersion: 1, Revision: 7, Generation: 2,
		Sociability: 80, Exploration: 20, Persistence: 65, Regularity: 50,
	}
	choice := DecidePersonality(cfg, []PersonalityCandidate{
		{ID: CandidateSilence, Feasible: true, Feature: PersonalityFeatureNeutral},
		{ID: "invite", Feasible: true, Feature: PersonalityFeature{Social: 1, Novelty: 0.2, Investment: 0.5, Rhythm: 0.4}},
	}, 42)
	if choice.SchemaVersion != cfg.SchemaVersion || choice.Revision != cfg.Revision {
		t.Fatalf("行为派生版本 = v%d#%d，期望 v%d#%d",
			choice.SchemaVersion, choice.Revision, cfg.SchemaVersion, cfg.Revision)
	}
	text := CompilePersonalityExpression(cfg)
	if !strings.Contains(text, "schema v1") || !strings.Contains(text, "修订 7") {
		t.Fatalf("表达编译未带同版号：\n%s", text)
	}
	// 表达里不出现第二套性格标签。
	for _, banned := range []string{"外向度", "外向", "大五", "神经质"} {
		if strings.Contains(text, banned) {
			t.Fatalf("表达里出现了第二套标签 %q：\n%s", banned, text)
		}
	}
}

// TestDecidePersonalityDeterministic 同输入同种子得同结果；逆序传入结果一致。
func TestDecidePersonalityDeterministic(t *testing.T) {
	cfg := PersonalityConfig{SchemaVersion: 1, Revision: 4, Sociability: 70, Exploration: 30, Persistence: 60, Regularity: 40}
	cands := []PersonalityCandidate{
		{ID: "roundtable", Feasible: true, Feature: PersonalityFeature{Social: 0.9, Novelty: 0.2, Investment: 0.5, Rhythm: 0.5}},
		{ID: CandidateSilence, Feasible: true, Feature: PersonalityFeature{Social: 0, Novelty: 0, Investment: 0.3, Rhythm: 0.6}},
		{ID: "new_spot", Feasible: true, Feature: PersonalityFeature{Social: 0.3, Novelty: 0.9, Investment: 0.4, Rhythm: 0.3}},
		{ID: "blocked", Feasible: false, Feature: PersonalityFeature{Social: 1, Novelty: 1, Investment: 1, Rhythm: 1}},
	}
	first := DecidePersonality(cfg, cands, 20261118)
	second := DecidePersonality(cfg, cands, 20261118)
	if first.ChosenID != second.ChosenID {
		t.Fatalf("同种子结果不同: %q vs %q", first.ChosenID, second.ChosenID)
	}
	if !reflect.DeepEqual(first.Weights, second.Weights) {
		t.Fatalf("同种子权重不同:\n%+v\n%+v", first.Weights, second.Weights)
	}
	// 逆序传入：稳定排序后权重与结果都一致。
	reversed := make([]PersonalityCandidate, 0, len(cands))
	for i := len(cands) - 1; i >= 0; i-- {
		reversed = append(reversed, cands[i])
	}
	third := DecidePersonality(cfg, reversed, 20261118)
	if third.ChosenID != first.ChosenID {
		t.Fatalf("逆序候选结果不同: %q vs %q", third.ChosenID, first.ChosenID)
	}
	if !reflect.DeepEqual(third.Weights, first.Weights) {
		t.Fatalf("逆序候选权重不同:\n%+v\n%+v", third.Weights, first.Weights)
	}
	// 换种子允许换结果，但权重分布不变。
	other := DecidePersonality(cfg, cands, 7)
	if !reflect.DeepEqual(other.Weights, first.Weights) {
		t.Fatal("换种子不应改变权重，只应改变抽签")
	}
}

// TestDecidePersonalityWeights 不可行候选权重为 0，可行候选归一；只有静默时必静默。
func TestDecidePersonalityWeights(t *testing.T) {
	cfg := DefaultPersonality()
	cands := []PersonalityCandidate{
		{ID: "doable", Feasible: true, Feature: PersonalityFeature{Social: 0.6, Novelty: 0.5, Investment: 0.5, Rhythm: 0.5}},
		{ID: "impossible", Feasible: false, Feature: PersonalityFeature{Social: 1, Novelty: 1, Investment: 1, Rhythm: 1}},
		{ID: CandidateSilence, Feasible: true, Feature: PersonalityFeatureNeutral},
	}
	choice := DecidePersonality(cfg, cands, 1)
	if !choice.Decided {
		t.Fatalf("有可行候选时应产出结果: %+v", choice)
	}
	sum := 0.0
	for _, w := range choice.Weights {
		if w.ID == "impossible" {
			if w.Weight != 0 || w.Score != 0 {
				t.Fatalf("不可行候选必须权重 0、得分 0: %+v", w)
			}
			continue
		}
		if w.Weight <= 0 {
			t.Fatalf("可行候选权重应大于 0: %+v", w)
		}
		sum += w.Weight
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("可行候选权重和 = %.12f，期望 1", sum)
	}

	only := DecidePersonality(cfg, []PersonalityCandidate{
		{ID: CandidateSilence, Feasible: true, Feature: PersonalityFeatureNeutral},
		{ID: "impossible", Feasible: false, Feature: PersonalityFeatureNeutral},
	}, 99)
	if !only.Decided || only.ChosenID != CandidateSilence {
		t.Fatalf("只有静默可行时必须静默: %+v", only)
	}
	for _, w := range only.Weights {
		if w.ID == CandidateSilence && math.Abs(w.Weight-1) > 1e-9 {
			t.Fatalf("唯一可行候选权重应为 1: %+v", w)
		}
	}
}

// TestDecidePersonalityEmptyAndDuplicate 空集返回无动作；重复 ID 是调用方错误。
func TestDecidePersonalityEmptyAndDuplicate(t *testing.T) {
	cfg := DefaultPersonality()
	for name, cands := range map[string][]PersonalityCandidate{
		"空集": nil,
		"全不可行": {
			{ID: "a", Feasible: false, Feature: PersonalityFeatureNeutral},
			{ID: "b", Feasible: false, Feature: PersonalityFeatureNeutral},
		},
	} {
		choice := DecidePersonality(cfg, cands, 5)
		if choice.Decided || choice.ChosenID != "" {
			t.Fatalf("%s 应返回无动作: %+v", name, choice)
		}
		if choice.Reason != ChoiceNoFeasible {
			t.Fatalf("%s 原因 = %q，期望 %q", name, choice.Reason, ChoiceNoFeasible)
		}
	}
	dup := DecidePersonality(cfg, []PersonalityCandidate{
		{ID: "same", Feasible: true, Feature: PersonalityFeatureNeutral},
		{ID: "same", Feasible: true, Feature: PersonalityFeatureNeutral},
	}, 5)
	if dup.Decided || dup.Reason != ChoiceDuplicateID {
		t.Fatalf("重复 ID 应被拒绝: %+v", dup)
	}
	// 非法配置也不产出决定。
	invalid := PersonalityConfig{SchemaVersion: 9}
	if got := DecidePersonality(invalid, []PersonalityCandidate{
		{ID: "a", Feasible: true},
	}, 5); got.Decided || got.Reason != ChoiceInvalidConfig {
		t.Fatalf("非法配置应拒绝派生: %+v", got)
	}
}

// TestDecidePersonalityThreeEvents 同一个选择器吃三类事件：权重都归一，且都产出结果。
func TestDecidePersonalityThreeEvents(t *testing.T) {
	cfg := PersonalityConfig{SchemaVersion: 1, Revision: 2, Sociability: 75, Exploration: 45, Persistence: 60, Regularity: 55}
	events := map[string][]PersonalityCandidate{
		// 圆桌发起/接话：社交权重高。
		"roundtable": {
			{ID: "join_talk", Feasible: true, Feature: PersonalityFeature{Social: 0.9, Novelty: 0.3, Investment: 0.5, Rhythm: 0.5}},
			{ID: CandidateSilence, Feasible: true, Feature: PersonalityFeature{Social: 0.1, Novelty: 0.1, Investment: 0.4, Rhythm: 0.6}},
		},
		// 玩家星/蓝反应与升级：社交中等、投入低。
		"player_catch": {
			{ID: "single_reply", Feasible: true, Feature: PersonalityFeature{Social: 0.6, Novelty: 0.2, Investment: 0.2, Rhythm: 0.3}},
			{ID: "upgrade_roundtable", Feasible: true, Feature: PersonalityFeature{Social: 0.8, Novelty: 0.3, Investment: 0.3, Rhythm: 0.4}},
			{ID: CandidateSilence, Feasible: true, Feature: PersonalityFeature{Social: 0, Novelty: 0, Investment: 0.2, Rhythm: 0.5}},
		},
		// 自己渔获播报/升级：作息与投入权重高。
		"own_catch": {
			{ID: "report", Feasible: true, Feature: PersonalityFeature{Social: 0.3, Novelty: 0.2, Investment: 0.7, Rhythm: 0.8}},
			{ID: CandidateSilence, Feasible: true, Feature: PersonalityFeature{Social: 0.1, Novelty: 0.1, Investment: 0.5, Rhythm: 0.7}},
		},
	}
	for name, cands := range events {
		choice := DecidePersonality(cfg, cands, 11)
		if !choice.Decided {
			t.Fatalf("%s 应产出结果: %+v", name, choice)
		}
		sum := 0.0
		for _, w := range choice.Weights {
			sum += w.Weight
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Fatalf("%s 权重和 = %.12f，期望 1", name, sum)
		}
		if len(choice.Weights) != len(cands) {
			t.Fatalf("%s 权重条数 = %d，期望 %d", name, len(choice.Weights), len(cands))
		}
	}
}

// TestPersonalityCombinedEndpoints 组合端点：高交往 + 高投入必须能同时表达，
// 且与单维拉高的排序不同（不是把两维压成一个「热情」标量）。
func TestPersonalityCombinedEndpoints(t *testing.T) {
	togetherLong := PersonalityFeature{Social: 1, Novelty: 0.1, Investment: 0.9, Rhythm: 0.5}
	aloneShort := PersonalityFeature{Social: 0.1, Novelty: 0.1, Investment: 0.2, Rhythm: 0.5}

	base := PersonalityConfig{SchemaVersion: 1, Sociability: 50, Exploration: 50, Persistence: 50, Regularity: 50}
	combo := PersonalityConfig{SchemaVersion: 1, Sociability: 95, Exploration: 50, Persistence: 95, Regularity: 50}
	socialOnly := PersonalityConfig{SchemaVersion: 1, Sociability: 95, Exploration: 50, Persistence: 50, Regularity: 50}
	persistOnly := PersonalityConfig{SchemaVersion: 1, Sociability: 50, Exploration: 50, Persistence: 95, Regularity: 50}

	// 中性配置对两个候选没有偏好差。
	if d := base.Score(togetherLong) - base.Score(aloneShort); math.Abs(d) > 1e-9 {
		t.Fatalf("中性配置不应有偏好差: %.6f", d)
	}
	// 组合端点对「一起久待」的偏好必须大于任一单维。
	comboPref := combo.Score(togetherLong) - combo.Score(aloneShort)
	socialPref := socialOnly.Score(togetherLong) - socialOnly.Score(aloneShort)
	persistPref := persistOnly.Score(togetherLong) - persistOnly.Score(aloneShort)
	if !(comboPref > socialPref && comboPref > persistPref) {
		t.Fatalf("组合端点偏好 %.4f 应大于单维 %.4f / %.4f", comboPref, socialPref, persistPref)
	}
	choice := DecidePersonality(combo, []PersonalityCandidate{
		{ID: "together_long", Feasible: true, Feature: togetherLong},
		{ID: "alone_short", Feasible: true, Feature: aloneShort},
	}, 3)
	var wTogether, wAlone float64
	for _, w := range choice.Weights {
		switch w.ID {
		case "together_long":
			wTogether = w.Weight
		case "alone_short":
			wAlone = w.Weight
		}
	}
	if !(wTogether > wAlone) {
		t.Fatalf("高交往+高投入应更偏向一起久待: %.4f vs %.4f", wTogether, wAlone)
	}
}

// TestDwellFloorMonotonicInPersistence 提高 p 不降低底线；r 只放大或持平。
func TestDwellFloorMonotonicInPersistence(t *testing.T) {
	tun := PersonalityTunables{HardFloorSeconds: 1200, ExtraSeconds: 3600, RestBaseSeconds: 600}
	prev := -1.0
	for p := 0; p <= 100; p++ {
		cfg := PersonalityConfig{SchemaVersion: 1, Sociability: 50, Exploration: 50, Persistence: p, Regularity: 50}
		got := cfg.DwellFloorSeconds(tun)
		if got < prev {
			t.Fatalf("p=%d 底线 %.2f 低于 p=%d 的 %.2f", p, got, p-1, prev)
		}
		prev = got
	}
	low := PersonalityConfig{SchemaVersion: 1, Persistence: 0, Regularity: 0}
	high := PersonalityConfig{SchemaVersion: 1, Persistence: 100, Regularity: 100}
	if got := low.DwellFloorSeconds(tun); math.Abs(got-1200) > 1e-9 {
		t.Fatalf("p=0 时底线应等于硬底线 1200，实际 %.2f", got)
	}
	if got := high.DwellFloorSeconds(tun); math.Abs(got-(1200+3600)) > 1e-9 {
		t.Fatalf("p=100,r=100 时底线应为 4800，实际 %.2f", got)
	}
	// 同样 p 下，作息更规律不会缩短底线（Dsoft 里的 (0.5+0.5r) 项）。
	a := PersonalityConfig{SchemaVersion: 1, Persistence: 60, Regularity: 0}
	b := PersonalityConfig{SchemaVersion: 1, Persistence: 60, Regularity: 100}
	if b.DwellFloorSeconds(tun) < a.DwellFloorSeconds(tun) {
		t.Fatal("r 更高不应缩短底线")
	}
	// 负的基数按 0 处理，不允许把底线压到硬底线以下。
	neg := PersonalityTunables{HardFloorSeconds: 1200, ExtraSeconds: -5000, RestBaseSeconds: -1}
	if got := high.DwellFloorSeconds(neg); got < 1200 {
		t.Fatalf("负基数不应压低底线: %.2f", got)
	}
}

// TestRestIntervalFromRegularity Trest=Tbase×(1+0.5r)：灵活 1.0 倍、规律 1.5 倍。
func TestRestIntervalFromRegularity(t *testing.T) {
	tun := PersonalityTunables{HardFloorSeconds: 1200, ExtraSeconds: 3600, RestBaseSeconds: 600}
	flexible := PersonalityConfig{SchemaVersion: 1, Regularity: 0}
	fixed := PersonalityConfig{SchemaVersion: 1, Regularity: 100}
	if got := flexible.RestIntervalSeconds(tun); math.Abs(got-600) > 1e-9 {
		t.Fatalf("r=0 休整节拍应为 600，实际 %.2f", got)
	}
	if got := fixed.RestIntervalSeconds(tun); math.Abs(got-900) > 1e-9 {
		t.Fatalf("r=100 休整节拍应为 900，实际 %.2f", got)
	}
	if fixed.RestIntervalSeconds(tun) <= flexible.RestIntervalSeconds(tun) {
		t.Fatal("作息越规律，休整节拍应更长或相等")
	}
}

// TestPersonalityFeatureHasNoSecondWeightSet 特征只有四项：多一组权重会让这里变红。
func TestPersonalityFeatureHasNoSecondWeightSet(t *testing.T) {
	typ := reflect.TypeOf(PersonalityFeature{})
	if typ.NumField() != 4 {
		t.Fatalf("特征应为 4 项，实际 %d 项", typ.NumField())
	}
	want := []string{"Social", "Novelty", "Investment", "Rhythm"}
	for i, name := range want {
		f := typ.Field(i)
		if f.Name != name {
			t.Errorf("第 %d 项 = %s，期望 %s", i, f.Name, name)
		}
		if f.Type.Kind() != reflect.Float64 {
			t.Errorf("%s 类型 = %v，期望 float64", f.Name, f.Type)
		}
	}
	// 候选结构也只有 ID / 可行 / 特征三项：不允许塞进旧概率或个体偏置。
	cand := reflect.TypeOf(PersonalityCandidate{})
	if cand.NumField() != 3 {
		t.Fatalf("候选应为 3 项，实际 %d 项", cand.NumField())
	}
	for _, banned := range []string{"Chance", "Bias", "Weight", "Talkativeness", "ReplyMode"} {
		if _, ok := cand.FieldByName(banned); ok {
			t.Fatalf("候选不应接受旧概率/个体权重字段 %s", banned)
		}
	}
}

// TestNeutralFeatureGivesUniformWeights 未知事实用中性同值：四个候选权重相等。
func TestNeutralFeatureGivesUniformWeights(t *testing.T) {
	cfg := PersonalityConfig{SchemaVersion: 1, Sociability: 100, Exploration: 0, Persistence: 80, Regularity: 20}
	cands := []PersonalityCandidate{
		{ID: "a", Feasible: true, Feature: PersonalityFeatureNeutral},
		{ID: "b", Feasible: true, Feature: PersonalityFeatureNeutral},
		{ID: CandidateSilence, Feasible: true, Feature: PersonalityFeatureNeutral},
		{ID: "d", Feasible: true, Feature: PersonalityFeatureNeutral},
	}
	choice := DecidePersonality(cfg, cands, 8)
	for _, w := range choice.Weights {
		if math.Abs(w.Weight-0.25) > 1e-9 {
			t.Fatalf("中性同值应给出均匀权重，%s=%.6f", w.ID, w.Weight)
		}
	}
	// 特征越界按端点处理，不产生 NaN。
	weird := PersonalityFeature{Social: 2, Novelty: -1, Investment: math.NaN(), Rhythm: 0.5}
	score := cfg.Score(weird)
	if math.IsNaN(score) || math.IsInf(score, 0) {
		t.Fatalf("越界特征不应算出 NaN/Inf: %v", score)
	}
	clamped := cfg.Score(PersonalityFeature{Social: 1, Novelty: 0, Investment: 0, Rhythm: 0.5})
	if math.Abs(score-clamped) > 1e-9 {
		t.Fatalf("越界特征应按端点计算: %v vs %v", score, clamped)
	}
}

// TestConfigWithValue 改单维保持其余维不变；未知维度报错。
func TestConfigWithValue(t *testing.T) {
	cfg := DefaultPersonality()
	got, err := cfg.WithValue(DimensionExploration, 88)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exploration != 88 || got.Sociability != 50 || got.Persistence != 50 || got.Regularity != 50 {
		t.Fatalf("改单维影响到了别的维: %+v", got)
	}
	if _, err := cfg.WithValue("extraversion", 50); !errors.Is(err, ErrPersonalityInvalid) {
		t.Fatalf("未知维度应报错: %v", err)
	}
}

// ---- R-113 反证回归：显式配置的字段完整性与修复修订口径 ----

// TestParsePersonalityRequiresEveryDimension 缺必需维度或非整数维度必须报需修复，
// 不能把「字段缺失」当成合法的 0；显式写 0 与缺值的中性默认要分得清。
func TestParsePersonalityRequiresEveryDimension(t *testing.T) {
	const full = `{"schema_version":1,"revision":7,"generation":1,"sociability":0,"exploration":0,"persistence":0,"regularity":0}`

	// 显式 0 是合法值，不能被当成缺值。
	cfg, present, err := ParsePersonality(full)
	if err != nil || !present {
		t.Fatalf("显式 0 应合法: present=%v err=%v", present, err)
	}
	if cfg.Sociability != 0 || cfg.Exploration != 0 || cfg.Persistence != 0 || cfg.Regularity != 0 {
		t.Fatalf("显式 0 应原样读出: %+v", cfg)
	}
	if cfg.Revision != 7 {
		t.Fatalf("显式配置的修订应为 7，实际 %d", cfg.Revision)
	}

	// 缺键不是错误：中性默认四项 50。
	cfg, present, err = ParsePersonality("")
	if err != nil || present || cfg != DefaultPersonality() {
		t.Fatalf("缺值处理不对: present=%v err=%v cfg=%+v", present, err, cfg)
	}

	// 主管反证原文：缺 regularity，其余合法。必须报需修复，且修订仍可查。
	const missingRegularity = `{"schema_version":1,"revision":7,"generation":1,"sociability":60,"exploration":50,"persistence":50}`
	cfg, present, err = ParsePersonality(missingRegularity)
	if err == nil {
		t.Fatal("缺必需维度 regularity 应报需修复，不能当成合法的 0")
	}
	if !errors.Is(err, ErrPersonalityInvalid) || !present {
		t.Fatalf("应报 ErrPersonalityInvalid 且 present=true: present=%v err=%v", present, err)
	}
	if cfg.Revision != 7 {
		t.Fatalf("需修复时原值修订仍应可查（7），实际 %d", cfg.Revision)
	}

	// 七个必需字段逐个缺失都必须报错。
	for _, field := range []string{
		"schema_version", "revision", "generation",
		"sociability", "exploration", "persistence", "regularity",
	} {
		if _, _, err := ParsePersonality(withoutJSONField(t, full, field)); err == nil {
			t.Errorf("缺必需字段 %s 应报需修复", field)
		}
	}

	// 非整数维度：小数、字符串、null 都不接受。
	for name, raw := range map[string]string{
		"小数":   `{"schema_version":1,"revision":7,"generation":1,"sociability":60.5,"exploration":50,"persistence":50,"regularity":50}`,
		"字符串":  `{"schema_version":1,"revision":7,"generation":1,"sociability":"60","exploration":50,"persistence":50,"regularity":50}`,
		"null": `{"schema_version":1,"revision":7,"generation":1,"sociability":null,"exploration":50,"persistence":50,"regularity":50}`,
	} {
		if _, present, err := ParsePersonality(raw); err == nil || !present {
			t.Errorf("%s 维度应报需修复: present=%v err=%v", name, present, err)
		}
	}
}

// TestPersonalityRevisionOfIsTheOnlyYardstick 读侧给出的可提交修订与写侧比较口径必须一致：
// 合法、越界、缺字段、坏 JSON、缺修订、负修订都要逐一对齐。
func TestPersonalityRevisionOfIsTheOnlyYardstick(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
		ok   bool
	}{
		{"合法", `{"schema_version":1,"revision":7,"generation":1,"sociability":50,"exploration":50,"persistence":50,"regularity":50}`, 7, true},
		{"维度越界", `{"schema_version":1,"revision":7,"generation":1,"sociability":120,"exploration":50,"persistence":50,"regularity":50}`, 7, true},
		{"缺维度", `{"schema_version":1,"revision":3,"generation":1,"sociability":60,"exploration":50,"persistence":50}`, 3, true},
		{"未知版本", `{"schema_version":2,"revision":5,"generation":1,"sociability":50,"exploration":50,"persistence":50,"regularity":50}`, 5, true},
		{"缺修订", `{"schema_version":1,"generation":1,"sociability":50,"exploration":50,"persistence":50,"regularity":50}`, 0, false},
		{"小数修订", `{"schema_version":1,"revision":7.5,"generation":1,"sociability":50,"exploration":50,"persistence":50,"regularity":50}`, 0, false},
		{"负修订", `{"schema_version":1,"revision":-3,"generation":1,"sociability":50,"exploration":50,"persistence":50,"regularity":50}`, 0, false},
		{"坏 JSON", `{oops`, 0, false},
		{"空值", ``, 0, false},
	}
	for _, c := range cases {
		got, ok := PersonalityRevisionOf(c.raw)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: PersonalityRevisionOf = (%d,%v)，期望 (%d,%v)", c.name, got, ok, c.want, c.ok)
		}
		// 读侧：ParsePersonality 报回来的修订必须与它能提交的修订一致。
		cfg, _, _ := ParsePersonality(c.raw)
		if cfg.Revision != got {
			t.Errorf("%s: ParsePersonality.Revision = %d，与可提交修订 %d 不一致", c.name, cfg.Revision, got)
		}
	}
}

// withoutJSONField 返回删掉一个顶层字段的 JSON 文本。
func withoutJSONField(t *testing.T, raw, field string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("拆字段: %v", err)
	}
	delete(fields, field)
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("重组字段: %v", err)
	}
	return string(b)
}

// ---- T-107 评分草稿：分值区间、取值契约与提示词 ----

// TestPersonalityBandsCoverDomainOnce 三段区间只此一份、首尾相接且覆盖 0..100：
// 改端点或另写一套阈值都会在这里变红。
func TestPersonalityBandsCoverDomainOnce(t *testing.T) {
	bands := PersonalityBands()
	if len(bands) != 3 {
		t.Fatalf("分段应为 3 段，实际 %d", len(bands))
	}
	want := []PersonalityBand{
		{Band: PersonalityBandLow, Label: "偏低", Min: 0, Max: 32},
		{Band: PersonalityBandMid, Label: "适中", Min: 33, Max: 67},
		{Band: PersonalityBandHigh, Label: "偏高", Min: 68, Max: 100},
	}
	for i, band := range bands {
		if band != want[i] {
			t.Errorf("第 %d 段 = %+v，期望 %+v", i+1, band, want[i])
		}
		if i > 0 && band.Min != bands[i-1].Max+1 {
			t.Errorf("%s 与上一段之间断档：%d", band.Band, band.Min)
		}
	}
	if bands[0].Min != PersonalityMin || bands[2].Max != PersonalityMax {
		t.Error("分段没有覆盖 0..100")
	}
	// 分段函数与分段边界必须是同一份判断。
	for _, tc := range []struct {
		value int
		band  string
	}{{0, PersonalityBandLow}, {32, PersonalityBandLow}, {33, PersonalityBandMid},
		{67, PersonalityBandMid}, {68, PersonalityBandHigh}, {100, PersonalityBandHigh}} {
		if got := PersonalityBandOf(tc.value); got != tc.band {
			t.Errorf("PersonalityBandOf(%d) = %q，期望 %q", tc.value, got, tc.band)
		}
	}
}

// TestPersonalityScoreValuesValidate 超域与非整数一律拒绝，不钳制。
func TestPersonalityScoreValuesValidate(t *testing.T) {
	good := PersonalityScoreValues{Sociability: 0, Exploration: 50, Persistence: 68, Regularity: 100}
	if err := good.Validate(); err != nil {
		t.Fatalf("合法取值被拒: %v", err)
	}
	for name, bad := range map[string]PersonalityScoreValues{
		"负值":  {Sociability: -1},
		"超上限": {Regularity: 101},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrPersonalityScoreInvalid) {
			t.Errorf("%s 应报 ErrPersonalityScoreInvalid，得到 %v", name, err)
		}
	}
	// 与配置取值的换算口径：同一份四维，不带修订/代次。
	cfg := PersonalityConfig{SchemaVersion: PersonalitySchemaVersion, Revision: 7,
		Sociability: 0, Exploration: 50, Persistence: 68, Regularity: 100}
	if got := cfg.ScoreValues(); got != good {
		t.Fatalf("ScoreValues = %+v，期望 %+v", got, good)
	}
	for _, dim := range PersonalityDimensions() {
		if _, ok := good.ValueOf(dim.ID); !ok {
			t.Errorf("四维 %s 取不到值", dim.ID)
		}
	}
	if _, ok := good.ValueOf("extraversion"); ok {
		t.Error("不接受第二套维度 ID")
	}
}

// TestPersonalityScoreDraftValidate 区间必须包含建议值，短理由必须合规。
func TestPersonalityScoreDraftValidate(t *testing.T) {
	draft := PersonalityScoreDraft{
		SchemaVersion: PersonalitySchemaVersion,
		Values:        PersonalityScoreValues{Sociability: 60, Exploration: 40, Persistence: 70, Regularity: 55},
		Ranges: PersonalityScoreRanges{
			Sociability: PersonalityScoreRange{45, 70},
			Exploration: PersonalityScoreRange{30, 55},
			Persistence: PersonalityScoreRange{55, 80},
			Regularity:  PersonalityScoreRange{40, 70},
		},
		Reasons: PersonalityScoreReasons{
			Sociability: "描述里爱主动约人",
			Exploration: "愿意尝试新水域",
			Persistence: "做事有耐心",
			Regularity:  "提到固定作息",
		},
	}
	if err := draft.Validate(); err != nil {
		t.Fatalf("合法草稿被拒: %v", err)
	}
	if !draft.Ranges.Sociability.Contains(60) || draft.Ranges.Sociability.Contains(44) {
		t.Error("Contains 判定错误")
	}

	cases := []struct {
		name   string
		mutate func(d *PersonalityScoreDraft)
	}{
		{"未知版本", func(d *PersonalityScoreDraft) { d.SchemaVersion = PersonalitySchemaVersion + 1 }},
		{"区间倒置", func(d *PersonalityScoreDraft) { d.Ranges.Sociability = PersonalityScoreRange{70, 45} }},
		{"区间越界", func(d *PersonalityScoreDraft) { d.Ranges.Sociability = PersonalityScoreRange{-1, 70} }},
		{"区间不含建议值", func(d *PersonalityScoreDraft) { d.Ranges.Sociability = PersonalityScoreRange{65, 70} }},
		{"建议值超域", func(d *PersonalityScoreDraft) { d.Values.Sociability = 120 }},
		{"理由为空", func(d *PersonalityScoreDraft) { d.Reasons.Persistence = "  " }},
		{"理由过长", func(d *PersonalityScoreDraft) {
			d.Reasons.Regularity = strings.Repeat("长", PersonalityScoreReasonMaxRunes+1)
		}},
		{"理由含换行", func(d *PersonalityScoreDraft) { d.Reasons.Exploration = "愿意尝试\n新水域" }},
	}
	for _, tc := range cases {
		bad := draft
		tc.mutate(&bad)
		if err := bad.Validate(); !errors.Is(err, ErrPersonalityScoreInvalid) {
			t.Errorf("%s 应报 ErrPersonalityScoreInvalid，得到 %v", tc.name, err)
		}
	}
	// 理由恰好到上限是合法的（边界不是超限）。
	atLimit := draft
	atLimit.Reasons.Sociability = strings.Repeat("长", PersonalityScoreReasonMaxRunes)
	if err := atLimit.Validate(); err != nil {
		t.Errorf("理由到上限应合法: %v", err)
	}
}

// TestDecodePersonalityScoreValuesStrict 严格解析四维：缺维度不能退化成 0，
// 非整数、超域与额外键一律拒绝。
func TestDecodePersonalityScoreValuesStrict(t *testing.T) {
	full := `{"sociability":60,"exploration":40,"persistence":70,"regularity":55}`
	got, err := DecodePersonalityScoreValues([]byte(full))
	if err != nil {
		t.Fatalf("合法四维被拒: %v", err)
	}
	if got != (PersonalityScoreValues{Sociability: 60, Exploration: 40, Persistence: 70, Regularity: 55}) {
		t.Fatalf("解析结果 = %+v", got)
	}
	cases := map[string]string{
		"缺一维":    `{"sociability":60,"exploration":40,"persistence":70}`,
		"缺两维":    `{"sociability":60}`,
		"小数":     `{"sociability":60.5,"exploration":40,"persistence":70,"regularity":55}`,
		"字符串":    `{"sociability":"60","exploration":40,"persistence":70,"regularity":55}`,
		"null":   `{"sociability":null,"exploration":40,"persistence":70,"regularity":55}`,
		"超域":     `{"sociability":160,"exploration":40,"persistence":70,"regularity":55}`,
		"负值":     `{"sociability":-1,"exploration":40,"persistence":70,"regularity":55}`,
		"额外维度":   `{"sociability":60,"exploration":40,"persistence":70,"regularity":55,"extraversion":70}`,
		"额外概率键":  `{"sociability":60,"exploration":40,"persistence":70,"regularity":55,"chance":0.5}`,
		"不是对象":   `[]`,
		"坏 JSON": `{oops`,
	}
	for name, raw := range cases {
		if _, err := DecodePersonalityScoreValues([]byte(raw)); !errors.Is(err, ErrPersonalityScoreInvalid) {
			t.Errorf("%s 应报 ErrPersonalityScoreInvalid，得到 %v", name, err)
		}
	}
}

// TestPersonalityScorePromptsUseSingleDefinition 提示词只能来自同一份四维定义：
// 四个 ID、标签、两端、JSON 键与「无法评估」文案一处不少、一处不多。
func TestPersonalityScorePromptsUseSingleDefinition(t *testing.T) {
	system := PersonalityScoreSystemPrompt()
	for _, dim := range PersonalityDimensions() {
		for _, want := range []string{dim.ID, dim.Label, dim.LowEnd, dim.HighEnd} {
			if !strings.Contains(system, want) {
				t.Errorf("系统提示词缺 %q", want)
			}
		}
	}
	// JSON 形状的键名必须与草稿结构体的 json tag 完全一致（防止提示词与解析漂移）。
	for _, v := range []any{PersonalityScoreValues{}, PersonalityScoreRanges{}, PersonalityScoreReasons{}} {
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				t.Fatalf("%s 缺 json tag", typ.Name())
			}
			if !strings.Contains(system, `"`+tag+`"`) {
				t.Errorf("系统提示词缺少键 %q", tag)
			}
		}
	}
	for _, want := range []string{
		`"status":"ok"`, `"status":"unassessable"`,
		"不要解释", "不要 Markdown 代码块", "不要给出能力、工具、事件、权限、概率、配额",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("系统提示词缺 %q", want)
		}
	}
	if !strings.Contains(system, "不超过 40 字") && !strings.Contains(system, "不超过 40") {
		t.Errorf("系统提示词没有写理由字数上限 %d", PersonalityScoreReasonMaxRunes)
	}

	user := PersonalityScoreUserPrompt("喜欢安静钓鱼，愿意尝试新水域。",
		PersonalityScoreValues{Sociability: 30, Exploration: 70, Persistence: 60, Regularity: 50})
	for _, want := range []string{"喜欢安静钓鱼，愿意尝试新水域。", "交往意愿 30", "尝新意愿 70",
		"持续投入 60", "作息规律 50"} {
		if !strings.Contains(user, want) {
			t.Errorf("用户提示词缺 %q：\n%s", want, user)
		}
	}
	// 空描述不是合法评分输入，但提示词函数仍要给出明确的占位，避免拼出空段落。
	neutral := DefaultPersonality().ScoreValues()
	if got := PersonalityScoreUserPrompt("   ", neutral); !strings.Contains(got, "（空：没有可评估的描述）") {
		t.Errorf("空描述应给占位说明：%s", got)
	}
	// 超长描述按上限截断，不把整份文档塞给模型。
	long := strings.Repeat("鱼", PersonalityScoreDescriptionLimit+50)
	got := PersonalityScoreUserPrompt(long, neutral)
	if n := strings.Count(got, "鱼"); n != PersonalityScoreDescriptionLimit {
		t.Errorf("超长描述截断后 %d 字，期望 %d", n, PersonalityScoreDescriptionLimit)
	}
}
