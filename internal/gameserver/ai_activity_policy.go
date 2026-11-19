package gameserver

// T-108 活动仲裁与共享派生消费。
//
// 本文件是「行为概率归性格」（DEV-110）在游戏服侧的落点：
//   - 活动切换的两道闸：硬驻留/锁/证据（第一闸）与 W≥0.6（第二闸）；
//   - 圆桌起桌、接话、玩家星蓝反应、自己上鱼报话、休息/换目标的意愿，
//     全部走 T-103 的同一个纯函数 aiconfig.DecidePersonality；
//   - 活动时长参数只从既有活动配置继承（类型级），没有每角色低层覆盖。
//
// 这里不读写数据库：所有事实由调用方读好传进来，决定本身是纯函数，
// 这样同一份输入在任何进程、任何顺序下都得到同一个结果，便于定向反证。
//
// 旧口径废止清单（与 T-102「清旧闸」一致）：
//   - ai_actor_rest_chance、ai_actor_switch_chance、ai_catch_star_chance、
//     ai_catch_super_chance、ai_catch_roundtable_chance、
//     roundtable_on_speech_chance、ai_fishing_talk_*_chance 不再参与行为决定；
//   - reply_mode / talkativeness 不再作为事件准入的第二份概率。

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

// activitySwitchAccept 是第二闸的接受阈值（原票 §2：W≥0.6 接受）。
const activitySwitchAccept = 0.6

// activityInviteInterval 是同玩家同邀请的去重窗口（秒）。
// activityInviteMax 是同一次邀请最多被算几次；超过即视为重复骚扰。
const (
	activityInviteInterval = 60.0
	activityInviteMax      = 3
)

// activityTunables 把现有活动配置映射成 T-103 的类型级 tunables。
//
// Bhard = 一段钓鱼的最短时长，Bextra = 可延长的最大幅度，Tbase = 一段休息的基数。
// 这是「默认继承现有活动配置」：真实时长真源仍是 ai_actor_* 设置，
// 本票不新增每角色常量，也不改上界。
func activityTunables(cfg store.AIActorConfig) aiconfig.PersonalityTunables {
	fishingMin := math.Max(0, cfg.FishingMin)
	extra := cfg.FishingMax - cfg.FishingMin
	if extra < 0 {
		extra = 0
	}
	restBase := math.Max(0, cfg.RestMin)
	return aiconfig.PersonalityTunables{
		HardFloorSeconds: fishingMin,
		ExtraSeconds:     extra,
		RestBaseSeconds:  restBase,
	}
}

// activityDwellFloor 返回这一次活动的硬驻留底线 Dmin（秒）。
//
// Dmin 只加不减：p 越高底线越长，r 只通过 (0.5+0.5r) 放大或持平。
func activityDwellFloor(cfg store.AIActorConfig, p aiconfig.PersonalityConfig) float64 {
	t := activityTunables(cfg)
	if err := p.Validate(); err != nil {
		// 显式非法：不产出派生决定，也不缩底线。用中性默认算底线只用于
		// 「先别动」，不构成一次新的派生。
		p = aiconfig.DefaultPersonality()
	}
	return p.DwellFloorSeconds(t)
}

// activityRestSeconds 返回一段休息的时长 Trest=Tbase×(1+0.5r)，并夹在既有区间内。
//
// 夹取是为了不越过后台配置的上界；夹取只会缩短，不会低于 RestMin。
func activityRestSeconds(cfg store.AIActorConfig, p aiconfig.PersonalityConfig) float64 {
	if err := p.Validate(); err != nil {
		p = aiconfig.DefaultPersonality()
	}
	t := activityTunables(cfg)
	v := p.RestIntervalSeconds(t)
	lo, hi := cfg.RestMin, cfg.RestMax
	if hi < lo {
		hi = lo
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}

// activityFishingSeconds 返回一段钓鱼的时长：区间随机与硬驻留取较大者。
//
// 硬驻留是下限，不是替代：roll 只可能把这一段拉长，不能把它压到 Dmin 以下。
func activityFishingSeconds(cfg store.AIActorConfig, p aiconfig.PersonalityConfig, roll func() float64) float64 {
	if roll == nil {
		roll = func() float64 { return 0.5 }
	}
	lo, hi := cfg.FishingMin, cfg.FishingMax
	if hi < lo {
		hi = lo
	}
	v := lo + roll()*(hi-lo)
	if floor := activityDwellFloor(cfg, p); v < floor {
		v = floor
	}
	return v
}

// activityCanSwitch 是第一闸：证据有效、无锁、且已过硬驻留。
//
// 三道条件各自独立：任何一条不成立都不产生动作，返回的原因用于回执与日志。
func activityCanSwitch(evidenceOK, locked bool, elapsed, dwellFloor float64) (bool, string) {
	if !evidenceOK {
		return false, "evidence_invalid"
	}
	if locked {
		return false, "locked"
	}
	if dwellFloor > 0 && elapsed < dwellFloor {
		return false, "hard_dwell"
	}
	return true, ""
}

// activitySwitchWeight 是第二闸：W=σ(2s+e−p−1+d×u+min(0.6,0.2n))。
//
//	s/e/p 是交往/尝新/持续投入的归一值（0..1）；
//	d=+1 表示普通消遣、−1 表示已有目标投入；
//	u=clamp((elapsed−Dmin)/max(Bextra,1),0,1)；
//	n 是同玩家同邀请的重复次数（1..3）。
//
// 只有 W 参与接受判定：重复邀请只抬高 W，不会降低 Dmin。
func activitySwitchWeight(p aiconfig.PersonalityConfig, invested bool,
	elapsed, dwellFloor, extra float64, repeats int) float64 {

	if err := p.Validate(); err != nil {
		p = aiconfig.DefaultPersonality()
	}
	s := float64(p.Sociability) / float64(aiconfig.PersonalityMax)
	e := float64(p.Exploration) / float64(aiconfig.PersonalityMax)
	pers := float64(p.Persistence) / float64(aiconfig.PersonalityMax)

	d := 1.0
	if invested {
		d = -1.0
	}
	if extra < 1 {
		extra = 1
	}
	u := (elapsed - dwellFloor) / extra
	if u < 0 {
		u = 0
	}
	if u > 1 {
		u = 1
	}
	if repeats < 0 {
		repeats = 0
	}
	bonus := math.Min(0.6, 0.2*float64(repeats))
	return activitySigmoid(2*s + e - pers - 1 + d*u + bonus)
}

// activitySwitchAccepted 报告 W 是否达到接受阈值。
func activitySwitchAccepted(w float64) bool { return w >= activitySwitchAccept }

// activitySigmoid 是第二闸用的 S 形函数。
func activitySigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

// aiEventSeedForTest 仅测试用：固定主动事件的抽样种子，让三条链可复现。
// 生产恒为 nil，种子只由频道与原话派生。
var aiEventSeedForTest func() int64

// aiEventSeed 给一次主动事件的 T-103 派生提供稳定种子。
//
// T-108 §9.2 E6 / G14：种子只由「频道 + 原话」派生，**不掺时间**。同一输入
// 两次派生必须得到同一组决定（同一任务回放、同一句重放都稳定）。掺时间会让
// 定向反证「同一输入两次派生不得换人」变红。
func aiEventSeed(channel, text string) int64 {
	if aiEventSeedForTest != nil {
		return aiEventSeedForTest()
	}
	base := int64(0)
	for _, r := range channel + "\x00" + text {
		base = base*131 + int64(r)
	}
	return base
}

// aiFishingStageForTest 仅测试用：观察虚拟钓鱼的四段诊断
// （attempt=尝试、quota=占额、enqueue=入队、delivery=送达）。
// 生产恒为 nil；四段同时写结构化日志，便于按 agent/task 追溯。
var aiFishingStageForTest func(stage, agentKey string, taskID int64)

// noteFishingStage 记录一次主动事件走到了哪一段（attempt/quota/enqueue/delivery）。
// 生产写结构化日志，测试可用 aiFishingStageForTest 逐段收集。
func (s *Server) noteFishingStage(stage, agentKey string, taskID int64) {
	if aiFishingStageForTest != nil {
		aiFishingStageForTest(stage, agentKey, taskID)
	}
	s.log.Info("AI 主动事件诊断", "stage", stage, "agent", agentKey, "task_id", taskID)
}

// activityInviteState 是「同玩家同邀请」的进程内去重状态。
//
// 只在内存：重启即清零是可接受的降噪语义（真正的冷却仍在 GS 事务里按
// ai_actor_switch_cooldown_s 判定），不需要新表或迁移。
type activityInviteState struct {
	mu   sync.Mutex
	last map[string]activityInviteEntry
}

type activityInviteEntry struct {
	at    float64
	count int
}

var activityInvites = &activityInviteState{last: map[string]activityInviteEntry{}}

// noteActivityInvite 记一次同玩家同邀请，返回这是第几次与是否仍在允许范围。
//
// 同一个 key（玩家 + 目标 + 地图）在 60 秒内重复出现才算同一邀请；
// 超过 activityInviteMax 次返回 allowed=false，调用方据此拒绝，且不消耗 W 之外的东西。
func (st *activityInviteState) note(key string, now float64) (n int, allowed bool) {
	if st == nil {
		return 1, true
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return 1, true
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.last == nil {
		st.last = map[string]activityInviteEntry{}
	}
	// 进程内状态会随玩家/目标增长：数量超过阈值时清掉过窗条目，避免长跑泄漏。
	if len(st.last) > 1024 {
		for k, v := range st.last {
			if now-v.at > activityInviteInterval {
				delete(st.last, k)
			}
		}
	}
	ent := st.last[key]
	if ent.at <= 0 || now-ent.at > activityInviteInterval {
		ent = activityInviteEntry{at: now}
	}
	ent.at = now
	ent.count++
	st.last[key] = ent
	if ent.count > activityInviteMax {
		return ent.count, false
	}
	return ent.count, true
}

// reset 只给测试用：清空去重状态，避免用例之间互相污染。
func (st *activityInviteState) reset() {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.last = map[string]activityInviteEntry{}
}

// activityInviteKey 是去重键：玩家 + 目标鱼 + 地图。
func activityInviteKey(uid int64, fishKey, locKey string) string {
	return strings.TrimSpace(fishKey) + "|" + strings.TrimSpace(locKey) + "|" +
		strconv.FormatInt(uid, 10)
}

// ---------------------------------------------------------------------------
// T-103 共享派生的唯一适配层
// ---------------------------------------------------------------------------

// activityDecide 在候选上跑 T-103 的 DecidePersonality，并统一处理「不产出决定」的
// 三种情形：配置非法、候选为空、没有可行候选。调用方不得在 !Decided 时补默认动作。
func activityDecide(p aiconfig.PersonalityConfig, cands []aiconfig.PersonalityCandidate,
	seed int64) aiconfig.PersonalityChoice {

	if err := p.Validate(); err != nil {
		return aiconfig.PersonalityChoice{Reason: aiconfig.ChoiceInvalidConfig}
	}
	if len(cands) == 0 {
		return aiconfig.PersonalityChoice{Reason: aiconfig.ChoiceNoFeasible}
	}
	return aiconfig.DecidePersonality(p, cands, seed)
}

// activityWants 用「做 / 不做」两个候选回答一个二选一行为问题。
//
// actFeature 是这次动作的事实特征；不做候选固定中性（代表什么都不做）。
// 返回的第二值是本次派生所用的版本与权重，供回执逐次记录。
func activityWants(p aiconfig.PersonalityConfig, seed int64,
	actFeature aiconfig.PersonalityFeature) (bool, aiconfig.PersonalityChoice) {

	choice := activityDecide(p, []aiconfig.PersonalityCandidate{
		{ID: "act", Feasible: true, Feature: actFeature},
		{ID: aiconfig.CandidateSilence, Feasible: true, Feature: activityInactionFeature},
	}, seed)
	if !choice.Decided {
		return false, choice
	}
	return choice.ChosenID == "act", choice
}

// activityInactionFeature 是「什么都不做」候选的特征。
//
// 它不是「未知事实的中性值」：什么都不做既没有社交参与、也没有新目标、
// 没有投入、没有作息吻合，四项全为 0，因此在任何四维下的得分都恰好是 0。
// 这样 softmax 比较的是「这件事对这个角色有多大吸引力」，
// 而不是「沉默本身也带着角色的偏好」。未知事实仍按 PersonalityFeatureNeutral
// 处理（同值比较，四项影响互相抵消）。
var activityInactionFeature = aiconfig.PersonalityFeature{}

// 行为动作的事实特征。它们只表达「这件事实在发生什么」，
// 不携带概率：概率全部由 T-103 的四维派生给出。

// activityRestFeature 是「这一段钓完去休息」的事实特征。
// 能量越低，休息越接近这个角色的作息；其余三项按休息本身的性质取低值。
func activityRestFeature(energy float64) aiconfig.PersonalityFeature {
	low := clamp01((60 - energy) / 60)
	return aiconfig.PersonalityFeature{
		Social: 0.25, Novelty: 0.15, Investment: 0.3, Rhythm: low,
	}
}

// activityReplyFeature 是「玩家上星/上蓝时接一句」的事实特征。
// 星/蓝越稀有，越值得应声；grade<3 不算这类事件。
func activityReplyFeature(grade int) aiconfig.PersonalityFeature {
	novelty := 0.55
	social := 0.85
	if grade >= 4 {
		novelty, social = 1.0, 1.0
	}
	return aiconfig.PersonalityFeature{Social: social, Novelty: novelty, Investment: 0.5, Rhythm: 0.5}
}

// activityCatchFeature 是「自己上鱼后报一句」的事实特征。
// 普通达标（ok）恒为 0：这一档根本没有候选，保持静默。
func activityCatchFeature(ev aiFishEvent) aiconfig.PersonalityFeature {
	var novelty float64
	switch ev {
	case evSuper:
		novelty = 1.0
	case evStar:
		novelty = 0.8
	case evNear:
		novelty = 0.35
	case evMiss:
		novelty = 0.15
	}
	investment := 0.55
	if ev == evSuper {
		investment = 0.75
	}
	return aiconfig.PersonalityFeature{Social: 0.5, Novelty: novelty, Investment: investment, Rhythm: 0.5}
}

// activityGatherFeature 是「主动起桌/主动开个话头」的事实特征。
func activityGatherFeature() aiconfig.PersonalityFeature {
	return aiconfig.PersonalityFeature{Social: 0.9, Novelty: 0.45, Investment: 0.5, Rhythm: 0.5}
}

// activitySpeakerFeature 是圆桌里「这个人此刻该不该开口」的事实特征。
//
// 被点名/被直接追问 → 社交参与高；还没说过 → 投入高；刚冷场过 → 参与与投入都低。
func activitySpeakerFeature(mentioned, spoken, passed int) aiconfig.PersonalityFeature {
	social := 0.5
	if mentioned > 0 {
		social = 1.0
	}
	investment := 0.7
	if spoken > 0 {
		investment = 0.3
	}
	if passed > 0 {
		social = math.Min(social, 0.25)
		investment = 0.2
	}
	return aiconfig.PersonalityFeature{Social: social, Novelty: 0.45, Investment: investment, Rhythm: 0.5}
}

// clamp01 把事实特征夹到 0..1；事实越界不是配置错误，按端点处理。
func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ---------------------------------------------------------------------------
// 能力（T-104）在游戏服侧的读法
// ---------------------------------------------------------------------------

// aiCapabilityState 读某个角色当前的能力投影。读取失败按失败关闭处理：
// 缺配置只保留基础私聊，不授予工具与事件。
func (s *Server) aiCapabilityState(ctx context.Context, agentKey string) aiconfig.CapabilityState {
	if s.DB == nil || strings.TrimSpace(agentKey) == "" {
		return aiconfig.CapabilityState{State: aiconfig.CapabilityStateMissing}
	}
	st, err := store.AIReadCapabilityState(ctx, s.DB.SQL, agentKey)
	if err != nil {
		s.log.Warn("AI 能力读取失败", "agent", agentKey, "err", err)
		return aiconfig.CapabilityState{
			AgentKey: agentKey, State: aiconfig.CapabilityStateInvalid, Reason: "read_failed",
		}
	}
	return st
}

// aiEventAuthorized 是 GS 入口的投递前复核：用 T-104 的 TrustedEvent + AuthorizeEvent
// 判定「这条主动事件现在能不能交给这个角色」。
//
// 返回的状态带本次读到的能力修订，调用方必须把它记进日志/回执——这是 GS 侧
// 唯一能自证的「来源 + 修订」证据。类别由服务端入口写死，不接受任何外来源。
func (s *Server) aiEventAuthorized(ctx context.Context, category, agentKey string) (aiconfig.CapabilityState, error) {
	st := s.aiCapabilityState(ctx, agentKey)
	return st, authorizeEventState(st, category, agentKey)
}

// authorizeEventState 是纯判定：状态已经读好时不再读库。
// 包级函数，供没有 Server 的只读调用点（例如后台侧的测试钩子前置校验）复用。
func authorizeEventState(st aiconfig.CapabilityState, category, agentKey string) error {
	ev := aiconfig.TrustedEvent{
		Category:       category,
		Source:         "system",
		AgentKeys:      []string{agentKey},
		ConfigRevision: st.Revision,
	}
	return aiconfig.AuthorizeEvent(ev, map[string]aiconfig.CapabilityState{agentKey: st})
}

// aiEventStateOK 报告一个能力投影是否还能承载主动事件（撤权/停用后为 false）。
//
// 只有显式配置过的集合才可能授权事件；missing / invalid / inactive / empty 都不行。
func aiEventStateOK(st aiconfig.CapabilityState) bool {
	if st.State != aiconfig.CapabilityStateConfigured {
		return false
	}
	for _, c := range st.Capabilities {
		if def, ok := aiconfig.CapabilityByID(c); ok && def.Event != "" {
			return true
		}
	}
	return false
}

// aiEventCapabilityHeld 报告某个具体事件类别此刻是否仍被授予。
func aiEventCapabilityHeld(st aiconfig.CapabilityState, category string) bool {
	def, ok := aiconfig.CapabilityForEvent(category)
	if !ok {
		return false
	}
	return st.State == aiconfig.CapabilityStateConfigured && st.Has(def.ID)
}

// aiPersonalitySettingPrefix 是 T-103 在 ai_setting 里的键前缀。
//
// 事务内不能开第二条连接（连接池只允许 1 条），所以 actor/建议事务里必须
// 用 tx.Setting 读同一行；前缀必须与 store 侧一致。一致性由
// TestPersonalitySettingKeyMatchesStore 用真实写入反证，不靠注释保证。
const aiPersonalitySettingPrefix = "ai_personality:"

func personalitySettingKey(agentKey string) string {
	return aiPersonalitySettingPrefix + strings.TrimSpace(agentKey)
}

// parsePersonalitySetting 解析事务内读到的一行性格值。
//
// 缺值 → 中性默认、derivable=true；显式非法 → derivable=false（不产出派生决定）。
func parsePersonalitySetting(raw string) (cfg aiconfig.PersonalityConfig, derivable bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return aiconfig.DefaultPersonality(), true
	}
	parsed, present, err := aiconfig.ParsePersonality(raw)
	if err != nil || !present {
		return aiconfig.DefaultPersonality(), false
	}
	return parsed, true
}

// actorPersonalityBinding 在 actorPersonality 的基础上多回答一个问题：
// 这一版四维是「真的存过」的修订，还是缺值时的中性默认？
//
// 缺值（Present=false）时选择照常按中性默认进行，但没有可证明的修订，
// 因而不写 GS 侧版本证据——「合法默认」与「有已保存且已生效的修订」必须分开。
func (s *Server) actorPersonalityBinding(ctx context.Context, agentKey string) (aiconfig.PersonalityConfig, bool, bool) {
	if s.DB == nil {
		return aiconfig.DefaultPersonality(), false, false
	}
	p, err := s.DB.AIGetPersonality(ctx, agentKey)
	if err != nil {
		s.log.Warn("AI 性格读取失败", "agent", agentKey, "err", err)
		return aiconfig.DefaultPersonality(), false, false
	}
	if !p.Derivable() {
		return aiconfig.DefaultPersonality(), false, false
	}
	return p.Config, true, p.Present && p.Config.Revision > 0
}

// aiPersonalityRevisionPtr 把「这次选择真正用了哪一版四维」包成可绑定的指针。
// 没有存过修订时返回 nil：宁可标未确认，也不补一个 0 当作已生效。
func aiPersonalityRevisionPtr(cfg aiconfig.PersonalityConfig, bindable bool) *int64 {
	if !bindable || cfg.Revision <= 0 {
		return nil
	}
	rev := cfg.Revision
	return &rev
}
