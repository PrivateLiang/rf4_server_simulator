package aibridge

// T-107 评分草稿的模型往返：一次调用、严格解析、只回草稿。
//
// 边界：
//   - 本文件不读写数据库、不鉴权。它只把「角色描述 + 当前四维」发给已配置的
//     厂商一次，并把回复按票面结构解析成草稿；落库只有管理员的显式保存。
//   - 不重试、不递归：一次请求只评估一次。失败就上抛，让调用方保留原草稿。
//   - 自然语言不是成功结果：回复必须是单个 JSON 对象，键名固定、四维齐全；
//     「无法评估」只能是 {status:"unassessable"}，不带 values/ranges/reasons。
//   - 建议区间只是把握范围，不是权限：域内越出区间仍可保存（页面提示），
//     超域或区间倒置属于非法输出，直接拒绝。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"rf4server/internal/aiconfig"
)

const (
	// PersonalityScoreMaxTokens 是评分回复的输出上限：四个整数、四个区间与
	// 四条短理由，700 token 足够；给太多只会鼓励模型写解释。
	PersonalityScoreMaxTokens = 700

	// PersonalityScoreDescriptionMaxRunes 是请求里允许的最大描述字数。
	// 送进提示词的部分按 aiconfig.PersonalityScoreDescriptionLimit 截断；
	// 超过这个上限的请求直接拒绝，避免把整份文档当描述发过来。
	PersonalityScoreDescriptionMaxRunes = 16000
)

var (
	// ErrPersonalityScoreRequest 表示评分请求本身非法（缺字段、未知版本、超域）。
	ErrPersonalityScoreRequest = errors.New("ai: 评分请求非法")
	// ErrPersonalityScoreReply 表示模型回复不符合约定（自然语言、缺项、额外键、
	// 超域、区间倒置、理由不合约）。调用方必须保留草稿并提示重试。
	ErrPersonalityScoreReply = errors.New("ai: 评分回复不符合约定")
)

// PersonalityScoreRequest 是一次评分请求：票面固定的六个输入字段。
//
// RequestID 由后台页面生成，只用于丢弃迟到的旧响应；不落库、不参与鉴权。
type PersonalityScoreRequest struct {
	RequestID              string                          `json:"request_id"`
	AgentKey               string                          `json:"agent_key"`
	BaseRevision           int64                           `json:"base_revision"`
	Description            string                          `json:"description"`
	DimensionSchemaVersion int                             `json:"dimension_schema_version"`
	CurrentValues          aiconfig.PersonalityScoreValues `json:"current_values"`
}

// PersonalityScoreResult 是返回给管理员的一条草稿。
//
// ok 时带 values/ranges/reasons；unassessable 只带固定文案 Reason，不带
// values/ranges/reasons——这样「无法评估」不可能被页面当成「建议调成 0」。
type PersonalityScoreResult struct {
	RequestID     string                            `json:"request_id"`
	BaseRevision  int64                             `json:"base_revision"`
	SchemaVersion int                               `json:"schema_version"`
	Status        string                            `json:"status"`
	Values        *aiconfig.PersonalityScoreValues  `json:"values,omitempty"`
	Ranges        *aiconfig.PersonalityScoreRanges  `json:"ranges,omitempty"`
	Reasons       *aiconfig.PersonalityScoreReasons `json:"reasons,omitempty"`
	Reason        string                            `json:"reason,omitempty"`
}

// Validate 校验请求的六个字段；未知维度版本或超域一律拒绝，不钳制。
func (r PersonalityScoreRequest) Validate() error {
	id := strings.TrimSpace(r.RequestID)
	if id == "" {
		return fmt.Errorf("%w: 缺 request_id", ErrPersonalityScoreRequest)
	}
	if n := len([]rune(id)); n > aiconfig.PersonalityScoreRequestIDMaxRunes {
		return fmt.Errorf("%w: request_id 过长（%d）", ErrPersonalityScoreRequest, n)
	}
	for _, ch := range id {
		if !(ch == '-' || ch == '_' || (ch >= '0' && ch <= '9') ||
			(ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')) {
			return fmt.Errorf("%w: request_id 只允许字母、数字、- 和 _", ErrPersonalityScoreRequest)
		}
	}
	if strings.TrimSpace(r.AgentKey) == "" {
		return fmt.Errorf("%w: 缺 agent_key", ErrPersonalityScoreRequest)
	}
	if r.BaseRevision < 0 {
		return fmt.Errorf("%w: base_revision=%d 不能为负", ErrPersonalityScoreRequest, r.BaseRevision)
	}
	if r.DimensionSchemaVersion != aiconfig.PersonalitySchemaVersion {
		return fmt.Errorf("%w: 未知 dimension_schema_version=%d（当前 %d）",
			ErrPersonalityScoreRequest, r.DimensionSchemaVersion, aiconfig.PersonalitySchemaVersion)
	}
	desc := strings.TrimSpace(r.Description)
	if desc == "" {
		return fmt.Errorf("%w: 描述为空，无法评分", ErrPersonalityScoreRequest)
	}
	if n := len([]rune(desc)); n > PersonalityScoreDescriptionMaxRunes {
		return fmt.Errorf("%w: 描述 %d 字超过上限 %d",
			ErrPersonalityScoreRequest, n, PersonalityScoreDescriptionMaxRunes)
	}
	if err := r.CurrentValues.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrPersonalityScoreRequest, err)
	}
	return nil
}

// personalityScoreReply* 是严格解码用的中间形状：四维全部用指针，
// 以便区分「缺失」与「合法的 0」。
type personalityScoreReplyValues struct {
	Sociability *int `json:"sociability"`
	Exploration *int `json:"exploration"`
	Persistence *int `json:"persistence"`
	Regularity  *int `json:"regularity"`
}

// personalityScoreReplyRanges 用 []int 而不是 [2]int：encoding/json 会静默丢弃
// 多余元素，三元的「区间」会被当成合法的两元区间。这里显式要求恰好两个整数。
type personalityScoreReplyRanges struct {
	Sociability *[]int `json:"sociability"`
	Exploration *[]int `json:"exploration"`
	Persistence *[]int `json:"persistence"`
	Regularity  *[]int `json:"regularity"`
}

type personalityScoreReplyReasons struct {
	Sociability *string `json:"sociability"`
	Exploration *string `json:"exploration"`
	Persistence *string `json:"persistence"`
	Regularity  *string `json:"regularity"`
}

// personalityScoreReply 是模型回复的唯一合法形状。
//
// 用 DisallowUnknownFields 解码：多出任何键（capabilities、tools、events、
// emergency_stop、chance、quota……）都直接拒绝，而不是静默忽略。
type personalityScoreReply struct {
	Status  *string                       `json:"status"`
	Values  *personalityScoreReplyValues  `json:"values"`
	Ranges  *personalityScoreReplyRanges  `json:"ranges"`
	Reasons *personalityScoreReplyReasons `json:"reasons"`
}

// RunPersonalityScore 用已构造的 Provider 发一次评分请求并解析结果。
//
// 只发一次，不重试；超时、网络与厂商错误原样上抛，由调用方决定状态码与文案。
func RunPersonalityScore(ctx context.Context, p Provider, req PersonalityScoreRequest) (PersonalityScoreResult, error) {
	if err := req.Validate(); err != nil {
		return PersonalityScoreResult{}, err
	}
	if p == nil {
		return PersonalityScoreResult{}, fmt.Errorf("%w: 没有可用的厂商", ErrPersonalityScoreRequest)
	}
	resp, err := p.Chat(ctx, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: aiconfig.PersonalityScoreSystemPrompt()},
			{Role: "user", Content: aiconfig.PersonalityScoreUserPrompt(req.Description, req.CurrentValues)},
		},
		MaxTokens: PersonalityScoreMaxTokens,
	})
	if err != nil {
		return PersonalityScoreResult{}, fmt.Errorf("%w: %w", ErrPersonalityScoreReply, err)
	}
	status, draft, err := parsePersonalityScoreReply(resp.Text)
	if err != nil {
		return PersonalityScoreResult{}, err
	}
	out := PersonalityScoreResult{
		RequestID:     strings.TrimSpace(req.RequestID),
		BaseRevision:  req.BaseRevision,
		SchemaVersion: aiconfig.PersonalitySchemaVersion,
		Status:        status,
	}
	if status == aiconfig.PersonalityScoreStatusUnassessable {
		out.Reason = aiconfig.PersonalityScoreUnassessableText
		return out, nil
	}
	out.Values = &draft.Values
	out.Ranges = &draft.Ranges
	out.Reasons = &draft.Reasons
	return out, nil
}

// parsePersonalityScoreReply 把模型原文解析成「有建议」或「无法评估」。
//
// 只容忍一种格式包装：整段就是一个 ``` 代码块（首行允许 json 标记）。
// 其余任何自然语言、尾随内容或多个对象都按非法输出处理。
func parsePersonalityScoreReply(raw string) (string, aiconfig.PersonalityScoreDraft, error) {
	text := stripPersonalityScoreFence(raw)
	if strings.TrimSpace(text) == "" {
		return "", aiconfig.PersonalityScoreDraft{}, fmt.Errorf("%w: 空回复", ErrPersonalityScoreReply)
	}
	var reply personalityScoreReply
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reply); err != nil {
		return "", aiconfig.PersonalityScoreDraft{}, fmt.Errorf("%w: %v", ErrPersonalityScoreReply, err)
	}
	// 单个 JSON 对象之后不允许任何其它内容（第二个对象、解释文字都算）。
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return "", aiconfig.PersonalityScoreDraft{}, fmt.Errorf("%w: JSON 之后还有其它内容", ErrPersonalityScoreReply)
	}
	if reply.Status == nil {
		return "", aiconfig.PersonalityScoreDraft{}, fmt.Errorf("%w: 缺 status", ErrPersonalityScoreReply)
	}
	switch status := strings.TrimSpace(*reply.Status); status {
	case aiconfig.PersonalityScoreStatusUnassessable:
		if reply.Values != nil || reply.Ranges != nil || reply.Reasons != nil {
			return "", aiconfig.PersonalityScoreDraft{}, fmt.Errorf(
				"%w: unassessable 不得带 values/ranges/reasons", ErrPersonalityScoreReply)
		}
		return aiconfig.PersonalityScoreStatusUnassessable, aiconfig.PersonalityScoreDraft{}, nil
	case aiconfig.PersonalityScoreStatusOK:
		draft, err := personalityScoreDraftOf(reply)
		if err != nil {
			return "", aiconfig.PersonalityScoreDraft{}, err
		}
		return aiconfig.PersonalityScoreStatusOK, draft, nil
	default:
		return "", aiconfig.PersonalityScoreDraft{}, fmt.Errorf(
			"%w: 未知 status %q", ErrPersonalityScoreReply, status)
	}
}

// personalityScoreDraftOf 把解码出的指针形状拼成草稿并做完整校验。
func personalityScoreDraftOf(reply personalityScoreReply) (aiconfig.PersonalityScoreDraft, error) {
	if reply.Values == nil || reply.Ranges == nil || reply.Reasons == nil {
		return aiconfig.PersonalityScoreDraft{}, fmt.Errorf(
			"%w: ok 必须同时给 values、ranges、reasons", ErrPersonalityScoreReply)
	}
	values := []struct {
		id  string
		ptr *int
	}{
		{aiconfig.DimensionSociability, reply.Values.Sociability},
		{aiconfig.DimensionExploration, reply.Values.Exploration},
		{aiconfig.DimensionPersistence, reply.Values.Persistence},
		{aiconfig.DimensionRegularity, reply.Values.Regularity},
	}
	ranges := []struct {
		id  string
		ptr *[]int
	}{
		{aiconfig.DimensionSociability, reply.Ranges.Sociability},
		{aiconfig.DimensionExploration, reply.Ranges.Exploration},
		{aiconfig.DimensionPersistence, reply.Ranges.Persistence},
		{aiconfig.DimensionRegularity, reply.Ranges.Regularity},
	}
	reasons := []struct {
		id  string
		ptr *string
	}{
		{aiconfig.DimensionSociability, reply.Reasons.Sociability},
		{aiconfig.DimensionExploration, reply.Reasons.Exploration},
		{aiconfig.DimensionPersistence, reply.Reasons.Persistence},
		{aiconfig.DimensionRegularity, reply.Reasons.Regularity},
	}
	draft := aiconfig.PersonalityScoreDraft{SchemaVersion: aiconfig.PersonalitySchemaVersion}
	for _, item := range values {
		if item.ptr == nil {
			return aiconfig.PersonalityScoreDraft{}, fmt.Errorf(
				"%w: values 缺 %s", ErrPersonalityScoreReply, item.id)
		}
	}
	for _, item := range ranges {
		if item.ptr == nil {
			return aiconfig.PersonalityScoreDraft{}, fmt.Errorf(
				"%w: ranges 缺 %s", ErrPersonalityScoreReply, item.id)
		}
		if len(*item.ptr) != 2 {
			return aiconfig.PersonalityScoreDraft{}, fmt.Errorf(
				"%w: ranges.%s 必须是 [min,max] 两个整数，实际 %d 个", ErrPersonalityScoreReply, item.id, len(*item.ptr))
		}
	}
	for _, item := range reasons {
		if item.ptr == nil {
			return aiconfig.PersonalityScoreDraft{}, fmt.Errorf(
				"%w: reasons 缺 %s", ErrPersonalityScoreReply, item.id)
		}
	}
	draft.Values = aiconfig.PersonalityScoreValues{
		Sociability: *reply.Values.Sociability,
		Exploration: *reply.Values.Exploration,
		Persistence: *reply.Values.Persistence,
		Regularity:  *reply.Values.Regularity,
	}
	draft.Ranges = aiconfig.PersonalityScoreRanges{
		Sociability: aiconfig.PersonalityScoreRange{(*reply.Ranges.Sociability)[0], (*reply.Ranges.Sociability)[1]},
		Exploration: aiconfig.PersonalityScoreRange{(*reply.Ranges.Exploration)[0], (*reply.Ranges.Exploration)[1]},
		Persistence: aiconfig.PersonalityScoreRange{(*reply.Ranges.Persistence)[0], (*reply.Ranges.Persistence)[1]},
		Regularity:  aiconfig.PersonalityScoreRange{(*reply.Ranges.Regularity)[0], (*reply.Ranges.Regularity)[1]},
	}
	draft.Reasons = aiconfig.PersonalityScoreReasons{
		Sociability: *reply.Reasons.Sociability,
		Exploration: *reply.Reasons.Exploration,
		Persistence: *reply.Reasons.Persistence,
		Regularity:  *reply.Reasons.Regularity,
	}
	if err := draft.Validate(); err != nil {
		return aiconfig.PersonalityScoreDraft{}, fmt.Errorf("%w: %v", ErrPersonalityScoreReply, err)
	}
	return draft, nil
}

// stripPersonalityScoreFence 去掉「整段就是一个代码块」的包装。
//
// 只处理 ``` / ```json 两种常见写法；围栏不成对或带别的内容时原样返回，
// 让 JSON 解析去报错——不猜模型意图。
func stripPersonalityScoreFence(raw string) string {
	text := strings.TrimSpace(raw)
	if !strings.HasPrefix(text, "```") {
		return text
	}
	body := strings.TrimPrefix(text, "```")
	if i := strings.IndexAny(body, "\r\n"); i >= 0 {
		head := strings.TrimSpace(body[:i])
		if head != "" && !strings.EqualFold(head, "json") {
			return text
		}
		body = body[i+1:]
	} else if len(body) >= 4 && strings.EqualFold(body[:4], "json") {
		body = body[4:]
	}
	body = strings.TrimSpace(body)
	if !strings.HasSuffix(body, "```") {
		return text
	}
	return strings.TrimSpace(strings.TrimSuffix(body, "```"))
}
