package aibridge

// AI 角色状态（aibridge 这一侧）：只读注入提示词，并在玩家"安利"某条鱼时
// 提交一条 target 建议，由唯一活动 gameserver 决定是否应用。
//
// 运行态写者只有 GS（T-020 / DEV-004）：这里不推进活动、不初始化状态、
// 不整行写回。状态缺失时宁可不注入位置句，也不为一个 prompt 自造真实状态。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

// actorLine 读 Agent 当前已确认的状态，压成一句可注入 system 的话。
// 同时返回所在地图 key，供鱼情卡对齐——AI 说自己在哪，卡就得是哪张图。
// 只读：不推进 Activity、不初始化缺失状态（写者只有 GS）。
func (r *Runner) actorLine(ctx context.Context, agentKey, fallbackLoc string) (line, loc string) {
	if r.db.AISetting(ctx, "ai_actor_enabled", "1") != "1" {
		return "", ""
	}
	a, err := r.db.AIGetActor(ctx, agentKey)
	if err != nil || a == nil {
		return "", ""
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

// activitySnapshotBudget 是一次只读快照的查询超时：读不到就写 unknown，不重试。
// 真正的超时与只读事务在 store.AIActivityFacts 内，这里保留常量供提示词文案引用。
const activitySnapshotBudget = store.AIActivityFactsBudget

// activityFactSnapshotHookForTest 仅测试用：每真正取一次快照触发一次，
// 用于证明「一个任务只取一份、圆桌全部发言者共用」。生产恒为 nil。
var activityFactSnapshotHookForTest func()

// SetActivityFactSnapshotHookForTest 安装快照计数钩子，返回恢复函数。
func SetActivityFactSnapshotHookForTest(fn func()) (restore func()) {
	prev := activityFactSnapshotHookForTest
	activityFactSnapshotHookForTest = fn
	return func() { activityFactSnapshotHookForTest = prev }
}

// activityFactSnapshot 返回本任务的只读活动事实快照（同一任务只读一次）。
//
// 边界（T-108 §2 / §9.3）：只读官方活动事实，不调用懒结算、不创建比赛实体、
// 不注册假事件、不写任何官方表。competition 投影只经 store.AIActivityFacts
// 这一个入口读（本文件不允许出现对官方表的裸 SQL）；AI 没有参赛身份，因此
// registered=false、capability=unavailable、资格留 unknown，
// 绝不由模型或本函数推断「正在比赛」。
//
// 每条事实都带来源、采样时间与陈旧标志：过期的行只说明「本地没结算」，
// 不是官方已结束，也不能当成退出许可。
func (r *Runner) activityFactSnapshot(ctx context.Context, agentKey string) string {
	if r.db == nil {
		return ""
	}
	if activityFactSnapshotHookForTest != nil {
		activityFactSnapshotHookForTest()
	}
	sampled := time.Now()
	facts, err := r.db.AIActivityFacts(ctx)
	if err == nil && !facts.SampledAt.IsZero() {
		sampled = facts.SampledAt
	}
	if err != nil {
		return "只读活动事实（source=official-db(本地投影)，sampled_at=" +
			sampled.Format("15:04:05") + "）：\n" +
			"- 活动状态：" + r.actorSnapshotSentence(ctx, agentKey) + "\n" +
			"- competition_snapshot=unknown（本地赛事投影读取失败），不得据此声称正在比赛。\n" +
			"- 你的参赛身份：registered=false（AI 无参赛身份），capability=unavailable，资格=unknown\n" +
			"- event_snapshot=unknown(source_unavailable)（本版没有允许的读取路径）；" +
			"负证据：不调用懒结算、不注册事件、不写官方表、不声称官方已结束\n" +
			"以上只说明本地事实；没有明确证据时不要声称自己正在比赛或已经报名。" +
			activitySnapshotTrailer(sampled, true)
	}

	list := facts.Competitions
	running := 0
	stale := facts.Stale
	phase := ""
	for _, c := range list {
		label := activityCompStateName(c.State)
		rowStale := c.EndsTS > 0 && sampled.After(time.Unix(int64(c.EndsTS), 0))
		if rowStale {
			// 本地未结算不等于官方已结束：阶段只能标 unknown，不宣称 finished。
			label = "unknown"
		}
		if c.State == store.CompetitionRunning {
			// 报名中（state=1）与未开始（state=5）不计入「进行中」。
			running++
		}
		if phase == "" {
			phase = label + "「" + c.Name + "」"
			if c.EndsTS > 0 {
				phase += "，本地预计结束 " + time.Unix(int64(c.EndsTS), 0).Format("15:04")
				if rowStale {
					phase += "（已过本地预计时间，状态 unknown）"
				}
			}
		}
	}
	if phase == "" {
		phase = "本地没有进行中/报名中的赛事"
	}
	var b strings.Builder
	b.WriteString("只读活动事实（source=official-db(本地投影)，sampled_at=" +
		sampled.Format("15:04:05") + "）：\n")
	b.WriteString("- 活动状态：" + r.actorSnapshotSentence(ctx, agentKey) + "\n")
	b.WriteString("- competition_snapshot：本地赛事 " + strconv.Itoa(len(list)) +
		" 条，进行中 " + strconv.Itoa(running) + "；阶段=" + phase + "\n")
	if len(list) > 0 {
		// 最多 5 条实际投影行；只写赛事自身的公开字段，不含玩家数据。
		b.WriteString("- 赛事明细（最多 5 条，不含玩家数据）：\n")
		for _, c := range list {
			label := activityCompStateName(c.State)
			if c.EndsTS > 0 && sampled.After(time.Unix(int64(c.EndsTS), 0)) {
				label = "unknown（已过本地预计时间，未结算）"
			}
			line := "  - id=" + c.ID + " 状态=" + label + "「" + c.Name + "」"
			if c.StartedTS > 0 {
				line += " 开始=" + time.Unix(int64(c.StartedTS), 0).Format("15:04:05")
			} else {
				line += " 开始=unknown"
			}
			if c.EndsTS > 0 {
				line += " 预计结束=" + time.Unix(int64(c.EndsTS), 0).Format("15:04:05")
			} else {
				line += " 预计结束=unknown"
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("- 你的参赛身份：registered=false（AI 无参赛身份），capability=unavailable，资格=unknown\n")
	b.WriteString("- event_snapshot=unknown(source_unavailable)（本版没有允许的 BR 读取路径）；" +
		"负证据：不调用懒结算、不注册事件、不写官方表、不声称官方已结束\n")
	b.WriteString("以上只说明本地事实；没有明确证据时不要声称自己正在比赛或已经报名。")
	b.WriteString(activitySnapshotTrailer(sampled, stale))
	return b.String()
}

// activityCompStateName 把官方比赛状态码翻成中文阶段名。
func activityCompStateName(state int64) string {
	switch state {
	case store.CompetitionRegistration:
		return "报名中"
	case store.CompetitionRunning:
		return "进行中"
	case store.CompetitionCreated:
		return "未开始"
	case store.CompetitionFinished:
		return "已结束"
	case store.CompetitionAborted:
		return "已中止"
	}
	return "unknown"
}

// activitySnapshotTrailer 统一挂上采样时间与陈旧标志。
func activitySnapshotTrailer(sampled time.Time, stale bool) string {
	flag := "stale=false"
	if stale {
		flag = "stale=true（本地投影未结算，不能当成官方已结束）"
	}
	return "（sampled_at=" + sampled.Format("15:04:05") + "，" + flag + "）"
}

// actorSnapshotSentence 复用角色状态句，作为快照里的「活动状态」一行。
func (r *Runner) actorSnapshotSentence(ctx context.Context, agentKey string) string {
	if agentKey == "" {
		return "unknown（没有可证实的角色状态）"
	}
	a, err := r.db.AIGetActor(ctx, agentKey)
	if err != nil || a == nil {
		return "unknown（没有可证实的角色状态）"
	}
	line := r.actorSentence(a)
	if line == "" {
		return "unknown（角色状态缺少地图）"
	}
	return line
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

// enqueueTargetSuggest 是唯一模型写工具的执行端：解析鱼/图、读控制代次、
// 生成稳定 request_id，然后投递一条 actor_target_suggest 命令。
//
// BR 只提交建议，绝不应用状态；冷却、概率、代次与范围由 GS 在写事务里判定。
// request_id 由服务端按 (namespace, agent, generation, task_id, tool_call_id,
// tool_name) 取 SHA-256，模型不能指定 ID 或身份；同一次工具调用重试复用同一
// ID，不同载荷会撞 payload_hash 被拒绝。
func (r *Runner) enqueueTargetSuggest(ctx context.Context, t *store.AITask, agentKey string,
	capRevision int64, fishName, locName, callID string) (ToolResult, error) {
	if t == nil || r.db == nil || r.knowledge == nil {
		return ToolResult{Status: "unavailable", Reason: "no_runtime"},
			fmt.Errorf("%w: 运行环境不可用", ErrToolUnavailable)
	}
	if r.db.AISetting(ctx, "ai_actor_enabled", "1") != "1" {
		return ToolResult{Status: "denied", Reason: "actor_disabled"},
			fmt.Errorf("%w: 虚拟角色总闸已关闭", ErrToolUnavailable)
	}
	a, err := r.db.AIGetActor(ctx, agentKey)
	if err != nil || a == nil {
		return ToolResult{Status: "unavailable", Reason: "no_actor_state"},
			fmt.Errorf("%w: 角色还没有可建议的运行态", ErrToolUnavailable)
	}
	fishName = strings.TrimSpace(fishName)
	if fishName == "" {
		return ToolResult{Status: "denied", Reason: "invalid_args"},
			fmt.Errorf("%w: 缺少 fish", ErrToolInvalidArgs)
	}
	// 模型可能把「河鲈」当鱼名；静态库按「鲈鱼」建索引，先归一化再校验。
	if resolved := r.knowledge.ResolveFishName(ctx, fishName); resolved != "" {
		fishName = resolved
	}
	if fishName == a.TargetName {
		return ToolResult{Status: "ok", Data: "这条鱼已经是当前目标，不用再提建议。"}, nil
	}
	fishKey := r.fishKeyByName(ctx, fishName)
	if fishKey == "" {
		return ToolResult{Status: "denied", Reason: "invalid_args"},
			fmt.Errorf("%w: 静态库里没有这条鱼 %q", ErrToolInvalidArgs, fishName)
	}
	locKey := strings.TrimSpace(locName)
	if locKey != "" {
		// 显式给了地图：中文名换成 level key；换不到就原样交给 GS，由 roster 拒绝。
		if alias := r.actorAliasKey(ctx, locKey); alias != "" {
			locKey = alias
		}
	} else {
		locKey, _ = r.bestMapForFish(ctx, fishName)
	}
	if locKey == "" {
		return ToolResult{Status: "denied", Reason: "invalid_args"},
			fmt.Errorf("%w: 找不到 %q 的可钓地图", ErrToolInvalidArgs, fishName)
	}
	// 一次任务只允许一条目标建议：模型工具、老旁路与重试都收敛到同一条命令。
	if existing, err := r.db.AIGetRuntimeCommandByParent(ctx, t.ID, store.AIRuntimeKindActorTargetSuggest); err == nil {
		if p, perr := store.AIRuntimePayloadTargetSuggestFull(existing.Payload); perr == nil &&
			p.FishKey == fishKey && p.LocKey == locKey {
			if existing.Status == store.AIRuntimeStatusPending || existing.Status == store.AIRuntimeStatusApplied {
				notApplied := false
				return ToolResult{
					Status: "accepted", RequestID: existing.RequestID, Applied: &notApplied,
					Data: "已提交，等待游戏服务确认。", SampledAt: time.Now().Unix(),
				}, nil
			}
			return ToolResult{Status: "unavailable", Reason: "already_finished", RequestID: existing.RequestID},
				fmt.Errorf("%w: 这条建议已结束（%s）", ErrToolUnavailable, existing.Status)
		}
		return ToolResult{Status: "denied", Reason: "conflict"},
			fmt.Errorf("%w: 本次任务已提交过另一条目标建议", ErrToolInvalidArgs)
	} else if !errors.Is(err, store.ErrAIRuntimeCommandNotFound) {
		return ToolResult{Status: "unavailable", Reason: "read_failed"}, err
	}
	ctl, err := r.db.AIGetRuntimeControl(ctx, agentKey)
	if err != nil {
		return ToolResult{Status: "unavailable", Reason: "control_unreadable"}, err
	}
	payload, err := json.Marshal(store.AIRuntimeActorTargetSuggestPayload{
		FishKey:     fishKey,
		LocKey:      locKey,
		Capability:  aiconfig.CapabilityAcceptTarget,
		CapRevision: capRevision,
	})
	if err != nil {
		return ToolResult{Status: "unavailable", Reason: "payload"}, err
	}
	requestID := actorTargetRequestID(actorTargetNamespace, agentKey, ctl.Generation,
		t.ID, callID, aiconfig.ToolActorTargetSuggest)
	cmd, _, err := r.db.AIEnqueueRuntimeCommand(ctx, store.AIRuntimeEnqueueRequest{
		RequestID:          requestID,
		ParentTaskID:       t.ID,
		Source:             store.AIRuntimeSourceBridge,
		Kind:               store.AIRuntimeKindActorTargetSuggest,
		AgentKey:           agentKey,
		ExpectedGeneration: ctl.Generation,
		ExpectedRevision:   ctl.Revision,
		Payload:            payload,
		TTL:                30 * time.Second,
	})
	if err != nil {
		// 队列满/去重冲突/DB 故障：明确告诉模型没提交成功，不能假装已受理。
		reason := "enqueue_failed"
		if errors.Is(err, store.ErrAIRuntimeQueueFull) {
			reason = "queue_full"
		}
		if errors.Is(err, store.ErrAIRuntimeCommandConflict) {
			return ToolResult{Status: "denied", Reason: "conflict"},
				fmt.Errorf("%w: 与已提交的建议不一致", ErrToolInvalidArgs)
		}
		r.log.Warn("AI 目标建议提交失败", "agent", agentKey, "err", err)
		return ToolResult{Status: "unavailable", Reason: reason},
			fmt.Errorf("%w: 建议未提交", ErrToolUnavailable)
	}
	// 受理不等于生效：只回固定话术，终态由后续任务读取本角色至多 8 条记录。
	_ = cmd
	notApplied := false
	return ToolResult{
		Status: "accepted", RequestID: requestID, Applied: &notApplied,
		Data: "已提交，等待游戏服务确认。", SampledAt: time.Now().Unix(),
	}, nil
}

// actorTargetNamespace 是写请求 ID 的命名空间；换版本必须同时给旧行兼容口径。
const actorTargetNamespace = "actor_target_suggest/v1"

// actorTargetRequestID 由服务端生成，模型不能指定。
func actorTargetRequestID(namespace, agentKey string, generation, taskID int64,
	callID, toolName string) string {
	if strings.TrimSpace(callID) == "" {
		callID = "no-call-id"
	}
	raw := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s\x00%s",
		namespace, agentKey, generation, taskID, callID, toolName)
	sum := sha256.Sum256([]byte(raw))
	return "ai-" + hex.EncodeToString(sum[:16])
}

// actorAliasKey 把中文水域名换成 level key；本来就是 key 时原样返回。
func (r *Runner) actorAliasKey(ctx context.Context, loc string) string {
	if r.knowledge == nil || r.knowledge.static == nil || loc == "" {
		return ""
	}
	var alias string
	if err := r.knowledge.static.QueryRowContext(ctx,
		`SELECT alias FROM loc_alias WHERE loc=? LIMIT 1`, loc).Scan(&alias); err == nil && alias != "" {
		return alias
	}
	var name string
	if err := r.knowledge.static.QueryRowContext(ctx,
		`SELECT loc FROM loc_alias WHERE alias=? LIMIT 1`, loc).Scan(&name); err == nil && name != "" {
		return loc
	}
	return ""
}

// fishKeyByName 把中文鱼名解析回静态库的 fish_key（GS 校验用）。
func (r *Runner) fishKeyByName(ctx context.Context, fishName string) string {
	if r.knowledge == nil || r.knowledge.static == nil || fishName == "" {
		return ""
	}
	var key string
	_ = r.knowledge.static.QueryRowContext(ctx, `
		SELECT fish_key FROM loc_fish WHERE name_zh=? ORDER BY catches DESC LIMIT 1`,
		fishName).Scan(&key)
	return key
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

// maybeSwitchActor 保留改造前的「玩家安利鱼」触发，但只在角色获准
// 「接受目标建议」时提交；没有能力就完全不走这条旁路。
//
// 它与模型写工具共用同一任务的首条建议（enqueueTargetSuggest 内按父任务去重），
// 因此同一次任务里旁路与工具调用不会产生两条命令。
func (r *Runner) maybeSwitchActor(ctx context.Context, t *store.AITask, agentKey string) {
	if t == nil || r.db == nil || r.knowledge == nil {
		return
	}
	if t.SourceKind != "human" || (t.Kind != "dm" && t.Kind != "channel") {
		return
	}
	if r.db.AISetting(ctx, "ai_actor_enabled", "1") != "1" {
		return
	}
	view, err := r.db.AIGetCapabilities(ctx, agentKey)
	if err != nil || !view.State.Has(aiconfig.CapabilityAcceptTarget) {
		return
	}
	a, err := r.db.AIGetActor(ctx, agentKey)
	if err != nil || a == nil {
		return
	}
	fish := r.knowledge.ResolveFishName(ctx, t.Text)
	if fish == "" || fish == a.TargetName || !fishMentioned(t.Text, fish) {
		return
	}
	// 本地冷却只是降噪：真正的冷却校验在 GS 事务里，BR 的结论不被信任。
	cooldown := float64(r.intSetting(ctx, "ai_actor_switch_cooldown_s", 1800, 60, 21600))
	if a.SwitchTS > 0 && float64(time.Now().Unix())-a.SwitchTS < cooldown {
		return
	}
	if _, err := r.enqueueTargetSuggest(ctx, t, agentKey, view.State.Revision, fish, "", "auto"); err != nil {
		r.log.Warn("AI 角色建议提交失败", "agent", agentKey, "err", err)
	}
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
