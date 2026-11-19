// Package aiconfig 是 AI 安全默认值的唯一声明点。
//
// 只放「键名 + 安全默认 + 判定」，不导入 admin / aibridge / gameserver，
// 避免后台、桥接进程与游戏服各写一份「键缺失就当成开」。
//
// 安全语义（V2 / T-102，DEV-104「只留急停」）：
//   - emergency_stop：唯一放行条件。只有明确的 "0" 才表示未停止；缺键取安全
//     默认 "0"，任何无法解析的非空值一律保守停止。
//   - outbound_enabled / 四项 consent / 部署出口主机名单：历史键仍按原格式
//     解析与展示（兼容旧调用者），但**不再参与运行资格判定**。历史值不能证明
//     用户当下意图，V2 也不再要求逐项确认。
//   - 配置发布（DEV-114 A）：活动修订、凭据引用、自动记录与复测结果都写在
//     ai_setting 的受保护键里；普通整卡保存必须拒收这些键。
//
// 本包不读取 config.AI.Enabled：它只决定是否启动 aibridge 进程，不是外发授权。
package aiconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

var (
	errEmptyConsent = errors.New("aiconfig: empty consent record")
	errEmptyRecord  = errors.New("aiconfig: empty record")
)

// ai_setting 键名。运行期判定与后台描述共用，避免字符串散落。
const (
	KeyOutboundEnabled = "outbound_enabled"
	KeyEmergencyStop   = "emergency_stop"
	KeyActorEnabled    = "ai_actor_enabled"
	KeyCatchTalk       = "ai_catch_talk"
	KeyMemoryInject    = "ai_memory_inject"
	// KeySearchEndpoint 是联网搜索端点。它不共用模型凭据，变更时单独记目的地。
	KeySearchEndpoint = "search_endpoint"
)

// 显式授权（consent）专用元数据键。
//
// 只能由后台独立的确认/撤销接口事务写入；普通整卡保存必须拒收这些键
// （T-006 §6.3：整卡保存会把同一模块的全部键重写一遍，不能让它顺带
// 把 consent 冲成「已授权」）。
//
// 键值是 JSON（见 ConsentRecord），不是 "0"/"1"：旧 value、updated_ts
// 与「审计里出现过键名」都不足以证明用户同意，只有这里的状态能。
const (
	KeyConsentOutbound     = "ai_consent_outbound"
	KeyConsentActor        = "ai_consent_actor"
	KeyConsentCatchTalk    = "ai_consent_catch_talk"
	KeyConsentMemoryInject = "ai_consent_memory_inject"

	// KeyEgressDestination 是「当前外发目的地」的有类型快照，由保存厂商配置
	// 或确认授权时与 consent 同事务写入。V2 仅把它作为可读兼容字段保留：
	// 目的地是否可换由配置发布合同（活动修订 + 同源判据）决定，不再靠它放行。
	KeyEgressDestination = "ai_egress_destination"
)

// 配置发布（DEV-114 A）的受保护键。活动修订、凭据引用与自动记录只能由
// 后台保存 / 迁移的同一 DB 事务写入；普通整卡保存必须拒收。
//
// 键值都是 JSON（见 ActiveConfig / ConfigRecord），不是 "0"/"1"。历史审计里
// 出现过键名、或旧库存在行，都不足以证明一次发布。
const (
	KeyConfigActive = "ai_config_active"

	// KeyConfigRecordPrefix + 修订号：一次模型配置发布的自动记录（只追加）。
	KeyConfigRecordPrefix = "ai_config_record:"
	// KeySearchRecordPrefix + 修订号：一次搜索去向变更的记录（只追加，
	// 与模型配置分开列目的地，不共用模型凭据）。
	KeySearchRecordPrefix = "ai_config_search_record:"

	// KeyConfigRetest 是最近一次「复测已保存连接」的结果，带被复测的修订号。
	// 页面只在它等于活动修订时显示「此连接已通过复测」。
	KeyConfigRetest = "ai_config_retest"

	// KeyEmergencyStopLiftedTS 是最近一次解除急停的时间（Unix 秒，字符串）。
	// 复测/任务/命令用它隔开停前工作：创建时间不晚于该时点的排队动作保守失效。
	KeyEmergencyStopLiftedTS = "emergency_stop_lifted_ts"
)

// ConfigSchemaVersion 是发布记录 / 活动指针的 schema 版本。
// 版本不符一律按「记录无效」锁定，不猜测兼容。
const ConfigSchemaVersion = 1

// ActiveConfig 是 ai_setting.KeyConfigActive 的值：一次发布的完整快照。
//
// 读者（Runner、后台复测、状态回读）只按这份快照取地址/模型/修订/凭据引用，
// 不在网络调用期间读第二处；发布提交后新读取只见新快照。
type ActiveConfig struct {
	Schema       int    `json:"schema"`
	Revision     int64  `json:"revision"`
	ProviderID   int64  `json:"provider_id"`
	ProviderName string `json:"provider"`
	Protocol     string `json:"protocol"`
	AuthScheme   string `json:"auth_scheme"`
	BaseURL      string `json:"base_url"`
	ModelRowID   int64  `json:"model_row_id"`
	ModelID      string `json:"model_id"`
	ConfigJSON   string `json:"config_json,omitempty"`
	// CredentialRef 是外部不可变密文对象的随机引用（不含秘密本身）。
	CredentialRef string `json:"credential_ref,omitempty"`
	// Source 记录这次发布的来源：admin_save / migration。
	Source string `json:"source"`
	Actor  string `json:"actor,omitempty"`
	TS     int64  `json:"ts"`
	// Legacy 为 true 表示这是「旧安装兼容」的只读套（尚无活动修订）。
	// 该标记只出现在读取结果里，不会被持久化成活动指针。
	Legacy bool `json:"-"`
}

// ConfigRecord 是一次配置发布的自动记录（只追加，不覆盖）。
// 只写操作者、来源、时间、用途/内容类别、规范化目的域、模型与修订；
// 不写明文密钥、玩家正文或带凭据的完整 URL。
type ConfigRecord struct {
	Schema   int      `json:"schema"`
	Revision int64    `json:"revision"`
	Kind     string   `json:"kind"` // model | search
	Actor    string   `json:"actor"`
	Source   string   `json:"source"` // admin_save | admin_settings | migration
	TS       int64    `json:"ts"`
	Purpose  []string `json:"purpose,omitempty"`
	Contents []string `json:"contents,omitempty"`
	// DestinationHost 是规范化后的目的域（小写主机名，可带端口）；
	// Destination 是去掉查询串与片段的可展示地址。
	DestinationHost string `json:"destination_host,omitempty"`
	Destination     string `json:"destination,omitempty"`
	SearchHost      string `json:"search_host,omitempty"`
	SearchEndpoint  string `json:"search_endpoint,omitempty"`
	Model           string `json:"model,omitempty"`
	// CredentialChanged 标记本次是否换过凭据对象；不记录任何指纹。
	CredentialChanged bool `json:"credential_changed,omitempty"`
}

// RetestStatus 是最近一次复测结果。Revision 必须与活动修订一致才算有效。
type RetestStatus struct {
	Epoch    int64  `json:"epoch,omitempty"`
	Attempt  string `json:"attempt,omitempty"`
	Pending  bool   `json:"pending,omitempty"`
	Revision int64  `json:"revision"`
	OK       bool   `json:"ok"`
	TS       int64  `json:"ts"`
	Actor    string `json:"actor,omitempty"`
	Host     string `json:"host,omitempty"`
	Model    string `json:"model,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ConfigRecordKey / SearchRecordKey 返回只追加记录的 ai_setting 键。
func ConfigRecordKey(revision int64) string {
	return KeyConfigRecordPrefix + strconv.FormatInt(revision, 10)
}

// SearchRecordKey 返回搜索去向记录的 ai_setting 键。
func SearchRecordKey(revision int64) string {
	return KeySearchRecordPrefix + strconv.FormatInt(revision, 10)
}

// IsPublicationMetadataKey 判断键是否属于配置发布元数据（活动指针、记录、复测）。
func IsPublicationMetadataKey(key string) bool {
	if key == KeyConfigActive || key == KeyConfigRetest || key == KeyEmergencyStopLiftedTS || key == KeyEmergencyEpochRecord {
		return true
	}
	return strings.HasPrefix(key, KeyConfigRecordPrefix) || strings.HasPrefix(key, KeySearchRecordPrefix)
}

// MarshalActive / ParseActive 是活动指针的序列化边界。
func MarshalActive(a ActiveConfig) (string, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseActive 解析活动指针；空串、非法 JSON、schema 不符或缺少关键字段时
// ok=false（调用方按「记录无效」锁定，不回退旧文件）。
func ParseActive(raw string) (ActiveConfig, bool) {
	if strings.TrimSpace(raw) == "" {
		return ActiveConfig{}, false
	}
	var a ActiveConfig
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return ActiveConfig{}, false
	}
	if a.Schema != ConfigSchemaVersion || a.Revision <= 0 {
		return ActiveConfig{}, false
	}
	if strings.TrimSpace(a.BaseURL) == "" || strings.TrimSpace(a.ModelID) == "" ||
		strings.TrimSpace(a.ProviderName) == "" {
		return ActiveConfig{}, false
	}
	return a, true
}

// MarshalConfigRecord / ParseConfigRecord 是自动记录的序列化边界。
func MarshalConfigRecord(r ConfigRecord) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseConfigRecord 解析自动记录；空串或非法 JSON 返回错误。
func ParseConfigRecord(raw string) (ConfigRecord, error) {
	var r ConfigRecord
	if strings.TrimSpace(raw) == "" {
		return r, errEmptyRecord
	}
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return r, err
	}
	return r, nil
}

// MarshalRetest / ParseRetest 是复测结果的序列化边界。
func MarshalRetest(s RetestStatus) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseRetest 解析复测结果；空串或非法 JSON 时 ok=false。
func ParseRetest(raw string) (RetestStatus, bool) {
	if strings.TrimSpace(raw) == "" {
		return RetestStatus{}, false
	}
	var s RetestStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return RetestStatus{}, false
	}
	if s.Revision < 0 {
		return RetestStatus{}, false
	}
	return s, true
}

// 授权项。四项分别记录，互不代替。
const (
	ConsentItemOutbound     = "outbound"
	ConsentItemActor        = "actor"
	ConsentItemCatchTalk    = "catch_talk"
	ConsentItemMemoryInject = "memory_inject"
)

// 授权记录版本。schema 变化时旧记录一律作废（fail-closed）。
const (
	ConsentSchemaVersion  = 1
	ConsentContentVersion = 1
)

// 授权记录状态（存储值）。
const (
	ConsentGranted = "granted"
	ConsentRevoked = "revoked"
)

// 授权判定状态（读取结果，与存储状态区分开）。
const (
	ConsentStateGranted      = "granted"
	ConsentStateMissing      = "missing"
	ConsentStateRevoked      = "revoked"
	ConsentStateInvalid      = "invalid"
	ConsentStateScopeChanged = "scope_changed"
)

// 内容范围词表。确认页必须逐项写明「会发送哪些玩家内容」。
var ConsentContentLabels = map[string]string{
	"chat_text":       "玩家聊天原文与频道上下文",
	"player_identity": "玩家昵称与 UID",
	"fishing_records": "玩家上星 / 上蓝 / 钓鱼记录",
	"memory_summary":  "长期记忆摘要",
	"actor_local":     "角色本地状态（不发送）",
}

// ContentLabel 是内容范围的可展示条目。
type ContentLabel struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// ConsentItemSpec 声明一个授权项：键名、界面文案、内容范围、是否需要目的地。
//
// actor 是本地意图，LocalOnly=true，不需要目的地，也永远不能单独授权外发。
type ConsentItemSpec struct {
	Item                string
	Key                 string
	Label               string
	Hint                string
	LocalOnly           bool
	RequiresDestination bool
	Contents            []string
}

// ConsentItemSpecs 顺序即界面顺序。四项分别确认、分别撤销。
var ConsentItemSpecs = []ConsentItemSpec{
	{
		Item: ConsentItemOutbound, Key: KeyConsentOutbound,
		Label: "允许模型调用出网", Hint: "把玩家内容发送到你填写的厂商地址；这是外发总闸的唯一授权来源",
		RequiresDestination: true,
		Contents:            []string{"chat_text", "player_identity", "fishing_records", "memory_summary"},
	},
	{
		Item: ConsentItemActor, Key: KeyConsentActor,
		Label: "角色状态模拟（本地）", Hint: "只影响本地钓鱼/休息/闲逛状态，不发送任何玩家内容",
		LocalOnly: true,
		Contents:  []string{"actor_local"},
	},
	{
		Item: ConsentItemCatchTalk, Key: KeyConsentCatchTalk,
		Label: "玩家上星/上蓝时告知 AI", Hint: "把玩家上星或上蓝的钓鱼记录发给模型，让 AI 接话",
		RequiresDestination: true,
		Contents:            []string{"fishing_records"},
	},
	{
		Item: ConsentItemMemoryInject, Key: KeyConsentMemoryInject,
		Label: "把记忆摘要注入上下文", Hint: "把结构化记忆摘要（星/蓝战绩等）发送给模型",
		RequiresDestination: true,
		Contents:            []string{"memory_summary"},
	},
}

// ConsentScope 是授权时冻结的范围。任何一项变化都会使旧授权失效。
type ConsentScope struct {
	ContentVersion int      `json:"content_version"`
	Contents       []string `json:"contents"`
	DestBaseURL    string   `json:"dest_base_url"`
	DestModel      string   `json:"dest_model"`
}

// ConsentRecord 是写入 ai_setting 的 JSON 值。
// 不含原始密钥、玩家内容或请求正文，只记录谁在什么时候授权了什么范围。
type ConsentRecord struct {
	Schema  int          `json:"schema"`
	Item    string       `json:"item"`
	State   string       `json:"state"`
	Subject string       `json:"subject"`
	TS      int64        `json:"ts"`
	Scope   ConsentScope `json:"scope"`
}

// ConsentDestination 是当前目的地范围。
type ConsentDestination struct {
	BaseURL string `json:"base_url"`
	ModelID string `json:"model_id"`
}

// ConsentStatus 是单个授权项的读取结果。
type ConsentStatus struct {
	Item    string        `json:"item"`
	State   string        `json:"state"`
	Valid   bool          `json:"valid"`
	Display string        `json:"display"`
	Subject string        `json:"subject,omitempty"`
	TS      int64         `json:"ts,omitempty"`
	Scope   *ConsentScope `json:"scope,omitempty"`
}

// ConsentItemView 是后台确认页的一项：范围说明 + 当前状态。
type ConsentItemView struct {
	Item      string         `json:"item"`
	Label     string         `json:"label"`
	Hint      string         `json:"hint"`
	LocalOnly bool           `json:"local_only"`
	Contents  []ContentLabel `json:"contents"`
	Status    ConsentStatus  `json:"status"`
}

// ConsentView 是 /api/ai/status 的 security.consent 字段。
type ConsentView struct {
	Schema         int                `json:"schema"`
	ContentVersion int                `json:"content_version"`
	Items          []ConsentItemView  `json:"items"`
	Destination    ConsentDestination `json:"destination"`
}

// 安全默认值。
//
// outbound_enabled 的历史 SQL 种子（00060:188）保持 "1" 不变——那是已发布的
// 迁移快照，不能改写；但代码侧的默认与判定一律按关闭处理。
const (
	DefaultOutboundEnabled = "0"
	DefaultEmergencyStop   = "0"

	// 三个本地行为键的现状默认仍是 "1"：它们只表示「角色/接话/记忆的本地意图」，
	// 不是外发授权。本包按同一份声明给后台展示，避免后台另抄一份默认值。
	DefaultActorEnabled = "1"
	DefaultCatchTalk    = "1"
	DefaultMemoryInject = "1"
)

// 判定状态，同时用于外发总闸与后台展示。
const (
	StateOff         = "off"
	StateUnconfirmed = "unconfirmed"
	StateMissing     = "missing"
	StateInvalid     = "invalid"
	StateEmergency   = "emergency"

	StateAllowed              = "allowed"
	StateConsentRevoked       = "consent_revoked"
	StateConsentScopeChanged  = "consent_scope_changed"
	StateConsentInvalidRecord = "consent_invalid"
)

// Lookup 返回 ai_setting 的原始值；缺行或读失败返回空串。
// 调用方用 db.AISetting(ctx, key, "") 适配——不要把历史默认值当读取兜底传进来。
type Lookup func(key string) string

// EgressStatus 是外发总闸的判定结果。Allowed 是运行期唯一该看的字段。
type EgressStatus struct {
	Allowed bool   `json:"allowed"`
	State   string `json:"state"`
	Display string `json:"display"`
}

// BoolState 是历史默认开启键在后台的展示状态。
type BoolState struct {
	Key     string `json:"key"`
	Stored  string `json:"stored"`
	State   string `json:"state"`
	Display string `json:"display"`
}

// SecurityView 是后台 /api/ai/status 的 security 字段：
// 前端只消费这里的描述，不再自己把「缺省」解释成开。
type SecurityView struct {
	Egress  EgressStatus         `json:"egress"`
	Hosts   EgressHostsStatus    `json:"egress_hosts"`
	Keys    map[string]BoolState `json:"keys"`
	Consent ConsentView          `json:"consent"`
}

// DefaultValue 返回键的声明默认值；未知键 ok=false。
func DefaultValue(key string) (string, bool) {
	switch key {
	case KeyOutboundEnabled:
		return DefaultOutboundEnabled, true
	case KeyEmergencyStop:
		return DefaultEmergencyStop, true
	case KeyActorEnabled:
		return DefaultActorEnabled, true
	case KeyCatchTalk:
		return DefaultCatchTalk, true
	case KeyMemoryInject:
		return DefaultMemoryInject, true
	}
	return "", false
}

// EmergencyStop 判断是否必须停止模型调用。
// 缺键取安全默认 "0"（未停止）；任何非空且非 "0" 的值都保守地视为停止。
func EmergencyStop(lookup Lookup) bool {
	return effective(lookup, KeyEmergencyStop) != "0"
}

// EmergencyReading holds the switch and its generation from one SQLite snapshot.
type EmergencyReading struct {
	StopRaw   string
	LiftedRaw string
	Version   int64
	Epoch     int64
	Err       error
}

type EmergencyDecision struct {
	Stopped  bool
	LiftedTS float64
	Epoch    int64
	Fault    string
	Display  string
}

const (
	FaultReadError    = "emergency_read_error"
	FaultEpochInvalid = "emergency_record_invalid"
	FaultStopInvalid  = "emergency_switch_invalid"

	IntentEmergencyStop      = "emergency_stop"
	IntentQueuedBeforeResume = "queued_before_resume"
	IntentOriginUnknown      = "origin_unknown"
)

func EvaluateEmergency(r EmergencyReading) EmergencyDecision {
	if r.Err != nil {
		return EmergencyDecision{Stopped: true, Fault: FaultReadError, Display: "状态待确认（读取失败），请刷新"}
	}
	if r.Version != 1 || r.Epoch <= 0 || r.Epoch == math.MaxInt64 {
		return EmergencyDecision{Stopped: true, Fault: FaultEpochInvalid, Display: "状态待确认（急停代次损坏），请刷新"}
	}
	d := EmergencyDecision{Epoch: r.Epoch}
	if ts, err := strconv.ParseFloat(strings.TrimSpace(r.LiftedRaw), 64); err == nil && ts > 0 && !math.IsInf(ts, 0) {
		d.LiftedTS = ts // display/audit only
	}
	switch r.StopRaw {
	case "", DefaultEmergencyStop:
		d.Display = "急停已解除（可发起新对话，旧回复不会补发）"
	case "1":
		d.Stopped = true
		d.Display = "急停已开启（已发出的请求可能仍会返回）"
	default:
		d.Stopped = true
		d.Fault = FaultStopInvalid
		d.Display = "急停开关异常，请核对并保存有效值"
	}
	return d
}

func (d EmergencyDecision) IntentActive(epoch int64) (bool, string) {
	if blocked, reason := d.RecordBlocked(); blocked {
		return false, reason
	}
	if epoch <= 0 {
		return false, IntentOriginUnknown
	}
	if epoch != d.Epoch {
		return false, IntentQueuedBeforeResume
	}
	return true, ""
}

func (d EmergencyDecision) RecordBlocked() (bool, string) {
	if d.Fault != "" {
		return true, d.Fault
	}
	if d.Stopped {
		return true, IntentEmergencyStop
	}
	if d.Epoch <= 0 {
		return true, FaultEpochInvalid
	}
	return false, ""
}

// Egress describes the legacy settings view. Runtime decisions use EvaluateEmergency.
func Egress(lookup Lookup) EgressStatus {
	if EmergencyStop(lookup) {
		return EgressStatus{State: StateEmergency, Display: "关（紧急停止）"}
	}
	return EgressStatus{Allowed: true, State: StateAllowed, Display: "开（未急停）"}
}

// EmergencyEpochKey is the request-only compare-and-swap field.
const EmergencyEpochKey = "emergency_stop_epoch"

// KeyEmergencyEpochRecord is the protected singleton in ai_setting.
const KeyEmergencyEpochRecord = "emergency_epoch_record"

// Security 汇总后台需要展示的安全描述（可读兼容字段）。
//
// V2 页面只消费 Egress（急停）与真实去向；Hosts / Keys / Consent 仍按原格式
// 返回，供旧调用者读取，但都不再是放行条件。四项 consent 由兼容接口读写，
// 不改变运行资格。
func Security(lookup Lookup) SecurityView {
	egress := Egress(lookup)
	view := SecurityView{
		Egress: egress,
		Hosts:  DescribeEgressHosts(EgressHostPolicy{}),
		Keys:   make(map[string]BoolState, 3),
		Consent: ConsentView{
			Schema:         ConsentSchemaVersion,
			ContentVersion: ConsentContentVersion,
			Items:          make([]ConsentItemView, 0, len(ConsentItemSpecs)),
		},
	}
	// outbound_enabled 不再进入键展示：V2 页面没有「模型调用出网」开关。
	for _, key := range []string{KeyActorEnabled, KeyCatchTalk, KeyMemoryInject} {
		view.Keys[key] = describeHistorical(lookup, key)
	}
	for _, spec := range ConsentItemSpecs {
		view.Consent.Items = append(view.Consent.Items, ConsentItemView{
			Item: spec.Item, Label: spec.Label, Hint: spec.Hint,
			LocalOnly: spec.LocalOnly, Contents: contentLabels(spec.Contents),
			Status: EvaluateConsent(lookup, spec.Item),
		})
	}
	return view
}

// ConsentItemSpecFor 按授权项取声明；未知项 ok=false。
func ConsentItemSpecFor(item string) (ConsentItemSpec, bool) {
	for _, spec := range ConsentItemSpecs {
		if spec.Item == item {
			return spec, true
		}
	}
	return ConsentItemSpec{}, false
}

// ConsentKeyFor 返回授权项对应的 ai_setting 键。
func ConsentKeyFor(item string) (string, bool) {
	spec, ok := ConsentItemSpecFor(item)
	if !ok {
		return "", false
	}
	return spec.Key, true
}

// IsConsentMetadataKey 判断键是否是受保护的授权/范围元数据。
// 普通整卡保存必须拒绝这些键；它们只能由独立接口事务写入。
func IsConsentMetadataKey(key string) bool {
	for _, spec := range ConsentItemSpecs {
		if spec.Key == key {
			return true
		}
	}
	return key == KeyEgressDestination
}

// IsProtectedConfigKey 判断键是否只能由专用接口写入：
// 授权/目的地元数据 + 配置发布元数据（活动指针、自动记录、复测、急停解除时点）。
// 通用设置保存必须整笔拒收，避免一次「保存卡片」伪造发布或授权。
func IsProtectedConfigKey(key string) bool {
	return IsConsentMetadataKey(key) || IsPublicationMetadataKey(key)
}

// NewConsentRecord 生成一条 granted 记录。scope 固化当时的内容版本、
// 内容清单与目的地；之后任何一项变化都必须重新确认。
func NewConsentRecord(item, subject string, ts int64, dest ConsentDestination) (ConsentRecord, bool) {
	spec, ok := ConsentItemSpecFor(item)
	if !ok {
		return ConsentRecord{}, false
	}
	scope := ConsentScope{
		ContentVersion: ConsentContentVersion,
		Contents:       append([]string(nil), spec.Contents...),
	}
	if spec.RequiresDestination {
		scope.DestBaseURL = strings.TrimSpace(dest.BaseURL)
		scope.DestModel = strings.TrimSpace(dest.ModelID)
	}
	return ConsentRecord{
		Schema: ConsentSchemaVersion, Item: item, State: ConsentGranted,
		Subject: subject, TS: ts, Scope: scope,
	}, true
}

// NewRevokedConsentRecord 生成一条 revoked 记录，保留原范围用于界面展示。
func NewRevokedConsentRecord(item, subject string, ts int64, scope ConsentScope) ConsentRecord {
	rec := ConsentRecord{
		Schema: ConsentSchemaVersion, Item: item, State: ConsentRevoked,
		Subject: subject, TS: ts, Scope: scope,
	}
	if rec.Scope.ContentVersion == 0 {
		if spec, ok := ConsentItemSpecFor(item); ok {
			rec.Scope.ContentVersion = ConsentContentVersion
			rec.Scope.Contents = append([]string(nil), spec.Contents...)
		}
	}
	return rec
}

// MarshalConsent / ParseConsent 是 consent 记录的序列化边界。
// 解析失败一律视为无效记录，不猜、不降级成已授权。
func MarshalConsent(rec ConsentRecord) (string, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseConsent 解析一个 JSON consent 记录；空串或非法 JSON 返回错误。
func ParseConsent(raw string) (ConsentRecord, error) {
	var rec ConsentRecord
	if strings.TrimSpace(raw) == "" {
		return rec, errEmptyConsent
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return rec, err
	}
	return rec, nil
}

// MarshalDestination / ParseDestination 是目的地快照的序列化边界。
func MarshalDestination(dest ConsentDestination) (string, error) {
	b, err := json.Marshal(ConsentDestination{
		BaseURL: strings.TrimSpace(dest.BaseURL),
		ModelID: strings.TrimSpace(dest.ModelID),
	})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseDestination 解析目的地快照；缺字段或非法 JSON 返回 ok=false。
func ParseDestination(raw string) (ConsentDestination, bool) {
	if strings.TrimSpace(raw) == "" {
		return ConsentDestination{}, false
	}
	var dest ConsentDestination
	if err := json.Unmarshal([]byte(raw), &dest); err != nil {
		return ConsentDestination{}, false
	}
	dest.BaseURL = strings.TrimSpace(dest.BaseURL)
	dest.ModelID = strings.TrimSpace(dest.ModelID)
	if dest.BaseURL == "" || dest.ModelID == "" {
		return ConsentDestination{}, false
	}
	return dest, true
}

// EvaluateConsent 读取并校验一个授权项。
//
// 判定顺序（全部 fail-closed）：
// 缺记录 → missing；非法 JSON / 项或版本不匹配 → invalid；
// revoked → revoked；内容版本或内容清单变化 → scope_changed；
// 需要目的地的项还要与当前目的地快照完全一致，否则同样 scope_changed。
func EvaluateConsent(lookup Lookup, item string) ConsentStatus {
	spec, ok := ConsentItemSpecFor(item)
	if !ok {
		return ConsentStatus{Item: item, State: ConsentStateInvalid, Display: "未知授权项"}
	}
	raw := read(lookup, spec.Key)
	if strings.TrimSpace(raw) == "" {
		return ConsentStatus{Item: item, State: ConsentStateMissing, Display: "未确认"}
	}
	rec, err := ParseConsent(raw)
	if err != nil {
		return ConsentStatus{Item: item, State: ConsentStateInvalid, Display: "记录无效"}
	}
	if rec.Schema != ConsentSchemaVersion || rec.Item != item {
		return ConsentStatus{Item: item, State: ConsentStateInvalid, Display: "记录无效"}
	}
	base := ConsentStatus{Item: item, Subject: rec.Subject, TS: rec.TS, Scope: &rec.Scope}
	switch rec.State {
	case ConsentRevoked:
		base.State, base.Display = ConsentStateRevoked, "已撤销"
		return base
	case ConsentGranted:
	default:
		base.State, base.Display = ConsentStateInvalid, "记录无效"
		return base
	}
	if strings.TrimSpace(rec.Subject) == "" || rec.TS <= 0 {
		base.State, base.Display = ConsentStateInvalid, "记录无效"
		return base
	}
	if !scopeMatchesSpec(rec.Scope, spec) {
		base.State, base.Display = ConsentStateScopeChanged, "范围已变化"
		return base
	}
	if spec.RequiresDestination {
		dest, ok := ParseDestination(read(lookup, KeyEgressDestination))
		if !ok ||
			dest.BaseURL != strings.TrimSpace(rec.Scope.DestBaseURL) ||
			dest.ModelID != strings.TrimSpace(rec.Scope.DestModel) {
			base.State, base.Display = ConsentStateScopeChanged, "范围已变化"
			return base
		}
	}
	base.State, base.Display, base.Valid = ConsentStateGranted, "已授权", true
	return base
}

// ConsentScopeMatches 判断一条记录的范围是否仍与当前授权项声明和目的地一致。
// 用于确认接口的幂等判断：完全一致时不重复写入。
func ConsentScopeMatches(scope ConsentScope, item string, dest ConsentDestination) bool {
	spec, ok := ConsentItemSpecFor(item)
	if !ok || !scopeMatchesSpec(scope, spec) {
		return false
	}
	if !spec.RequiresDestination {
		return true
	}
	return strings.TrimSpace(scope.DestBaseURL) == strings.TrimSpace(dest.BaseURL) &&
		strings.TrimSpace(scope.DestModel) == strings.TrimSpace(dest.ModelID)
}

func scopeMatchesSpec(scope ConsentScope, spec ConsentItemSpec) bool {
	if scope.ContentVersion != ConsentContentVersion {
		return false
	}
	if len(scope.Contents) != len(spec.Contents) {
		return false
	}
	for i, key := range spec.Contents {
		if scope.Contents[i] != key {
			return false
		}
	}
	return true
}

func contentLabels(keys []string) []ContentLabel {
	out := make([]ContentLabel, 0, len(keys))
	for _, key := range keys {
		label, ok := ConsentContentLabels[key]
		if !ok {
			label = key
		}
		out = append(out, ContentLabel{Key: key, Label: label})
	}
	return out
}

func describeHistorical(lookup Lookup, key string) BoolState {
	stored := read(lookup, key)
	switch effective(lookup, key) {
	case "0":
		return BoolState{Key: key, Stored: stored, State: StateOff, Display: "关"}
	case "1":
		// 历史默认值或整卡保存都会写成 "1"，都不能证明是用户显式选择。
		return BoolState{Key: key, Stored: stored, State: StateUnconfirmed, Display: "未确认"}
	default:
		return BoolState{Key: key, Stored: stored, State: StateInvalid, Display: "关（取值非法）"}
	}
}

func read(lookup Lookup, key string) string {
	if lookup == nil {
		return ""
	}
	return lookup(key)
}

func effective(lookup Lookup, key string) string {
	if raw := read(lookup, key); raw != "" {
		return raw
	}
	def, _ := DefaultValue(key)
	return def
}

// ---- 外部出口主机白名单（部署配置） ----
//
// 模型调用（provider.base_url）、联网搜索（search_endpoint）与后台「测试连接」
// 三个出口共用这一套判定。白名单只来自部署配置（config.ai.egress_allowed_hosts），
// 默认空；后台页面填写的 URL 不会扩大它。
//
// 判定分两层，任何一层不放行都不得建立连接：
//  1. CheckURL / CheckEndpoint：发送前的 URL 级检查——必须 HTTPS、不得含 userinfo、
//     host:port 精确命中白名单（大小写与尾点规范化，不做通配/后缀匹配）；
//  2. IsPublicEgressAddr：aibridge 的安全 Transport 在拨号前对「实际解析到的地址」
//     做检查，拒绝环回、私网、链路本地等非公网目标，并且只拨校验过的 IP，
//     防止 DNS 重绑定（解析一次、只信这一次的结果）。

// 出口主机白名单的拒绝状态。与 Egress/consent 的状态分开，后台可以同时
// 展示「总闸/授权」与「部署主机白名单」两道闸的拒绝原因。
const (
	StateEgressHostMissing    = "egress_host_missing"
	StateEgressHostDenied     = "egress_host_denied"
	StateEgressSchemeDenied   = "egress_scheme_denied"
	StateEgressUserinfoDenied = "egress_userinfo_denied"
	StateEgressAddrDenied     = "egress_addr_denied"
)

// HostPolicyError 是出口主机白名单的拒绝原因；运行期与后台共用同一份
// state/display，避免后台再抄一套文案。
type HostPolicyError struct {
	State   string `json:"state"`
	Display string `json:"display"`
}

func (e *HostPolicyError) Error() string { return "ai: 外发被拒绝：" + e.Display }

func hostPolicyDenied(state, display string) *HostPolicyError {
	return &HostPolicyError{State: state, Display: display}
}

// egressHostRule 是规范化后的白名单条目：小写主机名 + 十进制端口。
type egressHostRule struct {
	host string
	port string
}

// EgressHostPolicy 是部署级出口主机白名单。零值表示空白名单：
// 所有外部出口一律拒绝（fail-closed）。
type EgressHostPolicy struct {
	rules []egressHostRule
}

// NewEgressHostPolicy 规范化部署配置条目。
//
// 条目接受 host[:port] 或 https://host[:port]（默认端口 443）。非法条目
// （空、通配、userinfo、非 HTTPS、带路径/查询、非法端口）直接报错，
// 由 config.Load 在启动时失败，不静默忽略——静默忽略会把错配置变成「空名单」。
func NewEgressHostPolicy(entries []string) (EgressHostPolicy, error) {
	policy := EgressHostPolicy{}
	seen := make(map[egressHostRule]struct{}, len(entries))
	for i, entry := range entries {
		rule, err := parseEgressHostRule(entry)
		if err != nil {
			return EgressHostPolicy{}, fmt.Errorf("第 %d 条 %q: %w", i+1, strings.TrimSpace(entry), err)
		}
		if _, ok := seen[rule]; ok {
			continue
		}
		seen[rule] = struct{}{}
		policy.rules = append(policy.rules, rule)
	}
	return policy, nil
}

// Empty 报告部署是否没有配置任何出口主机。
func (p EgressHostPolicy) Empty() bool { return len(p.rules) == 0 }

// Allows 判断 host:port 是否精确命中白名单。host 允许大小写与结尾点差异，
// 端口缺省为 443；不做通配、子域或后缀匹配。
func (p EgressHostPolicy) Allows(host, port string) bool {
	h := normalizeEgressHost(host)
	pt, err := normalizeEgressPort(port)
	if h == "" || err != nil {
		return false
	}
	for _, rule := range p.rules {
		if rule.host == h && rule.port == pt {
			return true
		}
	}
	return false
}

// CheckURL 只做 URL 形状检查（HTTPS / 无 userinfo / 主机名合法）。
// 旧部署名单已不再参与放行判断（V2 只留急停），保留方法是为了兼容旧调用者
// 与解析测试；生产发送路径统一走 CheckEgressURL。
func (p EgressHostPolicy) CheckURL(u *url.URL) error {
	return CheckEgressURL(u)
}

// CheckEndpoint 是 CheckURL 的字符串入口。
func (p EgressHostPolicy) CheckEndpoint(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return hostPolicyDenied(StateEgressHostDenied, "关（出口地址不合法）")
	}
	return CheckEgressURL(u)
}

// SameEgressAuthority 判断两个 URL 是否同一主机与端口（缺省端口 443）。
// 重定向用它决定是否继续携带鉴权头：跨主机/跨端口一律不带。
func SameEgressAuthority(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	if normalizeEgressHost(a.Hostname()) != normalizeEgressHost(b.Hostname()) {
		return false
	}
	pa, errA := normalizeEgressPort(a.Port())
	pb, errB := normalizeEgressPort(b.Port())
	return errA == nil && errB == nil && pa == pb
}

// EgressHostsStatus 是后台 /api/ai/status 里展示的部署白名单状态。
// 只描述「配置了没有、几条」，不把主机清单回显给前端。
type EgressHostsStatus struct {
	Configured int    `json:"configured"`
	Display    string `json:"display"`
}

// DescribeEgressHosts 生成旧部署名单的状态描述。
// V2 只把它当部署记录展示，不再是放行条件。
func DescribeEgressHosts(policy EgressHostPolicy) EgressHostsStatus {
	if policy.Empty() {
		return EgressHostsStatus{Display: "未配置（仅部署记录，不参与放行）"}
	}
	return EgressHostsStatus{
		Configured: len(policy.rules),
		Display:    fmt.Sprintf("已记录 %d 个旧出口主机（不参与放行）", len(policy.rules)),
	}
}

// IsPublicEgressAddr 判断连接阶段解析出的地址是否是可外发的公网目标。
//
// 拒绝：环回、私网、链路本地、未指定、组播、CGNAT、保留/文档段与 IPv4 映射地址。
// aibridge 在拨号前对解析结果逐条检查（任何一条不合格即整体拒绝），
// 防止攻击者用 DNS 把已授权主机名解析到内网地址。
func IsPublicEgressAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	if ip.Is4() {
		b := ip.As4()
		switch {
		case b[0] == 0 || b[0] >= 240:
			return false // 0.0.0.0/8、240.0.0.0/4
		case b[0] == 100 && b[1] >= 64 && b[1] <= 127:
			return false // 100.64.0.0/10 CGNAT
		case b[0] == 192 && b[1] == 0 && b[2] == 0:
			return false // 192.0.0.0/24
		case b[0] == 192 && b[1] == 0 && b[2] == 2:
			return false // 192.0.2.0/24 文档段
		case b[0] == 198 && (b[1] == 18 || b[1] == 19):
			return false // 198.18.0.0/15 基准测试段
		case b[0] == 198 && b[1] == 51 && b[2] == 100:
			return false // 198.51.100.0/24 文档段
		case b[0] == 203 && b[1] == 0 && b[2] == 113:
			return false // 203.0.113.0/24 文档段
		}
		return true
	}
	if ip.Is6() {
		b := ip.As16()
		if b[0] == 0x20 && b[1] == 0x01 && b[2] == 0x0d && b[3] == 0xb8 {
			return false // 2001:db8::/32 文档段
		}
		return true
	}
	return false
}

func parseEgressHostRule(entry string) (egressHostRule, error) {
	raw := strings.TrimSpace(entry)
	if raw == "" {
		return egressHostRule{}, errors.New("空条目")
	}
	if strings.ContainsAny(raw, "*?") {
		return egressHostRule{}, errors.New("不允许通配或查询字符")
	}
	if strings.ContainsAny(raw, " \t\r\n\\") {
		return egressHostRule{}, errors.New("含空白或非法字符")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return egressHostRule{}, errors.New("不是合法 URL")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return egressHostRule{}, errors.New("只允许 https")
	}
	if u.User != nil {
		return egressHostRule{}, errors.New("不允许 userinfo")
	}
	if strings.TrimSpace(u.Host) == "" {
		return egressHostRule{}, errors.New("缺少主机")
	}
	if u.Path != "" && u.Path != "/" {
		return egressHostRule{}, errors.New("只允许主机与端口，不允许路径")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return egressHostRule{}, errors.New("不允许查询或片段")
	}
	host := normalizeEgressHost(u.Hostname())
	if host == "" || strings.Contains(host, "..") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return egressHostRule{}, errors.New("主机名不合法")
	}
	if strings.Contains(host, "%") {
		return egressHostRule{}, errors.New("不允许 IPv6 zone")
	}
	for _, r := range host {
		if r > 0x7f {
			return egressHostRule{}, errors.New("主机名必须是 ASCII")
		}
	}
	port, err := normalizeEgressPort(u.Port())
	if err != nil {
		return egressHostRule{}, err
	}
	return egressHostRule{host: host, port: port}, nil
}

func normalizeEgressHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	return strings.TrimSuffix(h, ".")
}

func normalizeEgressPort(port string) (string, error) {
	p := strings.TrimSpace(port)
	if p == "" {
		return "443", nil
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("端口不合法")
	}
	return strconv.Itoa(n), nil
}

// ---- 目的地地址校验（保存与展示共用） ----
//
// V2 不再用部署主机名单做放行判断，但下面这些检查全部保留：
//   - 只接受 http/https；真正发送仍要求 HTTPS（CheckEgressURL）；
//   - 不得包含 userinfo（http://user:pass@host）；
//   - 不得在查询串里夹带密钥类参数；
//   - 页面只回显「可安全展示」的地址（去查询串与片段）与规范化目的域。
//
// 解析地址 / 重定向 / 证书保护在 aibridge 的 Transport 层，这里只做输入口径。

// destSecretQueryParams 是地址查询串里不允许出现的参数名（小写）。
var destSecretQueryParams = []string{
	"api_key", "apikey", "key", "token", "access_token", "secret", "password", "passwd", "authorization",
}

// ValidateDestinationURL 校验后台保存的厂商地址。
// normalized 是去首尾空白后的原值（保留路径与查询串）；display 去掉查询串与片段，
// 只用于页面回显，不改变已保存的值。
func ValidateDestinationURL(raw string) (normalized, display string, err error) {
	normalized = strings.TrimSpace(raw)
	if normalized == "" {
		return "", "", nil
	}
	u, perr := url.Parse(normalized)
	if perr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", errors.New("厂商地址必须是 http(s) URL")
	}
	if u.User != nil {
		return "", "", errors.New("厂商地址不能包含用户名或密码")
	}
	for name := range u.Query() {
		lower := strings.ToLower(strings.TrimSpace(name))
		for _, banned := range destSecretQueryParams {
			if lower == banned {
				return "", "", errors.New("厂商地址的查询参数不能携带密钥或令牌")
			}
		}
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return "", "", errors.New("厂商地址缺少主机名")
	}
	display = RedactURLForDisplay(normalized)
	return normalized, display, nil
}

// RedactURLForDisplay 去掉地址里的查询串与片段，避免把可能夹带的一次性参数
// 回显到页面、日志或审计里。解析失败时返回主机为空的安全占位。
func RedactURLForDisplay(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	return u.String()
}

// DestinationHost 返回规范化后的目的域（小写主机名 + 非缺省端口）。
// 解析失败返回空串；页面状态与自动记录只用它，不回显完整地址。
func DestinationHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || strings.TrimSpace(u.Hostname()) == "" {
		return ""
	}
	host := normalizeEgressHost(u.Hostname())
	port, perr := normalizeEgressPort(u.Port())
	if perr != nil {
		return ""
	}
	if port == "443" && strings.EqualFold(u.Scheme, "https") {
		return host
	}
	if port == "80" && strings.EqualFold(u.Scheme, "http") {
		return host
	}
	return host + ":" + port
}

// SameDestinationOrigin 判断两个地址是否同源（scheme + 主机 + 有效端口）。
// 留空 Key 只允许在同源下沿用既有凭据；换源必须补目标 Key。
func SameDestinationOrigin(a, b string) bool {
	ua, erra := url.Parse(strings.TrimSpace(a))
	ub, errb := url.Parse(strings.TrimSpace(b))
	if erra != nil || errb != nil {
		return false
	}
	if !strings.EqualFold(ua.Scheme, ub.Scheme) {
		return false
	}
	if normalizeEgressHost(ua.Hostname()) != normalizeEgressHost(ub.Hostname()) {
		return false
	}
	pa, erra := normalizeEgressPort(ua.Port())
	pb, errb := normalizeEgressPort(ub.Port())
	return erra == nil && errb == nil && pa == pb
}

// CheckEgressURL 是真正发送前对请求目标的 URL 级检查：
// 必须 HTTPS、不含 userinfo、主机名合法。部署主机名单已不作为放行条件，
// 但解析地址与证书保护仍在 Transport 层执行。
func CheckEgressURL(u *url.URL) error {
	if u == nil || strings.TrimSpace(u.Host) == "" {
		return hostPolicyDenied(StateEgressHostDenied, "关（出口地址不合法）")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return hostPolicyDenied(StateEgressSchemeDenied, "关（出口地址必须使用 HTTPS）")
	}
	if u.User != nil {
		return hostPolicyDenied(StateEgressUserinfoDenied, "关（出口地址不得包含用户信息）")
	}
	host := normalizeEgressHost(u.Hostname())
	if host == "" || strings.Contains(host, "..") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return hostPolicyDenied(StateEgressHostDenied, "关（出口地址不合法）")
	}
	if _, err := normalizeEgressPort(u.Port()); err != nil {
		return hostPolicyDenied(StateEgressHostDenied, "关（出口端口不合法）")
	}
	return nil
}
