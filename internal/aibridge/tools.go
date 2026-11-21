package aibridge

// 工具注册表：一份声明式目录，统一校验、统一入口、统一错误口径。
//
// 为什么不再用「名字 switch」：旧实现的本地工具在 registry、远程工具在 runner
// 的 switch 里各认一次名字，新增工具要改两处，未知/禁用/无权也分不出来。
// 现在每个工具只在 newToolEntries 里声明一次（名称、参数、类别、路由、处理器），
// 启动时校验 Kind×Route、重名、缺 handler；运行期只走 Dispatch。
//
// 边界（T-104）：
//   - 模型只看到 Name/Description/Parameters 三个公开字段；
//   - 本地工具与预取使用同一份授权和错误口径；
//   - fish_query / map_vibe 是 gs_read，走 ai_tool_job，不进写队列；
//   - actor_target_suggest 是当前唯一写工具，只投递命令，不由 BR 应用；
//   - 未知 / 禁用 / 无权 / 参数不合法返回可区分的错误，不返回空串。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"rf4server/internal/aiconfig"
)

// ToolKind / ToolRoute 是启动期校验的两个维度。
type ToolKind string

const (
	ToolKindRead  ToolKind = "read"
	ToolKindWrite ToolKind = "write"
)

type ToolRoute string

const (
	ToolRouteLocal   ToolRoute = "local"
	ToolRouteGSRead  ToolRoute = "gs_read"
	ToolRouteGSWrite ToolRoute = "gs_write"
)

// ToolResult 是 Dispatch 的统一结果。status 取值：
// ok（读到数据）/ accepted（写已受理）/ denied（无权/禁用）/ unavailable（暂时不可用）。
type ToolResult struct {
	Status    string `json:"status"`
	Data      string `json:"data,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	SampledAt int64  `json:"sampled_at,omitempty"`
	// Applied 只在写回执里出现：受理时必须是 false（等待游戏服确认）。
	Applied   *bool `json:"applied,omitempty"`
	shopQuote *shopQuote
}

// ToolError 带稳定 reason code，便于日志、测试和模型侧区分失败原因。
type ToolError struct {
	Code string
	Msg  string
}

func (e *ToolError) Error() string { return e.Msg }

// Reason 返回稳定代码（未知工具/未授权/参数错/已暂停/队列满）。
func (e *ToolError) Reason() string { return e.Code }

var (
	ErrToolUnknown     = &ToolError{Code: "unknown_tool", Msg: "工具不存在"}
	ErrToolNotAllowed  = &ToolError{Code: "not_allowed", Msg: "工具未授权"}
	ErrToolInvalidArgs = &ToolError{Code: "invalid_args", Msg: "工具参数不合法"}
	ErrToolStopped     = &ToolError{Code: "stopped", Msg: "助手已暂停，工具不可用"}
	ErrToolUnavailable = &ToolError{Code: "unavailable", Msg: "工具暂时不可用"}
)

// ToolEnv 是受信调用环境：身份、代次、允许集合与两个跨进程出口都由 Runner 注入，
// 模型无法影响其中任何字段。
type ToolEnv struct {
	AgentKey         string
	TaskID           int64
	CallID           string
	Generation       int64
	Revision         int64
	TaskText         string
	TaskLoc          string
	Deadline         time.Time
	Allowed          map[string]bool
	observeShopQuote func(*shopQuote)
	// Active 是本次任务的原意图判定（急停 + 原代次）。
	Active func(context.Context) bool
	// Recheck 在每次执行前重新读取当前授权。任务开始时的允许集合只决定
	// 模型看到哪些工具；管理员在同一任务中途撤权后，这里必须立刻失败关闭。
	Recheck func(ctx context.Context, name string) (bool, error)
	// RemoteRead 把 gs_read 工具交给 gameserver 执行并等待结果。
	RemoteRead func(ctx context.Context, name, args string) (string, error)
	// TargetSuggest 把写工具交给 Runner 生成 request_id 并入队；
	// registry 不接触数据库，也不自行应用任何状态。
	TargetSuggest func(ctx context.Context, fish, loc, callID string) (ToolResult, error)
}

// aiToolLocalExecHookForTest 在本地工具处理器执行前触发（仅测试）。
// 撤权用例用它证明「被拒的调用没有碰到本地资料」。
var aiToolLocalExecHookForTest func(name string)

// toolArgs 是解析后的参数：只保字符串值，键名来自各工具声明。
type toolArgs map[string]string

// toolHandler 只服务 local 路由。
type toolHandler func(ctx context.Context, k *Knowledge, query string) (ToolResult, error)

func textTool(fn func(context.Context, *Knowledge, string) (string, error)) toolHandler {
	return func(ctx context.Context, k *Knowledge, query string) (ToolResult, error) {
		text, err := fn(ctx, k, query)
		return ToolResult{Data: text}, err
	}
}

// registryEntry 是一个工具的完整声明。
type registryEntry struct {
	spec    ToolSpec
	handler toolHandler
	// required 是必须非空的参数名；空表示全部可选（旧行为：没有查询词就不查）。
	required []string
}

// Registry 按名字注册工具。
type Registry struct {
	knowledge *Knowledge
	entries   map[string]registryEntry
	order     []string
}

// NewRegistry 注册全部工具并在启动时校验。
//
// 不合法的 Kind×Route、重名、缺 handler、缺超时/截断上限都会直接返回错误；
// 调用方（cmd/aibridge）必须把它当成启动失败，而不是降级成空注册表。
func NewRegistry(k *Knowledge) (*Registry, error) {
	r := &Registry{knowledge: k, entries: make(map[string]registryEntry)}
	for _, e := range newToolEntries() {
		if err := r.add(e); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) add(e registryEntry) error {
	name := strings.TrimSpace(e.spec.Name)
	if name == "" {
		return fmt.Errorf("aibridge: 工具名为空")
	}
	if _, ok := r.entries[name]; ok {
		return fmt.Errorf("aibridge: 工具重名 %q", name)
	}
	if !aiconfig.KnownTool(name) {
		return fmt.Errorf("aibridge: 工具 %q 不在能力目录里", name)
	}
	if e.spec.HandlerKey == "" {
		return fmt.Errorf("aibridge: 工具 %q 缺 handler key", name)
	}
	if e.spec.TimeoutMS <= 0 || e.spec.MaxResultBytes <= 0 {
		return fmt.Errorf("aibridge: 工具 %q 缺超时或截断上限", name)
	}
	if !e.spec.AgentScoped {
		return fmt.Errorf("aibridge: 工具 %q 必须是角色作用域的公开工具", name)
	}
	if len(e.spec.Parameters) == 0 || !json.Valid(e.spec.Parameters) {
		return fmt.Errorf("aibridge: 工具 %q 参数 schema 不是合法 JSON", name)
	}
	switch e.spec.Kind {
	case ToolKindRead:
		switch e.spec.Route {
		case ToolRouteLocal:
			if e.handler == nil {
				return fmt.Errorf("aibridge: 工具 %q 是本地读但缺处理器", name)
			}
		case ToolRouteGSRead:
			if e.handler != nil {
				return fmt.Errorf("aibridge: 工具 %q 是远程读但声明了本地处理器", name)
			}
		default:
			return fmt.Errorf("aibridge: 工具 %q 的 Kind×Route 不合法（%s×%s）",
				name, e.spec.Kind, e.spec.Route)
		}
	case ToolKindWrite:
		if e.spec.Route != ToolRouteGSWrite {
			return fmt.Errorf("aibridge: 工具 %q 的 Kind×Route 不合法（%s×%s）",
				name, e.spec.Kind, e.spec.Route)
		}
		if e.handler != nil {
			return fmt.Errorf("aibridge: 写工具 %q 不能有本地处理器", name)
		}
	default:
		return fmt.Errorf("aibridge: 工具 %q 的 Kind 不合法 %q", name, e.spec.Kind)
	}
	r.entries[name] = e
	r.order = append(r.order, name)
	return nil
}

// newToolEntries 是全部工具的唯一声明点。
func newToolEntries() []registryEntry {
	queryParams := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"中文名或关键词"}}}`)
	return []registryEntry{
		localNotesEntry(aiconfig.ToolFishingNotes, aiconfig.KnowledgeFishing, "查找本地垂钓心得及来源。", queryParams),
		localNotesEntry(aiconfig.ToolShopNotes, aiconfig.KnowledgeShop, "查找本地装备与商店心得及来源。", queryParams),
		localNotesEntry(aiconfig.ToolCraftNotes, aiconfig.KnowledgeCraft, "查找本地制作心得及来源。", queryParams),
		{
			spec: ToolSpec{
				Name: aiconfig.ToolSpeciesLookup, Description: "查鱼种出现在哪些水域。",
				Parameters: queryParams, Kind: ToolKindRead, Route: ToolRouteLocal,
				AgentScoped: true, HandlerKey: aiconfig.ToolSpeciesLookup,
				TimeoutMS: 3000, MaxResultBytes: 800,
			},
			handler: textTool(toolSpecies),
		},
		{
			spec: ToolSpec{
				Name: aiconfig.ToolBaitPref, Description: "查鱼种与饵料的对应关系。",
				Parameters: queryParams, Kind: ToolKindRead, Route: ToolRouteLocal,
				AgentScoped: true, HandlerKey: aiconfig.ToolBaitPref,
				TimeoutMS: 3000, MaxResultBytes: 800,
			},
			handler: textTool(toolBaitPref),
		},
		{
			spec: ToolSpec{
				Name: aiconfig.ToolSpotLookup, Description: "查水域点位与鱼种。",
				Parameters: queryParams, Kind: ToolKindRead, Route: ToolRouteLocal,
				AgentScoped: true, HandlerKey: aiconfig.ToolSpotLookup,
				TimeoutMS: 3000, MaxResultBytes: 800,
			},
			handler: textTool(toolSpot),
		},
		{
			spec: ToolSpec{
				Name: aiconfig.ToolItemLookup, Description: "按正式名、社区简称、型号或 ID 查装备与物品。",
				Parameters: queryParams, Kind: ToolKindRead, Route: ToolRouteLocal,
				AgentScoped: true, HandlerKey: aiconfig.ToolItemLookup,
				TimeoutMS: 3000, MaxResultBytes: 800,
			},
			handler: textTool(toolItem),
		},
		{
			spec: ToolSpec{
				Name: aiconfig.ToolShopPrice, Description: "查装备或道具的静态参考价，实时售价与库存以游戏商店为准。",
				Parameters: queryParams, Kind: ToolKindRead, Route: ToolRouteLocal,
				AgentScoped: true, HandlerKey: aiconfig.ToolShopPrice,
				TimeoutMS: 3000, MaxResultBytes: 800,
			},
			handler: toolShopPrice,
		},
		{
			spec: ToolSpec{
				Name: aiconfig.ToolRecipeLookup, Description: "查制作配方与产物。",
				Parameters: queryParams, Kind: ToolKindRead, Route: ToolRouteLocal,
				AgentScoped: true, HandlerKey: aiconfig.ToolRecipeLookup,
				TimeoutMS: 3000, MaxResultBytes: 800,
			},
			handler: textTool(toolRecipe),
		},
		{
			spec: ToolSpec{
				Name:         aiconfig.ToolFishQuery,
				Description:  "按鱼名查询推荐水域、饵、钓法与理论条每小时。结果来自服务端产线权重链，只读。",
				Parameters:   json.RawMessage(`{"type":"object","properties":{"fish":{"type":"string","description":"鱼名，例如 河鲈"}}}`),
				ResultSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
				Kind:         ToolKindRead, Route: ToolRouteGSRead, AgentScoped: true,
				HandlerKey: aiconfig.ToolFishQuery, TimeoutMS: 4000, MaxResultBytes: 4000,
			},
		},
		{
			spec: ToolSpec{
				Name:         aiconfig.ToolMapVibe,
				Description:  "看某张图此刻的天气和这片水域有哪些鱼。只有鱼种，不含饵料和点位，适合闲聊时顺口一提。",
				Parameters:   json.RawMessage(`{"type":"object","properties":{"loc":{"type":"string","description":"地图中文名或 level key，留空表示当前所在地图"}}}`),
				ResultSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
				Kind:         ToolKindRead, Route: ToolRouteGSRead, AgentScoped: true,
				HandlerKey: aiconfig.ToolMapVibe, TimeoutMS: 4000, MaxResultBytes: 4000,
			},
		},
		{
			spec: ToolSpec{
				Name: aiconfig.ToolActorTargetSuggest,
				Description: "玩家明显在安利某条鱼时，提交一条换鱼/换图建议。只是建议，由游戏服决定是否应用；" +
					"提交后只能告诉玩家「已提交，等待游戏服务确认」，不能说已经换好。",
				Parameters: json.RawMessage(`{"type":"object","properties":{` +
					`"fish":{"type":"string","description":"玩家提到的鱼名，例如 河鲈"},` +
					`"loc":{"type":"string","description":"可选，想换到的地图中文名或 level key"}},` +
					`"required":["fish"]}`),
				ResultSchema: json.RawMessage(`{"type":"object","properties":{` +
					`"status":{"type":"string"},"request_id":{"type":"string"},"applied":{"type":"boolean"}}}`),
				Kind: ToolKindWrite, Route: ToolRouteGSWrite, AgentScoped: true,
				HandlerKey: aiconfig.ToolActorTargetSuggest, TimeoutMS: 2000, MaxResultBytes: 200,
			},
			required: []string{"fish"},
		},
	}
}

// Specs 按允许集合生成模型可见清单；顺序跟允许集合一致，未知名字跳过。
func (r *Registry) Specs(allowed []string) []ToolSpec {
	if r == nil {
		return nil
	}
	out := make([]ToolSpec, 0, len(allowed))
	for _, name := range allowed {
		if e, ok := r.entries[name]; ok {
			out = append(out, e.spec)
		}
	}
	return out
}

// Dispatch 是唯一的执行入口。检查顺序：存在 → 能力 → 参数 → 急停 → 路由。
func (r *Registry) Dispatch(ctx context.Context, name, rawArgs string, env ToolEnv) (ToolResult, error) {
	if r == nil {
		return ToolResult{Status: "denied", Reason: ErrToolUnknown.Code}, ErrToolUnknown
	}
	entry, ok := r.entries[name]
	if !ok {
		return ToolResult{Status: "denied", Reason: ErrToolUnknown.Code}, ErrToolUnknown
	}
	if !env.Allowed[name] {
		return ToolResult{Status: "denied", Reason: ErrToolNotAllowed.Code}, ErrToolNotAllowed
	}
	// 能力检查紧跟存在检查：允许集合是任务开始时的快照，撤权后必须按
	// 当前配置再判一次，不能让同一任务的剩余轮次继续用旧授权。
	if env.Recheck != nil {
		allowed, err := env.Recheck(ctx, name)
		if err != nil {
			return ToolResult{Status: "unavailable", Reason: ErrToolUnavailable.Code},
				fmt.Errorf("%w: 授权读取失败", ErrToolUnavailable)
		}
		if !allowed {
			return ToolResult{Status: "denied", Reason: ErrToolNotAllowed.Code}, ErrToolNotAllowed
		}
	}
	args, err := parseToolArgs(rawArgs)
	if err != nil {
		return ToolResult{Status: "denied", Reason: ErrToolInvalidArgs.Code}, err
	}
	for _, key := range entry.required {
		if strings.TrimSpace(args[key]) == "" {
			return ToolResult{Status: "denied", Reason: ErrToolInvalidArgs.Code},
				fmt.Errorf("%w: 缺少 %s", ErrToolInvalidArgs, key)
		}
	}
	if env.Active != nil && !env.Active(ctx) {
		return ToolResult{Status: "denied", Reason: ErrToolStopped.Code}, ErrToolStopped
	}
	if !env.Deadline.IsZero() && time.Now().After(env.Deadline) &&
		entry.spec.Route != ToolRouteLocal {
		return ToolResult{Status: "unavailable", Reason: ErrToolUnavailable.Code},
			fmt.Errorf("%w: 已到任务截止时间", ErrToolUnavailable)
	}
	switch entry.spec.Route {
	case ToolRouteLocal:
		q := queryOf(args)
		if q == "" {
			return ToolResult{Status: "ok", SampledAt: time.Now().Unix()}, nil
		}
		if aiToolLocalExecHookForTest != nil {
			aiToolLocalExecHookForTest(name)
		}
		localCtx, cancel := context.WithTimeout(ctx, time.Duration(entry.spec.TimeoutMS)*time.Millisecond)
		defer cancel()
		out, err := entry.handler(localCtx, r.knowledge, q)
		if err != nil {
			return ToolResult{Status: "unavailable", Reason: ErrToolUnavailable.Code},
				fmt.Errorf("%w: %v", ErrToolUnavailable, err)
		}
		out.Status = "ok"
		if out.Data == "" {
			out.Status = "not_found"
		}
		out.Data = trimRunes(out.Data, entry.spec.MaxResultBytes)
		out.SampledAt = time.Now().Unix()
		return out, nil
	case ToolRouteGSRead:
		if env.RemoteRead == nil {
			return ToolResult{Status: "unavailable", Reason: ErrToolUnavailable.Code}, ErrToolUnavailable
		}
		arg := remoteReadArg(name, args, env)
		if arg == "" {
			return ToolResult{Status: "ok", SampledAt: time.Now().Unix()}, nil
		}
		out, err := env.RemoteRead(ctx, name, arg)
		if err != nil {
			return ToolResult{Status: "unavailable", Reason: ErrToolUnavailable.Code}, err
		}
		return ToolResult{
			Status: "ok", Data: trimRunes(out, entry.spec.MaxResultBytes),
			SampledAt: time.Now().Unix(),
		}, nil
	case ToolRouteGSWrite:
		if env.TargetSuggest == nil {
			return ToolResult{Status: "unavailable", Reason: ErrToolUnavailable.Code}, ErrToolUnavailable
		}
		fish := strings.TrimSpace(args["fish"])
		loc := strings.TrimSpace(args["loc"])
		res, err := env.TargetSuggest(ctx, fish, loc, env.CallID)
		if err != nil {
			return ToolResult{Status: "unavailable", Reason: ErrToolUnavailable.Code}, err
		}
		if res.Status == "" {
			res.Status = "accepted"
		}
		return res, nil
	default:
		return ToolResult{Status: "denied", Reason: ErrToolUnknown.Code}, ErrToolUnknown
	}
}

// remoteReadArg 保留旧参数口径：fish_query 传鱼名，map_vibe 传地图；
// 留空时退回入队时的文本/地图，和改造前完全一致。
func remoteReadArg(name string, args toolArgs, env ToolEnv) string {
	switch name {
	case aiconfig.ToolFishQuery:
		if fish := strings.TrimSpace(args["fish"]); fish != "" {
			return fish
		}
		return firstFishKeyword(env.TaskText)
	case aiconfig.ToolMapVibe:
		if loc := strings.TrimSpace(args["loc"]); loc != "" {
			return loc
		}
		return strings.TrimSpace(env.TaskLoc)
	}
	return ""
}

// queryOf 兼容旧别名：本地六工具既接受 query，也接受 fish。
func queryOf(args toolArgs) string {
	if q := strings.TrimSpace(args["query"]); q != "" {
		return q
	}
	return strings.TrimSpace(args["fish"])
}

// parseToolArgs 只接受一个 JSON 对象，值必须是字符串，未知键直接拒绝。
// 空串按 {} 处理（旧实现允许无参数调用）。
func parseToolArgs(raw string) (toolArgs, error) {
	out := toolArgs{}
	text := strings.TrimSpace(raw)
	if text == "" || text == "null" {
		return out, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrToolInvalidArgs, err)
	}
	for key, val := range m {
		if key == "" || len(key) > 64 {
			return nil, fmt.Errorf("%w: 参数名不合法", ErrToolInvalidArgs)
		}
		var s string
		if err := json.Unmarshal(val, &s); err != nil {
			return nil, fmt.Errorf("%w: 参数 %s 必须是字符串", ErrToolInvalidArgs, key)
		}
		if utf8.RuneCountInString(s) > 200 {
			return nil, fmt.Errorf("%w: 参数 %s 过长", ErrToolInvalidArgs, key)
		}
		out[key] = s
	}
	return out, nil
}

// Prefetch runs only previously authorized local tools, through the same
// permission check as a model-initiated call.
func (r *Registry) Prefetch(ctx context.Context, names []string, query string, env ToolEnv) string {
	if r == nil || strings.TrimSpace(query) == "" {
		return ""
	}
	args, _ := json.Marshal(toolArgs{"query": trimRunes(query, 240)})
	var b strings.Builder
	for _, name := range names {
		e, ok := r.entries[name]
		if !ok || e.spec.Route != ToolRouteLocal {
			continue
		}
		res, err := r.Dispatch(ctx, name, string(args), env)
		if err == nil && res.shopQuote != nil && env.observeShopQuote != nil {
			env.observeShopQuote(res.shopQuote)
		}
		if err == nil && res.Data != "" {
			fmt.Fprintf(&b, "[%s]\n%s\n", name, res.Data)
		}
		if b.Len() >= 1600 {
			break
		}
	}
	return trimRunes(b.String(), 1600)
}

// keywordsOf 把连续中文切成 2 字滑窗：整句 "河鲈在哪钓" 直接 LIKE 命中不了
// "河鲈"，必须切成 "河鲈"/"鲈在"/"在哪" 这类片段。
func keywordsOf(query string) []string {
	matches := hanWords.FindAllString(query, 3)
	out := make([]string, 0, 3)
	seen := map[string]bool{}
	add := func(s string) {
		if len([]rune(s)) < 2 || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, "%"+s+"%")
	}
	for _, m := range matches {
		r := []rune(m)
		if len(r) <= 4 {
			add(m)
		}
		for i := 0; i+2 <= len(r) && len(out) < 3; i++ {
			add(string(r[i : i+2]))
		}
		if len(out) >= 3 {
			break
		}
	}
	return out
}

func scanPairs(ctx context.Context, k *Knowledge, query string, args []any) ([][2]string, error) {
	if k == nil || k.static == nil {
		return nil, errors.New("静态资料库未挂载")
	}
	rows, err := k.static.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}

func searchPairs(ctx context.Context, k *Knowledge, query, statement, format string, repeats int) (string, error) {
	var out strings.Builder
	seen := map[[2]string]bool{}
	for _, kw := range keywordsOf(query) {
		args := make([]any, repeats)
		for i := range args {
			args[i] = kw
		}
		rows, err := scanPairs(ctx, k, statement, args)
		if err != nil {
			return "", err
		}
		for _, pair := range rows {
			if !seen[pair] {
				fmt.Fprintf(&out, format, pair[0], pair[1])
				seen[pair] = true
			}
		}
	}
	return out.String(), nil
}

func toolSpecies(ctx context.Context, k *Knowledge, query string) (string, error) {
	return searchPairs(ctx, k, query,
		`SELECT DISTINCT name_zh, loc FROM loc_fish WHERE name_zh LIKE ? ORDER BY name_zh, loc LIMIT 4`,
		"鱼种:%s 水域:%s\n", 1)
}

func toolBaitPref(ctx context.Context, k *Knowledge, query string) (string, error) {
	return searchPairs(ctx, k, query,
		`SELECT DISTINCT COALESCE(lf.name_zh, p.fish_key), p.bait_zh
		 FROM fish_bait_pref p LEFT JOIN loc_fish lf ON lf.fish_key=p.fish_key
		 WHERE lf.name_zh LIKE ? OR p.bait_zh LIKE ?
		 ORDER BY 1, 2 LIMIT 4`, "鱼:%s 饵:%s\n", 2)
}

func toolSpot(ctx context.Context, k *Knowledge, query string) (string, error) {
	return searchPairs(ctx, k, query,
		`SELECT DISTINCT s.loc, s.spot FROM spot_fish s
		 LEFT JOIN loc_fish lf ON lf.fish_key=s.fish_key AND lf.loc=s.loc
		 WHERE s.fish_key LIKE ? OR s.loc LIKE ? OR lf.name_zh LIKE ?
		 ORDER BY 1, 2 LIMIT 4`, "水域:%s 点位:%s\n", 3)
}

// firstFishKeyword 从自然语言里取第一个中文 2 字片段作为鱼名，
// 交给 gameserver 的 fishQueryReply 解析（它期望「鱼名 [地图]」）。
func firstFishKeyword(query string) string {
	kws := keywordsOf(query)
	if len(kws) == 0 {
		return ""
	}
	return strings.Trim(kws[0], "%")
}
