package admin

// T-107 四维性格与评分草稿的管理端接线。
//
// 三个路由（全部走既有管理鉴权，匿名/普通玩家一律 401）：
//
//	GET  /api/ai/personality        四维定义 + 每个角色当前值/修订/描述/生效证据
//	POST /api/ai/personality        按 base_revision 保存四维（同值不跳修订）
//	POST /api/ai/personality/score  一次模型评分，只回草稿（不落库、不改配置）
//
// 边界：
//   - 定义、分段、公式与派生预览全部来自 aiconfig/T-103 快照，不另定义标签或概率；
//   - 能力集合（T-104）、工具、事件能力与配置发布（T-102）仍由各自入口负责；
//   - 评分请求只接受票面六字段，额外键直接拒绝；模型回复同样按票面结构严格校验；
//   - 「已保存」以库里的修订为证，「已生效」必须有同版运行消费证据，二者不互相替代。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"rf4server/internal/aibridge"
	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

// aiPersonalityScoreTimeout 是评分调用的唯一超时点：服务端到点即取消，
// 客户端不重试；迟到的 2xx 由页面按请求序号丢弃。
const aiPersonalityScoreTimeout = 30 * time.Second

// aiPersonalitySaveAction 是保存审计的 action：既留操作痕迹，也是「保存时间」
// 的唯一取证来源（不复制 ai_setting 的键名到 admin）。
const aiPersonalitySaveAction = "ai_personality"

// aiPersonalityEffect 把「已保存」和「已生效」分开。
//
// 已生效必须有同版运行消费证据：T-108 的 store.AIAgentEvidenceReport 在同一个只读
// 快照里按角色当前代次/已保存修订取真实 task/result；本票只读它，不写任何消费凭据，
// 也不按任务完成数推断。Effective 恒由该证据判定；TasksSinceSave 只是运行事实，
// 用于诊断「保存后跑过多少任务」，绝不影响 Effective。
type aiPersonalityEffect struct {
	Effective      bool    `json:"effective"`
	Reason         string  `json:"reason"`
	Display        string  `json:"display"`
	TasksSinceSave int     `json:"tasks_since_save"`
	LastTaskAt     float64 `json:"last_task_at"`
}

// 生效证据的原因枚举。运行证据的原因码来自 store（同一份稳定常量），
// 这里只补「读取失败」这一种管理端本地原因。
const (
	aiPersonalityEffectNotSaved    = "not_saved"
	aiPersonalityEffectNeedsRepair = "needs_repair"
	aiPersonalityEffectInactive    = "inactive"
	aiPersonalityEffectUnreadable  = "evidence_unavailable"
)

// aiPersonalityAgentView 是一个角色的完整后台视图（T-103 快照 + 描述 + 证据）。
type aiPersonalityAgentView struct {
	aiPersonalityView
	Display     string              `json:"display"`
	Role        string              `json:"role"`
	Description string              `json:"description"`
	SavedAt     float64             `json:"saved_at"`
	SaveDisplay string              `json:"save_display"`
	Effect      aiPersonalityEffect `json:"effect"`
}

// newAIPersonalityScoreProvider 是评分 Provider 的注入点：生产只走
// aibridge.NewProvider；测试替换它注入本地 fixture Transport。
var newAIPersonalityScoreProvider = func(spec aibridge.ProviderSpec, timeout time.Duration) (*aibridge.OpenAICompatProvider, error) {
	return aibridge.NewProvider(spec, timeout)
}

// aiPersonalityAgentSnapshot 汇总一个角色的视图：T-103 快照 + 描述 + 保存/生效证据。
func (s *Server) aiPersonalityAgentSnapshot(ctx context.Context, agent store.AIAgent) (aiPersonalityAgentView, error) {
	view, err := aiPersonalitySnapshot(ctx, s.db, agent.Key)
	if err != nil {
		return aiPersonalityAgentView{}, err
	}
	out := aiPersonalityAgentView{
		aiPersonalityView: view,
		Display:           agent.Display,
		Role:              agent.Role,
		Description:       agent.Prompt,
	}
	out.SavedAt = s.aiPersonalitySavedAt(ctx, agent.Key)
	out.SaveDisplay, out.Effect = s.aiPersonalitySaveAndEffect(ctx, view, out.SavedAt)
	return out, nil
}

// aiPersonalitySavedAt 取最近一次「保存性格」的审计时间（Unix 秒）。
// 没有审计行返回 0：页面写「保存时间未知」，不当成刚保存过。
func (s *Server) aiPersonalitySavedAt(ctx context.Context, agentKey string) float64 {
	var ts float64
	err := s.db.SQL.QueryRowContext(ctx, `
		SELECT ts FROM audit_log WHERE action=? AND target=? ORDER BY id DESC LIMIT 1`,
		aiPersonalitySaveAction, agentKey).Scan(&ts)
	if err != nil {
		return 0
	}
	return ts
}

// aiPersonalitySaveAndEffect 同时产出「已保存」文案与「已生效」证据。
//
// 已保存只看库里的修订与是否可派生；已生效必须来自 store.AIAgentEvidenceReport
// 的同版真实消费证据（GS 选择 + BR 表达落到同一个 task/result）。查询只读，
// 不写任务、结果或任何消费凭据；证据不可读时按「等待」处理，不猜成生效。
func (s *Server) aiPersonalitySaveAndEffect(ctx context.Context, view aiPersonalityView,
	savedAt float64) (string, aiPersonalityEffect) {
	effect := aiPersonalityEffect{Effective: false, TasksSinceSave: -1}
	switch {
	case view.NeedsRepair:
		effect.Reason = aiPersonalityEffectNeedsRepair
		effect.Display = "配置需修复：保存后才会参与派生"
		return "需修复（修订 " + fmt.Sprint(view.Revision) + "）", effect
	case view.Inactive != "":
		effect.Reason = aiPersonalityEffectInactive
		effect.Display = "已停用（角色已删除或属旧代次）：保存一次才会重新生效"
		return "已停用（修订 " + fmt.Sprint(view.Revision) + "）", effect
	case !view.Present:
		effect.Reason = aiPersonalityEffectNotSaved
		effect.Display = "尚未保存性格：当前按中性 50 参与派生"
		return "尚未保存", effect
	}
	save := "已保存 · 修订 " + fmt.Sprint(view.Revision)
	if savedAt > 0 {
		save = fmt.Sprintf("已保存 · 修订 %d · %s", view.Revision, timeText(savedAt))
	}
	// 保存后的运行事实照旧统计，但只作诊断，不参与生效判定。
	if savedAt > 0 {
		count, last := s.aiPersonalityTasksSince(ctx, view.AgentKey, savedAt)
		effect.TasksSinceSave = count
		effect.LastTaskAt = last
	}

	rep, err := s.db.AIAgentEvidenceReport(ctx, view.AgentKey)
	if err != nil {
		effect.Reason = aiPersonalityEffectUnreadable
		effect.Display = fmt.Sprintf("已保存修订 %d；同版运行证据暂不可读，按等待处理", view.Revision)
		return save, effect
	}
	// 证据必须与本次 GET 读到的版本是同一份：保存/重建与证据读取之间若已换版，
	// 不得把旧版的证据标到新修订上，也不能反过来把新版证据退回旧版。
	reason := strings.TrimSpace(rep.Reason)
	switch {
	case view.CurrentGeneration > 0 && rep.AgentFound && rep.Generation != view.CurrentGeneration:
		effect.Reason = store.AITaskEvidenceReasonGeneration
		effect.Display = fmt.Sprintf("已保存修订 %d；最近证据属于旧代次（当前代次 %d），等待当前代次的同版运行证据",
			view.Revision, view.CurrentGeneration)
		return save, effect
	case rep.PersonalityPresent && rep.CurrentRevision != view.Revision:
		effect.Reason = store.AITaskEvidenceReasonPersonaDiff
		effect.Display = fmt.Sprintf("已保存修订 %d；同版运行证据查询时修订已变化，按等待处理", view.Revision)
		return save, effect
	}
	if rep.Matched {
		effect.Effective = true
		effect.Reason = store.AIEvidenceReasonMatched
		effect.Display = fmt.Sprintf("已生效 · 修订 %d（同版运行消费已取证：任务 %d / 结果 %d）",
			view.Revision, rep.Evidence.TaskID, rep.Evidence.ResultID)
		if !rep.Evidence.Delivered {
			effect.Display += "；表达已落库，投递待确认"
		}
		return save, effect
	}
	if reason == "" {
		reason = store.AITaskEvidenceReasonNoExpression
	}
	effect.Reason = reason
	effect.Display = fmt.Sprintf("已保存修订 %d；等待同版运行证据（%s）", view.Revision, aiPersonalityEffectReasonText(reason))
	if rep.Latest.ResultID > 0 {
		effect.Display += fmt.Sprintf("；最近一次表达：任务 %d / 结果 %d（修订 %d）",
			rep.Latest.TaskID, rep.Latest.ResultID, rep.Latest.BRRevision)
	}
	return save, effect
}

// aiPersonalityEffectReasonText 把 store 的稳定原因码翻成页面可读的一句话。
// 未登记的原因码原样显示，避免把新原因误报成「已生效」或空白。
func aiPersonalityEffectReasonText(reason string) string {
	switch reason {
	case store.AITaskEvidenceReasonNoExpression:
		return "保存后还没有按这一版完成表达"
	case store.AITaskEvidenceReasonPersonaDiff:
		return "已有证据属于其它修订，本修订尚未被消费"
	case store.AITaskEvidenceReasonGeneration:
		return "已有证据属于旧代次"
	case store.AITaskEvidenceReasonNoGSPersona:
		return "最近表达里 GS 没有用四维做选择"
	case store.AITaskEvidenceReasonMissing:
		return "任务证据已清理"
	case store.AITaskEvidenceReasonUnreadable:
		return "任务证据不可读"
	case store.AITaskEvidenceReasonNoParticipant:
		return "任务证据没有绑定该角色"
	case store.AIEvidenceReasonAttemptReclaimed:
		return "最近结果已被回收"
	case store.AIAgentEvidenceReasonResultVoided:
		return "最近结果已作废"
	case store.AIAgentEvidenceReasonAgentMissing, store.AIAgentEvidenceReasonAgentInactive:
		return "角色不存在或已停用"
	}
	return reason
}

// aiPersonalityTasksSince 统计保存时点之后结束的任务数与最近一条结果时间。
// 这是「运行发生过」的事实，不是「本修订被消费」的证据。
func (s *Server) aiPersonalityTasksSince(ctx context.Context, agentKey string, since float64) (int, float64) {
	var count int
	var last float64
	err := s.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(r.created_ts),0)
		FROM ai_result r JOIN ai_task t ON t.id=r.task_id
		WHERE t.agent=? AND r.created_ts>=?`, agentKey, since).Scan(&count, &last)
	if err != nil {
		return -1, 0
	}
	return count, last
}

// timeText 是后台统一的本地时间显示；0 表示未知。
func timeText(ts float64) string {
	if ts <= 0 {
		return "时间未知"
	}
	return time.Unix(int64(ts), 0).Local().Format("2006-01-02 15:04")
}

// handleAIPersonalityGet 返回四维定义、每个角色的当前值/修订/描述与证据，
// 以及评分要用的模型去向与急停状态（页面不自己拼地址或判断急停）。
func (s *Server) handleAIPersonalityGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agents := []aiPersonalityAgentView{}
	if list, err := s.db.AIListAgents(ctx); err == nil {
		for _, a := range list {
			view, err := s.aiPersonalityAgentSnapshot(ctx, a)
			if err != nil {
				continue
			}
			agents = append(agents, view)
		}
	}
	destination := map[string]any{"host": "", "model": "", "display": "未配置连接"}
	if snap, err := s.db.AIConfigSnapshot(ctx); err == nil {
		host := aiconfig.DestinationHost(snap.BaseURL)
		display := "未配置连接"
		if host != "" || snap.ModelID != "" {
			display = strings.TrimSpace(host + " / " + snap.ModelID)
		}
		destination = map[string]any{"host": host, "model": snap.ModelID, "display": display}
	}
	em := aiconfig.EvaluateEmergency(s.db.AIEmergencyState(ctx))
	blocked, reason := em.RecordBlocked()
	writeJSON(w, map[string]any{
		"schema_version": aiconfig.PersonalitySchemaVersion,
		"dimensions":     aiconfig.PersonalityDimensions(),
		"bands":          aiconfig.PersonalityBands(),
		"agents":         agents,
		"destination":    destination,
		"emergency": map[string]any{
			"stopped": em.Stopped, "fault": em.Fault, "blocked": blocked,
			"reason": reason, "display": em.Display,
		},
	})
}

// aiPersonalitySaveRequest 是保存四维的请求体：schema_version 必须显式给出。
// values 先按原始 JSON 收下再严格解析：缺维度不能被当成合法的 0。
type aiPersonalitySaveRequest struct {
	AgentKey      string           `json:"agent_key"`
	BaseRevision  *int64           `json:"base_revision"`
	SchemaVersion *int             `json:"schema_version"`
	Values        *json.RawMessage `json:"values"`
}

// handleAIPersonalitySave 保存一个角色的四维。
//
// 语义沿用 T-103 的 AISavePersonality：同事务比较 base_revision；冲突返回 409
// 且库里一个字都不改；同值不跳修订（与能力保存同一口径），避免「点两次存两版」。
func (s *Server) handleAIPersonalitySave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req aiPersonalitySaveRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	agentKey := strings.TrimSpace(req.AgentKey)
	if agentKey == "" || req.BaseRevision == nil || req.SchemaVersion == nil || req.Values == nil {
		httpError(w, 400, "缺少 agent_key / base_revision / schema_version / values")
		return
	}
	if *req.SchemaVersion != aiconfig.PersonalitySchemaVersion {
		httpError(w, 400, fmt.Sprintf("未知 schema_version=%d（当前 %d）",
			*req.SchemaVersion, aiconfig.PersonalitySchemaVersion))
		return
	}
	if *req.BaseRevision < 0 {
		httpError(w, 400, "base_revision 不能为负")
		return
	}
	values, err := aiconfig.DecodePersonalityScoreValues(*req.Values)
	if err != nil {
		httpError(w, 400, "四维取值不合法："+err.Error())
		return
	}
	view, err := aiPersonalitySnapshot(ctx, s.db, agentKey)
	if err != nil {
		httpError(w, 500, "读取性格配置失败")
		return
	}
	// 同值且配置可用：不写库、不跳修订，直接按当前修订回读。
	if view.Present && !view.NeedsRepair && view.Inactive == "" && view.Values.ScoreValues() == values {
		s.writePersonalitySaved(w, r, agentKey, view.Revision, false)
		return
	}
	cfg := aiconfig.PersonalityConfig{
		SchemaVersion: aiconfig.PersonalitySchemaVersion,
		Sociability:   values.Sociability,
		Exploration:   values.Exploration,
		Persistence:   values.Persistence,
		Regularity:    values.Regularity,
	}
	rev, err := s.db.AISavePersonality(ctx, agentKey, *req.BaseRevision, cfg)
	switch {
	case errors.Is(err, store.ErrAIPersonalityConflict):
		writeAIPersonalityError(w, 409, "性格配置已被其他管理员更新，请重新载入后再保存", "conflict")
		return
	case errors.Is(err, store.ErrAIPersonalityInvalid):
		httpError(w, 400, "四维取值不合法："+err.Error())
		return
	case errors.Is(err, store.ErrAIPersonalityInactive):
		httpError(w, 404, "角色不存在或已删除")
		return
	case err != nil:
		httpError(w, 500, "保存性格失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), aiPersonalitySaveAction, agentKey,
		fmt.Sprintf("revision=%d", rev))
	s.writePersonalitySaved(w, r, agentKey, rev, true)
}

// writePersonalitySaved 按保存后的真实状态回读并回应；回读失败只回修订号。
func (s *Server) writePersonalitySaved(w http.ResponseWriter, r *http.Request,
	agentKey string, revision int64, changed bool) {
	payload := map[string]any{
		"ok": true, "agent_key": agentKey, "revision": revision, "changed": changed,
	}
	if agent, err := s.db.AIAgentByKey(r.Context(), agentKey); err == nil {
		if view, err := s.aiPersonalityAgentSnapshot(r.Context(), *agent); err == nil {
			payload["view"] = view
		}
	}
	writeJSON(w, payload)
}

// aiPersonalityScoreRequest 是评分请求体：票面六字段，全部必填。
// current_values 同样先收原始 JSON 再严格解析（缺维度不得当成 0）。
type aiPersonalityScoreRequest struct {
	RequestID              string           `json:"request_id"`
	AgentKey               string           `json:"agent_key"`
	BaseRevision           *int64           `json:"base_revision"`
	Description            string           `json:"description"`
	DimensionSchemaVersion *int             `json:"dimension_schema_version"`
	CurrentValues          *json.RawMessage `json:"current_values"`
}

// handleAIPersonalityScore 调用一次模型评分，只返回草稿。
//
// 顺序固定：请求形状 → 角色与描述（以库为准）→ 配置修订 → 急停 → 已保存连接
// → 外发（30 秒截止，发送前再验急停与修订）。任何一步失败都不改任何配置；
// 成功也只回草稿，落库只发生在显式保存。
func (s *Server) handleAIPersonalityScore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body aiPersonalityScoreRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	if body.BaseRevision == nil || body.DimensionSchemaVersion == nil || body.CurrentValues == nil {
		httpError(w, 400, "缺少 base_revision / dimension_schema_version / current_values")
		return
	}
	current, err := aiconfig.DecodePersonalityScoreValues(*body.CurrentValues)
	if err != nil {
		httpError(w, 400, "当前四维不合法："+err.Error())
		return
	}
	req := aibridge.PersonalityScoreRequest{
		RequestID:              body.RequestID,
		AgentKey:               strings.TrimSpace(body.AgentKey),
		BaseRevision:           *body.BaseRevision,
		Description:            body.Description,
		DimensionSchemaVersion: *body.DimensionSchemaVersion,
		CurrentValues:          current,
	}
	if err := req.Validate(); err != nil {
		httpError(w, 400, "评分请求不合法："+err.Error())
		return
	}

	// 描述以库为准：页面传上来的只是「它以为的描述」，改过就拒绝，
	// 避免用旧描述评出的建议回填到新描述上。
	agent, err := s.db.AIAgentByKey(ctx, req.AgentKey)
	if err != nil {
		httpError(w, 404, "角色不存在或已删除")
		return
	}
	if strings.TrimSpace(agent.Prompt) != strings.TrimSpace(req.Description) {
		writeAIPersonalityError(w, 409, "角色描述已改变，请重新载入后再评分", "description_changed")
		return
	}
	view, err := aiPersonalitySnapshot(ctx, s.db, req.AgentKey)
	if err != nil {
		httpError(w, 500, "读取性格配置失败")
		return
	}
	switch {
	case view.NeedsRepair:
		writeAIPersonalityError(w, 409, "性格配置需先修复：保存一次后再评分", "needs_repair")
		return
	case view.Inactive != "":
		writeAIPersonalityError(w, 409, "性格配置已停用（角色已删除或属旧代次）", "inactive")
		return
	case view.Revision != req.BaseRevision:
		writeAIPersonalityError(w, 409,
			fmt.Sprintf("性格配置已更新（当前修订 %d），请重新载入后再评分", view.Revision), "stale_personality")
		return
	}

	// 急停优先：拒绝时连主密钥都不读取，更不会建立外部连接。
	em := aiconfig.EvaluateEmergency(s.db.AIEmergencyState(ctx))
	if blocked, _ := em.RecordBlocked(); blocked {
		writeAIPersonalityError(w, 409, "评分已拒绝："+em.Display, "emergency")
		return
	}

	base, err := s.db.AIConfigSnapshot(ctx)
	if err != nil {
		writeAIPersonalityError(w, 503, "已保存的连接不可用："+configReason(err), "config_locked")
		return
	}
	masterKey, err := aibridge.LoadMasterKey(aiMasterKeyPath())
	if err != nil {
		writeAIPersonalityError(w, 503, "评分不可用："+aiCredentialReason(err), "master_key_unavailable")
		return
	}
	snap, err := aibridge.ResolveSnapshot(ctx, s.db, base, aiMasterKeyPath(), masterKey)
	if err != nil {
		writeAIPersonalityError(w, 503, "已保存的连接不可用："+aiCredentialReason(err), "credential_locked")
		return
	}
	if strings.TrimSpace(snap.Spec.APIKey) == "" {
		writeAIPersonalityError(w, 503, "尚未配置可用的 API Key，请先保存连接", "not_configured")
		return
	}
	provider, err := newAIPersonalityScoreProvider(snap.Spec, aiPersonalityScoreTimeout)
	if err != nil {
		writeAIPersonalityError(w, 502, "评分不可用："+safeAIError(err), "provider_unavailable")
		return
	}
	// 发送前最后一刻再验：预检与发送之间的急停或新发布同样拦住。
	provider.SetEgressGate(func(gctx context.Context) error {
		return aibridge.Gate(gctx, s.db, snap.Revision())
	})

	callCtx, cancel := context.WithTimeout(ctx, aiPersonalityScoreTimeout)
	defer cancel()
	res, err := aibridge.RunPersonalityScore(callCtx, provider, req)
	if err != nil {
		s.writePersonalityScoreError(w, err, snap.Spec)
		return
	}
	writeJSON(w, res)
}

// writePersonalityScoreError 把评分失败映射成页面可直接显示的状态码与文案。
// 任何分支都不改配置、不改草稿，页面保留原四维与已填内容。
func (s *Server) writePersonalityScoreError(w http.ResponseWriter, err error, spec aibridge.ProviderSpec) {
	var denied *aibridge.EgressDeniedError
	if errors.As(err, &denied) {
		writeAIPersonalityError(w, 409, "评分已拒绝："+denied.Display, "emergency")
		return
	}
	var stale *aibridge.StaleConfigError
	if errors.As(err, &stale) {
		writeAIPersonalityError(w, 409, "模型配置已更新，本次评分作废，请重新评分", "stale_config")
		return
	}
	var hostDenied *aiconfig.HostPolicyError
	if errors.As(err, &hostDenied) {
		writeAIPersonalityError(w, 409, "评分已拒绝："+hostDenied.Display, "egress_host_denied")
		return
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeAIPersonalityError(w, 504, "评分超时（30 秒）：草稿未改动，可重试或手动调整", "timeout")
	case errors.Is(err, aibridge.ErrPersonalityScoreRequest):
		httpError(w, 400, "评分请求不合法："+err.Error())
	case errors.Is(err, aibridge.ErrPersonalityScoreReply):
		writeAIPersonalityError(w, 502,
			"模型输出不符合约定：草稿未改动，可重试或直接手动调整（"+safeAIError(errors.New(err.Error()))+"）",
			"illegal_output")
	default:
		writeAIPersonalityError(w, 502, "评分调用失败："+safeAIError(err), "call_failed")
	}
}

// writeAIPersonalityError 写出带 state 的错误体。页面的窄桥只消费 error 文案，
// state 供同一响应体的其它消费者（测试、运维）区分原因。
func writeAIPersonalityError(w http.ResponseWriter, code int, msg, state string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "state": state})
}
