package aibridge

// 任务编排：确定性快速路径（不调模型）→ 出网开关 → function calling 多轮 → 写结果。
// 成本控制只有思考开关/强度 + 并发、超时、轮次上限、紧急停止，不做用量计量。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/store"
)

const (
	replyNotConfigured = "助手还没配置好：请在管理后台填写厂商、模型 ID 和 API Key。"
	replyUnavailable   = "助手暂时有点问题，等会儿再试。"
	replyStopped       = "助手已暂停（出网开关关闭或紧急停止）。"
	aiToolWaitBudget   = 4 * time.Second
)

// Runner 是 aibridge 的主循环。
type Runner struct {
	db        *store.DB
	knowledge *Knowledge
	registry  *Registry
	sessions  *SessionStore
	cfg       config.AI
	masterKey []byte
	log       *slog.Logger
	sem       chan struct{}
	lastSweep time.Time
	lastPrune time.Time
}

func NewRunner(db *store.DB, k *Knowledge, cfg config.AI, masterKey []byte, log *slog.Logger) *Runner {
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = 1
	}
	return &Runner{
		db: db, knowledge: k, registry: NewRegistry(k), sessions: NewSessionStore(),
		cfg: cfg, masterKey: masterKey,
		log: log, sem: make(chan struct{}, n),
	}
}

func (r *Runner) Loop(ctx context.Context) {
	interval := time.Duration(r.cfg.PollIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r.pump(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.pump(ctx)
		}
	}
}

func (r *Runner) pump(ctx context.Context) {
	r.sweep(ctx)
	var wg sync.WaitGroup
	for i := 0; i < cap(r.sem); i++ {
		t, ok, err := r.db.AIPopTask(ctx)
		if err != nil {
			r.log.Warn("AI 任务读取失败", "err", err)
			break
		}
		if !ok {
			break
		}
		r.sem <- struct{}{}
		wg.Add(1)
		go func(task *store.AITask) {
			defer wg.Done()
			defer func() { <-r.sem }()
			// 单个任务的坏数据不能带崩整个桥接进程。
			defer func() {
				if rec := recover(); rec != nil {
					r.log.Error("AI 任务异常", "id", task.ID, "panic", rec)
					_ = r.db.AIFinishTask(context.Background(), task.ID, "fallback")
				}
			}()
			r.handle(ctx, task)
		}(t)
	}
	wg.Wait()
}

// sweep 清理超时任务、回收异常退出的 processing 任务，并低频删除历史元数据。
// 这些操作不需要每 500ms 做一次，30 秒一次足够。
func (r *Runner) sweep(ctx context.Context) {
	now := time.Now()
	if !r.lastSweep.IsZero() && now.Sub(r.lastSweep) < 30*time.Second {
		return
	}
	r.lastSweep = now
	nowF := float64(now.Unix())
	expired, recovered, err := r.db.AIExpireTasks(ctx, nowF, 60)
	if err != nil {
		r.log.Warn("AI 任务过期清理失败", "err", err)
	} else if expired > 0 || recovered > 0 {
		r.log.Info("AI 任务清理", "expired", expired, "recovered", recovered)
	}
	if n, err := r.db.AIExpireToolJobs(ctx, nowF-60); err != nil {
		r.log.Warn("AI 工具作业清理失败", "err", err)
	} else if n > 0 {
		r.log.Info("AI 工具作业超时", "count", n)
	}
	if r.lastPrune.IsZero() || now.Sub(r.lastPrune) >= time.Hour {
		r.lastPrune = now
		if err := r.db.AIPrune(ctx, nowF-7*24*3600); err != nil {
			r.log.Warn("AI 历史清理失败", "err", err)
		}
	}
}

// sessionKey 是会话记忆的分区键。
// 频道共享同一份上下文：回答的人可能是随机的，但都要接得上同一段频道聊天。
// 私聊按 Agent 分开，各自保留与本人的对话。
func sessionKey(t *store.AITask) string {
	if t.Kind == "channel" || t.Kind == "roundtable" {
		return "channel|" + t.ScopeKey
	}
	return t.Scope + "|" + t.ScopeKey + "|" + t.Agent
}

// stripSpeakerPrefix 去掉模型模仿历史格式而带出来的【名字】前缀。
func stripSpeakerPrefix(s string) string {
	if !strings.HasPrefix(s, "【") {
		return s
	}
	if i := strings.Index(s, "】"); i > 0 && i < 20 {
		return strings.TrimSpace(s[i+len("】"):])
	}
	return s
}

// sanitizeReply 把模型输出压成适合游戏内单行聊天/邮件的文本：
// 换行、制表符、连续空白和零宽字符都会破坏客户端聊天显示。
func sanitizeReply(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\ufeff', '\x00':
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func (r *Runner) finish(ctx context.Context, t *store.AITask, status, text, rawUsage, errText string) {
	// Agent 在任务执行期间被逻辑删除：不再写结果，避免删号后又冒出一条消息。
	if t.Agent != "" {
		if a, err := r.db.AIAgentByKey(ctx, t.Agent); err != nil || a == nil || !a.Enabled {
			_ = r.db.AIFinishTask(ctx, t.ID, "fallback")
			return
		}
	}
	// 频道里的失败提示会打断其他角色聊天，静默降级；私聊仍回一句可读提示。
	if t.Kind == "channel" && status == "fallback" {
		text = ""
	}
	if status == "done" && text != "" {
		text = sanitizeReply(text)
		key := sessionKey(t)
		said := text
		if t.Kind == "channel" {
			// 频道里可能有多个人说话，记下是谁说的，下次换人答也能接上。
			if a, err := r.db.AIAgentByKey(ctx, t.Agent); err == nil && a != nil {
				said = "【" + a.Display + "】" + text
			}
		}
		r.sessions.Append(key,
			Message{Role: "user", Content: t.Text},
			Message{Role: "assistant", Content: said})
	}
	if err := r.db.AIComplete(ctx, t.ID, status, text, rawUsage, errText); err != nil &&
		!errors.Is(err, store.ErrAITaskNotActive) {
		r.log.Warn("AI 结果写入失败", "id", t.ID, "err", err)
	}
}

func toolArg(argsJSON, key string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func (r *Runner) handle(ctx context.Context, t *store.AITask) {
	timeout := time.Duration(r.cfg.RequestTimeoutSec * float64(time.Second))
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	// 圆桌是独立的有界循环，用自己的超时（总时长上限 + 5 秒）。
	if t.Kind == "roundtable" {
		r.handleRoundtable(ctx, t)
		return
	}

	child, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	agent, err := r.db.AIAgentByKey(ctx, t.Agent)
	if err != nil || agent == nil || !agent.Enabled {
		r.log.Warn("AI Agent 未启用或不存在", "agent", t.Agent, "err", err)
		r.finish(ctx, t, "fallback", replyNotConfigured, "", "agent disabled")
		return
	}
	// 只看不说的 Agent 不应该在频道里冒泡；静默丢弃，不投递任何文案。
	if t.Kind == "channel" && agent.ReplyMode == "silent" {
		r.finish(ctx, t, "done", "", "", "")
		return
	}

	// 单一链路：只有模型 + function calling，没有本地快速路径。
	// 原有「查鱼助手」是独立联系人，不受这里影响，玩家仍能查鱼。
	if r.db.AISetting(ctx, "emergency_stop", "0") == "1" ||
		r.db.AISetting(ctx, "outbound_enabled", "1") != "1" {
		r.finish(ctx, t, "fallback", replyStopped, "", "outbound disabled")
		return
	}

	provider, err := r.loadProvider(ctx)
	if err != nil {
		r.log.Warn("AI 厂商未就绪", "err", err)
		r.finish(ctx, t, "fallback", replyNotConfigured, "", err.Error())
		return
	}

	p := profileChannel
	if t.Kind == "dm" {
		p = profileDM
	}
	budget := r.contextBudget(ctx, p)
	replyChars := r.replyChars(ctx, p)
	softChars := r.replySoftChars(ctx, p)
	sessKey := sessionKey(t)
	firstTalk := len(r.sessions.History(sessKey, 1)) == 0
	// 角色状态只对「会去钓鱼的 Agent」生效；纯查数据的助手保持空。
	actorText := ""
	if agent.ActorEnabled {
		// 玩家刚刚安利了某条鱼？按低概率把目标换过去，再读状态注入。
		// 系统任务（如玩家上星反应）不是玩家建议，不触发换目标。
		if t.SourceKind == "human" && (t.Kind == "dm" || t.Kind == "channel") {
			r.maybeSwitchActor(ctx, agent.Key, t.Text)
		}
		actorText, _ = r.actorLine(ctx, agent.Key, t.Loc)
	}
	system := trimToTokens(agent.Prompt, 600)
	if actorText != "" {
		system += "\n\n" + actorText
	}
	// 工具结果与内部资料只取一份，避免同类信息重复注入。
	if toolText := r.registry.Run(child, agent.ToolsJSON, t.Text); toolText != "" {
		system += "\n\n内部只读工具结果：\n" + trimRunes(toolText, budget.InternalChars)
	} else if r.knowledge != nil {
		if internal := r.knowledge.Internal(child, t.Text); internal != "" {
			system += "\n\n内部只读资料（确定性事实，优先采用）：\n" + trimRunes(internal, budget.InternalChars)
		}
	}
	if r.knowledge != nil && r.db.AISetting(ctx, "web_search_enabled", "0") == "1" {
		ep := r.db.AISetting(ctx, "search_endpoint", "")
		if ext := r.knowledge.WebSearch(child, t.Text, ep); ext != "" {
			system += "\n\n外部检索参考（只作补充，不得覆盖内部数值）：\n" + trimRunes(ext, budget.InternalChars)
		}
	}
	if mem := r.memoryLine(ctx, agent.Key); mem != "" {
		system += "\n\n你记得自己上过：" + mem + "。聊到了可以自然提起，别反复吹，也别报精确数字。"
	}
	// 玩家主动问渔获时才查全量；平时只给一句常驻简报，压住「空军」这种自相矛盾。
	if agent.ActorEnabled {
		if line := r.catchBriefLine(ctx, agent.Key); line != "" {
			system += "\n\n" + line
		}
		if catchQuestion(t.Text) {
			if line := r.catchLine(ctx, agent.Key); line != "" {
				system += "\n\n" + line
			}
		}
	}
	system += "\n\n资料没有的内容明确说不知道，不要编造坐标、价格或概率。像玩家聊天一样简短，尽量不超过 " +
		strconv.Itoa(softChars) + " 字。"
	if t.Kind == "channel" {
		// 频道共享上下文：别的助手说过的话也在历史里，别当成自己说的。
		system += "频道里不止你一个助手，历史里【名字】开头的是当时说话的人；" +
			"你回复时直接说话，不要带这种前缀。"
		if peers := r.channelPeers(ctx, t.ScopeKey, agent.Key); peers != "" {
			system += "这个频道里还有 " + peers + "，他们也会接话；" +
				"别把成员名单当公告念出来。"
		}
		if firstTalk {
			// 第一句顺口报个名字就够了，别做自我介绍式的开场白。
			system += "这是你和这个玩家的第一次对话，可以顺口带一句你叫什么，一句话即可。"
		}
	}

	// 思考模式下 reasoning 也占 max_tokens；不给余量会把正文挤空。
	maxTokens := replyChars + 50
	if agent.ThinkingEnabled {
		maxTokens += thinkingHeadroom(agent.ThinkingEffort)
	}
	maxRounds := 2
	if v, err := strconv.Atoi(r.db.AISetting(ctx, "max_tool_rounds", "2")); err == nil && v > 0 && v <= 4 {
		maxRounds = v
	}

	msgs := buildMessages(system, r.sessions.History(sessKey, budget.HistoryMessages), t.Text, budget)
	specs := r.registry.Specs(agent.ToolsJSON)
	var lastUsage string

	// ③ function calling 多轮：模型主动选工具，工具结果回传后再让模型收尾。
	for round := 0; round < maxRounds; round++ {
		resp, err := provider.Chat(child, ChatRequest{
			Messages:  msgs,
			Tools:     specs,
			Thinking:  ThinkingConfig{Enabled: agent.ThinkingEnabled, Effort: agent.ThinkingEffort},
			MaxTokens: maxTokens,
		})
		if err != nil {
			r.log.Warn("AI 模型调用失败", "agent", t.Agent, "round", round, "err", err)
			r.finish(ctx, t, "fallback", replyUnavailable, "", err.Error())
			return
		}
		if resp.RawUsage != "" {
			lastUsage = resp.RawUsage
		}
		if len(resp.ToolCalls) == 0 {
			text := trimRunes(sanitizeReply(stripSpeakerPrefix(strings.TrimSpace(resp.Text))), replyChars)
			if text == "" {
				text = replyUnavailable
			}
			r.finish(ctx, t, "done", text, lastUsage, "")
			return
		}
		msgs = append(msgs, Message{
			Role: "assistant", Content: resp.Text,
			ToolCalls: resp.ToolCalls, Reasoning: resp.ReasoningContent,
		})
		for _, call := range resp.ToolCalls {
			var out string
			switch call.Name {
			case "fish_query":
				fish := toolArg(call.Arguments, "fish")
				if fish == "" {
					fish = firstFishKeyword(t.Text)
				}
				if fish != "" {
					out = r.remoteTool(child, t.ID, t.UID, "fish_query", fish)
				}
			case "map_vibe":
				loc := toolArg(call.Arguments, "loc")
				if loc == "" {
					loc = t.Loc
				}
				if loc != "" {
					out = r.remoteTool(child, t.ID, t.UID, "map_vibe", loc)
				}
			default:
				out = r.registry.RunOne(child, call.Name, call.Arguments)
			}
			if out == "" {
				out = "（工具没有返回数据）"
			}
			msgs = append(msgs, Message{
				Role: "tool", Name: call.Name,
				ToolCallID: call.ID, Content: trimRunes(out, budget.InternalChars),
			})
		}
	}
	// 轮次用完时模型可能还在要工具：再给一次「不许调工具」的收尾机会，
	// 把已有工具结果变成回答，而不是直接回一句失败。
	resp, err := provider.Chat(child, ChatRequest{
		Messages:  msgs,
		Thinking:  ThinkingConfig{Enabled: false},
		MaxTokens: maxTokens,
	})
	if err != nil {
		r.log.Warn("AI 收尾调用失败", "agent", t.Agent, "err", err)
		r.finish(ctx, t, "fallback", replyUnavailable, lastUsage, err.Error())
		return
	}
	if resp.RawUsage != "" {
		lastUsage = resp.RawUsage
	}
	text := trimRunes(sanitizeReply(stripSpeakerPrefix(strings.TrimSpace(resp.Text))), replyChars)
	if text == "" {
		text = replyUnavailable
	}
	r.finish(ctx, t, "done", text, lastUsage, "")
}

// thinkingHeadroom 是思考档位额外预留的输出 token。
// reasoning_content 与正文共享 max_tokens，纯按正文字数截会被思考过程吃光。
func thinkingHeadroom(effort string) int {
	switch effort {
	case "low":
		return 256
	case "medium":
		return 512
	case "high":
		return 1024
	case "max":
		return 2048
	default:
		return 0
	}
}

// channelPeers 返回同频道的其他助手名字（不含自己），用于让发言知道「旁边还有谁」。
func (r *Runner) channelPeers(ctx context.Context, channel, self string) string {
	list, err := r.db.AIChannelAgents(ctx, channel)
	if err != nil {
		return ""
	}
	names := make([]string, 0, len(list))
	for _, a := range list {
		if a.Key != self {
			names = append(names, a.Display)
		}
	}
	return strings.Join(names, "、")
}

// memoryLine 把结构化战绩压成一句 ≤150 字的话，注入 system。
// 它独立于对话历史，所以聊多久都不会忘了自己上过什么鱼。
//
// 只保留「什么鱼 + 什么档次」，不列精确重量：人记得住自己上过一条星的鲈鱼，
// 记不住「1.20kg」。具体数字归玩家真问时的 catchLine。
func (r *Runner) memoryLine(ctx context.Context, agentKey string) string {
	if r.db.AISetting(ctx, "ai_memory_inject", "1") != "1" {
		return ""
	}
	recentN := r.intSetting(ctx, "ai_memory_recent", 3, 0, 5)
	bestN := r.intSetting(ctx, "ai_memory_best", 2, 0, 5)
	if recentN == 0 && bestN == 0 {
		return ""
	}
	recent, _ := r.db.AIRecentMemories(ctx, agentKey, recentN)
	best, _ := r.db.AIBestMemories(ctx, agentKey, bestN)
	seen := map[int64]bool{}
	parts := make([]string, 0, recentN+bestN)
	add := func(m store.AIMemory) {
		if seen[m.ID] || m.FishName == "" {
			return
		}
		seen[m.ID] = true
		if m.Kind == "super" {
			parts = append(parts, "蓝冠的"+m.FishName)
		} else {
			parts = append(parts, "上星的"+m.FishName)
		}
	}
	for _, m := range recent {
		add(m)
	}
	for _, m := range best {
		add(m)
	}
	if len(parts) == 0 {
		return ""
	}
	return trimRunes(strings.Join(parts, "、"), 150)
}

// catchQuestion 判断玩家是不是在问 AI 自己的渔获。
// 只有这类问题才查 ai_catch，避免每句话都把战绩塞进上下文。
// catchBriefLine 把 Agent 自己的渔获压成一句常驻的定性事实，避免角色
// 「明明上了鱼却说自己一条没影」。
//
// 虚拟上鱼只写 ai_catch 表、不发频道消息（见 gameserver/ai_fishing.go），
// 所以模型平时完全不知道自己上过鱼；框架注入的状态句又只有「在哪、钓什么」，
// 模型很容易顺着把「蹲很久」演成「空军了」。这里补一句极短的事实把它压住。
//
// 刻意不给条数/重量：陪聊不是报账，谁也不会张口就说「累计 7 条、总重 12.4kg」。
// 玩家真问「钓了多少」时，catchLine 会另给一份带数字的准确记录。
// 只有真的有渔获才注入（0 条时沉默，避免又引导出「我没钓到」）。
func (r *Runner) catchBriefLine(ctx context.Context, agentKey string) string {
	if r.db.AISetting(ctx, "ai_catch_brief_inject", "1") != "1" {
		return ""
	}
	st, err := r.db.AICatchStats(ctx, agentKey)
	if err != nil || st.Total == 0 {
		return ""
	}
	// 只在「确实上过鱼」这件事上定性，不给数字，也不替角色评价手气。
	return "你今天确实上过鱼，不是空军。别再说自己一条没上；具体数目不用记，玩家问了再说。"
}

func catchQuestion(msg string) bool {
	for _, w := range []string{
		"钓了多少", "钓了几", "多少条", "几条鱼", "渔获", "收获",
		"战绩", "最近钓", "钓到过", "钓过什么", "最大的一条", "上过什么鱼",
		// 自然语言里常见的问法，以前会漏（「你们钓了哪些鱼了」实测没命中）。
		"钓了哪些", "钓了什么", "钓了啥", "上鱼了吗", "上鱼没", "有鱼吗",
		"钓到没", "钓到鱼没", "钓上", "空军",
	} {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}

// catchLine 查 AI 渔获库，压成一句可注入的事实（≤180 字）。
func (r *Runner) catchLine(ctx context.Context, agentKey string) string {
	st, err := r.db.AICatchStats(ctx, agentKey)
	if err != nil {
		return ""
	}
	if st.Total == 0 {
		return "你查了自己的渔获记录：还没有钓到鱼。"
	}
	parts := []string{fmt.Sprintf("累计 %d 条、总重 %.2fkg", st.Total, float64(st.TotalG)/1000)}
	if st.Today > 0 {
		parts = append(parts, fmt.Sprintf("今天 %d 条", st.Today))
	}
	if st.BestName != "" {
		best := fmt.Sprintf("最大的是 %.2fkg 的%s", float64(st.BestWeightG)/1000, st.BestName)
		if st.BestLoc != "" {
			best += "（" + st.BestLoc + "）"
		}
		parts = append(parts, best)
	}
	if recent, _ := r.db.AIRecentCatches(ctx, agentKey, 3); len(recent) > 0 {
		items := make([]string, 0, len(recent))
		for _, c := range recent {
			if c.FishName != "" {
				items = append(items, fmt.Sprintf("%.2fkg %s", float64(c.WeightG)/1000, c.FishName))
			}
		}
		if len(items) > 0 {
			parts = append(parts, "最近 "+strings.Join(items, "、"))
		}
	}
	return trimRunes("你查了自己的渔获记录："+strings.Join(parts, "；")+"。", 180)
}

func (r *Runner) intSetting(ctx context.Context, key string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.db.AISetting(ctx, key, strconv.Itoa(def)))
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

func otherNames(agents []*store.AIAgent, self string) string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.Key != self {
			names = append(names, a.Display)
		}
	}
	if len(names) == 0 {
		return "其他助手"
	}
	return strings.Join(names, "、")
}

// isRoundtableStop 判断模型是否表示「这轮我不说」。
// 标记都很短，模型偶尔会带标点或括号，所以先剥壳再按包含判定并限长，
// 避免把一句正常发言误当成沉默。
func isRoundtableStop(s string) bool {
	t := strings.TrimSpace(s)
	t = strings.Trim(t, "[]【】()（）<>《》「」『』。.!！?？,， ")
	if t == "" || len([]rune(t)) > 8 {
		return false
	}
	switch strings.ToLower(t) {
	case "散了", "不语", "跳过", "沉默", "pass", "silent", "skip":
		return true
	}
	return strings.Contains(t, "散了")
}

// rtSpeakerState 记录一次圆桌里「谁说过、谁在听、冷了几轮」。
// 发言权不是按轮次分配，而是每个候选独立判断：想接才接。
type rtSpeakerState struct {
	spoken  map[string]int
	passed  map[string]int
	pending []*store.AIAgent // 刚被点名、应该优先回应的人
	last    string
	streak  int // 连续 AI 发言数
}

func newRTSpeakerState() *rtSpeakerState {
	return &rtSpeakerState{spoken: map[string]int{}, passed: map[string]int{}}
}

func (st *rtSpeakerState) removePending(key string) {
	out := st.pending[:0]
	for _, a := range st.pending {
		if a.Key != key {
			out = append(out, a)
		}
	}
	st.pending = out
}

type rtConfig struct {
	maxTurns  int
	maxStreak int
	chime     float64
	gap       time.Duration
}

// rtMentions 找出一段话里被叫到的参与者，按出现位置排序（显示名优先，其次 key）。
func rtMentions(text string, agents []*store.AIAgent) []*store.AIAgent {
	type hit struct {
		pos int
		a   *store.AIAgent
	}
	hits := make([]hit, 0, len(agents))
	for _, a := range agents {
		pos := -1
		if a.Display != "" {
			pos = strings.Index(text, a.Display)
		}
		// 只认显式的 @key，避免短 key 在英文句子里误命中。
		if pos < 0 && a.Key != "" {
			pos = strings.Index(text, "@"+a.Key)
		}
		if pos >= 0 {
			hits = append(hits, hit{pos, a})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	out := make([]*store.AIAgent, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.a)
	}
	return out
}

// rtChance 是某个 Agent 这次主动开口的概率。
// 活跃度越高越爱接话；已经说过、刚被冷落、连续 AI 发言都会压低概率。
func rtChance(a *store.AIAgent, st *rtSpeakerState, first bool, base float64) float64 {
	if a.Key == st.last {
		return 0
	}
	p := base
	if first {
		p = math.Max(p, 0.85) // 真人刚说完，至少有一个人该接
	}
	p *= 0.4 + float64(a.Talkativeness)/100*1.2
	p /= float64(1 + st.spoken[a.Key])
	p *= math.Pow(0.72, float64(st.streak))
	if st.passed[a.Key] > 0 {
		p *= math.Pow(0.5, float64(st.passed[a.Key]))
	}
	if p > 0.95 {
		p = 0.95
	}
	return p
}

// rtPickSpeaker 选下一个说话人；没有合适的人就返回 nil（自然冷场）。
// 被点名的优先，其次是满足概率的主动接话者；attempted 防止同一轮反复追问同一个人。
func rtPickSpeaker(agents []*store.AIAgent, st *rtSpeakerState, attempted map[string]bool,
	cfg rtConfig, first bool, rng *rand.Rand) *store.AIAgent {
	for len(st.pending) > 0 {
		a := st.pending[0]
		st.pending = st.pending[1:]
		if a.Key == st.last || attempted[a.Key] {
			continue
		}
		return a
	}
	if st.streak >= cfg.maxStreak || len(attempted) >= len(agents) {
		return nil
	}
	cands := make([]*store.AIAgent, 0, len(agents))
	for _, a := range agents {
		if a.ReplyMode != "social" || attempted[a.Key] || a.Key == st.last {
			continue
		}
		cands = append(cands, a)
	}
	rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	for _, a := range cands {
		if rng.Float64() < rtChance(a, st, first, cfg.chime) {
			return a
		}
	}
	return nil
}

// handleRoundtable 是一次真人发言触发的有界圆桌。
// 与旧版「3 人轮流、每人必说」不同：每个候选独立判断要不要开口，
// 沉默是正常结果；被点名、有新信息才说。发言只写 ai_result，绝不回流 ai_task。
func (r *Runner) handleRoundtable(ctx context.Context, t *store.AITask) {
	if r.db.AISetting(ctx, "emergency_stop", "0") == "1" ||
		r.db.AISetting(ctx, "outbound_enabled", "1") != "1" {
		_ = r.db.AIFinishTask(ctx, t.ID, "fallback")
		return
	}
	var keys []string
	if err := json.Unmarshal([]byte(t.Participants), &keys); err != nil || len(keys) < 2 {
		_ = r.db.AIFinishTask(ctx, t.ID, "fallback")
		return
	}
	budget := r.contextBudget(ctx, profileRoundtable)
	replyChars := r.replyChars(ctx, profileRoundtable)
	softChars := r.replySoftChars(ctx, profileRoundtable)
	maxSpeakers := r.intSetting(ctx, "roundtable_max_speakers", 3, 2, 3)
	cfg := rtConfig{
		maxTurns:  r.intSetting(ctx, "roundtable_max_turns", 6, 2, 6),
		maxStreak: r.intSetting(ctx, "roundtable_max_streak", 3, 1, 6),
		chime:     func() float64 { v, _ := r.floatSetting(ctx, "roundtable_chime_chance", 0.55); return v }(),
		gap:       time.Duration(r.intSetting(ctx, "roundtable_gap_ms", 3000, 500, 10000)) * time.Millisecond,
	}
	maxMS := r.intSetting(ctx, "roundtable_max_ms", 20000, 5000, 30000)
	if len(keys) > maxSpeakers {
		keys = keys[:maxSpeakers]
	}

	agents := make([]*store.AIAgent, 0, len(keys))
	for _, k := range keys {
		if a, err := r.db.AIAgentByKey(ctx, k); err == nil && a != nil && a.Enabled && a.Joined && a.ReplyMode != "silent" {
			agents = append(agents, a)
		}
	}
	if len(agents) < 2 {
		_ = r.db.AIFinishTask(ctx, t.ID, "fallback")
		return
	}
	provider, err := r.loadProvider(ctx)
	if err != nil {
		r.log.Warn("圆桌厂商未就绪", "err", err)
		_ = r.db.AIFinishTask(ctx, t.ID, "fallback")
		return
	}

	rtCtx, cancel := context.WithTimeout(ctx, time.Duration(maxMS+5000)*time.Millisecond)
	defer cancel()

	// 每个参与者先读一次角色状态；圆桌里也要知道自己此刻在干嘛。
	actorLines := map[string]string{}
	for _, a := range agents {
		if !a.ActorEnabled {
			continue
		}
		if line, _ := r.actorLine(ctx, a.Key, t.Loc); line != "" {
			actorLines[a.Key] = line
		}
	}
	sessKey := sessionKey(t)
	// 真人原话先落一次，后面的发言都挂在这条下面。
	r.sessions.Append(sessKey, Message{Role: "user", Content: t.Text})

	msgs := []Message{{Role: "user", Content: t.Text}}
	st := newRTSpeakerState()
	st.pending = rtMentions(t.Text, agents)
	attempted := map[string]bool{}
	lastText := ""
	turns := 0
	first := true
	start := time.Now()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for turns < cfg.maxTurns {
		if time.Since(start) > time.Duration(maxMS)*time.Millisecond {
			break
		}
		select {
		case <-rtCtx.Done():
			turns = cfg.maxTurns
			continue
		default:
		}
		if r.db.AISetting(ctx, "emergency_stop", "0") == "1" {
			break
		}
		if newer, _ := r.db.AIHasNewerHumanTask(ctx, t.ID); newer {
			break // 人类优先：有人插话就停
		}
		a := rtPickSpeaker(agents, st, attempted, cfg, first, rng)
		if a == nil {
			break // 没人想接：正常冷场，不硬凑
		}
		system := "你是「" + a.Display + "」。" + trimToTokens(a.Prompt, 400)
		if line := actorLines[a.Key]; line != "" {
			system += "\n\n" + line
		}
		// 每个发言者都知道自己上过鱼；有人明确问渔获时再各自补一份完整记录。
		if a.ActorEnabled {
			if brief := r.catchBriefLine(ctx, a.Key); brief != "" {
				system += "\n\n" + brief
			}
			if catchQuestion(t.Text) {
				if line := r.catchLine(ctx, a.Key); line != "" {
					system += "\n\n" + line
				}
			}
		}
		system += "\n\n这是多人频道，不是轮流发言。你在跟 " + otherNames(agents, a.Key) + " 一起聊天。" +
			"只在你确实有想说的（接住某个具体的点、补充新信息、纠正错误、被点名）时开口；" +
			"没想法就只回复 [不语]，不要为了凑轮次硬说；不要复述、不要总结全场、不要自问自答。" +
			"像钓友闲聊一样，简短一句，尽量不超过 " + strconv.Itoa(softChars) + " 字。"
		callMsgs := make([]Message, 0, budget.HistoryMessages+3)
		callMsgs = append(callMsgs, Message{Role: "system", Content: system})
		callMsgs = append(callMsgs, roundtableWindow(msgs, budget.HistoryMessages)...)
		resp, err := provider.Chat(rtCtx, ChatRequest{
			Messages:  callMsgs,
			Thinking:  ThinkingConfig{Enabled: false},
			MaxTokens: replyChars + 40,
		})
		if err != nil {
			r.log.Warn("圆桌发言失败", "agent", a.Key, "turn", turns, "err", err)
			break
		}
		text := trimRunes(sanitizeReply(stripSpeakerPrefix(strings.TrimSpace(resp.Text))), replyChars)
		// AI 自己决定闭嘴：不投递、不占条数，换下一个人判断。
		if text == "" || isRoundtableStop(text) || (lastText != "" && similarText(text, lastText)) {
			st.passed[a.Key]++
			attempted[a.Key] = true
			st.removePending(a.Key)
			continue
		}
		// Agent 可能刚被后台逻辑删除：不再替它发言。
		if cur, err := r.db.AIAgentByKey(ctx, a.Key); err != nil || cur == nil {
			attempted[a.Key] = true
			continue
		}
		if err := r.db.AIAddChannelResult(ctx, t.ID, a.ContactUID, text); err != nil {
			r.log.Warn("圆桌发言写入失败", "agent", a.Key, "err", err)
			break
		}
		said := "【" + a.Display + "】" + text
		msgs = append(msgs, Message{Role: "assistant", Content: said})
		r.sessions.Append(sessKey, Message{Role: "assistant", Content: said})
		st.spoken[a.Key]++
		st.last = a.Key
		st.streak++
		st.removePending(a.Key)
		// 它的话里如果点了别人，下一句优先给被点名的人。
		for _, m := range rtMentions(text, agents) {
			if m.Key != a.Key {
				st.pending = append(st.pending, m)
			}
		}
		attempted = map[string]bool{}
		lastText = text
		turns++
		first = false
		gap := cfg.gap + time.Duration(rng.Intn(400))*time.Millisecond
		select {
		case <-rtCtx.Done():
			turns = cfg.maxTurns
		case <-time.After(gap):
		}
	}
	_ = r.db.AIFinishTask(ctx, t.ID, "done")
}

// similarText 判断两条发言是不是明显复读（相同或前 12 字相同）。
func similarText(a, b string) bool {
	if a == b {
		return true
	}
	ra, rb := []rune(a), []rune(b)
	n := 12
	if len(ra) < n {
		n = len(ra)
	}
	if len(rb) < n {
		n = len(rb)
	}
	if n < 8 {
		return false
	}
	return string(ra[:n]) == string(rb[:n])
}

// remoteTool 把只读工具请求交给 gameserver，轮询取回结果。
// aibridge 是独立进程，天气、实时点位这类数据只有 gameserver 算得出来。
func (r *Runner) remoteTool(ctx context.Context, taskID, uid int64, tool, args string) string {
	jobID, err := r.db.AIEnqueueToolJob(ctx, taskID, tool, args, uid)
	if err != nil {
		r.log.Warn("AI 工具入队失败", "tool", tool, "err", err)
		return ""
	}
	deadline := time.Now().Add(aiToolWaitBudget)
	for {
		if out, ok, err := r.db.AIGetToolResult(ctx, jobID); err == nil && ok {
			return out
		}
		if time.Now().After(deadline) {
			return "（本地查鱼超时）"
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(120 * time.Millisecond):
		}
	}
}

// loadProvider 每次重新读取配置，后台改厂商/模型/Key 无需重启进程。
func (r *Runner) loadProvider(ctx context.Context) (Provider, error) {
	return LoadProviderFromDB(ctx, r.db, r.masterKey,
		time.Duration(r.cfg.RequestTimeoutSec*float64(time.Second)))
}
