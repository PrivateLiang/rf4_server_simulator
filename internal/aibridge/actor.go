package aibridge

// AI 角色状态（在 aibridge 这一侧）：读出来注入提示词，并在玩家"安利"某条鱼时
// 按低概率 + 冷却改一次目标。状态本身存在主库 ai_actor_state，跨进程共享。
//
// 活动不是固定标签：钓鱼会累、会去休息，休息够了再回来。这里在注入前推进一次。

import (
	"context"
	"math/rand"
	"strconv"
	"strings"
	"time"
	"unicode"

	"rf4server/internal/store"
)

// actorLine 读 Agent 当前状态，压成一句可注入 system 的话。
// 同时返回所在地图 key，供鱼情卡对齐——AI 说自己在哪，卡就得是哪张图。
// 没有状态时用玩家当前地图初始化一份；活动到期时先推进再描述。
func (r *Runner) actorLine(ctx context.Context, agentKey, fallbackLoc string) (line, loc string) {
	if r.db.AISetting(ctx, "ai_actor_enabled", "1") != "1" {
		return "", ""
	}
	now := float64(time.Now().Unix())
	rng := rand.New(rand.NewSource(store.AIActorSeed(now, agentKey)))
	a, err := r.db.AIGetActor(ctx, agentKey)
	if err != nil {
		return "", ""
	}
	if a == nil {
		locKey, locName := fallbackLoc, r.actorLocName(ctx, fallbackLoc)
		if k, n, mapErr := r.db.AIRandomMap(ctx); mapErr == nil && k != "" {
			locKey, locName = k, n
		}
		if locKey == "" {
			return "", ""
		}
		a = store.AIActorStart(agentKey, locKey, locName, now,
			r.db.AIActorConfig(ctx), rng.Float64)
		if err := r.db.AISaveActor(ctx, a); err != nil {
			return "", ""
		}
	}
	// 后台先打开开关时还没有地图；用随机地图补上，避免所有角色在同一张图。
	changed := false
	if a.Loc == "" {
		if k, n, mapErr := r.db.AIRandomMap(ctx); mapErr == nil && k != "" {
			a.Loc, a.LocName, a.LocSinceTS = k, n, now
			changed = true
		} else if fallbackLoc != "" {
			a.Loc, a.LocName, a.LocSinceTS = fallbackLoc, r.actorLocName(ctx, fallbackLoc), now
			changed = true
		}
	}
	if a.Advance(now, r.db.AIActorConfig(ctx), rng.Float64) {
		changed = true
	}
	if changed {
		if err := r.db.AISaveActor(ctx, a); err != nil {
			r.log.Warn("AI 角色状态推进失败", "agent", agentKey, "err", err)
		}
	}
	return r.actorSentence(a), a.Loc
}

// actorSentence 按当前活动写一句人话。休息时不能再说「正钓着」。
//
// 这里刻意不给精确分钟数：没人会记得自己蹲了 27 分钟，报数反而出戏。
// 只给一个定性的时间感（刚到 / 一阵子 / 挺久），让模型自己凭感觉说。
// 顺带也让状态句不再每分钟变化，前缀缓存能稳定命中。
func (r *Runner) actorSentence(a *store.AIActor) string {
	if a == nil {
		return ""
	}
	loc := a.LocName
	if loc == "" {
		loc = a.Loc
	}
	if loc == "" {
		return ""
	}
	var text string
	switch a.Activity {
	case "resting":
		text = "你在" + loc + "歇着，暂时没抛竿。"
	case "moving":
		text = "你正在去" + loc + "的路上。"
	case "idle":
		text = "你在" + loc + "闲逛，没在钓鱼。"
	default:
		text = "你在" + loc
		if a.TargetName != "" {
			text += "，正钓着" + a.TargetName
		}
		text += "。"
	}
	if a.SinceTS > 0 {
		text += durationFeel(time.Since(time.Unix(int64(a.SinceTS), 0)))
	}
	return "你现在的位置和状态：" + text +
		"（这是你此刻真实在干的事，别把地点说岔；说多久凭感觉就行，别报精确时间。）"
}

// durationFeel 把时长压成一句人话，只分三档，跨档才变。
func durationFeel(d time.Duration) string {
	switch {
	case d < 5*time.Minute:
		return "刚到没多久。"
	case d < 30*time.Minute:
		return "待了一阵子了。"
	default:
		return "在这儿磨了挺久了。"
	}
}

// actorLocName 把 level key 换成中文水域名；查不到就原样返回。
func (r *Runner) actorLocName(ctx context.Context, loc string) string {
	if loc == "" {
		return ""
	}
	var name string
	if r.knowledge != nil && r.knowledge.static != nil {
		_ = r.knowledge.static.QueryRowContext(ctx,
			`SELECT loc FROM loc_alias WHERE alias=? LIMIT 1`, loc).Scan(&name)
	}
	if name != "" {
		return name
	}
	return loc
}

// maybeSwitchActor 玩家提到某条鱼时，按概率把 Agent 的目标鱼/地图切过去。
// 概率默认 0.25、冷却默认 30 分钟：既听得进劝，又不会两句话就换个地方。
// 如果它本来在休息，被说动后会重新开始钓。
func (r *Runner) maybeSwitchActor(ctx context.Context, agentKey, msg string) {
	if r.db.AISetting(ctx, "ai_actor_enabled", "1") != "1" || r.knowledge == nil {
		return
	}
	a, err := r.db.AIGetActor(ctx, agentKey)
	if err != nil || a == nil {
		return
	}
	fish := r.knowledge.ResolveFishName(ctx, msg)
	if fish == "" || fish == a.TargetName || !fishMentioned(msg, fish) {
		return
	}
	// 冷却：刚换过就先稳住，避免来回横跳。
	cooldown := float64(r.intSetting(ctx, "ai_actor_switch_cooldown_s", 1800, 60, 21600))
	if a.SwitchTS > 0 && float64(time.Now().Unix())-a.SwitchTS < cooldown {
		return
	}
	chance, ok := r.floatSetting(ctx, "ai_actor_switch_chance", 0.25)
	if !ok || chance <= 0 || rand.Float64() >= chance {
		return
	}
	locKey, locName := r.bestMapForFish(ctx, fish)
	if locKey == "" {
		return
	}
	now := float64(time.Now().Unix())
	a.TargetFish, a.TargetName = fish, fish
	if locKey != a.Loc {
		a.Loc, a.LocName, a.LocSinceTS = locKey, locName, now
	}
	a.Activity = "fishing"
	a.SinceTS = now
	a.SwitchTS = now
	if a.ActivityUntilTS <= now {
		a.ActivityUntilTS = now + r.db.AIActorConfig(ctx).FishingMax
	}
	if err := r.db.AISaveActor(ctx, a); err != nil {
		r.log.Warn("AI 角色状态切换失败", "agent", agentKey, "err", err)
	}
}

// bestMapForFish 找这条鱼渔获最多的一张图，返回 level key 与中文水域名。
func (r *Runner) bestMapForFish(ctx context.Context, fishName string) (key, name string) {
	if r.knowledge == nil || r.knowledge.static == nil || fishName == "" {
		return "", ""
	}
	if err := r.knowledge.static.QueryRowContext(ctx, `
		SELECT loc FROM loc_fish WHERE name_zh=? ORDER BY catches DESC LIMIT 1`,
		fishName).Scan(&name); err != nil || name == "" {
		return "", ""
	}
	// 中文水域名 → level key；查不到就只换目标不换图。
	var alias string
	_ = r.knowledge.static.QueryRowContext(ctx,
		`SELECT alias FROM loc_alias WHERE loc=? LIMIT 1`, name).Scan(&alias)
	return alias, name
}

// fishMentioned 判断玩家这句话是不是真的在说这条鱼。
// ResolveFishName 有单字回退（「河鲈」→「鲈鱼」），如果直接接受，
// 「今天钓鱼」也会被归一成某条鱼，角色就会莫名换目标。
// 这里要求：完整鱼名命中、2 字片段命中，或至少一个非通用字的单字重叠。
func fishMentioned(msg, fish string) bool {
	if fish == "" {
		return false
	}
	if strings.Contains(msg, fish) {
		return true
	}
	for _, kw := range keywordsOf(msg) {
		w := strings.Trim(kw, "%")
		if len([]rune(w)) >= 2 && strings.Contains(fish, w) {
			return true
		}
	}
	for _, ch := range msg {
		if !unicode.Is(unicode.Han, ch) || ch == '鱼' || fishNameStopRunes[ch] {
			continue
		}
		if strings.ContainsRune(fish, ch) {
			return true
		}
	}
	return false
}

// floatSetting 读浮点设置；缺失时返回默认值与 true。
func (r *Runner) floatSetting(ctx context.Context, key string, def float64) (float64, bool) {
	raw := r.db.AISetting(ctx, key, "")
	if raw == "" {
		return def, true
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def, true
	}
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v, true
}
