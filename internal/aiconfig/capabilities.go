package aiconfig

// 角色能力的唯一声明点：能力目录、工具投影、版本化配置与受信事件合同都在这里。
//
// 为什么集中在一个文件：后台保存、桥接注册表、游戏服执行三处都要回答同一个
// 问题——「这个角色此刻允许做什么」。三份清单一旦分开，就会漂移成
// 「页面能勾、模型能调、GS 不认」的三套口径。本包只做纯函数与校验，
// 不读写数据库、不导入 store / admin / aibridge。
//
// 边界（T-104 / DEV-111 A）：
//   - 能力 ID 由服务端映射，模型自称有权不生效；
//   - 目录只收有调用点的资料授权与角色行为；
//   - 事件类能力不新增模型可调用的「任意发言 / 任意发鱼」工具；
//   - 缺配置 = 只保留基础私聊（无工具）；显式空 = 全部拒绝；
//     非法 / 旧代次 / 不可验证 = 失败关闭（fail closed），不猜一个默认值；
//   - 旧 tools_json 只在没有新记录时作为只读子集的降级快照，永不授予写或事件。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// CapabilitySchemaVersion 是能力配置的存储版本。改结构必须同时给读侧迁移口径。
const CapabilitySchemaVersion = 1

// 能力 ID。只增不删；删 ID 等于删历史记录里的语义。
const (
	CapabilityGuideHelp        = "guide_help" // 攻略帮助
	CapabilityFishingKnowledge = "fishing_knowledge"
	CapabilityShopKnowledge    = "shop_knowledge"
	CapabilityCraftKnowledge   = "craft_knowledge"
	CapabilityOnlineSearch     = "online_search"
	CapabilityReplyStar        = "reply_star"            // 回应玩家星蓝
	CapabilityJoinRoundtable   = "join_roundtable"       // 参加多人对话
	CapabilityVirtualFishing   = "virtual_fishing"       // 虚拟垂钓
	CapabilityBroadcastCatch   = "broadcast_catch"       // 播报自己渔获
	CapabilityIdleChat         = "idle_chat"             // 闲时搭话
	CapabilityAcceptTarget     = "accept_target_suggest" // 接受目标建议
)

// 只读工具名。顺序即界面顺序，也是投影与比对的稳定顺序。
const (
	ToolFishQuery     = "fish_query"
	ToolSpeciesLookup = "species_lookup"
	ToolBaitPref      = "bait_pref"
	ToolSpotLookup    = "spot_lookup"
	ToolItemLookup    = "item_lookup"
	ToolShopPrice     = "shop_price"
	ToolRecipeLookup  = "recipe_lookup"
	ToolMapVibe       = "map_vibe"
	ToolFishingNotes  = "local_fishing_notes"
	ToolShopNotes     = "local_shop_notes"
	ToolCraftNotes    = "local_craft_notes"
)

// 写工具名。当前写集合只有这一个（DEV-106 最小写集）。
const ToolActorTargetSuggest = "actor_target_suggest"

// CapabilityTool 是工具在能力目录里的公开条目：名称、中文标签、说明与路由。
// 真正的执行实现分别在 aibridge（本地 / 写）与 gameserver（GS 只读），
// 这里只拥有「叫什么、给谁看、属于哪一类」。
type CapabilityTool struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Desc        string `json:"desc"`
	Kind        string `json:"kind"`  // read | write
	Route       string `json:"route"` // local | gs_read | gs_write
	ReadOnly    bool   `json:"read_only"`
	AgentPublic bool   `json:"agent_public"`
}

// Capability 是一条可授权能力。
//
// 能力与工具的对应关系由服务端固定，页面只提交能力 ID。
type Capability struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Desc       string   `json:"desc"`
	ReadTools  []string `json:"read_tools,omitempty"`
	WriteTools []string `json:"write_tools,omitempty"`
	Group      string   `json:"group,omitempty"`
	Legacy     bool     `json:"legacy,omitempty"`
	// Event 是这条能力对应的事件类别；空表示不参与事件。
	Event string `json:"event,omitempty"`
	// RequiresRoundtable 表示升级为多人对话需要同时具备「参加多人对话」。
	RequiresRoundtable bool `json:"requires_roundtable,omitempty"`
}

// 受信事件类别。由服务端入口填写，禁止从自然语言或 HTTP body 解析。
const (
	EventReplyStar      = "reply_star"      // 玩家上星/上蓝后的系统频道回应
	EventRoundtable     = "roundtable"      // 系统/游戏服发起的圆桌
	EventVirtualFishing = "virtual_fishing" // 虚拟钓鱼节拍
	EventBroadcastCatch = "broadcast_catch" // 自己已提交渔获后的单人频道播报
	EventIdleChat       = "idle_chat"       // 频道安静时自己找个话头
)

// capabilityCatalog 是唯一能力目录，顺序即界面顺序。
var capabilityCatalog = []Capability{
	{
		ID:    CapabilityGuideHelp,
		Label: "旧攻略授权",
		Desc:  "保留该角色原有的只读工具。改用独立资料授权时，请取消此项。",
		ReadTools: []string{
			ToolSpeciesLookup, ToolBaitPref, ToolSpotLookup, ToolItemLookup,
			ToolShopPrice, ToolRecipeLookup, ToolFishQuery, ToolMapVibe,
		},
		Group: "knowledge", Legacy: true,
	},
	{ID: CapabilityFishingKnowledge, Label: "垂钓资料", Desc: "鱼种、饵料、钓点、游戏鱼情和垂钓心得。",
		Group: "knowledge", ReadTools: []string{ToolSpeciesLookup, ToolBaitPref, ToolSpotLookup, ToolFishQuery, ToolMapVibe, ToolFishingNotes}},
	{ID: CapabilityShopKnowledge, Label: "装备与商店", Desc: "装备、道具、静态参考价和商店心得。",
		Group: "knowledge", ReadTools: []string{ToolItemLookup, ToolShopPrice, ToolShopNotes}},
	{ID: CapabilityCraftKnowledge, Label: "制作配方", Desc: "配方、产物和制作心得。",
		Group: "knowledge", ReadTools: []string{ToolRecipeLookup, ToolCraftNotes}},
	{ID: CapabilityOnlineSearch, Label: "联网搜索", Desc: "启用全局搜索且本地未命中时，可将玩家的问题发送给搜索服务。",
		Group: "knowledge"},
	{
		ID:    CapabilityReplyStar,
		Label: "回应玩家星蓝",
		Desc:  "玩家自己上星或上蓝时，允许在系统频道接一句。",
		Event: EventReplyStar,
	},
	{
		ID:    CapabilityJoinRoundtable,
		Label: "参加多人对话",
		Desc:  "允许加入游戏服或桥接发起的多人对话。",
		Event: EventRoundtable,
	},
	{
		ID:    CapabilityVirtualFishing,
		Label: "虚拟垂钓",
		Desc:  "允许按游戏服原有节拍进行虚拟钓鱼。",
		Event: EventVirtualFishing,
	},
	{
		ID:                 CapabilityBroadcastCatch,
		Label:              "播报自己渔获",
		Desc:               "自己已提交渔获后，允许在单人频道报一句；升级为多人对话还需「参加多人对话」。",
		Event:              EventBroadcastCatch,
		RequiresRoundtable: true,
	},
	{
		ID:    CapabilityIdleChat,
		Label: "闲时搭话",
		Desc:  "没鱼、频道里也没人说话时，允许自己找个话头。",
		Event: EventIdleChat,
	},
	{
		ID:         CapabilityAcceptTarget,
		Label:      "接受目标建议",
		Desc:       "允许模型通过目标建议工具提交换鱼/换图建议，由游戏服在事务内决定是否应用。",
		WriteTools: []string{ToolActorTargetSuggest},
	},
}

// capabilityTools 是模型可见工具的公开定义，顺序与目录一致。
var capabilityTools = []CapabilityTool{
	{Name: ToolFishQuery, Label: "查鱼", Desc: "查某鱼在这张图用什么饵、什么点位（走游戏内实时数据）", Kind: "read", Route: "gs_read", ReadOnly: true, AgentPublic: true},
	{Name: ToolSpeciesLookup, Label: "鱼种资料", Desc: "按名字查鱼种的中文名与出没水域", Kind: "read", Route: "local", ReadOnly: true, AgentPublic: true},
	{Name: ToolBaitPref, Label: "饵料偏好", Desc: "查某种鱼常吃的饵", Kind: "read", Route: "local", ReadOnly: true, AgentPublic: true},
	{Name: ToolSpotLookup, Label: "点位", Desc: "查某张图的钓点坐标", Kind: "read", Route: "local", ReadOnly: true, AgentPublic: true},
	{Name: ToolItemLookup, Label: "物品资料", Desc: "查装备、道具的基本资料", Kind: "read", Route: "local", ReadOnly: true, AgentPublic: true},
	{Name: ToolShopPrice, Label: "商店参考价", Desc: "查静态参考价，实时售价与库存以游戏商店为准", Kind: "read", Route: "local", ReadOnly: true, AgentPublic: true},
	{Name: ToolRecipeLookup, Label: "配方", Desc: "查制作配方", Kind: "read", Route: "local", ReadOnly: true, AgentPublic: true},
	{Name: ToolMapVibe, Label: "陪玩鱼情", Desc: "当前地图 + 此刻天气 + 本图鱼种（不含饵料，适合角色聊天）", Kind: "read", Route: "gs_read", ReadOnly: true, AgentPublic: true},
	{Name: ToolFishingNotes, Label: "本地垂钓心得", Desc: "检索管理员维护的垂钓资料", Kind: "read", Route: "local", ReadOnly: true, AgentPublic: true},
	{Name: ToolShopNotes, Label: "本地装备心得", Desc: "检索管理员维护的装备与商店资料", Kind: "read", Route: "local", ReadOnly: true, AgentPublic: true},
	{Name: ToolCraftNotes, Label: "本地制作心得", Desc: "检索管理员维护的制作资料", Kind: "read", Route: "local", ReadOnly: true, AgentPublic: true},
	{Name: ToolActorTargetSuggest, Label: "换目标建议", Desc: "玩家明显在安利某条鱼时，提交一条换鱼/换图建议；由游戏服决定是否应用。", Kind: "write", Route: "gs_write", ReadOnly: false, AgentPublic: true},
}

// CapabilityCatalog 返回能力目录的副本（调用方不得改写包内状态）。
func CapabilityCatalog() []Capability {
	out := make([]Capability, len(capabilityCatalog))
	for i, c := range capabilityCatalog {
		out[i] = c
		out[i].ReadTools = append([]string(nil), c.ReadTools...)
		out[i].WriteTools = append([]string(nil), c.WriteTools...)
	}
	return out
}

// CapabilityToolCatalog 返回工具公开定义（页面只渲染这份，避免出现不存在的工具名）。
func CapabilityToolCatalog() []CapabilityTool {
	out := make([]CapabilityTool, len(capabilityTools))
	copy(out, capabilityTools)
	return out
}

// GuideReadTools 返回攻略能力覆盖的全部只读工具（稳定顺序）。
func GuideReadTools() []string {
	for _, c := range capabilityCatalog {
		if c.ID == CapabilityGuideHelp {
			return append([]string(nil), c.ReadTools...)
		}
	}
	return nil
}

// CapabilityByID 按 ID 取定义。
func CapabilityByID(id string) (Capability, bool) {
	for _, c := range capabilityCatalog {
		if c.ID == id {
			return c, true
		}
	}
	return Capability{}, false
}

// CapabilityForEvent 返回事件类别所需的能力定义。
func CapabilityForEvent(event string) (Capability, bool) {
	if event == "" {
		return Capability{}, false
	}
	for _, c := range capabilityCatalog {
		if c.Event == event {
			return c, true
		}
	}
	return Capability{}, false
}

// CapabilityRequiresActorSimulation 报告这条能力是否需要 GS 原模拟处于开启态。
//
// 目录里只有「虚拟垂钓」直接对应 GS 的模拟开关；其余事件能力由 T-108 的
// 投递前消费按能力交集判定，不能因为保存一条事件能力就擅自打开模拟。
func CapabilityRequiresActorSimulation(id string) bool {
	return id == CapabilityVirtualFishing
}

// RequiresActorSimulation 报告一个已保存集合是否要求 GS 原模拟。
func RequiresActorSimulation(caps []string) bool {
	for _, id := range caps {
		if CapabilityRequiresActorSimulation(id) {
			return true
		}
	}
	return false
}

// capabilityRuntimeTag 是「能力保存触发的 GS 运行变更」请求 ID 的命名空间。
//
// 它把能力修订写进 request_id，使 GS 应用侧能拒绝旧修订的命令，也让读侧
// 能只凭一条命令解释「代次推进是这次保存自己造成的」。请求 ID 仍是普通的
// ASCII 字符串，不改变 ai_runtime_command 的持久列。
const capabilityRuntimeTag = "cap1"

// CapabilityRuntimeRequestPrefix 返回某角色某能力修订的命令 ID 前缀。
// 前缀同时包含角色摘要与修订，不同角色/修订不会撞 ID。
func CapabilityRuntimeRequestPrefix(agentKey string, revision int64) string {
	sum := sha256.Sum256([]byte(agentKey))
	return capabilityRuntimeTag + "-" + hex.EncodeToString(sum[:6]) + "-r" +
		strconv.FormatInt(revision, 10) + "-"
}

// CapabilityRuntimeRequestID 返回一次补派的请求 ID。attempt<=1 是基础 ID；
// 失败后按当前修订再次补派时递增 attempt，不覆盖已终结的历史行。
func CapabilityRuntimeRequestID(agentKey string, revision int64, enabled bool, attempt int) string {
	flag := "0"
	if enabled {
		flag = "1"
	}
	base := CapabilityRuntimeRequestPrefix(agentKey, revision) + flag
	if attempt <= 1 {
		return base
	}
	return base + "-a" + strconv.Itoa(attempt)
}

// CapabilityRuntimeRequestTag 返回能力运行命令 request_id 的公共前缀。
// 存储层用它做前缀筛选，避免把旧后台的 actor 命令误认成能力命令。
func CapabilityRuntimeRequestTag() string { return capabilityRuntimeTag + "-" }

// IsCapabilityRuntimeRequest 报告请求 ID 是否由能力保存触发。
// GS 只用它决定是否额外复核能力修订，不解析其余请求 ID。
func IsCapabilityRuntimeRequest(requestID string) bool {
	return strings.HasPrefix(requestID, CapabilityRuntimeRequestTag())
}

// KnownTool 报告名字是否属于目录里的工具。
func KnownTool(name string) bool {
	for _, t := range capabilityTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// 能力配置解析错误。保存路径原样报给管理员，读取路径按失败关闭处理。
var (
	ErrCapabilityInvalid = errors.New("aiconfig: 能力配置非法")
	// ErrCapabilityUnknown 同时是 Invalid 的一种，管理接口只需判 Invalid。
	ErrCapabilityUnknown = fmt.Errorf("%w: 未知能力", ErrCapabilityInvalid)
)

// CapabilitySet 是一个角色的版本化能力集合（ai_setting 里的一条 JSON）。
//
// ReadOnlyTools 只用于旧配置升级：保留每个可证明的旧工具 ID 的粒度。
// 新保存的显式集合不写它，表示攻略能力覆盖目录里的全部只读工具。
type CapabilitySet struct {
	SchemaVersion int      `json:"schema_version"`
	Revision      int64    `json:"revision"`
	Generation    int64    `json:"generation"`
	Capabilities  []string `json:"capabilities"`
	ReadOnlyTools []string `json:"readonly_tools,omitempty"`
	UpdatedTS     float64  `json:"updated_ts,omitempty"`
}

// NormalizeCapabilities 去重并按目录顺序排列；未知 ID 直接报错。
func NormalizeCapabilities(ids []string) ([]string, error) {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, fmt.Errorf("%w: 空能力 ID", ErrCapabilityInvalid)
		}
		if _, ok := CapabilityByID(id); !ok {
			return nil, fmt.Errorf("%w: %s", ErrCapabilityUnknown, id)
		}
		seen[id] = true
	}
	out := make([]string, 0, len(seen))
	for _, c := range capabilityCatalog {
		if seen[c.ID] {
			out = append(out, c.ID)
		}
	}
	return out, nil
}

// NormalizeReadOnlyTools 校验旧只读子集：只允许攻略目录内的工具，去重保持目录顺序。
func NormalizeReadOnlyTools(names []string) ([]string, error) {
	allowed := map[string]bool{}
	for _, n := range GuideReadTools() {
		allowed[n] = true
	}
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !allowed[n] {
			return nil, fmt.Errorf("%w: 只读子集含非法工具 %s", ErrCapabilityInvalid, n)
		}
		seen[n] = true
	}
	out := make([]string, 0, len(seen))
	for _, n := range GuideReadTools() {
		if seen[n] {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		// 空子集按「没有收窄」处理：nil 才是全目录，[] 会被误读成零工具。
		return nil, nil
	}
	return out, nil
}

// Validate 校验一条待保存的配置。显式空集合是合法的，表示全部拒绝。
func (s CapabilitySet) Validate() error {
	if s.SchemaVersion != CapabilitySchemaVersion {
		return fmt.Errorf("%w: 未知 schema_version=%d（当前支持 %d）",
			ErrCapabilityInvalid, s.SchemaVersion, CapabilitySchemaVersion)
	}
	if s.Capabilities == nil {
		// 显式空必须写成 []；null 与「缺记录」无法区分，不能落库。
		return fmt.Errorf("%w: capabilities 不能为 null，显式空请写 []", ErrCapabilityInvalid)
	}
	caps, err := NormalizeCapabilities(s.Capabilities)
	if err != nil {
		return err
	}
	if len(caps) != len(s.Capabilities) {
		return fmt.Errorf("%w: 能力 ID 重复或空值", ErrCapabilityInvalid)
	}
	for i, id := range s.Capabilities {
		if id != caps[i] {
			return fmt.Errorf("%w: 能力未按目录顺序排列", ErrCapabilityInvalid)
		}
	}
	if len(s.ReadOnlyTools) > 0 {
		tools, err := NormalizeReadOnlyTools(s.ReadOnlyTools)
		if err != nil {
			return err
		}
		if len(tools) != len(s.ReadOnlyTools) {
			return fmt.Errorf("%w: 只读子集重复或含空值", ErrCapabilityInvalid)
		}
		hasGuide := false
		for _, id := range s.Capabilities {
			if id == CapabilityGuideHelp {
				hasGuide = true
			}
		}
		if !hasGuide {
			return fmt.Errorf("%w: 只读子集必须与攻略帮助同时存在", ErrCapabilityInvalid)
		}
	}
	return nil
}

// MarshalCapabilitySet 编码为存储 JSON（键序固定）。
func MarshalCapabilitySet(s CapabilitySet) (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseCapabilitySet 解析存储 JSON；未知版本、未知能力、重复项都算非法。
func ParseCapabilitySet(raw string) (CapabilitySet, error) {
	var s CapabilitySet
	if strings.TrimSpace(raw) == "" {
		return s, fmt.Errorf("%w: 空记录", ErrCapabilityInvalid)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%w: %v", ErrCapabilityInvalid, err)
	}
	if err := s.Validate(); err != nil {
		return s, err
	}
	return s, nil
}

// CapabilityRevisionOf 只从记录里取修订号，供冲突重试使用。
// 坏记录只要修订号还能读出，就必须用它比较，不能要求 base_revision=0。
func CapabilityRevisionOf(raw string) (int64, bool) {
	var probe struct {
		Revision *int64 `json:"revision"`
	}
	if json.Unmarshal([]byte(raw), &probe) != nil || probe.Revision == nil {
		return 0, false
	}
	return *probe.Revision, true
}

// 能力状态的判定结果。任何非 configured 状态都不得授予工具或事件。
const (
	CapabilityStateMissing    = "missing"    // 没有配置：只保留基础私聊
	CapabilityStateConfigured = "configured" // 显式配置，按集合投影
	CapabilityStateEmpty      = "empty"      // 显式空集合：全部拒绝
	CapabilityStateInvalid    = "invalid"    // 记录损坏或含未知能力：失败关闭
	CapabilityStateInactive   = "inactive"   // 角色已删除或配置属旧代次：不生效
)

// CapabilityState 是运行态最终使用的能力投影。
type CapabilityState struct {
	AgentKey     string
	State        string
	Revision     int64
	Generation   int64
	Capabilities []string
	ReadTools    []string
	WriteTools   []string
	Legacy       bool
	Reason       string
}

// Has 报告能力是否被显式授予。
func (s CapabilityState) Has(id string) bool {
	for _, c := range s.Capabilities {
		if c == id {
			return true
		}
	}
	return false
}

// ToolAllowed 报告某工具此刻是否可用。
func (s CapabilityState) ToolAllowed(name string) bool {
	for _, t := range s.ReadTools {
		if t == name {
			return true
		}
	}
	for _, t := range s.WriteTools {
		if t == name {
			return true
		}
	}
	return false
}

// Tools 返回本状态允许的工具（只读在前，写在后，稳定顺序）。
func (s CapabilityState) Tools() []string {
	out := make([]string, 0, len(s.ReadTools)+len(s.WriteTools))
	out = append(out, s.ReadTools...)
	out = append(out, s.WriteTools...)
	return out
}

// ProjectTools 把能力集合投影成允许的读/写工具。
//
// 旧子集只收窄旧攻略授权。明确勾选的新主题可独立授权。
func ProjectTools(caps []string, legacyReadTools []string) (read, write []string) {
	readSet, writeSet := map[string]bool{}, map[string]bool{}
	for _, id := range caps {
		c, ok := CapabilityByID(id)
		if !ok {
			continue
		}
		for _, name := range c.ReadTools {
			if id != CapabilityGuideHelp || legacyReadTools == nil || slices.Contains(legacyReadTools, name) {
				readSet[name] = true
			}
		}
		for _, name := range c.WriteTools {
			writeSet[name] = true
		}
	}
	for _, name := range GuideReadTools() {
		if readSet[name] {
			read = append(read, name)
			delete(readSet, name)
		}
	}
	for _, tool := range capabilityTools {
		if readSet[tool.Name] {
			read = append(read, tool.Name)
		}
		if writeSet[tool.Name] {
			write = append(write, tool.Name)
		}
	}
	return read, write
}

// LegacyReadTools 从旧 tools_json 里取出可证明的只读工具子集。
//
// 旧字段只保留只读、且属于攻略目录的 ID；未知工具、写工具、事件能力一律不升级。
// 返回 nil 表示旧值不可解析（视为没有可证明的授权）。
func LegacyReadTools(toolsJSON string) []string {
	var names []string
	if err := json.Unmarshal([]byte(toolsJSON), &names); err != nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, n := range GuideReadTools() {
		allowed[n] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, n := range GuideReadTools() {
		for _, old := range names {
			if old == n && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// ResolveCapabilityState 是读侧的唯一投影：把存储记录 + 旧快照 + 当前代次
// 合成为一个可直接使用的状态。任何不确定都返回空工具集。
func ResolveCapabilityState(agentKey string, rec *CapabilitySet, hasRecord bool,
	parseErr error, legacyToolsJSON string, currentGeneration int64) CapabilityState {
	out := CapabilityState{AgentKey: agentKey, State: CapabilityStateMissing}
	switch {
	case !hasRecord:
		// 没有新记录：旧 tools_json 只映射成只读子集；没有可证明的旧值就是
		// 缺配置（基础私聊保留，无工具、无事件）。
		return LegacyCapabilityState(agentKey, legacyToolsJSON, currentGeneration)
	case parseErr != nil:
		out.State = CapabilityStateInvalid
		out.Reason = parseErr.Error()
		return out
	case rec == nil:
		out.State = CapabilityStateInvalid
		out.Reason = "空记录"
		return out
	}
	out.Revision = rec.Revision
	out.Generation = rec.Generation
	if rec.Generation != currentGeneration {
		out.State = CapabilityStateInactive
		out.Reason = "generation"
		return out
	}
	if len(rec.Capabilities) == 0 {
		out.State = CapabilityStateEmpty
		return out
	}
	legacy := rec.ReadOnlyTools
	out.State = CapabilityStateConfigured
	out.Capabilities = append([]string(nil), rec.Capabilities...)
	out.ReadTools, out.WriteTools = ProjectTools(rec.Capabilities, legacy)
	return out
}

// LegacyCapabilityState 是「还没有新记录」时的读侧结果：
// 旧 tools_json 只映射成攻略帮助 + 只读子集，绝不授予写或事件。
func LegacyCapabilityState(agentKey, toolsJSON string, currentGeneration int64) CapabilityState {
	tools := LegacyReadTools(toolsJSON)
	out := CapabilityState{AgentKey: agentKey, State: CapabilityStateMissing, Generation: currentGeneration}
	if len(tools) == 0 {
		return out
	}
	out.State = CapabilityStateConfigured
	out.Legacy = true
	out.Capabilities = []string{CapabilityGuideHelp}
	out.ReadTools = tools
	return out
}

// 受信事件合同。
//
// 事件类别、来源角色与适用配置修订必须由服务端入口显式传入执行侧；
// 缺类别、未知类别、来源不是 system 都无法验证，按原票终止该事件，
// 禁止解析 prompt 或 HTTP body 当作权限凭证。
var (
	ErrEventUnverifiable = errors.New("aiconfig: 事件来源不可验证")
	ErrEventNotGranted   = errors.New("aiconfig: 角色未获该事件能力")
)

// TrustedEvent 是一条事件任务在服务端的可信描述。
type TrustedEvent struct {
	Category       string   // Event* 常量
	Source         string   // human | system
	AgentKeys      []string // 事件涉及的 Agent
	ConfigRevision int64    // 入队时读到的能力配置修订；0 表示无证据
	TaskID         int64
}

// Validate 只校验合同自身是否完整，不判断权限。
func (e TrustedEvent) Validate() error {
	if e.Source != "system" && e.Source != "human" {
		return fmt.Errorf("%w: 未知来源 %q", ErrEventUnverifiable, e.Source)
	}
	if e.Source == "system" {
		if e.Category == "" {
			return fmt.Errorf("%w: 缺事件类别", ErrEventUnverifiable)
		}
		if _, ok := CapabilityForEvent(e.Category); !ok {
			return fmt.Errorf("%w: 未知事件类别 %q", ErrEventUnverifiable, e.Category)
		}
		if len(e.AgentKeys) == 0 {
			return fmt.Errorf("%w: 事件没有角色", ErrEventUnverifiable)
		}
		for _, k := range e.AgentKeys {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("%w: 事件角色为空", ErrEventUnverifiable)
			}
		}
	}
	return nil
}

// AuthorizeEvent 在投递前消费：要求事件合同可验证、能力已授予且修订一致。
//
// 修订为 0 的旧事件视为无证据：终止，不猜「大概有权限」。
func AuthorizeEvent(e TrustedEvent, states map[string]CapabilityState) error {
	if err := e.Validate(); err != nil {
		return err
	}
	if e.Source == "human" {
		return nil // 真人私聊/公聊不属于事件能力，基础对话始终保留
	}
	def, ok := CapabilityForEvent(e.Category)
	if !ok {
		return fmt.Errorf("%w: 未知事件类别 %q", ErrEventUnverifiable, e.Category)
	}
	for _, key := range e.AgentKeys {
		st, ok := states[key]
		if !ok {
			return fmt.Errorf("%w: 缺角色 %s 的能力状态", ErrEventNotGranted, key)
		}
		if st.State != CapabilityStateConfigured && st.State != CapabilityStateEmpty {
			return fmt.Errorf("%w: %s 状态 %s", ErrEventNotGranted, key, st.State)
		}
		if !st.Has(def.ID) {
			return fmt.Errorf("%w: %s 缺 %s", ErrEventNotGranted, key, def.ID)
		}
		if e.ConfigRevision != 0 && st.Revision != e.ConfigRevision {
			return fmt.Errorf("%w: %s 配置修订 %d，事件携带 %d",
				ErrEventNotGranted, key, st.Revision, e.ConfigRevision)
		}
		if def.RequiresRoundtable && !st.Has(CapabilityJoinRoundtable) {
			return fmt.Errorf("%w: %s 的 %s 需要先具备 %s",
				ErrEventNotGranted, key, def.ID, CapabilityJoinRoundtable)
		}
	}
	return nil
}
