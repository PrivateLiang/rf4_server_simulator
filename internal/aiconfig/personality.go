package aiconfig

// 性格四维的唯一声明点：定义、存取边界、共享派生与同版表达编译都在这里。
//
// 为什么集中在一个文件：四维真源要同时喂「行为选择」与「表达约束」，
// 两份定义一旦分开就会漂移（界面写 0..32 偏低、公式按 33 分界）；派生参数
// 也不能按角色各存一份。本包只做纯函数与校验，不读写数据库、不导入
// store / admin / aibridge，避免后台、桥接与游戏服各写一套口径。
//
// 边界（T-103）：
//   - 真源只有一份：每 Agent 一条 {schema_version, revision, generation, 四维}；
//   - 缺值 → 四维各 50（中性），派生仍可用，但表达不加约束；
//   - 显式非法 → 保存拒绝，读取标记需修复，且不得产出新的派生决定；
//   - 旧 reply_mode / talkativeness / 各旧概率一律不是本包输入；
//   - 时间长度只由类型级 tunables 决定，没有每角色低层覆盖。

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
)

// 四维的存储/定义版本与取值域。schema 变化时旧记录一律按需修复处理。
const (
	PersonalitySchemaVersion = 1

	// PersonalityNeutral 是缺值时的中性默认值：四维同值。
	PersonalityNeutral = 50

	// 取值域与表达分段：0..32 偏低、33..67 适中、68..100 偏高。
	PersonalityMin    = 0
	PersonalityMax    = 100
	PersonalityLowMax = 32
	PersonalityMidMax = 67
)

// 四个维度 ID。只有这四个；不引入外向度、大五或情绪标签。
const (
	DimensionSociability = "sociability"
	DimensionExploration = "exploration"
	DimensionPersistence = "persistence"
	DimensionRegularity  = "regularity"
)

// 表达分段。Band 是给机器看的枚举，中文标签只在这里写一次。
const (
	PersonalityBandLow  = "low"
	PersonalityBandMid  = "mid"
	PersonalityBandHigh = "high"
)

// 保留候选 ID：沉默与维持必须作为候选参与比较，不能靠「候选集为空」代替。
// 调度层把它们放进候选集，权重照算；本包不特殊对待它们。
const (
	CandidateSilence = "silent"
	CandidateKeep    = "keep"
)

// ErrPersonalityInvalid 表示一条性格配置非法（超域、未知版本、坏 JSON）。
// 保存路径必须把它原样报给管理员，不能钳制后落库。
var ErrPersonalityInvalid = errors.New("aiconfig: 性格配置非法")

// PersonalityDimension 是一维的完整定义：ID、中文标签、两端与三段行为约束。
//
// 界面端点、存储取值、派生权重与表达编译都从这里取，禁止另写一份标签或阈值。
type PersonalityDimension struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	LowEnd   string `json:"low_end"`
	HighEnd  string `json:"high_end"`
	LowBand  string `json:"low_band"`
	MidBand  string `json:"mid_band"`
	HighBand string `json:"high_band"`
}

// personalityDimensions 是四维定义本身，顺序即界面顺序。
var personalityDimensions = []PersonalityDimension{
	{
		ID: DimensionSociability, Label: "交往意愿",
		LowEnd: "独处优先", HighEnd: "乐于回应邀请",
		LowBand:  "交往意愿偏低：更愿意一个人待着，不主动约人，被直接邀请时才回应。",
		MidBand:  "交往意愿适中：有人叫就一起，没人叫就自己钓。",
		HighBand: "交往意愿偏高：倾向和钓友一起行动，被邀请时爽快答应，也愿意主动凑过去。",
	},
	{
		ID: DimensionExploration, Label: "尝新意愿",
		LowEnd: "熟悉目标", HighEnd: "探索新目标",
		LowBand:  "尝新意愿偏低：守在熟悉的目标和钓点，不轻易换新地方。",
		MidBand:  "尝新意愿适中：熟悉的地方钓得动就继续，有合适机会才换。",
		HighBand: "尝新意愿偏高：愿意尝试新目标和新钓点，哪怕还没验证过。",
	},
	{
		ID: DimensionPersistence, Label: "持续投入",
		LowEnd: "做够就换", HighEnd: "再多留一会",
		LowBand:  "持续投入偏低：一段事做到够就收，不硬撑。",
		MidBand:  "持续投入适中：做到差不多再决定要不要多留一会。",
		HighBand: "持续投入偏高：愿意在一件事上多留一会，不轻易提前收竿。",
	},
	{
		ID: DimensionRegularity, Label: "作息规律",
		LowEnd: "灵活休整", HighEnd: "偏好固定作息",
		LowBand:  "作息灵活：累了就休整，不看固定时段。",
		MidBand:  "作息大致有节奏：累了会休整，偶尔按熟悉的时段来。",
		HighBand: "作息偏好固定：倾向在固定的时段休息和出门。",
	},
}

// PersonalityDimensions 返回四维定义的副本（固定顺序）。
func PersonalityDimensions() []PersonalityDimension {
	return append([]PersonalityDimension(nil), personalityDimensions...)
}

// PersonalityDimensionByID 按 ID 取一维定义；未知 ID ok=false。
func PersonalityDimensionByID(id string) (PersonalityDimension, bool) {
	for _, d := range personalityDimensions {
		if d.ID == id {
			return d, true
		}
	}
	return PersonalityDimension{}, false
}

// PersonalityConfig 是按 Agent 保存的唯一一条值。
//
// JSON 形状即票面约定：schema_version + 修订 + 代次 + 四维，四维平铺。
// 派生行为与派生表达都不另存可编辑副本。
type PersonalityConfig struct {
	SchemaVersion int   `json:"schema_version"`
	Revision      int64 `json:"revision"`
	Generation    int64 `json:"generation"`
	Sociability   int   `json:"sociability"`
	Exploration   int   `json:"exploration"`
	Persistence   int   `json:"persistence"`
	Regularity    int   `json:"regularity"`
}

// DefaultPersonality 返回缺值时的中性配置：四维 50、修订 0。
func DefaultPersonality() PersonalityConfig {
	return PersonalityConfig{
		SchemaVersion: PersonalitySchemaVersion,
		Sociability:   PersonalityNeutral,
		Exploration:   PersonalityNeutral,
		Persistence:   PersonalityNeutral,
		Regularity:    PersonalityNeutral,
	}
}

// Validate 检查版本与四维取值域；越界一律报错，不钳制。
func (c PersonalityConfig) Validate() error {
	if c.SchemaVersion != PersonalitySchemaVersion {
		return fmt.Errorf("%w: 未知 schema_version=%d", ErrPersonalityInvalid, c.SchemaVersion)
	}
	if c.Revision < 0 || c.Generation < 0 {
		return fmt.Errorf("%w: revision=%d generation=%d", ErrPersonalityInvalid, c.Revision, c.Generation)
	}
	for _, d := range personalityDimensions {
		v, _ := c.ValueOf(d.ID)
		if v < PersonalityMin || v > PersonalityMax {
			return fmt.Errorf("%w: %s=%d 超出 %d..%d",
				ErrPersonalityInvalid, d.ID, v, PersonalityMin, PersonalityMax)
		}
	}
	return nil
}

// ValueOf 按维度 ID 取当前取值；未知 ID ok=false。
func (c PersonalityConfig) ValueOf(id string) (int, bool) {
	switch id {
	case DimensionSociability:
		return c.Sociability, true
	case DimensionExploration:
		return c.Exploration, true
	case DimensionPersistence:
		return c.Persistence, true
	case DimensionRegularity:
		return c.Regularity, true
	}
	return 0, false
}

// WithValue 返回改一维后的副本；未知 ID 报错，越界交给 Validate。
func (c PersonalityConfig) WithValue(id string, v int) (PersonalityConfig, error) {
	switch id {
	case DimensionSociability:
		c.Sociability = v
	case DimensionExploration:
		c.Exploration = v
	case DimensionPersistence:
		c.Persistence = v
	case DimensionRegularity:
		c.Regularity = v
	default:
		return PersonalityConfig{}, fmt.Errorf("%w: 未知维度 %q", ErrPersonalityInvalid, id)
	}
	return c, nil
}

// PersonalityBandOf 返回取值所属的表达分段。调用方应先 Validate。
func PersonalityBandOf(v int) string {
	switch {
	case v <= PersonalityLowMax:
		return PersonalityBandLow
	case v <= PersonalityMidMax:
		return PersonalityBandMid
	default:
		return PersonalityBandHigh
	}
}

// bandLabel 是分段的中文标签（只此一份）。
func bandLabel(band string) string {
	switch band {
	case PersonalityBandLow:
		return "偏低"
	case PersonalityBandMid:
		return "适中"
	case PersonalityBandHigh:
		return "偏高"
	}
	return "未知"
}

// bandText 返回该分段的行为约束句。
func (d PersonalityDimension) bandText(band string) string {
	switch band {
	case PersonalityBandLow:
		return d.LowBand
	case PersonalityBandHigh:
		return d.HighBand
	default:
		return d.MidBand
	}
}

// PersonalityDimensionValue 是一维在某个配置下的当前取值与所属分段。
type PersonalityDimensionValue struct {
	PersonalityDimension
	Value int    `json:"value"`
	Band  string `json:"band"`
	Text  string `json:"text"`
}

// DimensionValues 按定义顺序返回四维当前取值、分段与约束句。
func (c PersonalityConfig) DimensionValues() []PersonalityDimensionValue {
	out := make([]PersonalityDimensionValue, 0, len(personalityDimensions))
	for _, d := range personalityDimensions {
		v, _ := c.ValueOf(d.ID)
		band := PersonalityBandOf(v)
		out = append(out, PersonalityDimensionValue{
			PersonalityDimension: d, Value: v, Band: band, Text: d.bandText(band),
		})
	}
	return out
}

// personalityRequiredFields 是存值里必须出现的字段，顺序即 JSON 形状顺序。
//
// 七个字段缺一不可：少了任何一个都不能按「合法零值」处理，否则「没配过」
// 和「配置写坏了」在数据上不可区分。
var personalityRequiredFields = []string{
	"schema_version", "revision", "generation",
	"sociability", "exploration", "persistence", "regularity",
}

// ParsePersonality 解析 ai_setting 里存的一条 JSON 值。
//
// present=false 表示缺键（不是错误，返回中性默认）；err 非 nil 表示显式配置
// 非法但必须原样保留：能解析出的取值仍会返回，供界面显示「修复哪一项」。
//
// 判定比 json.Unmarshal 严格：七个字段必须都在场且都是整数（null、小数、
// 字符串都算非法）。缺失字段不能退化成合法的 0——那会把「写坏了」当成
// 「显式调成 0」参与派生。
func ParsePersonality(raw string) (cfg PersonalityConfig, present bool, err error) {
	if strings.TrimSpace(raw) == "" {
		return DefaultPersonality(), false, nil
	}
	var fields map[string]json.RawMessage
	if uerr := json.Unmarshal([]byte(raw), &fields); uerr != nil {
		return DefaultPersonality(), true,
			fmt.Errorf("%w: %v", ErrPersonalityInvalid, uerr)
	}

	// 先按「能读出多少读多少」填值，再判非法：原值仍要可查（尤其修订号）。
	var decoded PersonalityConfig
	var firstErr error
	note := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}
	for _, name := range personalityRequiredFields {
		v, derr := decodePersonalityInt(fields[name], name)
		if derr != nil {
			note(derr)
			continue
		}
		switch name {
		case "schema_version":
			decoded.SchemaVersion = int(v)
		case "revision":
			decoded.Revision = v
		case "generation":
			decoded.Generation = v
		case "sociability":
			decoded.Sociability = int(v)
		case "exploration":
			decoded.Exploration = int(v)
		case "persistence":
			decoded.Persistence = int(v)
		case "regularity":
			decoded.Regularity = int(v)
		}
	}
	if firstErr == nil {
		switch {
		case decoded.Revision < 0:
			note(fmt.Errorf("%w: revision=%d 不能为负", ErrPersonalityInvalid, decoded.Revision))
		case decoded.Generation < 0:
			note(fmt.Errorf("%w: generation=%d 不能为负", ErrPersonalityInvalid, decoded.Generation))
		default:
			note(decoded.Validate())
		}
	}

	// 读侧给出的「可提交修订」与写侧比较 base_revision 的口径必须是同一个：
	// 读不出时这里是 0，写侧也按 0 比较，修复才不会互相打架。
	cfg = decoded
	cfg.Revision, _ = PersonalityRevisionOf(raw)
	return cfg, true, firstErr
}

// decodePersonalityInt 严格读一个整数字段：缺字段、null、小数、字符串一律报非法。
//
// 不能用 json.Unmarshal 直接塞进 struct：那样「字段缺失」会静默留成 0，
// 和「显式写了 0」从数据上不可区分。
func decodePersonalityInt(raw json.RawMessage, name string) (int64, error) {
	if len(raw) == 0 {
		return 0, fmt.Errorf("%w: 缺必需字段 %s", ErrPersonalityInvalid, name)
	}
	text := strings.TrimSpace(string(raw))
	if text == "null" {
		return 0, fmt.Errorf("%w: %s 不能为 null", ErrPersonalityInvalid, name)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, fmt.Errorf("%w: %s 必须是整数（原始值 %s）", ErrPersonalityInvalid, name, text)
	}
	return v, nil
}

// PersonalityRevisionOf 从一条存值里取「可用于比较」的修订号。
//
// 读侧报给管理员的可提交修订，与写侧比较 base_revision 的口径必须是同一个；
// 两处都只调它，而不是先要求整条配置合法。缺字段、非整数、坏 JSON 或负数
// 一律 ok=false（调用方按 0 处理，即「用 base 0 重写整条记录」）。
func PersonalityRevisionOf(raw string) (int64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return 0, false
	}
	rev, err := decodePersonalityInt(fields["revision"], "revision")
	if err != nil || rev < 0 {
		return 0, false
	}
	return rev, true
}

// MarshalPersonality 序列化一条配置；先校验，非法不落库。
func MarshalPersonality(c PersonalityConfig) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CompilePersonalityExpression 把同一份定义与同一修订编译成提示词里的四句约束。
//
// 输出带 schema 与修订号：调用方据此证明表达与行为读到的是同一个版本。
// 人设文本仍只放背景与口癖；这里不另设「外向度」之类的第二套标签。
func CompilePersonalityExpression(c PersonalityConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b,
		"性格约束（schema v%d · 修订 %d，只约束说话方式与举止，不改身份、权限和工具）：",
		c.SchemaVersion, c.Revision)
	for _, dv := range c.DimensionValues() {
		fmt.Fprintf(&b, "\n- %s %d/100（%s）：%s",
			dv.Label, dv.Value, bandLabel(dv.Band), dv.Text)
	}
	b.WriteString("\n人设描述只提供背景和口癖；上面四条与描述冲突时，按上面四条说话，不要自己另设一套性格。")
	return b.String()
}

// PersonalityFeature 是一个候选在当下事实里的四项语义特征，各自 0..1。
//
// 特征来自真实活动/邀请事实，不按角色各写一份；没有第二套可调权重。
type PersonalityFeature struct {
	Social     float64 `json:"social"`
	Novelty    float64 `json:"novelty"`
	Investment float64 `json:"investment"`
	Rhythm     float64 `json:"rhythm"`
}

// PersonalityFeatureNeutral 是「未知事实」的中性取法：所有候选同值，
// 四个维度的影响互相抵消，权重退化为均匀。
var PersonalityFeatureNeutral = PersonalityFeature{Social: 0.5, Novelty: 0.5, Investment: 0.5, Rhythm: 0.5}

// normalized 把特征夹到 0..1。事实侧越界不是配置错误，按端点处理即可。
func (f PersonalityFeature) normalized() PersonalityFeature {
	clamp := func(v float64) float64 {
		if math.IsNaN(v) {
			return 0
		}
		return math.Max(0, math.Min(1, v))
	}
	return PersonalityFeature{
		Social:     clamp(f.Social),
		Novelty:    clamp(f.Novelty),
		Investment: clamp(f.Investment),
		Rhythm:     clamp(f.Rhythm),
	}
}

// PersonalityCandidate 是一个候选：稳定 ID + 是否可行 + 真实语义特征。
//
// 不可行（配额、时长、冷却、权限已先排除）的候选权重恒为 0；
// 沉默/维持也作为普通候选参与比较，本包不认识它们的特殊语义。
type PersonalityCandidate struct {
	ID       string             `json:"id"`
	Feasible bool               `json:"feasible"`
	Feature  PersonalityFeature `json:"feature"`
}

// PersonalityWeight 是一个候选的得分与归一化权重（稳定顺序，按 ID 升序）。
type PersonalityWeight struct {
	ID     string  `json:"id"`
	Score  float64 `json:"score"`
	Weight float64 `json:"weight"`
}

// PersonalityChoice 是一次共享派生的结果：所用版本 + 权重 + 抽中的候选。
type PersonalityChoice struct {
	SchemaVersion int                 `json:"schema_version"`
	Revision      int64               `json:"revision"`
	Weights       []PersonalityWeight `json:"weights"`
	ChosenID      string              `json:"chosen_id"`
	Decided       bool                `json:"decided"`
	Reason        string              `json:"reason"`
}

// 派生失败的原因（不产出任何动作）。
const (
	ChoiceNoFeasible    = "no_feasible"
	ChoiceDuplicateID   = "duplicate_id"
	ChoiceInvalidConfig = "invalid_config"
)

// Score 计算候选得分 z=Σ(2x−1)f/2，x=值/100。
func (c PersonalityConfig) Score(f PersonalityFeature) float64 {
	f = f.normalized()
	term := func(value int, feature float64) float64 {
		x := float64(value) / float64(PersonalityMax)
		return (2*x - 1) * feature / 2
	}
	return term(c.Sociability, f.Social) +
		term(c.Exploration, f.Novelty) +
		term(c.Persistence, f.Investment) +
		term(c.Regularity, f.Rhythm)
}

// DecidePersonality 在可行候选上按 softmax(z) 抽样，种子决定唯一结果。
//
// 输入只有：同版四维、可行候选与语义特征、稳定候选 ID、受控随机种子。
// 输出带所用 schema 与修订号。不可行候选权重为 0；没有可行候选时
// Decided=false（调用方不得补默认动作）；重复 ID 视为调用方错误，同样拒绝。
func DecidePersonality(c PersonalityConfig, cands []PersonalityCandidate, seed int64) PersonalityChoice {
	out := PersonalityChoice{
		SchemaVersion: c.SchemaVersion,
		Revision:      c.Revision,
		Weights:       make([]PersonalityWeight, 0, len(cands)),
	}
	if err := c.Validate(); err != nil {
		out.Reason = ChoiceInvalidConfig
		return out
	}
	// 稳定顺序：先按 ID 排序，逆序传入也得到同一结果。
	ordered := append([]PersonalityCandidate(nil), cands...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for i := 1; i < len(ordered); i++ {
		if ordered[i].ID == ordered[i-1].ID {
			out.Reason = ChoiceDuplicateID
			return out
		}
	}

	feasible := make([]PersonalityWeight, 0, len(ordered))
	var maxScore float64
	for _, cand := range ordered {
		w := PersonalityWeight{ID: cand.ID}
		if cand.Feasible {
			w.Score = c.Score(cand.Feature)
			if len(feasible) == 0 || w.Score > maxScore {
				maxScore = w.Score
			}
			feasible = append(feasible, w)
		}
		out.Weights = append(out.Weights, w)
	}
	if len(feasible) == 0 {
		out.Reason = ChoiceNoFeasible
		return out
	}

	var sum float64
	for i := range feasible {
		feasible[i].Weight = math.Exp(feasible[i].Score - maxScore)
		sum += feasible[i].Weight
	}
	for i := range feasible {
		feasible[i].Weight /= sum
	}
	byID := make(map[string]float64, len(feasible))
	for _, w := range feasible {
		byID[w.ID] = w.Weight
	}
	for i := range out.Weights {
		if w, ok := byID[out.Weights[i].ID]; ok {
			out.Weights[i].Weight = w
		}
	}

	// 单次抽签：同种子同输入得到同一个候选，与候选传入顺序无关。
	draw := rand.New(rand.NewSource(seed)).Float64()
	cumulative := 0.0
	chosen := feasible[len(feasible)-1].ID
	for _, w := range feasible {
		cumulative += w.Weight
		if draw < cumulative {
			chosen = w.ID
			break
		}
	}
	out.ChosenID = chosen
	out.Decided = true
	return out
}

// PersonalityTunables 是类型级派生参数，只有这三个基数。
//
// 它们是活动类型的基线，不是每角色配置，也不代表新增时长真源：
// 默认继承现有活动时长配置（硬底线、可延长幅度、休整基数）。
type PersonalityTunables struct {
	HardFloorSeconds float64 `json:"hard_floor_seconds"`
	ExtraSeconds     float64 `json:"extra_seconds"`
	RestBaseSeconds  float64 `json:"rest_base_seconds"`
}

func (t PersonalityTunables) normalized() PersonalityTunables {
	clamp := func(v float64) float64 {
		if math.IsNaN(v) || v < 0 {
			return 0
		}
		return v
	}
	return PersonalityTunables{
		HardFloorSeconds: clamp(t.HardFloorSeconds),
		ExtraSeconds:     clamp(t.ExtraSeconds),
		RestBaseSeconds:  clamp(t.RestBaseSeconds),
	}
}

// DwellFloorSeconds 计算 Dmin=Bhard+Bextra×p̂×(0.5+0.5r̂)。
//
// 只加不减：p̂ 越高底线越长；r̂ 只通过 0.5+0.5r̂ 项放大或持平，不会缩短。
func (c PersonalityConfig) DwellFloorSeconds(t PersonalityTunables) float64 {
	t = t.normalized()
	p := float64(c.Persistence) / float64(PersonalityMax)
	r := float64(c.Regularity) / float64(PersonalityMax)
	return t.HardFloorSeconds + t.ExtraSeconds*p*(0.5+0.5*r)
}

// RestIntervalSeconds 计算 Trest=Tbase×(1+0.5r̂)。
//
// 只影响叙事休整节拍，不改能量或世界钟。
func (c PersonalityConfig) RestIntervalSeconds(t PersonalityTunables) float64 {
	t = t.normalized()
	r := float64(c.Regularity) / float64(PersonalityMax)
	return t.RestBaseSeconds * (1 + 0.5*r)
}

// ---- 评分草稿的取值契约（T-107） ----
//
// 评分只产出「草稿」：模型给四个建议值与建议区间，管理员逐项或全部撤销，
// 只有显式保存才落库。所以这里全部是纯函数：草稿的域校验、短理由边界，以及
// 一次性问模型的提示词。模型往返与严格 JSON 解析在 aibridge，鉴权与路由在
// admin；本包不新增第二套维度、阈值或概率。
//
// 建议区间是「把握范围」而不是权限：域内越出建议范围仍可保存（页面给黄色提示），
// 超域或非整数才拒绝。自然语言永远不是成功结果——模型必须给结构化对象。

const (
	// PersonalityScoreStatusOK 是「有建议」的唯一状态值。
	PersonalityScoreStatusOK = "ok"
	// PersonalityScoreStatusUnassessable 是「无法评估」的状态值：不猜、不补值。
	PersonalityScoreStatusUnassessable = "unassessable"

	// PersonalityScoreUnassessableText 是「无法评估」的唯一文案。
	// 不把模型的原话转给管理员：原话可能夹带自然语言结论或越权建议。
	PersonalityScoreUnassessableText = "请审查提示词：描述不足或互相矛盾"

	// PersonalityScoreReasonMaxRunes 是单条短理由的字数上限。
	PersonalityScoreReasonMaxRunes = 40

	// PersonalityScoreDescriptionLimit 是送进提示词的描述字数上限。
	// 角色描述真源仍在角色页；这里只截断输入，不另存一份。
	PersonalityScoreDescriptionLimit = 4000

	// PersonalityScoreRequestIDMaxRunes 是评分请求关联 ID 的长度上限。
	// 请求 ID 由后台页面生成，只用于丢弃迟到的旧响应，不落库。
	PersonalityScoreRequestIDMaxRunes = 64
)

// ErrPersonalityScoreInvalid 表示一条评分草稿/请求不合约（超域、缺项、区间倒置、
// 理由为空或过长）。调用方必须原样拒绝，不得钳制后当成合法建议回填。
var ErrPersonalityScoreInvalid = errors.New("aiconfig: 评分草稿非法")

// PersonalityBand 是一段表达区间的边界与中文标签。
//
// 后台渲染「当前值属于哪一段」与建议区间时只需要这三个边界：写在这里，
// 页面与接口不再各抄一份 32/67 阈值或「偏低/适中/偏高」文案。
type PersonalityBand struct {
	Band  string `json:"band"`
	Label string `json:"label"`
	Min   int    `json:"min"`
	Max   int    `json:"max"`
}

// PersonalityBands 返回三段固定区间（只此一份）：0..32 偏低、33..67 适中、68..100 偏高。
func PersonalityBands() []PersonalityBand {
	return []PersonalityBand{
		{Band: PersonalityBandLow, Label: bandLabel(PersonalityBandLow), Min: PersonalityMin, Max: PersonalityLowMax},
		{Band: PersonalityBandMid, Label: bandLabel(PersonalityBandMid), Min: PersonalityLowMax + 1, Max: PersonalityMidMax},
		{Band: PersonalityBandHigh, Label: bandLabel(PersonalityBandHigh), Min: PersonalityMidMax + 1, Max: PersonalityMax},
	}
}

// PersonalityScoreValues 是四维的「建议值」：只有四个整数，不带修订与代次。
//
// 与 PersonalityConfig 分开：草稿不是配置，混进修订号会让「草稿未保存」和
// 「已保存的修订」在数据上不可区分。
type PersonalityScoreValues struct {
	Sociability int `json:"sociability"`
	Exploration int `json:"exploration"`
	Persistence int `json:"persistence"`
	Regularity  int `json:"regularity"`
}

// ScoreValues 取当前配置的四维值（不带修订/代次），作为评分输入里的「当前值」。
func (c PersonalityConfig) ScoreValues() PersonalityScoreValues {
	return PersonalityScoreValues{
		Sociability: c.Sociability,
		Exploration: c.Exploration,
		Persistence: c.Persistence,
		Regularity:  c.Regularity,
	}
}

// ValueOf 按维度 ID 取建议值；未知 ID ok=false。
func (v PersonalityScoreValues) ValueOf(id string) (int, bool) {
	switch id {
	case DimensionSociability:
		return v.Sociability, true
	case DimensionExploration:
		return v.Exploration, true
	case DimensionPersistence:
		return v.Persistence, true
	case DimensionRegularity:
		return v.Regularity, true
	}
	return 0, false
}

// Validate 检查四维都在 0..100；越界一律报错，不钳制。
func (v PersonalityScoreValues) Validate() error {
	for _, d := range personalityDimensions {
		got, _ := v.ValueOf(d.ID)
		if got < PersonalityMin || got > PersonalityMax {
			return fmt.Errorf("%w: %s=%d 超出 %d..%d",
				ErrPersonalityScoreInvalid, d.ID, got, PersonalityMin, PersonalityMax)
		}
	}
	return nil
}

// DecodePersonalityScoreValues 严格解析四维 JSON 对象。
//
// 不能直接把 JSON 塞进结构体：缺字段会静默留成 0，而 0 是合法取值，
// 「没填」和「显式调成 0」就分不开了。所以四个维度缺一不可，值必须是整数，
// 且不接受任何额外键（第二套维度、概率或配额都不许从这里进来）。
func DecodePersonalityScoreValues(raw json.RawMessage) (PersonalityScoreValues, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return PersonalityScoreValues{}, fmt.Errorf("%w: 不是 JSON 对象：%v", ErrPersonalityScoreInvalid, err)
	}
	out := PersonalityScoreValues{}
	for _, d := range personalityDimensions {
		field, ok := fields[d.ID]
		if !ok {
			return PersonalityScoreValues{}, fmt.Errorf("%w: 缺维度 %s", ErrPersonalityScoreInvalid, d.ID)
		}
		value, err := decodePersonalityInt(field, d.ID)
		if err != nil {
			return PersonalityScoreValues{}, fmt.Errorf("%w: %v", ErrPersonalityScoreInvalid, err)
		}
		if value < PersonalityMin || value > PersonalityMax {
			return PersonalityScoreValues{}, fmt.Errorf("%w: %s=%d 超出 %d..%d",
				ErrPersonalityScoreInvalid, d.ID, value, PersonalityMin, PersonalityMax)
		}
		switch d.ID {
		case DimensionSociability:
			out.Sociability = int(value)
		case DimensionExploration:
			out.Exploration = int(value)
		case DimensionPersistence:
			out.Persistence = int(value)
		case DimensionRegularity:
			out.Regularity = int(value)
		}
	}
	for key := range fields {
		if _, ok := PersonalityDimensionByID(key); !ok {
			return PersonalityScoreValues{}, fmt.Errorf("%w: 未知字段 %q", ErrPersonalityScoreInvalid, key)
		}
	}
	return out, nil
}

// PersonalityScoreRange 是一维的建议区间，按票面形状编码成 [min,max] 两元数组。
//
// 不用 {min,max} 对象：模型对两元数组更不容易写错键名，也省掉一次「字段名猜错
// 但结构合法」的歧义。
type PersonalityScoreRange [2]int

// Min / Max 取区间两端。
func (r PersonalityScoreRange) Min() int { return r[0] }
func (r PersonalityScoreRange) Max() int { return r[1] }

// Validate 检查 0 ≤ min ≤ max ≤ 100。
func (r PersonalityScoreRange) Validate() error {
	if r[0] < PersonalityMin || r[1] > PersonalityMax || r[0] > r[1] {
		return fmt.Errorf("%w: 建议区间 [%d,%d] 需满足 0 ≤ min ≤ max ≤ 100",
			ErrPersonalityScoreInvalid, r[0], r[1])
	}
	return nil
}

// Contains 报告某个取值是否落在区间内。
func (r PersonalityScoreRange) Contains(v int) bool { return v >= r[0] && v <= r[1] }

// PersonalityScoreRanges 是四维建议区间，键名与四维 ID 一致。
type PersonalityScoreRanges struct {
	Sociability PersonalityScoreRange `json:"sociability"`
	Exploration PersonalityScoreRange `json:"exploration"`
	Persistence PersonalityScoreRange `json:"persistence"`
	Regularity  PersonalityScoreRange `json:"regularity"`
}

// RangeOf 按维度 ID 取建议区间；未知 ID ok=false。
func (r PersonalityScoreRanges) RangeOf(id string) (PersonalityScoreRange, bool) {
	switch id {
	case DimensionSociability:
		return r.Sociability, true
	case DimensionExploration:
		return r.Exploration, true
	case DimensionPersistence:
		return r.Persistence, true
	case DimensionRegularity:
		return r.Regularity, true
	}
	return PersonalityScoreRange{}, false
}

// PersonalityScoreReasons 是四维短理由，键名与四维 ID 一致。
type PersonalityScoreReasons struct {
	Sociability string `json:"sociability"`
	Exploration string `json:"exploration"`
	Persistence string `json:"persistence"`
	Regularity  string `json:"regularity"`
}

// ReasonOf 按维度 ID 取短理由；未知 ID ok=false。
func (r PersonalityScoreReasons) ReasonOf(id string) (string, bool) {
	switch id {
	case DimensionSociability:
		return r.Sociability, true
	case DimensionExploration:
		return r.Exploration, true
	case DimensionPersistence:
		return r.Persistence, true
	case DimensionRegularity:
		return r.Regularity, true
	}
	return "", false
}

// PersonalityScoreDraft 是一条「有建议」的评分草稿（status=ok）。
//
// unassessable 没有草稿：它只有一句固定文案，不带 values/ranges/reasons，
// 这样「无法评估」永远不可能被页面误读成「建议都调成 0」。
type PersonalityScoreDraft struct {
	SchemaVersion int
	Values        PersonalityScoreValues
	Ranges        PersonalityScoreRanges
	Reasons       PersonalityScoreReasons
}

// Validate 校验一条评分草稿：版本、四维域、区间包含建议值、四短理由。
func (d PersonalityScoreDraft) Validate() error {
	if d.SchemaVersion != PersonalitySchemaVersion {
		return fmt.Errorf("%w: 未知 schema_version=%d", ErrPersonalityScoreInvalid, d.SchemaVersion)
	}
	if err := d.Values.Validate(); err != nil {
		return err
	}
	for _, dim := range personalityDimensions {
		value, _ := d.Values.ValueOf(dim.ID)
		rng, _ := d.Ranges.RangeOf(dim.ID)
		if err := rng.Validate(); err != nil {
			return err
		}
		if !rng.Contains(value) {
			return fmt.Errorf("%w: %s=%d 不在建议区间 [%d,%d] 内",
				ErrPersonalityScoreInvalid, dim.ID, value, rng.Min(), rng.Max())
		}
		reason, _ := d.Reasons.ReasonOf(dim.ID)
		if err := ValidatePersonalityScoreReason(reason); err != nil {
			return fmt.Errorf("%w: %s", err, dim.ID)
		}
	}
	return nil
}

// ValidatePersonalityScoreReason 校验一条短理由：去空白后非空、不超过上限、
// 不含换行或其它控制字符（理由只给人看，不能夹带第二段结构化内容）。
func ValidatePersonalityScoreReason(reason string) error {
	text := strings.TrimSpace(reason)
	if text == "" {
		return fmt.Errorf("%w: 理由为空", ErrPersonalityScoreInvalid)
	}
	if n := len([]rune(text)); n > PersonalityScoreReasonMaxRunes {
		return fmt.Errorf("%w: 理由 %d 字超过上限 %d", ErrPersonalityScoreInvalid, n, PersonalityScoreReasonMaxRunes)
	}
	for _, r := range text {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return fmt.Errorf("%w: 理由含控制字符", ErrPersonalityScoreInvalid)
		}
	}
	return nil
}

// PersonalityScoreSystemPrompt 是评分助手的唯一系统提示词。
//
// 维度、端点与顺序全部来自本文开头的定义，不另写一份标签；输出形状写死成
// 一个对象，键名与 PersonalityScoreValues/Ranges/Reasons 的 JSON 键一致。
func PersonalityScoreSystemPrompt() string {
	var b strings.Builder
	b.WriteString("你是游戏后台的「性格评分」助手：只把管理员写好的角色描述翻译成四个生活维度的建议值。\n")
	b.WriteString("只输出一个 JSON 对象。不要解释，不要 Markdown 代码块，不要调用工具，不要给第二套性格标签。\n\n")
	b.WriteString("四个维度（整数 0..100，缺省 50）：\n")
	for _, d := range personalityDimensions {
		fmt.Fprintf(&b, "- %s %s：0 = %s；100 = %s\n", d.ID, d.Label, d.LowEnd, d.HighEnd)
	}
	b.WriteString("\n输出形状（键名固定，四项必须齐全）：\n")
	fmt.Fprintf(&b, "{\"status\":%q,\"values\":{\"sociability\":60,\"exploration\":40,\"persistence\":70,\"regularity\":55},"+
		"\"ranges\":{\"sociability\":[45,70],\"exploration\":[30,55],\"persistence\":[55,80],\"regularity\":[40,70]},"+
		"\"reasons\":{\"sociability\":\"描述里爱主动约人\",\"exploration\":\"愿意尝试新水域\","+
		"\"persistence\":\"做事有耐心\",\"regularity\":\"提到固定作息\"}}\n", PersonalityScoreStatusOK)
	b.WriteString("\n规则：\n")
	fmt.Fprintf(&b, "1. 每维给建议值、建议区间和一条不超过 %d 字的理由；区间必须包含建议值：0 ≤ min ≤ value ≤ max ≤ 100。\n",
		PersonalityScoreReasonMaxRunes)
	b.WriteString("2. 理由只引用描述里的证据，不写「可能」「也许」，不给评分之外的结论。\n")
	b.WriteString("3. 描述不足以判断、或两条线索互相矛盾时，只输出 ")
	fmt.Fprintf(&b, "{\"status\":%q}", PersonalityScoreStatusUnassessable)
	b.WriteString("，不要猜，不要给 values、ranges 或 reasons。\n")
	b.WriteString("4. 当前取值只表示「描述已经体现了什么」，不要因为当前值就附和：描述与当前值冲突时按描述建议，并在理由里点出冲突。\n")
	b.WriteString("5. 不要给出能力、工具、事件、权限、概率、配额、时长或任何其它字段。\n")
	return b.String()
}

// PersonalityScoreUserPrompt 组装一次评分请求的用户侧内容。
//
// 描述超长时截断到 PersonalityScoreDescriptionLimit，避免把整份人设原文塞给
// 模型；截断发生在输入侧，不改角色页的描述真源。
func PersonalityScoreUserPrompt(description string, current PersonalityScoreValues) string {
	text := strings.TrimSpace(description)
	if n := []rune(text); len(n) > PersonalityScoreDescriptionLimit {
		text = string(n[:PersonalityScoreDescriptionLimit])
	}
	if text == "" {
		text = "（空：没有可评估的描述）"
	}
	var b strings.Builder
	b.WriteString("角色描述：\n")
	b.WriteString(text)
	b.WriteString("\n\n当前四维：")
	for i, d := range personalityDimensions {
		if i > 0 {
			b.WriteString("、")
		}
		v, _ := current.ValueOf(d.ID)
		fmt.Fprintf(&b, "%s %d", d.Label, v)
	}
	b.WriteString("\n\n请按约定输出一个 JSON 对象。")
	return b.String()
}
