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

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

const aiFishingTick = 10 * time.Second

// aiEgress 是 gameserver 侧唯一的外发/急停判定入口。
// 后台描述与这里共用 aiconfig，避免三处各写一份「缺省即开」。
// 一次读取同时取开关与最近解除点：解除记录损坏或读取失败一律不产生新副作用。
func aiEgress(ctx context.Context, db *store.DB) aiconfig.EgressStatus {
	if db == nil {
		return aiconfig.EgressStatus{State: aiconfig.StateEmergency, Display: "关（紧急停止）"}
	}
	dec := aiconfig.EvaluateEmergency(db.AIEmergencyState(ctx))
	if blocked, _ := dec.RecordBlocked(); blocked {
		return aiconfig.EgressStatus{State: aiconfig.StateEmergency, Display: "关（紧急停止）"}
	}
	return aiconfig.EgressStatus{Allowed: true, State: aiconfig.StateAllowed, Display: "开（未急停）"}
}

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

// aiFishingAgent 是一个此刻真的在抛竿的角色（能力已获准 + 状态为 fishing）。
// 闲时搭话与上鱼播报共用这份候选。
type aiFishingAgent struct {
	agent store.AIAgent
	actor *store.AIActor
}

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
// AI 运行态命令消费者也挂在这个生命周期入口上（T-020）：cmd/gameserver 只
// 启动这里，不改 cmd/* 就同时得到 tick 与单写者命令循环。
func (s *Server) StartAIFishing(db *store.DB, stop <-chan struct{}) {
	if db == nil {
		return
	}
	s.StartAIRuntimeCommands(db, stop)
	// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
	s.startAIFishingTalkTest(db, stop)
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
	ctx, active := s.bindAIIntent(ctx, db)
	if !active {
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
	if db.AISetting(ctx, aiconfig.KeyActorEnabled, aiconfig.DefaultActorEnabled) != "1" {
		return
	}
	// T-108：垂钓资格改用「虚拟垂钓」能力（代替旧 aiAgentCanChime 对抽鱼的依赖）。
	agents, rejects := s.aiFishingAgents(ctx, channel)
	s.logEventRejects(channel, aiconfig.EventVirtualFishing, rejects)
	if len(agents) == 0 {
		return
	}
	// DEV-113 A：旧全局开关 ai_fishing_talk_enabled 由「频道内是否有已配置、生效的
	// 播报角色」替代。没有播报角色时沿用静默抽取节拍；有时保留原预占时点。
	broadcasters, bRejects := s.aiChannelBroadcasters(ctx, channel)
	s.logEventRejects(channel, aiconfig.EventBroadcastCatch, bRejects)
	talkOn := len(broadcasters) > 0
	broadcastable := make(map[string]bool, len(broadcasters))
	for _, a := range broadcasters {
		broadcastable[a.Key] = true
	}
	// 在钓鱼开关关掉就永远不钓；开着也可能正在休息/闲逛。
	// 先推进状态，再只保留此刻真的在抛竿的人。
	fishing := make([]aiFishingAgent, 0, len(agents))
	for _, a := range agents {
		if !a.ActorEnabled {
			continue
		}
		// tick 在同一事务里重验开关、初始化/推进状态；禁用/删除一旦应用，
		// 这里的旧快照就再也不能复活状态。
		st := s.actorTick(ctx, a.Key, "")
		if st == nil {
			continue
		}
		if st.Activity != "fishing" || st.Loc == "" {
			continue
		}
		fishing = append(fishing, aiFishingAgent{agent: a, actor: st})
	}
	if len(fishing) == 0 {
		return
	}
	// 闲时搭话与上鱼播报是两条独立通道：先看要不要自己找个话头。
	// 说了就跳过本 tick 的上鱼，避免两条消息挤在同一时刻。
	if s.maybeAIIdleChat(ctx, db, channel, uid, fishing) {
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
	s.noteFishingStage("attempt", "", 0)

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

	// 每一次上鱼都要先过频道节拍：静默档也占节拍，否则每个 tick 都可能白捡一条。
	// 节拍通过后再决定「这一次能不能开口」：普通达标、不可播报、配额用完都只写鱼。
	attempt := true
	if talkOn {
		attempt = broadcastable[agent.Key] && ev != evOk
	}

	v, _ := aiFishingStates.LoadOrStore(channel, &aiFishingChannelState{interval: base})
	st := v.(*aiFishingChannelState)
	hourLimit := s.aiIntSetting(ctx, "ai_fishing_hourly_limit", 12, 1, 24)
	st.mu.Lock()
	if st.hourStart.IsZero() || now.Sub(st.hourStart) >= time.Hour {
		st.hourStart = now
		st.fishCount = 0
		st.interval = base // 新的一小时从基础节拍重新开始
	}
	if !st.lastAt.IsZero() && now.Sub(st.lastAt) < st.interval {
		st.mu.Unlock()
		return
	}
	// 这一条鱼确定要入库：推进节拍；只有「可尝试且配额未满」才允许开口。
	st.lastAt = now
	st.interval = time.Duration(float64(st.interval) * 1.2)
	if st.interval > maxI {
		st.interval = maxI
	}
	canTalk := attempt && st.fishCount < hourLimit
	if canTalk {
		st.fishCount++
	}
	st.mu.Unlock()
	if canTalk {
		s.noteFishingStage("quota", agent.Key, 0)
	}

	// 写渔获 + 推进角色状态 + 星蓝战绩，抽鱼本身与「说不说」无关。
	if !s.writeAICatch(ctx, db, agent, fish, loc) {
		return
	}
	if !canTalk {
		return
	}
	// 写库之后再决定「说还是不说」：预算与性格都在 maybeAIFishingTalk 里。
	seed := aiEventSeed(channel, "fishing|"+agent.Key+"|"+fish.FishKey)
	if taskID, spoken := s.maybeAIFishingTalk(ctx, db, channel, uid, agent, fish, seed); spoken {
		s.noteFishingStage("enqueue", agent.Key, taskID)
	}
}

// writeAICatch 把一次虚拟上鱼写入 ai_catch，并同步角色状态与星蓝战绩。
// 抽鱼与播报解耦：无论这次说不说，渔获都要入库，玩家问「你钓了多少」才查得到。
//
// 渔获、角色状态、战绩记忆与剪枝在同一 BEGIN IMMEDIATE 事务里提交：
// 任一步失败整体回滚，不会出现「记忆写了一半」或「状态推进了但渔获丢了」。
func (s *Server) writeAICatch(ctx context.Context, db *store.DB, agent store.AIAgent,
	fish aiFishTemplate, loc string) bool {

	ctx, active := s.bindAIIntent(ctx, db)
	if !active {
		return false
	}

	rareKind := ""
	if fish.Event.rare() {
		rareKind = "star"
		if fish.Event == evSuper {
			rareKind = "super"
		}
	}
	keep := s.aiIntSetting(ctx, "ai_memory_max_rows", 50, 5, 200)
	stopped := false

	if aiRuntimeWriteBarrierForTest != nil {
		aiRuntimeWriteBarrierForTest()
	}
	err := db.AIWithRuntimeTx(ctx, func(rt *store.AIRuntimeTx) error {
		// 在写事务内复核急停：停前快照不能让停后新增运行记录（渔获/状态/记忆）。
		if aiRuntimeStopped(rt) {
			stopped = true
			return nil
		}
		enabled, _, actorEnabled, found, err := rt.AgentFlags(agent.Key)
		if err != nil {
			return err
		}
		if !found || !enabled {
			return nil // 已被删除/禁用：本次渔获作废，不复活状态
		}
		if err := rt.AddCatch(store.AICatch{
			AgentKey: agent.Key,
			FishKey:  fish.FishKey,
			FishName: fish.Name,
			WeightG:  fish.WeightG,
			Loc:      fish.Loc,
			LocName:  s.actorLocNameQ(rt.Context(), rt.SQL(), fish.Loc),
			Event:    string(fish.Event),
			Bait:     fish.Bait,
		}); err != nil {
			return err
		}
		if actorEnabled {
			if err := s.actorNoteCatchTx(rt, agent.Key, loc, fish.FishKey, fish.Name); err != nil {
				return err
			}
		}
		if rareKind != "" {
			if err := rt.AddMemory(agent.Key, rareKind, fish.FishKey, fish.Name, fish.WeightG, fish.Loc); err != nil {
				return err
			}
			if err := rt.PruneMemories(agent.Key, keep); err != nil {
				return err
			}
			if _, err := rt.BumpControl(agent.Key, 0, false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.Warn("AI 渔获写入失败", "agent", agent.Key, "err", err)
		return false
	}
	if stopped {
		return false
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
