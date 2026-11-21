package store

// AI 角色状态：每个 Agent 一行，记录「在哪张图、在干什么、想钓什么」。
// 它与对话历史解耦：历史会按预算裁剪，状态不会，所以聊多久都不会精神分裂。
//
// 状态不是固定标签，而是一段有始有终的活动：钓鱼 → 休息 → 再钓鱼。
// 推进只发生在这两处：gameserver 的钓鱼节拍、aibridge 注入状态之前。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
)

// AIActor 是 Agent 的当前状态。
type AIActor struct {
	AgentKey        string
	Loc             string
	LocName         string
	Activity        string // fishing / resting / moving / idle
	TargetFish      string
	TargetName      string
	SinceTS         float64 // 当前活动开始时间
	LocSinceTS      float64
	SwitchTS        float64
	ActivityUntilTS float64 // 当前活动预计结束时间；到点后重新掷下一段
	Energy          float64 // 0-100，钓鱼消耗、休息恢复
	UpdatedTS       float64
}

// AIActorConfig 是活动时长的参数。后台可调，但都有上下限。
type AIActorConfig struct {
	FishingMin float64
	FishingMax float64
	RestMin    float64
	RestMax    float64
	// RestChance 已废止（T-108 / DEV-110：行为概率归性格）。
	//
	// 保留字段只为不破坏既有调用方的结构体字面量；AdvanceWith 不再读取它，
	// 「这段钓完要不要休息」由调用方按 T-103 的共享派生给出。
	RestChance float64
}

// AIActorConfig 从 ai_setting 读参数；缺失时用默认值。
func (d *DB) AIActorConfig(ctx context.Context) AIActorConfig {
	return aiActorConfigFromSetting(func(key string) string { return d.AISetting(ctx, key, "") })
}

// aiActorConfigFromSetting 是角色活动参数的单一口径：DB 与运行态事务
// （AIRuntimeTx.ActorConfig）共用，避免两处默认值/上下限漂移。
func aiActorConfigFromSetting(get func(key string) string) AIActorConfig {
	c := AIActorConfig{FishingMin: 1200, FishingMax: 4800, RestMin: 600, RestMax: 2400}
	if v := aiFloatSetting(get, "ai_actor_fishing_session_min_s", c.FishingMin); v > 0 {
		c.FishingMin = v
	}
	if v := aiFloatSetting(get, "ai_actor_fishing_session_max_s", c.FishingMax); v > 0 {
		c.FishingMax = v
	}
	if v := aiFloatSetting(get, "ai_actor_rest_min_s", c.RestMin); v > 0 {
		c.RestMin = v
	}
	if v := aiFloatSetting(get, "ai_actor_rest_max_s", c.RestMax); v > 0 {
		c.RestMax = v
	}
	// ai_actor_rest_chance 不再读取：休息与否由 T-103 共享派生决定（T-108）。
	if c.FishingMax < c.FishingMin {
		c.FishingMax = c.FishingMin
	}
	if c.RestMax < c.RestMin {
		c.RestMax = c.RestMin
	}
	return c
}

func aiFloatSetting(get func(key string) string, key string, def float64) float64 {
	raw := get(key)
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
}

func (c AIActorConfig) fishingDur(roll func() float64) float64 {
	return c.FishingMin + roll()*(c.FishingMax-c.FishingMin)
}

func (c AIActorConfig) restDur(roll func() float64) float64 {
	return c.RestMin + roll()*(c.RestMax-c.RestMin)
}

// AIActorStart 初始化一个角色状态：多数时候在钓鱼，也可能已经在休息或闲逛。
func AIActorStart(agentKey, loc, locName string, now float64, cfg AIActorConfig, roll func() float64) *AIActor {
	a := &AIActor{
		AgentKey: agentKey, Loc: loc, LocName: locName,
		Activity: "fishing", Energy: 55 + roll()*35,
		SinceTS: now, LocSinceTS: now, ActivityUntilTS: now + cfg.fishingDur(roll),
	}
	switch r := roll(); {
	case r < 0.2:
		a.Activity = "resting"
		a.Energy = 30 + roll()*30
		a.ActivityUntilTS = now + cfg.restDur(roll)
	case r < 0.28:
		a.Activity = "idle"
		a.Energy = 60 + roll()*20
		a.ActivityUntilTS = now + cfg.restDur(roll)
	}
	return a
}

// airRandomMap 从有鱼的地图里随机挑一张，优先挑还没有 AI 在钓的图。
// 用于新角色初始化：不能让所有 Agent 都挤在玩家当前地图。
// q 既可以是 *sql.DB，也可以是运行态事务连接。
func airRandomMap(ctx context.Context, q AIQueryer) (loc, name string, err error) {
	rows, err := q.QueryContext(ctx, `
		SELECT loc, COUNT(*) FROM map_fish_roster
		WHERE loc<>'' GROUP BY loc`)
	if err != nil {
		return "", "", err
	}
	type cand struct {
		key string
		w   int64
	}
	list := make([]cand, 0, 32)
	for rows.Next() {
		var key string
		var w int64
		if rows.Scan(&key, &w) == nil && key != "" {
			if w < 1 {
				w = 1
			}
			list = append(list, cand{key, w})
		}
	}
	_ = rows.Close()
	if len(list) == 0 {
		return "", "", nil
	}

	used := map[string]bool{}
	if urows, uerr := q.QueryContext(ctx,
		`SELECT DISTINCT loc FROM ai_actor_state WHERE loc<>''`); uerr == nil {
		for urows.Next() {
			var l string
			if urows.Scan(&l) == nil {
				used[l] = true
			}
		}
		_ = urows.Close()
	}
	free := make([]cand, 0, len(list))
	for _, c := range list {
		if !used[c.key] {
			free = append(free, c)
		}
	}
	if len(free) > 0 {
		list = free
	}

	total := int64(0)
	for _, c := range list {
		total += c.w
	}
	pick := rand.Int63n(total)
	loc = list[len(list)-1].key
	for _, c := range list {
		pick -= c.w
		if pick < 0 {
			loc = c.key
			break
		}
	}
	_ = q.QueryRowContext(ctx,
		`SELECT loc FROM loc_alias WHERE alias=? LIMIT 1`, loc).Scan(&name)
	if name == "" {
		name = loc
	}
	return loc, name, nil
}

// AIRandomMap 只保留给既有存储层测试；生产运行态在事务内调用 airRandomMap。
func (d *DB) AIRandomMap(ctx context.Context) (loc, name string, err error) {
	return airRandomMap(ctx, d.SQL)
}

// AIActorSeed 给角色状态掷骰用：同一秒给多个 Agent 初始化，也要有不同序列。
func AIActorSeed(now float64, agentKey string) int64 {
	h := int64(0)
	for _, r := range agentKey {
		h = h*131 + int64(r)
	}
	return int64(now)*31 + h
}

// AIActorReroll 立即换一个与当前不同的活动，保留地图与目标鱼。
// 连续点「换个状态」时不能因为随机到同一个值而看起来没反应。
func AIActorReroll(a *AIActor, now float64, cfg AIActorConfig, roll func() float64) {
	if a == nil {
		return
	}
	old := a.Activity
	for i := 0; i < 8; i++ {
		n := AIActorStart(a.AgentKey, a.Loc, a.LocName, now, cfg, roll)
		if n.Activity != old {
			a.Activity, a.SinceTS = n.Activity, n.SinceTS
			a.ActivityUntilTS, a.Energy = n.ActivityUntilTS, n.Energy
			return
		}
	}
	// 兜底：在钓鱼与休息之间切换。
	if old == "fishing" {
		a.Activity = "resting"
		a.Energy = clampF(a.Energy+15, 0, 100)
		a.ActivityUntilTS = now + cfg.restDur(roll)
	} else {
		a.Activity = "fishing"
		a.ActivityUntilTS = now + cfg.fishingDur(roll)
	}
	a.SinceTS = now
}

// AIActorAdvanceInput 是推进一段活动的全部输入。
//
// 「这段钓完要不要休息」「一段钓多久」「一段休息多久」都由调用方按 T-103 的
// 共享派生给出：存储层不再自己掷骰子决定行为，只负责把结果落库。
// 三个回调都为 nil 时退回既有随机区间（仅供非生产调用与存储层单测）。
type AIActorAdvanceInput struct {
	Now  float64
	Cfg  AIActorConfig
	Roll func() float64
	// FishingSeconds / RestSeconds 覆盖下一段时长；nil 时用 Cfg 的区间。
	FishingSeconds func() float64
	RestSeconds    func() float64
	// RestAfterFishing 是钓鱼结束后的休息决定；nil 视为「不休息」。
	RestAfterFishing func(energy float64) bool
}

func (in AIActorAdvanceInput) fishingSeconds() float64 {
	if in.FishingSeconds != nil {
		return in.FishingSeconds()
	}
	return in.Cfg.fishingDur(in.Roll)
}

func (in AIActorAdvanceInput) restSeconds() float64 {
	if in.RestSeconds != nil {
		return in.RestSeconds()
	}
	return in.Cfg.restDur(in.Roll)
}

func (in AIActorAdvanceInput) wantsRest(energy float64) bool {
	if in.RestAfterFishing == nil {
		return false
	}
	return in.RestAfterFishing(energy)
}

// Advance 保留旧签名：只在调用方没有 T-103 结果时使用。
//
// 它不再读 AIActorConfig.RestChance —— 旧概率已废止（T-108 / DEV-110）。
func (a *AIActor) Advance(now float64, cfg AIActorConfig, roll func() float64) bool {
	return a.AdvanceWith(AIActorAdvanceInput{Now: now, Cfg: cfg, Roll: roll})
}

// AdvanceWith 在活动到期时把状态推到下一段；返回是否有改动。
// 没到点、或本来就没有状态时返回 false。
func (a *AIActor) AdvanceWith(in AIActorAdvanceInput) bool {
	if a == nil {
		return false
	}
	roll := in.Roll
	if roll == nil {
		roll = func() float64 { return 0.5 }
	}
	now := in.Now
	if a.Activity == "" {
		a.Activity = "fishing"
	}
	if a.ActivityUntilTS <= 0 {
		// 旧数据没有到期时间：按当前活动补一段，不立刻改活动。
		if a.Activity == "resting" {
			a.ActivityUntilTS = now + in.restSeconds()
		} else if a.Activity == "fishing" {
			a.ActivityUntilTS = now + in.fishingSeconds()
		} else {
			a.ActivityUntilTS = now + in.restSeconds()
		}
		return true
	}
	if now < a.ActivityUntilTS {
		return false
	}
	switch a.Activity {
	case "fishing":
		a.Energy = clampF(a.Energy-12, 0, 100)
		// 休息与否只看调用方的性格派生与能量底线；旧 RestChance 不再参与。
		if a.Energy < 25 || in.wantsRest(a.Energy) {
			a.Activity = "resting"
			a.SinceTS = now
			a.Energy = clampF(a.Energy+20, 0, 100)
			a.ActivityUntilTS = now + in.restSeconds()
		} else {
			// 继续钓，不重置 SinceTS：玩家问「蹲多久了」应该继续累加。
			a.ActivityUntilTS = now + in.fishingSeconds()
		}
	case "resting":
		a.Energy = clampF(a.Energy+25, 0, 100)
		if a.Energy >= 65 || roll() < 0.5 {
			a.Activity = "fishing"
			a.SinceTS = now
			a.ActivityUntilTS = now + in.fishingSeconds()
		} else {
			a.ActivityUntilTS = now + in.restSeconds()
		}
	default: // idle / moving
		a.SinceTS = now
		if roll() < 0.7 {
			a.Activity = "fishing"
			a.ActivityUntilTS = now + in.fishingSeconds()
		} else {
			a.Activity = "resting"
			a.Energy = clampF(a.Energy+10, 0, 100)
			a.ActivityUntilTS = now + in.restSeconds()
		}
	}
	return true
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// aiActorRowScanner 是 *sql.Row / *sql.Rows 的公共 Scan 子集。
type aiActorRowScanner interface{ Scan(dest ...any) error }

// scanAIActorRow 是所有 actor 读取的唯一列口径。
func scanAIActorRow(row aiActorRowScanner) (*AIActor, error) {
	var a AIActor
	if err := row.Scan(&a.AgentKey, &a.Loc, &a.LocName, &a.Activity, &a.TargetFish,
		&a.TargetName, &a.SinceTS, &a.LocSinceTS, &a.SwitchTS, &a.ActivityUntilTS,
		&a.Energy, &a.UpdatedTS); err != nil {
		return nil, err
	}
	return &a, nil
}

// AIGetActor 读一个 Agent 的状态；没有记录时返回 nil, nil。
// 运行态写者已收敛到 GS：这里只读，不再提供整行覆盖写入口。
func (d *DB) AIGetActor(ctx context.Context, agentKey string) (*AIActor, error) {
	a, err := scanAIActorRow(d.SQL.QueryRowContext(ctx, `
		SELECT agent_key, loc, loc_name, activity, target_fish, target_name,
		       since_ts, loc_since_ts, switch_ts, activity_until_ts, energy, updated_ts
		FROM ai_actor_state WHERE agent_key=?`, agentKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// saveAIActor 是运行态事务内唯一的整行写入；调用方必须已经持有写事务。
func saveAIActor(ctx context.Context, q AIQueryer, a *AIActor) error {
	if a == nil || a.AgentKey == "" {
		return errors.New("ai actor: 缺少 agent_key")
	}
	a.UpdatedTS = nowTS()
	_, err := q.ExecContext(ctx, `
		INSERT INTO ai_actor_state(agent_key, loc, loc_name, activity, target_fish, target_name,
		                           since_ts, loc_since_ts, switch_ts, activity_until_ts, energy, updated_ts)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(agent_key) DO UPDATE SET
		  loc=excluded.loc, loc_name=excluded.loc_name, activity=excluded.activity,
		  target_fish=excluded.target_fish, target_name=excluded.target_name,
		  since_ts=excluded.since_ts, loc_since_ts=excluded.loc_since_ts,
		  switch_ts=excluded.switch_ts, activity_until_ts=excluded.activity_until_ts,
		  energy=excluded.energy, updated_ts=excluded.updated_ts`,
		a.AgentKey, a.Loc, a.LocName, a.Activity, a.TargetFish, a.TargetName,
		a.SinceTS, a.LocSinceTS, a.SwitchTS, a.ActivityUntilTS, a.Energy, a.UpdatedTS)
	if err != nil {
		return fmt.Errorf("ai actor save: %w", err)
	}
	return nil
}

// deleteAIActor 是运行态事务内删除 Actor 行的唯一入口；control 行不删。
func deleteAIActor(ctx context.Context, q AIQueryer, agentKey string) error {
	_, err := q.ExecContext(ctx, `DELETE FROM ai_actor_state WHERE agent_key=?`, agentKey)
	return err
}

// AIListActors 读全部状态（后台展示「现在都在干嘛」用）。
func (d *DB) AIListActors(ctx context.Context) ([]AIActor, error) {
	rows, err := d.SQL.QueryContext(ctx, `
		SELECT agent_key, loc, loc_name, activity, target_fish, target_name,
		       since_ts, loc_since_ts, switch_ts, activity_until_ts, energy, updated_ts
		FROM ai_actor_state ORDER BY agent_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIActor
	for rows.Next() {
		a, err := scanAIActorRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}
