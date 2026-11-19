package gameserver

// AI 角色状态：每个已接入的 Agent 都有一份「我在哪、在干什么、想钓什么」。
// 状态存在主库 ai_actor_state，与对话历史解耦——历史会被预算裁掉，状态不会，
// 所以聊再久都不会出现「上一句在这张图、下一句飘到别处」。
//
// 写者：只有 gameserver，而且必须在 AIRuntimeTx（BEGIN IMMEDIATE）里，
// 同事务读 control/开关、更新状态并 revision++。aibridge 只读，admin 只入队
// （T-020 / DEV-004）。

import (
	"context"
	"math/rand"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

// actorEnsure 在运行态事务里取 Agent 状态；没有就按「当前地图 + 随机一个本图鱼」
// 初始化。初始活动不一定是钓鱼：也可能已经在休息或闲逛。
func (s *Server) actorEnsure(ctx context.Context, agentKey, loc string) *store.AIActor {
	if s.DB == nil || agentKey == "" {
		return nil
	}
	var out *store.AIActor
	ctx, active := s.bindAIIntent(ctx, s.DB)
	if !active {
		return nil
	}
	err := s.DB.AIWithRuntimeTx(ctx, func(rt *store.AIRuntimeTx) error {
		if aiRuntimeStopped(rt) {
			return nil
		}
		a, err := s.actorEnsureTx(rt, agentKey, loc)
		if err != nil {
			return err
		}
		out = a
		return nil
	})
	if err != nil {
		s.log.Warn("AI 角色状态初始化失败", "agent", agentKey, "err", err)
		return nil
	}
	return out
}

// actorEnsureTx 是 actorEnsure 的事务内实现；调用方必须已持有写事务。
func (s *Server) actorEnsureTx(rt *store.AIRuntimeTx, agentKey, loc string) (*store.AIActor, error) {
	if agentKey == "" {
		return nil, nil
	}
	a, err := rt.Actor(agentKey)
	if err != nil {
		return nil, err
	}
	now := float64(time.Now().Unix())
	if a != nil {
		changed := false
		// 后台先打开开关时还没有地图/目标鱼；用随机地图补全，避免所有角色挤一张图。
		if (a.Loc == "" || a.TargetFish == "") && loc != "" {
			if a.Loc == "" {
				if k, n, mapErr := rt.RandomMap(); mapErr == nil && k != "" {
					a.Loc, a.LocName = k, n
				} else {
					a.Loc, a.LocName = loc, s.actorLocNameQ(rt.Context(), rt.SQL(), loc)
				}
				a.LocSinceTS = now
			}
			if a.TargetFish == "" {
				a.TargetFish, a.TargetName = s.actorPickFishQ(rt.Context(), rt.SQL(), a.Loc)
			}
			changed = true
		}
		if changed {
			if err := rt.SaveActor(a); err != nil {
				return nil, err
			}
			if _, err := rt.BumpControl(agentKey, 0, false); err != nil {
				return nil, err
			}
		}
		return a, nil
	}
	rng := rand.New(rand.NewSource(store.AIActorSeed(now, agentKey)))
	cfg := rt.ActorConfig()
	// 新角色的地图随机分散，而不是统一放在玩家当前地图。
	startLoc, startName := loc, s.actorLocNameQ(rt.Context(), rt.SQL(), loc)
	if k, n, mapErr := rt.RandomMap(); mapErr == nil && k != "" {
		startLoc, startName = k, n
	}
	a = store.AIActorStart(agentKey, startLoc, startName, now, cfg, rng.Float64)
	a.TargetFish, a.TargetName = s.actorPickFishQ(rt.Context(), rt.SQL(), startLoc)
	if err := rt.SaveActor(a); err != nil {
		return nil, err
	}
	if _, err := rt.BumpControl(agentKey, 0, false); err != nil {
		return nil, err
	}
	return a, nil
}

// actorTick 在一个事务里「重验开关 → ensure → 到期推进」；返回推进后的状态。
// 禁用/删除与 tick 竞争时，事务内重验开关会让 tick 直接放弃，不会复活状态。
//
// T-108：推进用 T-103 的共享派生（休息/继续），性格在事务外读一次快照；
// 事务内不再开第二条连接（连接池只有一条），也不产生第二份概率。
func (s *Server) actorTick(ctx context.Context, agentKey, loc string) *store.AIActor {
	if s.DB == nil || agentKey == "" {
		return nil
	}
	var out *store.AIActor
	ctx, active := s.bindAIIntent(ctx, s.DB)
	if !active {
		return nil
	}
	personality, derivable := s.actorPersonality(ctx, agentKey)
	if aiRuntimeWriteBarrierForTest != nil {
		aiRuntimeWriteBarrierForTest()
	}
	err := s.DB.AIWithRuntimeTx(ctx, func(rt *store.AIRuntimeTx) error {
		// 写事务内复核急停：停前快照不能让停后继续推进/新建角色状态。
		if aiRuntimeStopped(rt) {
			return nil
		}
		enabled, _, actorEnabled, found, err := rt.AgentFlags(agentKey)
		if err != nil {
			return err
		}
		if !found || !enabled || !actorEnabled {
			return nil
		}
		a, err := s.actorEnsureTx(rt, agentKey, loc)
		if err != nil || a == nil {
			return err
		}
		if _, err := s.actorAdvanceTx(rt, a, personality, derivable); err != nil {
			return err
		}
		out = a
		return nil
	})
	if err != nil {
		s.log.Warn("AI 角色状态推进失败", "agent", agentKey, "err", err)
		return nil
	}
	return out
}

// actorPersonality 读角色四维的只读快照（事务外）。
//
// 读失败或显式非法都返回不可派生：此时只保留能量底线，不产出新的派生决定。
func (s *Server) actorPersonality(ctx context.Context, agentKey string) (aiconfig.PersonalityConfig, bool) {
	if s.DB == nil {
		return aiconfig.DefaultPersonality(), false
	}
	p, err := s.DB.AIGetPersonality(ctx, agentKey)
	if err != nil {
		s.log.Warn("AI 性格读取失败", "agent", agentKey, "err", err)
		return aiconfig.DefaultPersonality(), false
	}
	if !p.Derivable() {
		return aiconfig.DefaultPersonality(), false
	}
	return p.Config, true
}

// actorAdvanceTx 把到期的活动推进到下一段；有改动时同事务落库并 revision++。
//
// 硬驻留只约束「钓鱼」这一段：没到 Dmin 时只把到期时间顺延到 Dmin，
// 不换活动；休息段不受这条底线影响。
func (s *Server) actorAdvanceTx(rt *store.AIRuntimeTx, a *store.AIActor,
	personality aiconfig.PersonalityConfig, derivable bool) (bool, error) {

	if a == nil {
		return false, nil
	}
	now := float64(time.Now().Unix())
	cfg := rt.ActorConfig()
	rng := rand.New(rand.NewSource(int64(now)))
	dwell := activityDwellFloor(cfg, personality)
	if a.Activity == "fishing" && a.ActivityUntilTS <= now && a.SinceTS > 0 {
		if floorEnd := a.SinceTS + dwell; now < floorEnd {
			a.ActivityUntilTS = floorEnd
			if err := rt.SaveActor(a); err != nil {
				return false, err
			}
			if _, err := rt.BumpControl(a.AgentKey, 0, false); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	seed := store.AIActorSeed(a.SinceTS, a.AgentKey)
	in := store.AIActorAdvanceInput{
		Now: now, Cfg: cfg, Roll: rng.Float64,
		FishingSeconds: func() float64 { return activityFishingSeconds(cfg, personality, rng.Float64) },
		RestSeconds:    func() float64 { return activityRestSeconds(cfg, personality) },
		RestAfterFishing: func(energy float64) bool {
			if !derivable {
				return false
			}
			want, _ := activityWants(personality, seed, activityRestFeature(energy))
			return want
		},
	}
	if !a.AdvanceWith(in) {
		return false, nil
	}
	if err := rt.SaveActor(a); err != nil {
		return false, err
	}
	if _, err := rt.BumpControl(a.AgentKey, 0, false); err != nil {
		return false, err
	}
	return true, nil
}

// actorLocName 把 level key 换成中文水域名；查不到就原样返回。
func (s *Server) actorLocName(ctx context.Context, loc string) string {
	if s.DB == nil {
		return loc
	}
	return s.actorLocNameQ(ctx, s.DB.SQL, loc)
}

// actorLocNameQ 是只读版本：事务内直接传事务连接，避免走第二个连接。
func (s *Server) actorLocNameQ(ctx context.Context, q store.AIQueryer, loc string) string {
	if loc == "" {
		return ""
	}
	var name string
	_ = q.QueryRowContext(ctx,
		`SELECT loc FROM loc_alias WHERE alias=?`, loc).Scan(&name)
	if name != "" {
		return name
	}
	return loc
}

// actorPickFish 从本图鱼种里随机挑一条当目标（按样本量加权，常见的更容易被选中）。
func (s *Server) actorPickFish(ctx context.Context, loc string) (key, name string) {
	if s.DB == nil {
		return "", ""
	}
	return s.actorPickFishQ(ctx, s.DB.SQL, loc)
}

func (s *Server) actorPickFishQ(ctx context.Context, q store.AIQueryer, loc string) (key, name string) {
	if loc == "" {
		return "", ""
	}
	rows, err := q.QueryContext(ctx, `
		SELECT r.fish_key,
		       COALESCE(NULLIF(lf.name_zh,''), NULLIF(lw.fish_zh,''), ''),
		       COALESCE(g.catches,0)
		FROM map_fish_roster r
		LEFT JOIN loc_alias la ON la.alias=r.loc
		LEFT JOIN loc_fish lf ON lf.fish_key=r.fish_key AND lf.loc=la.loc
		LEFT JOIN loc_wiki_fish lw ON lw.fish_key=r.fish_key AND lw.loc=la.loc
		LEFT JOIN species_grade g ON g.fish_key=r.fish_key
		WHERE r.loc=?`, loc)
	if err != nil {
		return "", ""
	}
	defer rows.Close()
	type cand struct {
		key, name string
		w         int64
	}
	list := make([]cand, 0, 32)
	var total int64
	for rows.Next() {
		var c cand
		if rows.Scan(&c.key, &c.name, &c.w) != nil || c.key == "" {
			continue
		}
		if c.w < 1 {
			c.w = 1
		}
		if c.name == "" {
			c.name = c.key
		}
		list = append(list, c)
		total += c.w
	}
	if len(list) == 0 {
		return "", ""
	}
	pick := rand.Int63n(total)
	for _, c := range list {
		pick -= c.w
		if pick < 0 {
			return c.key, c.name
		}
	}
	return list[0].key, list[0].name
}

// actorNoteCatchTx 记一次 AI 上鱼：对齐地图、必要时把目标鱼换成同图另一条，
// 让「一直在钓同一条」不显得机械。调用方必须已持有运行态事务。
func (s *Server) actorNoteCatchTx(rt *store.AIRuntimeTx, agentKey, loc, fishKey, fishName string) error {
	a, err := s.actorEnsureTx(rt, agentKey, loc)
	if err != nil || a == nil {
		return err
	}
	now := float64(time.Now().Unix())
	changed := false
	if loc != "" && a.Loc != loc {
		a.Loc, a.LocName, a.LocSinceTS = loc, s.actorLocNameQ(rt.Context(), rt.SQL(), loc), now
		changed = true
	}
	// 钓到当前目标就换一条，否则保持。
	if fishKey == a.TargetFish {
		k, n := s.actorPickFishQ(rt.Context(), rt.SQL(), a.Loc)
		if k != "" && k != a.TargetFish {
			a.TargetFish, a.TargetName, a.SinceTS = k, n, now
			changed = true
		}
	}
	if a.Activity != "fishing" {
		a.Activity, a.SinceTS = "fishing", now
		changed = true
	}
	if a.ActivityUntilTS <= now {
		cfg := rt.ActorConfig()
		a.ActivityUntilTS = now + cfg.FishingMax // 上鱼后至少再钓一整段
		changed = true
	}
	if !changed {
		return nil
	}
	if err := rt.SaveActor(a); err != nil {
		return err
	}
	_, err = rt.BumpControl(agentKey, 0, false)
	return err
}
