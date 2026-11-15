package gameserver

// AI 自动钓鱼：频道级统一配额，所有已接入 Agent 共享。
// 只读静态库、独立随机源；不碰 FishState / catch_log / 玩家 RNG / 经济。
//
// 与旧版「上鱼演出」不同：AI 钓到的鱼只写入 ai_catch，不再往频道发播报。
// 玩家问「你钓了多少」时，aibridge 再查 ai_catch 回答。
// 五档权重只决定渔获构成（未达标/达标/下饭/星/蓝），每一档都会记录。

import (
	"context"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"rf4server/internal/store"
)

const aiFishingTick = 10 * time.Second

// aiFishEvent 是一次上鱼的事件档位。阈值来自 species_weight：
// trophy_line_g 是奖杯线（星），super_trophy_line_g 是蓝冠线，trophy_g 是达标线。
type aiFishEvent string

const (
	evMiss  aiFishEvent = "miss"  // 未达标
	evOk    aiFishEvent = "ok"    // 普通达标
	evNear  aiFishEvent = "near"  // 下饭鱼：达标但接近星线
	evStar  aiFishEvent = "star"  // 上星
	evSuper aiFishEvent = "super" // 上蓝
)

// rare 表示是否值得写入战绩记忆（只有星/蓝）。
func (e aiFishEvent) rare() bool { return e == evStar || e == evSuper }

type aiFishingChannelState struct {
	mu        sync.Mutex
	hourStart time.Time
	fishCount int // 本小时已记录条数（频道级，所有 Agent 共享）
	lastAt    time.Time
	interval  time.Duration
}

var aiFishingStates sync.Map // channel -> *aiFishingChannelState

type aiFishTemplate struct {
	FishKey string
	Name    string
	Bait    string
	WeightG int64
	Event   aiFishEvent
	Loc     string
}

// StartAIFishing 启动后台钓鱼循环；与 StartAdminSubscriber 并列，互不依赖。
func (s *Server) StartAIFishing(db *store.DB, stop <-chan struct{}) {
	if db == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(aiFishingTick)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.aiFishingTick(db)
			}
		}
	}()
}

func (s *Server) aiFishingTick(db *store.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if db.AISetting(ctx, "ai_fishing_enabled", "0") != "1" {
		return
	}
	if db.AISetting(ctx, "emergency_stop", "0") == "1" ||
		db.AISetting(ctx, "outbound_enabled", "1") != "1" {
		return
	}

	// 只遍历在线会话所在的频道，顺手记下一个 uid 供播报任务归属。
	// 空频道根本不会进这个 map —— 这就是「只在有玩家的频道推送」。
	s.mu.Lock()
	channelPlayers := map[string]int64{}
	for _, sess := range s.sessions {
		if sess != nil && !sess.Closed() && sess.Userid > 0 {
			channelPlayers[sessChannel(sess).Name] = sess.Userid
		}
	}
	s.mu.Unlock()

	for channel, uid := range channelPlayers {
		s.aiFishingForChannel(ctx, db, channel, uid)
	}
}

func (s *Server) aiFishingForChannel(ctx context.Context, db *store.DB, channel string, uid int64) {
	if db.AISetting(ctx, "ai_actor_enabled", "1") != "1" {
		return
	}
	agents := s.aiAgentsForChannel(ctx, channel)
	if len(agents) == 0 {
		return
	}
	// 在钓鱼开关关掉就永远不钓；开着也可能正在休息/闲逛。
	// 先推进状态，再只保留此刻真的在抛竿的人。
	type fishingAgent struct {
		agent store.AIAgent
		actor *store.AIActor
	}
	fishing := make([]fishingAgent, 0, len(agents))
	for _, a := range agents {
		if !a.ActorEnabled || !aiAgentCanChime(a) {
			continue
		}
		st := s.actorEnsure(ctx, a.Key, "")
		if st == nil {
			continue
		}
		s.actorAdvance(ctx, st)
		if st.Activity != "fishing" || st.Loc == "" {
			continue
		}
		fishing = append(fishing, fishingAgent{agent: a, actor: st})
	}
	if len(fishing) == 0 {
		return
	}
	base := time.Duration(s.aiIntSetting(ctx, "ai_fishing_base_interval_ms", 300000, 60000, 1800000)) * time.Millisecond
	maxI := time.Duration(s.aiIntSetting(ctx, "ai_fishing_max_interval_ms", 600000, 60000, 3600000)) * time.Millisecond
	if base > maxI {
		base = maxI
	}

	now := time.Now()
	rng := rand.New(rand.NewSource(now.UnixNano()))

	// 抽档位；普通达标也记录，只是不会写星/蓝战绩记忆。
	ev := s.rollFishEvent(ctx, rng)

	// 先确认本图真的能抽出鱼，再扣配额：静态数据缺图时不浪费这一小时的额度。
	pick := fishing[rng.Intn(len(fishing))]
	agent, act := pick.agent, pick.actor
	loc := act.Loc
	if loc == "" {
		return
	}
	fish, ok := s.pickAIFish(ctx, rng, loc, ev)
	if !ok {
		return
	}

	// 配额计「说出口的话」，不是抽鱼次数。
	// 静默档（ok）与概率未命中都不占配额、不推进节拍，这样
	// hourly_limit 就是准确的发言频率，不会被大量静默抽鱼稀释。
	talkOn := db.AISetting(ctx, "ai_fishing_talk_enabled", "0") == "1"
	willTalk := talkOn && agent.FishingTalkEnabled &&
		s.aiFishingTalkChance(ctx, ev) > 0

	v, _ := aiFishingStates.LoadOrStore(channel, &aiFishingChannelState{interval: base})
	st := v.(*aiFishingChannelState)
	hourLimit := s.aiIntSetting(ctx, "ai_fishing_hourly_limit", 12, 1, 24)
	st.mu.Lock()
	if st.hourStart.IsZero() || now.Sub(st.hourStart) >= time.Hour {
		st.hourStart = now
		st.fishCount = 0
		st.interval = base // 新的一小时从基础节拍重新开始
	}
	// 播报关闭时沿用旧行为：每次抽鱼都计一次配额，保持静默上鱼节奏不变。
	// 播报开启时只有真正要说的话才检查/占用配额。
	if talkOn && !willTalk {
		st.mu.Unlock()
		// 仍要写库，让「你钓了多少」查得到；只是不发消息、不占配额。
		s.writeAICatch(ctx, db, agent, fish, loc)
		return
	}
	if st.fishCount >= hourLimit || (!st.lastAt.IsZero() && now.Sub(st.lastAt) < st.interval) {
		st.mu.Unlock()
		return
	}
	st.fishCount++
	st.lastAt = now
	st.interval = time.Duration(float64(st.interval) * 1.2)
	if st.interval > maxI {
		st.interval = maxI
	}
	st.mu.Unlock()

	// 写渔获 + 推进角色状态 + 星蓝战绩，抽鱼本身与「说不说」无关。
	if !s.writeAICatch(ctx, db, agent, fish, loc) {
		return
	}
	if talkOn {
		// 只在抽中它的这个 channel 发一条；aiFishingForChannel 本来就只遍历
		// 在线会话所在的频道，所以空频道不会被推送。
		s.maybeAIFishingTalk(ctx, db, channel, uid, agent, fish, rng)
	}
}

// writeAICatch 把一次虚拟上鱼写入 ai_catch，并同步角色状态与星蓝战绩。
// 抽鱼与播报解耦：无论这次说不说，渔获都要入库，玩家问「你钓了多少」才查得到。
func (s *Server) writeAICatch(ctx context.Context, db *store.DB, agent store.AIAgent,
	fish aiFishTemplate, loc string) bool {

	if err := db.AIAddCatch(ctx, store.AICatch{
		AgentKey: agent.Key,
		FishKey:  fish.FishKey,
		FishName: fish.Name,
		WeightG:  fish.WeightG,
		Loc:      fish.Loc,
		LocName:  s.actorLocName(ctx, fish.Loc),
		Event:    string(fish.Event),
		Bait:     fish.Bait,
	}); err != nil {
		s.log.Warn("AI 渔获写入失败", "agent", agent.Key, "err", err)
		return false
	}
	s.actorNoteCatch(ctx, agent.Key, loc, fish.FishKey, fish.Name)
	if fish.Event.rare() {
		kind := "star"
		if fish.Event == evSuper {
			kind = "super"
		}
		if err := db.AIAddMemory(ctx, agent.Key, kind, fish.FishKey, fish.Name, fish.WeightG, fish.Loc); err != nil {
			s.log.Warn("AI 战绩记忆写入失败", "agent", agent.Key, "err", err)
		} else {
			_ = db.AIPruneMemories(ctx, agent.Key, s.aiIntSetting(ctx, "ai_memory_max_rows", 50, 5, 200))
		}
	}
	return true
}

func (s *Server) aiAgentsForChannel(ctx context.Context, channel string) []store.AIAgent {
	if s.DB == nil {
		return nil
	}
	keys, err := s.DB.AIChannelSubscribed(ctx, channel)
	if err != nil {
		return nil
	}
	out := make([]store.AIAgent, 0, len(keys))
	for _, k := range keys {
		if a, err := s.DB.AIAgentByKey(ctx, k); err == nil && a != nil && a.Enabled && a.Joined {
			out = append(out, *a)
		}
	}
	return out
}

// aiFloatSetting 读浮点设置并夹在区间内。
func (s *Server) aiFloatSetting(ctx context.Context, key string, def, lo, hi float64) float64 {
	v, err := strconv.ParseFloat(s.DB.AISetting(ctx, key, strconv.FormatFloat(def, 'f', -1, 64)), 64)
	if err != nil {
		v = def
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}

// rollFishEvent 按后台权重抽本次上鱼的档位。
// 权重是「这次上鱼属于哪一档」的构成比例，不是单条鱼的命中率：
// 蓝:星 = superW:starW，再叠上下饭、未达标，剩下的回落到普通达标。
func (s *Server) rollFishEvent(ctx context.Context, rng *rand.Rand) aiFishEvent {
	starW := s.aiFloatSetting(ctx, "ai_fishing_star_weight", 0.02, 0, 1)
	superW := s.aiFloatSetting(ctx, "ai_fishing_super_weight", 0.002, 0, 1)
	nearW := s.aiFloatSetting(ctx, "ai_fishing_near_weight", 0.12, 0, 1)
	missW := s.aiFloatSetting(ctx, "ai_fishing_miss_weight", 0.33, 0, 1)
	r := rng.Float64()
	acc := 0.0
	for _, b := range []struct {
		w  float64
		ev aiFishEvent
	}{{superW, evSuper}, {starW, evStar}, {nearW, evNear}, {missW, evMiss}} {
		acc += b.w
		if acc > 0 && r < acc {
			return b.ev
		}
	}
	return evOk
}

type aiFishCand struct {
	key, name                  string
	trophyLine, superLine, max int64
	samples                    int64
}

// pickAIFish 按档位在对应重量区间里抽一条鱼。
// 区间以星线（trophy_line_g）为标尺，五档首尾相接，不会出现跨档的怪重量。
func (s *Server) pickAIFish(ctx context.Context, rng *rand.Rand, loc string, ev aiFishEvent) (aiFishTemplate, bool) {
	if s.DB == nil || loc == "" {
		return aiFishTemplate{}, false
	}
	missRatio := s.aiFloatSetting(ctx, "ai_fishing_miss_ratio", 0.35, 0.05, 0.6)
	nearRatio := s.aiFloatSetting(ctx, "ai_fishing_near_ratio", 0.9, 0.6, 0.99)
	if nearRatio <= missRatio {
		nearRatio = missRatio + 0.1
	}
	rows, err := s.DB.SQL.QueryContext(ctx, `
		SELECT w.fish_key,
		       COALESCE(NULLIF(lf.name_zh,''), NULLIF(lw.fish_zh,''), ''),
		       w.trophy_line_g,
		       COALESCE(w.super_trophy_line_g,0), COALESCE(w.max_g,0), COALESCE(w.samples,0)
		FROM map_fish_roster r
		JOIN species_weight w ON w.fish_key=r.fish_key
		JOIN species_grade g ON g.fish_key=w.fish_key
		LEFT JOIN loc_alias la ON la.alias=r.loc
		LEFT JOIN loc_fish lf ON lf.fish_key=w.fish_key AND lf.loc=la.loc
		LEFT JOIN loc_wiki_fish lw ON lw.fish_key=w.fish_key AND lw.loc=la.loc
		WHERE r.loc=? AND w.trophy_line_g>0 AND g.catches>=20`, loc)
	if err != nil {
		return aiFishTemplate{}, false
	}
	defer rows.Close()
	cands := make([]aiFishCand, 0, 64)
	for rows.Next() {
		var c aiFishCand
		if rows.Scan(&c.key, &c.name, &c.trophyLine, &c.superLine, &c.max, &c.samples) == nil {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return aiFishTemplate{}, false
	}
	total := int64(0)
	for _, c := range cands {
		total += maxI64(c.samples, 1)
	}
	pick := rng.Int63n(total)
	var c aiFishCand
	for _, x := range cands {
		pick -= maxI64(x.samples, 1)
		if pick < 0 {
			c = x
			break
		}
	}
	if c.key == "" {
		return aiFishTemplate{}, false
	}
	lo, hi := fishWeightBand(c, ev, missRatio, nearRatio)
	if hi <= lo {
		hi = lo + 1
	}
	weight := lo + rng.Int63n(hi-lo)
	name := c.name
	if name == "" {
		name = c.key
	}
	return aiFishTemplate{
		FishKey: c.key, Name: name, Bait: s.aiFishingBait(ctx, rng, c.key),
		WeightG: weight, Event: ev, Loc: loc,
	}, true
}

// fishWeightBand 算某档位的重量区间 [lo, hi)。
// 分割点：未达标 < 星线×missRatio < 星线×nearRatio <= 下饭 < 星线 <= 星 < 蓝线 <= 蓝。
func fishWeightBand(c aiFishCand, ev aiFishEvent, missRatio, nearRatio float64) (int64, int64) {
	line := c.trophyLine
	missLine := int64(float64(line) * missRatio)
	nearLine := int64(float64(line) * nearRatio)
	superLine := c.superLine
	if superLine <= line {
		superLine = line * 5 / 3
	}
	switch ev {
	case evSuper:
		return superLine, superLine * 5 / 4
	case evStar:
		return line, superLine
	case evNear:
		return nearLine, line
	case evMiss:
		if missLine < 2 {
			missLine = 2
		}
		return missLine / 2, missLine
	default: // evOk
		return missLine, nearLine
	}
}

func (s *Server) aiFishingBait(ctx context.Context, rng *rand.Rand, fishKey string) string {
	rows, err := s.DB.SQL.QueryContext(ctx,
		`SELECT bait_zh FROM fish_bait_pref WHERE fish_key=? LIMIT 8`, fishKey)
	if err != nil {
		return ""
	}
	defer rows.Close()
	baits := make([]string, 0, 8)
	for rows.Next() {
		var b string
		if rows.Scan(&b) == nil && b != "" {
			baits = append(baits, b)
		}
	}
	if len(baits) == 0 {
		return ""
	}
	return baits[rng.Intn(len(baits))]
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
