package admin

// AI 设置后台：厂商/模型/API Key、思考强度、出网二分开关。
// API Key 用与 aibridge 相同的主密钥加密，明文不落库、不回显、不进日志。

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"rf4server/internal/aibridge"
	"rf4server/internal/aiconfig"
	"rf4server/internal/gameserver"
	"rf4server/internal/store"
)

// ConfigureAI 由进程入口在读取配置后调用，让后台与 aibridge 使用同一份
// 主密钥路径和观测地址，避免两边各用一套默认值导致 Key 解不开。
func ConfigureAI(masterKeyFile, bridgeAddr string) {
	if strings.TrimSpace(masterKeyFile) != "" {
		aiMasterKeyFile = strings.TrimSpace(masterKeyFile)
	}
	if strings.TrimSpace(bridgeAddr) != "" {
		aiBridgeAddr = strings.TrimSpace(bridgeAddr)
	}
}

var (
	aiMasterKeyFile = "data/ai_master.key"
	aiBridgeAddr    = "127.0.0.1:9504"
)

// aiMasterKeyPath 优先使用进程入口注入的配置，其次环境变量。
func aiMasterKeyPath() string {
	if v := os.Getenv("RF4_AI_MASTER_KEY_FILE"); v != "" {
		return v
	}
	return aiMasterKeyFile
}

// aiCredentialsPath 是旧版外部受保护凭据文件：与主密钥同目录、分文件保存。
// V2 只读兼容（旧安装解析），新 Key 写不可变的版本目录。
func aiCredentialsPath() string {
	return aibridge.CredentialsPath(aiMasterKeyPath())
}

// aiCredentialDir 是不可变密文对象的版本目录（与主密钥同目录）。
func aiCredentialDir() string {
	return aibridge.CredentialDir(aiMasterKeyPath())
}

// aiCredentialReason 把主密钥/凭据错误映射成不含密钥内容的后台提示。
func aiCredentialReason(err error) string {
	switch {
	case errors.Is(err, aibridge.ErrMasterKeyMissing):
		return "主密钥缺失（已有加密凭据时不会自动新建，请恢复主密钥文件）"
	case errors.Is(err, aibridge.ErrMasterKeyInvalid):
		return "主密钥文件损坏（已保留原文件）"
	case errors.Is(err, aibridge.ErrCredentialKeyMismatch):
		return "主密钥与凭据不匹配（已保留原文件）"
	case errors.Is(err, aibridge.ErrCredentialFileInvalid):
		return "凭据文件损坏（已保留原文件）"
	case errors.Is(err, aibridge.ErrCredentialFileMissing):
		return "旧凭据文件缺失"
	case errors.Is(err, aibridge.ErrCredentialObjectMissing):
		return "活动凭据对象缺失（已锁定，未回退旧文件）"
	case errors.Is(err, aibridge.ErrCredentialObjectInvalid):
		return "活动凭据对象损坏（已锁定，未覆盖）"
	case errors.Is(err, aibridge.ErrCredentialObjectMismatch):
		return "活动凭据对象与厂商配置不匹配（已锁定）"
	default:
		return "请检查服务日志"
	}
}

// ensureMasterKeyForSave 返回保存凭据要用的主密钥。
// 判定与 CLI 的 -init-key 同源：只有「主密钥缺失 + 旧库无密文 + 外部凭据/
// 版本目录都没有密文」才由这次显式保存完成首次初始化；否则锁定并保留原文件。
//
// 首次初始化会写一条独立审计；审计失败时本次保存不能报成功（主密钥文件
// 已落盘则保留，下次保存幂等复用，不补造管理员操作）。
func (s *Server) ensureMasterKeyForSave(ctx context.Context, actor string) ([]byte, error) {
	res, err := aibridge.EnsureMasterKeyForExplicitInit(ctx, s.db, aiMasterKeyPath())
	if err != nil {
		return nil, err
	}
	if res.Created {
		if aerr := s.db.AuditLogWrite(ctx, actor, "ai_master_key", "init", "explicit_first_install"); aerr != nil {
			return nil, fmt.Errorf("主密钥已初始化但审计写入失败：%w", aerr)
		}
	}
	return res.Key, nil
}

// aiCredentialStatus 解析活动凭据的可用状态与遮蔽值，供状态回读与保存响应共用。
// state: configured | missing | locked。
func (s *Server) aiCredentialStatus(ctx context.Context) (state, masked, reason string) {
	snap, err := s.db.AIConfigSnapshot(ctx)
	if err != nil {
		return "locked", "", "配置记录不可用：" + configReason(err)
	}
	return s.aiCredentialStatusFor(ctx, snap)
}

func credentialSaveAction(err error) string {
	err = aibridge.CredentialInstallIssue(err)
	switch {
	case errors.Is(err, aibridge.ErrCredentialDirectoryBusy):
		return "请关闭占用测试数据目录的程序，再点保存"
	case errors.Is(err, aibridge.ErrCredentialDiskFull):
		return "请释放本机磁盘空间，再点保存"
	case errors.Is(err, os.ErrPermission):
		return "请检查 Launcher 账户对实例数据目录的写权限，再点保存"
	case errors.Is(err, aibridge.ErrCredentialVolumeUnsupported):
		return "请按部署说明把配套的数据和凭据迁到本机固定 NTFS 卷，重启后再保存"
	case errors.Is(err, aibridge.ErrCredentialInstallNotDurable):
		return "暂时无法确认凭据已持久保存，请检查数据目录后重试"
	case errors.Is(err, aibridge.ErrMasterKeyMissing):
		return "主密钥缺失，请恢复与现有凭据配套的主密钥备份，再重试"
	case errors.Is(err, aibridge.ErrMasterKeyInvalid):
		return "主密钥损坏，请恢复与现有凭据配套的主密钥备份，再重试"
	case errors.Is(err, aibridge.ErrCredentialKeyMismatch):
		return "主密钥与凭据不匹配，请恢复配套备份，再重试"
	default:
		return "请稍后重试；如果仍失败，请让服务端维护者检查日志"
	}
}

func (s *Server) aiCredentialStatusFor(ctx context.Context, snap store.AIConfigSnapshot) (state, masked, reason string) {
	masterKey, err := aibridge.LoadMasterKey(aiMasterKeyPath())
	if err != nil {
		if snap.CredentialRef != "" || !errors.Is(err, aibridge.ErrMasterKeyMissing) {
			return "locked", "", aiCredentialReason(err)
		}
		var legacy bool
		dbErr := s.db.SQL.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM ai_secret)").Scan(&legacy)
		_, fileErr := os.Stat(aiCredentialsPath())
		objects, dirErr := aibridge.CredentialDirHasCiphertext(aiCredentialDir())
		if dbErr != nil || !errors.Is(fileErr, os.ErrNotExist) || dirErr != nil || legacy || objects {
			return "locked", "", aiCredentialReason(err)
		}
		return "missing", "", "尚未配置密钥"
	}
	if snap.CredentialRef != "" {
		obj, _, err := aibridge.ResolveCredentialObject(aiCredentialDir(), snap.CredentialRef, masterKey, snap.ProviderID)
		if err != nil {
			return "locked", "", aiCredentialReason(err)
		}
		return "configured", obj.Masked, ""
	}
	// 旧安装兼容：外部旧文件优先，其次旧库遮蔽值。
	if f, ferr := aibridge.LoadCredentialFile(aiCredentialsPath()); ferr == nil {
		if m := f.MaskedFor(snap.ProviderName); m != "" {
			if plain, found, derr := f.APIKey(snap.ProviderName, masterKey); derr == nil && found && plain != "" {
				return "configured", m, ""
			}
			return "locked", "", "旧凭据文件无法用当前主密钥解密"
		}
	}
	var legacyMasked string
	if snap.ProviderName != "" {
		_ = s.db.SQL.QueryRowContext(ctx, `
			SELECT s.masked FROM ai_secret s JOIN ai_provider p ON p.id=s.provider_id
			WHERE p.name=? ORDER BY p.id LIMIT 1`, snap.ProviderName).Scan(&legacyMasked)
	}
	if plain, _, rerr := aibridge.ResolveProviderAPIKey(ctx, s.db, aiCredentialsPath(), masterKey, snap.ProviderName); rerr != nil {
		return "locked", "", aiCredentialReason(rerr)
	} else if plain != "" {
		return "configured", legacyMasked, ""
	}
	return "missing", "", "尚未配置密钥"
}

// aiResolveCurrentKey 解析当前活动凭据的明文（只用于同源留空校验与旧安装迁移）。
func (s *Server) aiResolveCurrentKey(ctx context.Context, snap store.AIConfigSnapshot, masterKey []byte) (string, error) {
	if snap.CredentialRef != "" {
		_, plain, err := aibridge.ResolveCredentialObject(
			aiCredentialDir(), snap.CredentialRef, masterKey, snap.ProviderID)
		if err != nil {
			return "", err
		}
		return plain, nil
	}
	plain, _, err := aibridge.ResolveProviderAPIKey(ctx, s.db, aiCredentialsPath(), masterKey, snap.ProviderName)
	if err != nil {
		return "", err
	}
	return plain, nil
}

// configReason 把配置发布错误映射成不含密钥/完整地址的提示。
func configReason(err error) string {
	switch {
	case errors.Is(err, store.ErrAIConfigRevisionConflict):
		return "配置已被其他管理员更新，请刷新后重试"
	case errors.Is(err, store.ErrAIConfigHistoryConflict):
		return "配置发布历史与活动指针矛盾，已锁定，请核对凭据与配置记录"
	case errors.Is(err, store.ErrAIConfigSnapshotInvalid):
		return "活动配置记录损坏，已锁定"
	case errors.Is(err, store.ErrAIConfigSourceChanged):
		return "厂商配置在保存过程中被改变，请刷新后重试"
	case errors.Is(err, store.ErrAIConfigNoProvider):
		return "没有启用的厂商"
	case errors.Is(err, store.ErrAIConfigCredentialRequired):
		return "缺少可用凭据，请重新填写 API Key"
	default:
		return "请检查服务日志"
	}
}

// aiToolCatalog 是 Agent 可勾选的只读工具清单，直接来自唯一能力目录。
// 前台只渲染这份目录，避免出现「填了个不存在的工具名，AI 永远用不上」。
var aiToolCatalog = capabilityToolCatalog()

func capabilityToolCatalog() []struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Desc  string `json:"desc"`
} {
	tools := aiconfig.CapabilityToolCatalog()
	out := make([]struct {
		Name  string `json:"name"`
		Label string `json:"label"`
		Desc  string `json:"desc"`
	}, 0, len(tools))
	for _, t := range tools {
		// 旧页面只列只读工具；写工具不进旧勾选器。
		if !t.ReadOnly {
			continue
		}
		out = append(out, struct {
			Name  string `json:"name"`
			Label string `json:"label"`
			Desc  string `json:"desc"`
		}{Name: t.Name, Label: t.Label, Desc: t.Desc})
	}
	return out
}

func (s *Server) handleAITools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"tools": aiToolCatalog})
}

// handleAICapabilities 返回能力目录与每个角色当前生效的集合。
//
// 这是新页面（T-105）的唯一数据源：目录、工具投影、修订与可用性都由服务端
// 给出，页面不自行拼工具名。
func (s *Server) handleAICapabilities(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agents := []map[string]any{}
	if list, err := s.db.AIListAgents(ctx); err == nil {
		for _, a := range list {
			view, err := s.db.AIGetCapabilities(ctx, a.Key)
			if err != nil {
				continue
			}
			rt, err := s.db.AICapabilityRuntimeStatus(ctx, a.Key, view.State)
			if err != nil {
				rt = store.AICapabilityRuntimeStatus{
					AgentKey: a.Key, Revision: view.Revision, State: view.State.State,
					Availability: store.AICapabilityAvailabilityFailed, Reason: "runtime_read_failed",
				}
			}
			agents = append(agents, capabilityViewJSON(a.Key, view, rt))
		}
	}
	writeJSON(w, map[string]any{
		"catalog": aiconfig.CapabilityCatalog(),
		"tools":   aiconfig.CapabilityToolCatalog(),
		"agents":  agents,
	})
}

// handleAICapabilitiesSave 按 base_revision 原子保存一个角色的能力集合。
//
// 回执同时给出：保存后的修订、是否真的变了、当前可用性、所需 GS 运行命令的
// ID/请求 ID，以及只作已应用事实的 actor 运行态。命令由 GS 消费者应用，
// 管理端不直接改运行态；重复保存同一集合不会跳修订，也不会重复补派。
func (s *Server) handleAICapabilitiesSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentKey     string    `json:"agent_key"`
		BaseRevision int64     `json:"base_revision"`
		Capabilities *[]string `json:"capabilities"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.AgentKey) == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	if req.Capabilities == nil {
		// null 与「缺记录」无法区分；显式空必须传 []。
		httpError(w, 400, "缺少能力集合；清空请提交空数组")
		return
	}
	agentKey := strings.TrimSpace(req.AgentKey)
	ctx := r.Context()
	res, err := s.db.AISaveCapabilitiesWithRuntime(ctx, agentKey,
		req.BaseRevision, *req.Capabilities, nil)
	switch {
	case errors.Is(err, store.ErrAICapabilityConflict):
		httpError(w, 409, "能力配置已被其他管理员更新，请刷新后重试")
		return
	case errors.Is(err, store.ErrAICapabilityInvalid):
		httpError(w, 400, "能力集合不合法："+err.Error())
		return
	case errors.Is(err, store.ErrAICapabilityInactive):
		httpError(w, 404, "角色不存在或已删除")
		return
	case err != nil:
		httpError(w, 500, "能力保存失败")
		return
	}
	state := res.State
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_capabilities", agentKey,
		"revision="+strconv.FormatInt(state.Revision, 10)+
			" changed="+strconv.FormatBool(res.Changed)+
			" availability="+res.Runtime.Availability+
			" command_id="+strconv.FormatInt(res.Runtime.CommandID, 10)+
			" capabilities="+strings.Join(state.Capabilities, ","))
	body := map[string]any{
		"ok":           true,
		"agent_key":    agentKey,
		"revision":     state.Revision,
		"changed":      res.Changed,
		"state":        state.State,
		"generation":   state.Generation,
		"capabilities": emptyIfNil(state.Capabilities),
		"read_tools":   emptyIfNil(state.ReadTools),
		"write_tools":  emptyIfNil(state.WriteTools),
		"availability": res.Runtime.Availability,
		"runtime":      capabilityRuntimeJSON(res.Runtime),
		"commands":     capabilityCommandsJSON(res.Runtime.Commands),
	}
	if res.Runtime.CommandID > 0 {
		body["command_id"] = res.Runtime.CommandID
		body["request_id"] = res.Runtime.RequestID
		body["command_status"] = res.Runtime.Status
	}
	if res.RuntimeError != "" {
		body["runtime_error"] = res.RuntimeError
	}
	writeJSON(w, body)
}

func capabilityViewJSON(agentKey string, view store.AICapabilityView,
	rt store.AICapabilityRuntimeStatus) map[string]any {
	return map[string]any{
		"key":           agentKey,
		"present":       view.Present,
		"revision":      view.Revision,
		"generation":    view.Generation,
		"state":         view.State.State,
		"capabilities":  emptyIfNil(view.State.Capabilities),
		"read_tools":    emptyIfNil(view.State.ReadTools),
		"write_tools":   emptyIfNil(view.State.WriteTools),
		"legacy":        view.State.Legacy,
		"reason":        view.State.Reason,
		"actor_enabled": view.ActorEnabled,
		"availability":  rt.Availability,
		"runtime":       capabilityRuntimeJSON(rt),
		"commands":      capabilityCommandsJSON(rt.Commands),
	}
}

func capabilityRuntimeJSON(rt store.AICapabilityRuntimeStatus) map[string]any {
	return map[string]any{
		"agent_key":       rt.AgentKey,
		"revision":        rt.Revision,
		"generation":      rt.Generation,
		"state":           rt.State,
		"required":        rt.Required,
		"desired":         rt.Desired,
		"actual":          rt.Actual,
		"availability":    rt.Availability,
		"reason":          rt.Reason,
		"command_id":      rt.CommandID,
		"request_id":      rt.RequestID,
		"status":          rt.Status,
		"applied":         rt.Status == store.AIRuntimeStatusApplied,
		"applied_ts":      rt.AppliedTS,
		"result_revision": rt.ResultRevision,
	}
}

func capabilityCommandsJSON(cmds []store.AICapabilityRuntimeCommand) []map[string]any {
	out := make([]map[string]any, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, map[string]any{
			"id":                  c.ID,
			"request_id":          c.RequestID,
			"status":              c.Status,
			"reason":              c.Reason,
			"enabled":             c.Enabled,
			"revision":            c.Revision,
			"expected_generation": c.ExpectedGeneration,
			"created_ts":          c.CreatedTS,
			"applied_ts":          c.AppliedTS,
			"result_revision":     c.ResultRevision,
		})
	}
	return out
}

func emptyIfNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// aiOnboardingAgent 是一个角色的「首句链路」证据，全部来自已有表：
//
//   - DM 入队：ai_task(kind='dm')，只有玩家客户端真的把消息发给这个联系人
//     才会有行，因此它同时是「联系人已出现在游戏里」的玩家侧证据；
//   - BR 回复：最近一条 ai_result 的 status/err/created_ts；
//   - GS 投递：状态为 done 且 delivered_ts>0 的回复结果（投递时正文会被清空），
//     以及 mail(sender=contact_uid) 的落库时间（联系人可见证据）。
//
// 失败提示（status='fallback'）也会被 GS 投进个人消息并带正数 delivered_ts，
// 所以投递时间本身不是「回复成功」的证据；首句完成只认 done 的投递结果。
// 这里只读事实，不推断「应该能看到」。没有证据时前端显示「待确认」。
type aiOnboardingAgent struct {
	ContactVisible  bool
	VisibleEvidence string
	LastDMID        int64
	LastDMState     string
	LastDMTS        float64
	LastReplyStatus string
	LastReplyError  string
	LastReplyTS     float64
	LastDeliveredTS float64 // 最近一次 status='done' 且 GS 已投递的时间（首句完成证据）
	LastMailTS      float64 // 该联系人发出的最近一封邮件时间（联系人可见证据）
	PendingCommands int
	CatchTotal      int
	CatchTotalG     int64
	CatchToday      int
	CatchTodayG     int64
	MemoryTotal     int
	MemoryStar      int
	MemorySuper     int
	Unknown         bool
}

// aiOnboardingAgentEvidence 读取单个角色的链路证据。
// 任何一项查询失败只把该角色标成 unknown，不让整张总览失败。
func (s *Server) aiOnboardingAgentEvidence(ctx context.Context, key string, contactUID int64) aiOnboardingAgent {
	var ev aiOnboardingAgent
	if err := s.db.SQL.QueryRowContext(ctx, `
		SELECT id, state, created_ts FROM ai_task
		WHERE agent=? AND kind='dm' ORDER BY id DESC LIMIT 1`, key).
		Scan(&ev.LastDMID, &ev.LastDMState, &ev.LastDMTS); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			ev.Unknown = true
		}
	}
	if err := s.db.SQL.QueryRowContext(ctx, `
		SELECT r.status, COALESCE(r.err,''), r.created_ts
		FROM ai_result r JOIN ai_task t ON t.id=r.task_id
		WHERE t.agent=? AND t.kind='dm'
		ORDER BY r.id DESC LIMIT 1`, key).
		Scan(&ev.LastReplyStatus, &ev.LastReplyError, &ev.LastReplyTS); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		ev.Unknown = true
	}
	// 首句完成证据：覆盖已成功投递的历史，取 last_delivered_ts 的最大值。
	// 后续失败（包括失败提示被投递）不撤销已经完成的首句。
	if err := s.db.SQL.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(r.delivered_ts),0)
		FROM ai_result r JOIN ai_task t ON t.id=r.task_id
		WHERE t.agent=? AND t.kind='dm' AND r.status='done' AND COALESCE(r.delivered_ts,0)>0`, key).
		Scan(&ev.LastDeliveredTS); err != nil {
		ev.Unknown = true
	}
	ev.LastReplyError = safeAIError(errors.New(ev.LastReplyError))
	if contactUID > 0 {
		if err := s.db.SQL.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(created_ts),0) FROM mail WHERE sender=?`, contactUID).
			Scan(&ev.LastMailTS); err != nil {
			ev.Unknown = true
		}
	}
	if err := s.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ai_runtime_command
		WHERE agent_key=? AND status='pending'`, key).Scan(&ev.PendingCommands); err != nil {
		ev.Unknown = true
	}
	if err := s.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(weight_g),0) FROM ai_catch WHERE agent_key=?`, key).
		Scan(&ev.CatchTotal, &ev.CatchTotalG); err != nil {
		ev.Unknown = true
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	if err := s.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(weight_g),0) FROM ai_catch
		WHERE agent_key=? AND created_ts>=?`, key, float64(today)).
		Scan(&ev.CatchToday, &ev.CatchTodayG); err != nil {
		ev.Unknown = true
	}
	if err := s.db.SQL.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN kind='star' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN kind='super' THEN 1 ELSE 0 END),0)
		FROM ai_memory WHERE agent_key=?`, key).
		Scan(&ev.MemoryTotal, &ev.MemoryStar, &ev.MemorySuper); err != nil {
		ev.Unknown = true
	}
	// 「玩家可见」只认游戏端证据：玩家确实给这个联系人发过私聊，或收到过它发出的邮件。
	// 仅有管理端的 joined=1 不算玩家可见。
	switch {
	case ev.LastDMID > 0:
		ev.ContactVisible = true
		ev.VisibleEvidence = "dm"
	case ev.LastMailTS > 0:
		ev.ContactVisible = true
		ev.VisibleEvidence = "mail"
	}
	return ev
}

func (ev aiOnboardingAgent) JSON() map[string]any {
	display := "管理配置已保存；还没有游戏端证据"
	if ev.ContactVisible {
		switch ev.VisibleEvidence {
		case "dm":
			display = "玩家已在游戏里私聊过它（联系人可见）"
		case "mail":
			display = "玩家已收到过它的游戏内邮件（联系人可见）"
		}
	}
	replyDisplay := "还没有回复记录"
	switch {
	case ev.LastReplyStatus == "fallback":
		replyDisplay = "回复失败：原因待确认"
		if strings.TrimSpace(ev.LastReplyError) != "" {
			replyDisplay = "回复失败：" + ev.LastReplyError
		}
	case ev.LastReplyStatus != "" && ev.LastDeliveredTS > 0:
		replyDisplay = "已投递"
	case ev.LastReplyStatus != "":
		replyDisplay = "回复已生成，等待游戏服投递"
	}
	return map[string]any{
		"contact_visible":  ev.ContactVisible,
		"visible_evidence": ev.VisibleEvidence,
		"visible_display":  display,
		"last_dm":          map[string]any{"id": ev.LastDMID, "state": ev.LastDMState, "ts": ev.LastDMTS},
		"last_reply":       map[string]any{"status": ev.LastReplyStatus, "error": ev.LastReplyError, "display": replyDisplay, "ts": ev.LastReplyTS, "delivered_ts": ev.LastDeliveredTS},
		"last_mail_ts":     ev.LastMailTS,
		"pending_commands": ev.PendingCommands,
		"catch_total":      ev.CatchTotal,
		"catch_total_g":    ev.CatchTotalG,
		"catch_today":      ev.CatchToday,
		"catch_today_g":    ev.CatchTodayG,
		"memory_total":     ev.MemoryTotal,
		"memory_star":      ev.MemoryStar,
		"memory_super":     ev.MemorySuper,
		"unknown":          ev.Unknown,
	}
}

func (s *Server) handleAIStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings := map[string]string{}
	for _, spec := range AISettingSpecs {
		settings[spec.Key] = s.db.AISetting(ctx, spec.Key, spec.Def)
	}
	// 安全判定与展示文案统一由 aiconfig 给出；V2 只有急停是放行条件。
	security := aiconfig.Security(func(key string) string {
		return s.db.AISetting(ctx, key, "")
	})
	// 部署级出口名单仅作部署记录展示，不再是放行条件。
	security.Hosts = aiconfig.DescribeEgressHosts(s.aiEgressHostPolicy())

	// 活动配置：地址、模型、修订、凭据引用在单次读取中绑定。
	snap, reading, retest, snapErr := s.db.AIConnectionState(ctx)
	em := aiconfig.EvaluateEmergency(reading)
	if blocked, _ := em.RecordBlocked(); blocked {
		security.Egress = aiconfig.EgressStatus{State: aiconfig.StateEmergency, Display: em.Display}
	} else {
		security.Egress = aiconfig.Egress(func(string) string { return reading.StopRaw })
	}
	if reading.StopRaw != "" {
		settings[aiconfig.KeyEmergencyStop] = reading.StopRaw
	} else {
		settings[aiconfig.KeyEmergencyStop] = aiconfig.DefaultEmergencyStop
	}
	var baseURL, modelID, providerName string
	var revision int64
	var source string
	configState, configError := "ok", ""
	if snapErr != nil {
		configState, configError = "locked", configReason(snapErr)
	} else {
		providerName = snap.ProviderName
		baseURL = snap.BaseURL
		modelID = snap.ModelID
		revision = snap.Revision
		source = snap.Source
		if snap.Legacy {
			configState = "legacy"
		}
	}
	credState, credMasked, credReason := "locked", "", configError
	if snapErr == nil {
		credState, credMasked, credReason = s.aiCredentialStatusFor(ctx, snap)
	}
	retestCurrent := snapErr == nil && credState == "configured" && currentAIRetest(em, revision, retest)
	destHost := aiconfig.DestinationHost(baseURL)
	displayBase := aiconfig.RedactURLForDisplay(baseURL)
	searchConfig := aiconfig.WebSearchSettings{
		Enabled: settings[aiconfig.KeyWebSearchEnabled] == "1",
		Mode:    settings[aiconfig.KeySearchMode], Endpoint: settings[aiconfig.KeySearchEndpoint],
	}
	searchSupported := snapErr == nil && aiconfig.DeepSeekNativeSearchSupported(baseURL, snap.Protocol, modelID, snap.AuthScheme)
	var searchEvidence aiconfig.NativeSearchEvidence
	if searchSupported && revision > 0 {
		err := json.Unmarshal([]byte(s.db.AISetting(ctx, aiconfig.KeyNativeSearchEvidence, "")), &searchEvidence)
		if err != nil || searchEvidence.Revision != revision {
			searchEvidence = aiconfig.NativeSearchEvidence{}
		}
	}

	savedDisplay := "尚未保存可用连接"
	switch {
	case configState == "locked":
		savedDisplay = "配置已锁定：" + configError
	case credState == "locked":
		savedDisplay = "凭据已锁定"
	case credState != "configured" || destHost == "" || modelID == "":
		savedDisplay = "尚未保存，没有可用连接"
	case configState == "legacy":
		savedDisplay = "旧安装配置（尚未发布修订）"
	case revision > 0:
		savedDisplay = fmt.Sprintf("已保存 · %s / %s / 修订 %d，待复测", destHost, modelID, revision)
		if retestCurrent && retest.OK {
			savedDisplay = fmt.Sprintf("此连接已通过复测 · %s / %s / 修订 %d", destHost, modelID, revision)
		}
	}
	retestDisplay := "待复测"
	if retestCurrent {
		if retest.OK {
			retestDisplay = "此连接已通过复测"
		} else {
			retestDisplay = "复测失败：" + retest.Error
		}
	}

	type agentView struct {
		Key                string `json:"key"`
		Display            string `json:"display"`
		Role               string `json:"role"`
		ContactUID         int64  `json:"contact_uid"`
		ThinkingEnabled    bool   `json:"thinking_enabled"`
		ThinkingEffort     string `json:"thinking_effort"`
		Joined             bool   `json:"joined"`
		ActorEnabled       bool   `json:"actor_enabled"`
		ReplyMode          string `json:"reply_mode"`
		Talkativeness      int    `json:"talkativeness"`
		FishingTalkEnabled bool   `json:"fishing_talk_enabled"`
		// 运行态代次/版本：页面把「看到的状态」作为入队期望值，GS 拒绝过期操作。
		Generation int64          `json:"generation"`
		Revision   int64          `json:"revision"`
		Actor      map[string]any `json:"actor,omitempty"`
		Tools      string         `json:"tools"`
		ToolsList  []string       `json:"tools_list"`
		Prompt     string         `json:"prompt"`
		// 四步开聊与钓鱼面板只读证据（来源见 aiOnboardingAgentEvidence）。
		ContactVisible  bool    `json:"contact_visible"`
		VisibleEvidence string  `json:"visible_evidence"`
		PendingCommands int     `json:"pending_commands"`
		CatchTotal      int     `json:"catch_total"`
		CatchTotalG     int64   `json:"catch_total_g"`
		CatchToday      int     `json:"catch_today"`
		CatchTodayG     int64   `json:"catch_today_g"`
		MemoryTotal     int     `json:"memory_total"`
		LastDMTS        float64 `json:"last_dm_ts"`
		LastReplyStatus string  `json:"last_reply_status"`
		LastReplyError  string  `json:"last_reply_error"`
		LastDeliveredTS float64 `json:"last_delivered_ts"`
		LastMailTS      float64 `json:"last_mail_ts"`
	}
	actorByKey := map[string]store.AIActor{}
	if actors, err := s.db.AIListActors(ctx); err == nil {
		for _, a := range actors {
			actorByKey[a.AgentKey] = a
		}
	}
	agents := []agentView{}
	onboardingAgents := map[string]any{}
	joinedCount, visibleCount, dmCount, deliveredCount := 0, 0, 0, 0
	pendingCommands := 0
	if list, err := s.db.AIListAgents(ctx); err == nil {
		for _, a := range list {
			tools := []string{}
			_ = json.Unmarshal([]byte(a.ToolsJSON), &tools)
			ev := s.aiOnboardingAgentEvidence(ctx, a.Key, a.ContactUID)
			v := agentView{
				Key: a.Key, Display: a.Display, Role: a.Role, ContactUID: a.ContactUID,
				ThinkingEnabled: a.ThinkingEnabled, ThinkingEffort: a.ThinkingEffort,
				Joined: a.Joined, ActorEnabled: a.ActorEnabled,
				ReplyMode: a.ReplyMode, Talkativeness: a.Talkativeness,
				FishingTalkEnabled: a.FishingTalkEnabled,
				Generation:         a.Generation, Revision: a.Revision,
				Tools: a.ToolsJSON, ToolsList: tools, Prompt: a.Prompt,
				ContactVisible:  ev.ContactVisible,
				VisibleEvidence: ev.VisibleEvidence,
				PendingCommands: ev.PendingCommands,
				CatchTotal:      ev.CatchTotal,
				CatchTotalG:     ev.CatchTotalG,
				CatchToday:      ev.CatchToday,
				CatchTodayG:     ev.CatchTodayG,
				MemoryTotal:     ev.MemoryTotal,
				LastDMTS:        ev.LastDMTS,
				LastReplyStatus: ev.LastReplyStatus,
				LastReplyError:  ev.LastReplyError,
				LastDeliveredTS: ev.LastDeliveredTS,
				LastMailTS:      ev.LastMailTS,
			}
			if act, ok := actorByKey[a.Key]; ok {
				v.Actor = map[string]any{
					"activity": act.Activity, "loc_name": act.LocName, "target_name": act.TargetName,
					"since_ts": act.SinceTS, "until_ts": act.ActivityUntilTS,
					"energy": act.Energy, "updated_ts": act.UpdatedTS,
				}
			}
			agents = append(agents, v)
			onboardingAgents[a.Key] = ev.JSON()
			if a.Joined {
				joinedCount++
			}
			if ev.ContactVisible {
				visibleCount++
			}
			if ev.LastDMID > 0 {
				dmCount++
			}
			if ev.LastDeliveredTS > 0 {
				deliveredCount++
			}
			pendingCommands += ev.PendingCommands
		}
	}
	// 最近一条失败任务的错误摘要，供后台直接看到「AI 节点出错」。
	lastError := map[string]any{}
	var errText, errAgent string
	var errTS float64
	if err := s.db.SQL.QueryRowContext(ctx, `
		SELECT r.err, COALESCE(t.agent,''), r.created_ts
		FROM ai_result r LEFT JOIN ai_task t ON t.id=r.task_id
		WHERE r.status='fallback' AND r.err<>''
		ORDER BY r.id DESC LIMIT 1`).Scan(&errText, &errAgent, &errTS); err == nil {
		lastError = map[string]any{
			"agent": errAgent, "error": safeAIError(errors.New(errText)), "ts": errTS,
		}
	}
	queue := map[string]any{}
	if q, err := s.db.AIQueueStats(ctx); err == nil {
		queue = map[string]any{
			"pending": q.Pending, "processing": q.Processing,
			"tool_pending": q.ToolPending, "tool_processing": q.ToolProcessing,
		}
	}
	// 四步开聊：状态只从已有事实推导，不让页面自己猜。
	// 优先级：急停 > 需登录（前端） > 缺 Key > 连接失败 > 命令等待 > 可试说；
	// 缺数据一律写「待确认」，不把「还没读到」当成「没问题」。
	step1, step1Display := "blocked", "先填写厂商地址、模型 ID 与 API Key"
	switch {
	case em.Stopped || em.Fault != "":
		step1, step1Display = "blocked", "紧急停止优先：先解除急停，再复测"
	case configState == "locked":
		step1, step1Display = "unknown", "配置状态待确认："+configError
	case credState != "configured":
		step1, step1Display = "blocked", "缺 Key：填写 API Key 并保存"
	case revision == 0 && strings.TrimSpace(modelID) == "":
		step1, step1Display = "blocked", "缺模型配置：填写厂商地址与模型 ID"
	case !retestCurrent:
		step1, step1Display = "current", "已保存，待复测（只测已保存连接）"
	case !retest.OK:
		step1, step1Display = "current", "复测失败："+retest.Error+"；只需重新复测"
	default:
		step1 = "done"
		step1Display = fmt.Sprintf("已保存并通过复测（%s / %s / 修订 %d）", destHost, modelID, revision)
	}

	step2, step2Display := "blocked", "先完成连接配置"
	switch {
	case len(agents) > 0:
		step2 = "done"
		step2Display = fmt.Sprintf("已有 %d 个角色；联系人 UID 由服务端自动分配", len(agents))
	case step1 == "done" || step1 == "current":
		step2, step2Display = "current", "还没有角色：填 Key 与展示名即可新增"
	}

	step3, step3Display := "blocked", "先创建角色"
	switch {
	case visibleCount > 0:
		step3 = "done"
		step3Display = fmt.Sprintf("%d 个角色已有游戏端证据（私聊或邮件）", visibleCount)
	case joinedCount > 0:
		step3 = "current"
		step3Display = fmt.Sprintf("%d 个角色已保存接入；在游戏联系人里看到并私聊后才算玩家可见", joinedCount)
	case step2 == "done":
		step3, step3Display = "current", "选一个角色点「接入游戏」"
	}

	step4, step4Display := "blocked", "先接入游戏"
	switch {
	case deliveredCount > 0:
		step4 = "done"
		step4Display = "已收到游戏内回复，游戏服已投递到个人消息"
	case dmCount > 0:
		step4 = "current"
		step4Display = "私聊已入队，等待 AI 节点回复"
	case joinedCount > 0:
		step4, step4Display = "current", "在游戏私聊里给角色发「你好」，回复会进入个人消息"
	}

	priority, statusDisplay, statusNext := "incomplete", "还有未完成的步骤", "按顺序完成下面的四步"
	switch {
	case em.Stopped || em.Fault != "":
		priority, statusDisplay = "emergency", em.Display
		statusNext = "到「连接」页解除紧急停止；解除后重新复测"
	case configState == "locked":
		priority, statusDisplay = "unknown", "待确认："+configError
		statusNext = "刷新页面；仍待确认时检查服务日志"
	case credState != "configured":
		priority, statusDisplay = "missing_key", "缺 Key：尚未配置可用凭据"
		statusNext = "到「连接」页填写 API Key 并保存"
	case retestCurrent && !retest.OK:
		priority, statusDisplay = "connection_failed", "连接失败："+retest.Error
		statusNext = "只重测已保存连接；仍失败时检查厂商地址、模型 ID 与 Key"
	case pendingCommands > 0:
		priority = "command_pending"
		statusDisplay = fmt.Sprintf("命令等待：%d 条运行命令待游戏服应用", pendingCommands)
		statusNext = "等待游戏服应用；被拒绝时到「性格与交互」重试未应用部分"
	case step1 == "done" && step2 == "done" && step3 == "done" && step4 == "done":
		priority, statusDisplay = "ready", "可试说：四步均有证据"
		statusNext = "保持游戏在线；就绪不等于玩家可见，以客户端回复为准"
	case step1 == "done":
		priority, statusDisplay = "ready", "可试说：连接已通过复测"
		statusNext = "按下面的主动作继续"
	}
	firstAgent := ""
	for _, a := range agents {
		if a.Joined {
			firstAgent = a.Display
			break
		}
	}
	if firstAgent == "" && len(agents) > 0 {
		firstAgent = agents[0].Display
	}
	firstSentence := "先在「角色与订阅」创建一个角色，再接入游戏。"
	if firstAgent != "" {
		firstSentence = fmt.Sprintf(
			"在游戏里打开联系人，找到「%s」，私聊发送「你好」；回复会进入个人消息。如果看不到联系人，先确认已点「接入游戏」，再重登客户端。",
			firstAgent)
	}
	onboarding := map[string]any{
		"status": map[string]any{
			"priority": priority, "display": statusDisplay, "next": statusNext,
		},
		"connection": map[string]any{
			"state": step1, "display": step1Display,
			"retest_current": retestCurrent, "retest_ok": retest.OK,
		},
		"steps": []map[string]any{
			{"id": "connection", "title": "保存模型厂商与密钥并复测", "budget_s": 90, "state": step1, "display": step1Display, "action": "retest"},
			{"id": "create", "title": "创建 Agent", "budget_s": 45, "state": step2, "display": step2Display, "action": "create"},
			{"id": "join", "title": "接入游戏并找到联系人", "budget_s": 45, "state": step3, "display": step3Display, "action": "join"},
			{"id": "hello", "title": "在游戏私聊发「你好」并收到回复", "budget_s": 60, "state": step4, "display": step4Display, "action": "say_hello"},
		},
		"first_sentence": map[string]any{
			"text": "你好", "agent": firstAgent, "display": firstSentence,
		},
		"agents":           onboardingAgents,
		"agents_total":     len(agents),
		"joined_total":     joinedCount,
		"visible_total":    visibleCount,
		"dm_total":         dmCount,
		"delivered_total":  deliveredCount,
		"pending_commands": pendingCommands,
		"buffer_s":         60,
		"target_total_s":   300,
	}
	writeJSON(w, map[string]any{
		"settings": settings, "specs": AISettingSpecs, "modules": AIModules,
		"security": security,
		"provider": providerName, "base_url": displayBase,
		"model_id": modelID, "api_key_masked": credMasked, "agents": agents,
		"web_search": map[string]any{
			"mode": searchConfig.EffectiveMode(), "native_supported": searchSupported,
			"native_confirmed_at": searchEvidence.ConfirmedAt,
		},
		"last_error": lastError, "bridge": s.aiBridgeHealth(), "queue": queue,
		// 配置发布状态（DEV-114 A）：页面只呈现实际去向、修订、凭据与复测状态。
		"revision":     revision,
		"config_state": configState, "config_error": configError, "config_source": source,
		"destination": map[string]any{
			"host": destHost, "base_url": displayBase, "model": modelID,
		},
		"credential": map[string]any{
			"state": credState, "masked": credMasked, "reason": credReason,
			"display": credentialDisplay(credState, credReason),
		},
		"retest": map[string]any{
			"revision": retest.Revision, "ok": retest.OK, "ts": retest.TS,
			"current": retestCurrent, "display": retestDisplay, "attempt": retest.Attempt,
		},
		"emergency_stop": map[string]any{
			"stopped": em.Stopped, "fault": em.Fault, "lifted_ts": em.LiftedTS,
			"display": em.Display, "epoch": strconv.FormatInt(em.Epoch, 10),
		},
		"saved_display": savedDisplay,
		"onboarding":    onboarding,
		// 说话节奏预设：页面只渲染，不自己维护一份预算数字。
		"talk_moods": aiconfig.TalkMoodPresets(),
	})
}

// credentialDisplay 把凭据状态映射成页面文案（不回显任何密钥内容）。
func credentialDisplay(state, reason string) string {
	switch state {
	case "configured":
		return "凭据已就绪"
	case "locked":
		return "凭据锁定：" + reason
	default:
		return "尚未配置密钥"
	}
}

// validBaseURL 只允许 http/https，避免把模型调用指到 file:// 等非预期协议。
func validBaseURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (s *Server) handleAIConfigSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL        string `json:"base_url"`
		ModelID        string `json:"model_id"`
		APIKey         string `json:"api_key"`
		Revision       int64  `json:"revision"`
		SearchEndpoint string `json:"search_endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	base, _, verr := aiconfig.ValidateDestinationURL(req.BaseURL)
	if verr != nil {
		httpError(w, 400, verr.Error())
		return
	}
	model := strings.TrimSpace(req.ModelID)
	ctx := r.Context()
	actor := adminOf(r)

	// 1) 同一读事务选定当前一套（活动修订 + 确切行 + 凭据引用）。
	cur, err := s.db.AIConfigSnapshot(ctx)
	if err != nil {
		s.log.l.Error("AI 配置快照不可用", "err", err)
		if errors.Is(err, store.ErrAIConfigHistoryConflict) || errors.Is(err, store.ErrAIConfigSnapshotInvalid) {
			httpError(w, 423, "配置已锁定："+configReason(err))
			return
		}
		httpError(w, 500, "配置读取失败："+configReason(err))
		return
	}
	targetBase := base
	if targetBase == "" {
		targetBase = cur.BaseURL
	}
	targetModel := model
	if targetModel == "" {
		targetModel = cur.ModelID
	}
	key := strings.TrimSpace(req.APIKey)

	// 2) 带旧修订的重放：字段与「实际密钥」都与活动套一致时按无变更返回，
	//    不新建修订、不写第二条成功记录（客户端丢回包后的安全重试）。
	if req.Revision > 0 && req.Revision != cur.Revision {
		if ok := s.aiReplayMatches(ctx, cur, targetBase, targetModel, key); ok {
			writeJSON(w, s.aiConfigView(ctx, cur.Revision, cur.BaseURL, cur.ModelID, true))
			return
		}
		httpError(w, 409, "配置已被其他管理员更新，请刷新后重试")
		return
	}
	if req.Revision == 0 && cur.Revision > 0 {
		if ok := s.aiReplayMatches(ctx, cur, targetBase, targetModel, key); ok {
			writeJSON(w, s.aiConfigView(ctx, cur.Revision, cur.BaseURL, cur.ModelID, true))
			return
		}
	}
	if targetBase == "" || targetModel == "" {
		httpError(w, 400, "请先填写厂商地址与模型 ID")
		return
	}
	sameOrigin := aiconfig.SameDestinationOrigin(cur.BaseURL, targetBase)
	if key == "" && !sameOrigin {
		httpError(w, 400, "更换厂商地址或端口时必须重新填写 API Key")
		return
	}

	// 3) 凭据：新 Key 先落不可覆盖的外部密文对象；留空只允许同源沿用。
	// migrated 只在「沿用旧安装已有凭据」时为真：那份凭据不是这次管理员输入的，
	// 配置记录的来源必须与显式保存区分开。
	credRef, credChanged, masked := "", false, ""
	migrated := false
	if key != "" {
		masterKey, kerr := s.ensureMasterKeyForSave(ctx, actor)
		if kerr != nil {
			s.log.l.Error("AI 主密钥不可用，凭据写入已锁定", "err", kerr)
			httpError(w, 500, s.saveFailureMessage(ctx)+"；"+credentialSaveAction(kerr))
			return
		}
		obj, cerr := aibridge.CreateCredentialObject(
			aiCredentialDir(), masterKey, cur.ProviderID, cur.ProviderName, key)
		if cerr != nil {
			s.log.l.Error("AI 凭据对象创建失败", "err", cerr)
			httpError(w, 500, s.saveFailureMessage(ctx)+"；"+credentialSaveAction(cerr))
			return
		}
		credRef, credChanged, masked = obj.Ref, true, obj.Masked
	} else {
		masterKey, merr := aibridge.LoadMasterKey(aiMasterKeyPath())
		if cur.CredentialRef == "" {
			// 还没有任何活动凭据引用：只能尝试旧安装兼容来源；
			// 主密钥都缺失时直接要求填 Key，不把「首次安装」报成锁定。
			if merr != nil {
				httpError(w, 400, "首次保存请填写 API Key")
				return
			}
			plain, rerr := s.aiResolveCurrentKey(ctx, cur, masterKey)
			if rerr != nil || strings.TrimSpace(plain) == "" {
				httpError(w, 400, "首次保存请填写 API Key")
				return
			}
			// 旧安装迁移：用原凭据生成不可变对象，原件字节不变、旧库行不动。
			obj, cerr := aibridge.CreateCredentialObject(
				aiCredentialDir(), masterKey, cur.ProviderID, cur.ProviderName, plain)
			if cerr != nil {
				s.log.l.Error("旧安装凭据迁移失败", "err", cerr)
				httpError(w, 500, s.saveFailureMessage(ctx)+"；"+credentialSaveAction(cerr))
				return
			}
			credRef, credChanged, masked = obj.Ref, true, obj.Masked
			migrated = true
		} else if merr != nil {
			httpError(w, 423, "当前凭据已锁定，无法沿用："+aiCredentialReason(merr))
			return
		} else {
			plain, rerr := s.aiResolveCurrentKey(ctx, cur, masterKey)
			if rerr != nil {
				httpError(w, 423, "当前凭据已锁定，无法沿用："+aiCredentialReason(rerr))
				return
			}
			if strings.TrimSpace(plain) == "" {
				httpError(w, 400, "首次保存请填写 API Key")
				return
			}
		}
	}

	// 4) 同一 DB 事务发布：目的地 + 模型 + 活动修订/引用 + 自动记录 + 审计。
	searchEndpoint := strings.TrimSpace(req.SearchEndpoint)
	searchChanged := searchEndpoint != "" && searchEndpoint != s.db.AISetting(ctx, aiconfig.KeySearchEndpoint, "")
	// 只有「沿用旧安装已有凭据」才是 migration；管理员这次输入了新 Key 的显式保存
	// （包括干净库首次保存）一律记 admin_save。
	source := "admin_save"
	if migrated {
		source = "migration"
	}
	pub, perr := s.db.AIPublishConfig(ctx, store.AIPublishRequest{
		ExpectedRevision:  req.Revision,
		PriorRevision:     cur.Revision,
		PriorBaseURL:      cur.BaseURL,
		ProviderID:        cur.ProviderID,
		ModelRowID:        cur.ModelRowID,
		BaseURL:           targetBase,
		ModelID:           targetModel,
		CredentialRef:     credRef,
		CredentialChanged: credChanged,
		CredentialMasked:  masked,
		Source:            source,
		Actor:             actor,
		Purpose:           []string{"chat", "catch_talk", "memory", "roundtable"},
		Contents:          []string{"chat_text", "player_identity", "fishing_records", "memory_summary"},
		SearchEndpoint:    searchEndpoint,

		SearchChanged: searchChanged,
		Now:           time.Now().Unix(),
	})
	if perr != nil {
		s.log.l.Error("AI 配置发布失败", "err", perr)
		switch {
		case errors.Is(perr, store.ErrAIConfigRevisionConflict), errors.Is(perr, store.ErrAIConfigSourceChanged):
			httpError(w, 409, "配置已被其他管理员更新，请刷新后重试")
		case errors.Is(perr, store.ErrAIConfigHistoryConflict), errors.Is(perr, store.ErrAIConfigSnapshotInvalid):
			httpError(w, 423, "配置已锁定："+configReason(perr))
		case errors.Is(perr, store.ErrAIConfigCredentialRequired):
			httpError(w, 400, "缺少可用凭据，请重新填写 API Key")
		default:
			httpError(w, 500, s.saveFailureMessage(ctx)+"；请重试保存")
		}
		return
	}
	s.log.l.Info("AI 配置已发布", "revision", pub.Revision,
		"dest", aiconfig.DestinationHost(pub.Active.BaseURL), "model", pub.Active.ModelID)
	writeJSON(w, s.aiConfigView(ctx, pub.Revision, pub.Active.BaseURL, pub.Active.ModelID, false))
}

// aiReplayMatches 判断一次提交是否只是「同一套已发布配置的重试」：
// 目标地址/模型与活动套一致，且页面带的 Key（若有）与活动凭据的实际明文一致。
// 密钥比较用常数时间，不把明文写进日志或响应。
func (s *Server) aiReplayMatches(ctx context.Context, cur store.AIConfigSnapshot, base, model, key string) bool {
	if cur.Revision <= 0 || strings.TrimSpace(base) != cur.BaseURL || strings.TrimSpace(model) != cur.ModelID {
		return false
	}
	if strings.TrimSpace(key) == "" {
		return true
	}
	masterKey, err := aibridge.LoadMasterKey(aiMasterKeyPath())
	if err != nil {
		return false
	}
	plain, err := s.aiResolveCurrentKey(ctx, cur, masterKey)
	if err != nil || strings.TrimSpace(plain) == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(key), []byte(plain)) == 1
}

// A failed write can race another publisher. Re-read the active selection before
// describing the connection that the user can still use.
func (s *Server) saveFailureMessage(ctx context.Context) string {
	active, err := s.db.AIConfigSnapshot(ctx)
	if err != nil {
		if errors.Is(err, store.ErrAIConfigHistoryConflict) || errors.Is(err, store.ErrAIConfigSnapshotInvalid) {
			return "保存失败，配置已锁定"
		}
		return "保存结果待确认，请刷新查看当前连接"
	}
	state, _, _ := s.aiCredentialStatusFor(ctx, active)
	if state == "locked" {
		return "保存失败，凭据已锁定"
	}
	host := aiconfig.DestinationHost(active.BaseURL)
	if state != "configured" || host == "" || strings.TrimSpace(active.ModelID) == "" {
		return "保存失败，尚未保存，没有可用连接"
	}
	if active.Revision == 0 {
		return fmt.Sprintf("保存失败，仍使用原配置（旧配置 · %s / %s）", host, active.ModelID)
	}
	return fmt.Sprintf("保存失败，仍使用原配置（%s / %s / 修订 %d）", host, active.ModelID, active.Revision)
}

// aiConfigView 构造保存/重放响应的可安全展示状态：只给域、模型、修订、
// 凭据状态与复测状态，不回显 Key、完整地址或玩家内容。
func (s *Server) aiConfigView(ctx context.Context, revision int64, baseURL, modelID string, replayed bool) map[string]any {
	host := aiconfig.DestinationHost(baseURL)
	credState, masked, credReason := s.aiCredentialStatus(ctx)
	snap, reading, retest, snapErr := s.db.AIConnectionState(ctx)
	retestCurrent := snapErr == nil && snap.Revision == revision && currentAIRetest(aiconfig.EvaluateEmergency(reading), revision, retest)
	retestDisplay := "待复测"
	if retestCurrent {
		if retest.OK {
			retestDisplay = "此连接已通过复测"
		} else {
			retestDisplay = "复测失败：" + retest.Error
		}
	}
	message := fmt.Sprintf("已保存 · %s / %s / 修订 %d，待复测", host, modelID, revision)
	if retestCurrent && retest.OK {
		message = fmt.Sprintf("此连接已通过复测 · %s / %s / 修订 %d", host, modelID, revision)
	}
	return map[string]any{
		"ok": true, "saved": true, "replayed": replayed,
		"revision": revision, "state": "saved_pending_retest", "message": message,
		"destination": map[string]any{
			"host": host, "base_url": aiconfig.RedactURLForDisplay(baseURL), "model": modelID,
		},
		"credential": map[string]any{
			"state": credState, "masked": masked, "reason": credReason,
			"display": credentialDisplay(credState, credReason),
		},
		"retest": map[string]any{
			"revision": retest.Revision, "ok": retest.OK, "ts": retest.TS,
			"current": retestCurrent, "display": retestDisplay, "attempt": retest.Attempt,
		},
	}
}

func currentAIRetest(em aiconfig.EmergencyDecision, revision int64, st aiconfig.RetestStatus) bool {
	active, _ := em.IntentActive(st.Epoch)
	return active && st.Attempt != "" && !st.Pending && st.Revision == revision
}

// handleAISettingsSave 按参数注册表通用写入。
// 参数声明见 ai_settings.go：新增参数只改注册表一行，不用再动这里。
func (s *Server) handleAISettingsSave(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	// 授权/目的地/配置发布元数据只能走专用接口。普通整卡保存必须整笔拒收，
	// 否则一次「保存连接卡片」就可能伪造发布或授权（T-006 §6.3 / T-102）。
	for key := range body {
		if aiconfig.IsProtectedConfigKey(key) {
			httpError(w, 400, "授权与配置发布元数据只能由专用接口修改")
			return
		}
	}
	ctx := r.Context()
	var seen *int64
	if raw, ok := body[aiconfig.EmergencyEpochKey]; ok {
		value := strings.Trim(string(raw), "\"")
		if epoch, err := strconv.ParseInt(value, 10, 64); err == nil && epoch > 0 {
			seen = &epoch
		}
	}
	updates := make(map[string]string, len(body))
	for key, raw := range body {
		spec, ok := settingByKey[key]
		if !ok {
			continue // 未知 key 静默忽略，避免把前端拼错的字段写进库
		}
		var val any
		if err := json.Unmarshal(raw, &val); err != nil {
			continue
		}
		if key == aiconfig.KeySearchMode {
			mode, _ := val.(string)
			if !aiconfig.ValidSearchMode(strings.TrimSpace(mode)) {
				httpError(w, 400, "搜索方式不合法")
				return
			}
		}
		// 氛围档只接受预设或 custom；非法值直接拒绝，避免库里留下无法解释的值。
		if key == "ai_fishing_mood" {
			id, _ := val.(string)
			id = strings.TrimSpace(id)
			if id != aiconfig.TalkMoodCustom {
				if _, ok := aiconfig.TalkMoodByID(id); !ok {
					httpError(w, 400, "氛围档不合法")
					return
				}
			}
			updates[key] = id
			continue
		}
		if v, valid := NormalizeSettingValue(spec, val); valid {
			updates[key] = v
		}
	}
	if len(updates) == 0 {
		writeJSON(w, map[string]any{"ok": true, "updated": 0})
		return
	}
	// 变更搜索去向时，同事务记一条「实际搜索端 + 用途」；不与模型凭据共用。
	var rec *aiconfig.ConfigRecord
	if endpoint, ok := updates[aiconfig.KeySearchEndpoint]; ok {
		revision := int64(0)
		if snap, serr := s.db.AIConfigSnapshot(ctx); serr == nil {
			revision = snap.Revision
		}
		rec = &aiconfig.ConfigRecord{
			Schema: aiconfig.ConfigSchemaVersion, Revision: revision, Kind: "search",
			Actor: adminOf(r), Source: "admin_settings", TS: time.Now().Unix(),
			Purpose: []string{"web_search"}, SearchHost: aiconfig.DestinationHost(endpoint),
			SearchEndpoint: aiconfig.RedactURLForDisplay(endpoint),
		}
	}
	if err := s.db.AISetAdminSettings(ctx, updates, seen, adminOf(r), rec); err != nil {
		if errors.Is(err, store.ErrAIEmergencyConflict) {
			httpError(w, http.StatusConflict, "急停状态已变化或不可确认，请刷新后重试")
			return
		}
		httpError(w, 500, "设置保存失败")
		return
	}
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_settings", "global", "keys="+strings.Join(keys, ","))
	writeJSON(w, map[string]any{"ok": true, "updated": len(updates)})
}

// handleAIConsentConfirm 记录一项显式授权。四项分别确认、分别记录；
// 只有 outbound 授权会进入 aiconfig.Egress 的放行分支。
func (s *Server) handleAIConsentConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Item string `json:"item"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	item := strings.TrimSpace(req.Item)
	spec, ok := aiconfig.ConsentItemSpecFor(item)
	if !ok {
		httpError(w, 400, "未知的授权项")
		return
	}
	subject := strings.TrimSpace(adminOf(r))
	if subject == "" {
		httpError(w, 403, "缺少操作主体")
		return
	}
	ctx := r.Context()
	dest, destOK := s.aiDestination(ctx)
	if spec.RequiresDestination && !destOK {
		httpError(w, 409, "请先保存厂商地址与模型 ID，再确认授权")
		return
	}
	key, _ := aiconfig.ConsentKeyFor(item)
	prevRaw := s.db.AISetting(ctx, key, "")
	if prev, err := aiconfig.ParseConsent(prevRaw); err == nil &&
		prev.Item == item && prev.State == aiconfig.ConsentGranted &&
		prev.Subject == subject && aiconfig.ConsentScopeMatches(prev.Scope, item, dest) {
		// 重复确认幂等：同样的主体与范围不重写记录，时间戳也不变。
		writeJSON(w, map[string]any{"ok": true, "consent": s.aiConsentStatus(ctx, item)})
		return
	}
	rec, _ := aiconfig.NewConsentRecord(item, subject, time.Now().Unix(), dest)
	raw, err := aiconfig.MarshalConsent(rec)
	if err != nil {
		httpError(w, 500, "授权记录生成失败")
		return
	}
	updates := map[string]string{key: raw}
	if spec.RequiresDestination {
		destRaw, _ := aiconfig.MarshalDestination(dest)
		// 授权与目的地快照同事务写入，避免「新授权 + 旧范围」的半套状态。
		updates[aiconfig.KeyEgressDestination] = destRaw
	}
	if err := s.db.AISetSettings(ctx, updates); err != nil {
		httpError(w, 500, "授权写入失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, subject, "ai_consent", item,
		consentAuditDetail(item, aiconfig.ConsentGranted, rec.Scope))
	writeJSON(w, map[string]any{"ok": true, "consent": s.aiConsentStatus(ctx, item)})
}

// handleAIConsentRevoke 撤销一项授权。撤销是安全动作，不依赖厂商配置是否完整；
// 写入后所有后续出口检查立即读到 revoked。
func (s *Server) handleAIConsentRevoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Item string `json:"item"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	item := strings.TrimSpace(req.Item)
	if _, ok := aiconfig.ConsentItemSpecFor(item); !ok {
		httpError(w, 400, "未知的授权项")
		return
	}
	subject := strings.TrimSpace(adminOf(r))
	if subject == "" {
		subject = "unknown"
	}
	ctx := r.Context()
	key, _ := aiconfig.ConsentKeyFor(item)
	prev, err := aiconfig.ParseConsent(s.db.AISetting(ctx, key, ""))
	if err == nil && prev.Item == item && prev.State == aiconfig.ConsentRevoked {
		writeJSON(w, map[string]any{"ok": true, "consent": s.aiConsentStatus(ctx, item)})
		return
	}
	rec := aiconfig.NewRevokedConsentRecord(item, subject, time.Now().Unix(), prev.Scope)
	raw, err := aiconfig.MarshalConsent(rec)
	if err != nil {
		httpError(w, 500, "撤销记录生成失败")
		return
	}
	if err := s.db.AISetSettings(ctx, map[string]string{key: raw}); err != nil {
		httpError(w, 500, "撤销写入失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, subject, "ai_consent", item,
		consentAuditDetail(item, aiconfig.ConsentRevoked, rec.Scope))
	writeJSON(w, map[string]any{"ok": true, "consent": s.aiConsentStatus(ctx, item)})
}

// aiDestination 读取当前启用厂商的目的地范围；地址或模型为空时 ok=false。
func (s *Server) aiDestination(ctx context.Context) (aiconfig.ConsentDestination, bool) {
	var base, model string
	err := s.db.SQL.QueryRowContext(ctx, `
		SELECT p.base_url, COALESCE(m.model_id,'')
		FROM ai_provider p LEFT JOIN ai_model m ON m.provider_id=p.id AND m.enabled=1
		WHERE p.enabled=1 ORDER BY p.id, m.id LIMIT 1`).Scan(&base, &model)
	if err != nil {
		return aiconfig.ConsentDestination{}, false
	}
	dest := aiconfig.ConsentDestination{
		BaseURL: strings.TrimSpace(base), ModelID: strings.TrimSpace(model),
	}
	if dest.BaseURL == "" || dest.ModelID == "" {
		return dest, false
	}
	return dest, true
}

func (s *Server) aiConsentStatus(ctx context.Context, item string) aiconfig.ConsentStatus {
	return aiconfig.EvaluateConsent(func(key string) string {
		return s.db.AISetting(ctx, key, "")
	}, item)
}

// consentAuditDetail 只写项名、状态、内容版本与目的地摘要；
// 原始密钥、完整地址与玩家内容都不进授权审计。
func consentAuditDetail(item, state string, scope aiconfig.ConsentScope) string {
	detail := "item=" + item + " state=" + state + " content_version=" + strconv.Itoa(scope.ContentVersion)
	if scope.DestBaseURL != "" {
		sum := sha256.Sum256([]byte(scope.DestBaseURL + "\n" + scope.DestModel))
		detail += " dest_sha256=" + hex.EncodeToString(sum[:6])
	}
	return detail
}

func (s *Server) handleAIAgentSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key             string `json:"key"`
		ThinkingEnabled bool   `json:"thinking_enabled"`
		ThinkingEffort  string `json:"thinking_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	switch req.ThinkingEffort {
	case "off", "low", "medium", "high", "max":
	default:
		req.ThinkingEffort = "off"
	}
	ctx := r.Context()
	enabled := 0
	if req.ThinkingEnabled {
		enabled = 1
	}
	res, err := s.db.SQL.ExecContext(ctx, `
		UPDATE ai_agent SET thinking_enabled=?, thinking_effort=? WHERE key=?`,
		enabled, req.ThinkingEffort, req.Key)
	if err != nil {
		httpError(w, 500, "Agent 保存失败")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpError(w, 404, "Agent 不存在")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent", req.Key,
		"thinking="+strconv.Itoa(enabled)+" effort="+req.ThinkingEffort)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleAIChannels(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("name")
	if channel == "" {
		channel = "nav1_03"
	}
	ctx := r.Context()
	type agentView struct {
		Key     string `json:"key"`
		Display string `json:"display"`
	}
	agents := []agentView{}
	agentList, err := s.db.AIListAgents(ctx)
	if err != nil {
		httpError(w, 500, "读取角色列表失败")
		return
	}
	for _, a := range agentList {
		agents = append(agents, agentView{a.Key, a.Display})
	}
	subscribed, err := s.db.AIChannelSubscribed(ctx, channel)
	if err != nil {
		httpError(w, 500, "读取频道订阅失败")
		return
	}
	// 频道下拉：数据库里见过的频道 + 已接入 Agent 数/名单。
	channelList, err := s.db.AIListChannels(ctx)
	if err != nil {
		httpError(w, 500, "读取频道列表失败")
		return
	}
	channels := make([]map[string]any, 0, len(channelList))
	for _, c := range channelList {
		channels = append(channels, map[string]any{
			"name": c.Name, "label": c.Label, "count": c.Count, "agents": c.Agents,
			"agent_keys": emptyIfNil(c.AgentKeys),
		})
	}
	// 地图级订阅：官服一张图有十几个到上百个区域频道，按地图订阅才不用逐个拉。
	mapList, err := s.db.AIListMaps(ctx)
	if err != nil {
		httpError(w, 500, "读取地图列表失败")
		return
	}
	maps := make([]map[string]any, 0, len(mapList))
	for _, m := range mapList {
		maps = append(maps, map[string]any{
			"map_key": m.MapKey, "label": m.Label, "count": m.Count, "agents": m.Agents,
			"agent_keys": emptyIfNil(m.AgentKeys),
		})
	}
	// 当前频道所属的地图：让后台能直接显示「这个频道归哪张图管」。
	curMap, curMapKey := "", ""
	if k, ok := s.db.ChannelMapKey(ctx, channel); ok {
		curMapKey = k
		for _, m := range mapList {
			if m.MapKey == k {
				curMap = m.Label
				break
			}
		}
	}
	writeJSON(w, map[string]any{
		"channel": channel, "agents": agents, "subscribed": subscribed,
		"channels": channels, "maps": maps,
		"channel_map_key": curMapKey, "channel_map_label": curMap,
	})
}

func (s *Server) handleAIChannelSave(w http.ResponseWriter, r *http.Request) {
	ctx := s.bindAIAdminIntent(r.Context())
	var req struct {
		Channel string  `json:"channel"`
		Agent   string  `json:"agent"`
		Enabled bool    `json:"enabled"`
		Label   *string `json:"label"`
		MapKey  string  `json:"map_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	// 地图级订阅：map_key 非空时按地图写，与精确频道订阅互不影响。
	if req.MapKey != "" {
		if req.Agent == "" {
			httpError(w, 400, "请选择 Agent")
			return
		}
		if err := s.db.AISetMapAgent(ctx, req.MapKey, req.Agent, req.Enabled); err != nil {
			httpError(w, 500, "地图订阅保存失败")
			return
		}
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_map_agent", req.MapKey,
			"agent="+req.Agent+" enabled="+strconv.FormatBool(req.Enabled))
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if req.Channel == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	if req.Label != nil {
		if err := s.db.AISetChannelLabel(ctx, req.Channel, strings.TrimSpace(*req.Label)); err != nil {
			httpError(w, 500, "显示名保存失败")
			return
		}
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_channel_label", req.Channel, strings.TrimSpace(*req.Label))
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if req.Agent == "" {
		httpError(w, 400, "请选择 Agent")
		return
	}
	payload, _ := json.Marshal(store.AIRuntimeChannelSetMemberPayload{
		Channel: req.Channel, Enabled: req.Enabled,
	})
	cmd, ok := s.enqueueAIIntent(ctx, w, store.AIRuntimeEnqueueRequest{
		Source:   store.AIRuntimeSourceAdmin,
		Kind:     store.AIRuntimeKindChannelSetMember,
		AgentKey: req.Agent,
		Payload:  payload,
	})
	if !ok {
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_channel", req.Channel,
		"agent="+req.Agent+" enabled="+strconv.FormatBool(req.Enabled)+" accepted")
	writeAICommandAccepted(w, cmd)
}

// safeAIError 把厂商错误体里的密钥抹掉并截断，避免后台把 Key 回显出来。
func safeAIError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, "sk-"); i >= 0 {
		s = s[:i] + "sk-***"
	}
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

// aiEgressHostPolicy 从部署配置构造出口主机白名单（三个出口共用）。
// config.Load 已在启动时校验；这里仍按 fail-closed 处理构造失败：
// 退回空白名单（拒绝一切外发），不退回「不限制」。
func (s *Server) aiEgressHostPolicy() aiconfig.EgressHostPolicy {
	policy, err := aiconfig.NewEgressHostPolicy(s.cfg.AIEgressAllowedHosts)
	if err != nil {
		return aiconfig.EgressHostPolicy{}
	}
	return policy
}

// newAITestProvider 是「复测已保存连接」构造 Provider 的注入点：生产实现只走
// aibridge.NewProvider；测试替换它注入本地 fixture Transport。
var newAITestProvider = func(spec aibridge.ProviderSpec, timeout time.Duration) (*aibridge.OpenAICompatProvider, error) {
	return aibridge.NewProvider(spec, timeout)
}

// handleAITest 用「已保存」的活动配置发一次最小请求，确认厂商/模型/Key 联通。
//
// V2 语义（DEV-104 / DEV-114 A）：
//   - 只看急停；不再要求 consent / 出网开关 / 部署主机名单；
//   - 只读活动快照（地址、模型、修订、凭据在同一次读取中绑定），页面未保存的
//     地址/模型/Key 一律拒绝，绝不把新输入发到旧域；
//   - 只使用固定合成提示，不携带玩家内容；
//   - 复测结果按被复测的修订记录：迟到的旧响应不会把新配置标成已测。
func (s *Server) handleAITest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reading := s.db.AIEmergencyState(ctx)
	em := aiconfig.EvaluateEmergency(reading)
	var req struct {
		BaseURL string `json:"base_url"`
		ModelID string `json:"model_id"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	// 先判急停：拒绝时连主密钥都不读取，更不会建立外部连接。
	// 同一读取同时取开关与解除点；读取失败/解除记录损坏同样不放行。
	if blocked, _ := em.RecordBlocked(); blocked {
		writeAITestEgressDenied(w, aiconfig.EgressStatus{
			State: aiconfig.StateEmergency, Display: "关（紧急停止）",
		})
		return
	}
	base, retest, err := s.db.AIBeginRetest(ctx, aiconfig.RetestStatus{
		Epoch: em.Epoch, Attempt: newAIRequestID(), TS: time.Now().Unix(), Actor: adminOf(r),
	})
	if err != nil {
		if errors.Is(err, store.ErrAIIntentInactive) {
			writeAITestStale(w, 0)
		} else {
			writeJSON(w, map[string]any{"ok": false, "error": "无法读取已保存的连接：" + configReason(err), "state": "config_locked"})
		}
		return
	}
	// 只读加载主密钥：缺失不新建；已有密文时缺失即锁定。
	masterKey, err := aibridge.LoadMasterKey(aiMasterKeyPath())
	if err != nil {
		writeJSON(w, map[string]any{
			"ok": false, "error": "复测不可用：" + aiCredentialReason(err),
			"state": "master_key_unavailable", "credential": map[string]any{"state": "locked"},
		})
		return
	}
	snap, err := aibridge.ResolveSnapshot(ctx, s.db, base, aiMasterKeyPath(), masterKey)
	if err != nil {
		state := "credential_locked"
		reason := aiCredentialReason(err)
		if errors.Is(err, store.ErrAIConfigHistoryConflict) || errors.Is(err, store.ErrAIConfigSnapshotInvalid) {
			state, reason = "config_locked", configReason(err)
		}
		writeJSON(w, map[string]any{
			"ok": false, "error": "已保存的连接不可用：" + reason, "state": state,
		})
		return
	}
	if strings.TrimSpace(snap.Spec.APIKey) == "" {
		writeJSON(w, map[string]any{
			"ok": false, "error": "尚未配置可用的 API Key，请先保存连接", "state": "not_configured",
		})
		return
	}
	// 页面字段只用于核对是否已保存；真正外发只用活动快照里的配置与 Key。
	if msg := aiTestUnsavedInput(req.BaseURL, req.ModelID, req.APIKey, snap.Spec); msg != "" {
		writeJSON(w, map[string]any{"ok": false, "error": msg, "state": "unsaved_input"})
		return
	}
	host := aiconfig.DestinationHost(snap.Spec.BaseURL)
	revision := snap.Revision()
	provider, err := newAITestProvider(snap.Spec, 12*time.Second)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": safeAIError(err)})
		return
	}
	// 发送前最后一刻再查：预检与发送之间的急停/新发布同样拦住。
	// 与 aibridge.Runner 安装的是同一个判定。
	provider.SetEgressGate(func(gctx context.Context) error {
		return s.db.AIValidateRetest(gctx, retest, snap.CredentialRef)
	})
	started := time.Now()
	resp, err := provider.Chat(ctx, aibridge.ChatRequest{
		Messages:  []aibridge.Message{{Role: "user", Content: "请只回复两个字：正常"}},
		MaxTokens: 16,
	})
	latency := time.Since(started).Milliseconds()
	if gateErr := s.db.AIValidateRetest(ctx, retest, snap.CredentialRef); gateErr != nil {
		writeAITestStale(w, revision)
		return
	}
	retest.OK = err == nil
	if err != nil {
		retest.Error = aiRetestFailureAction(err)
	}
	if err := s.db.AIRecordRetest(ctx, retest, snap.CredentialRef); err != nil {
		writeAITestStale(w, revision)
		return
	}
	if err != nil {
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_test", snap.Spec.Name, "failed")
		var denied *aibridge.EgressDeniedError
		if errors.As(err, &denied) {
			writeAITestEgressDenied(w, aiconfig.EgressStatus{State: denied.State, Display: denied.Display})
			return
		}
		var stale *aibridge.StaleConfigError
		if errors.As(err, &stale) {
			writeJSON(w, map[string]any{
				"ok": false, "revision": revision, "state": "stale_revision",
				"error": "配置已更新，本次复测作废，请重新复测",
			})
			return
		}
		var hostDenied *aiconfig.HostPolicyError
		if errors.As(err, &hostDenied) {
			writeAITestHostDenied(w, hostDenied)
			return
		}
		writeJSON(w, map[string]any{
			"ok": false, "latency_ms": latency, "revision": revision,
			"epoch": strconv.FormatInt(em.Epoch, 10), "attempt": retest.Attempt,
			"destination": map[string]any{"host": host, "model": snap.Spec.ModelID},
			"provider":    snap.Spec.Name, "model": snap.Spec.ModelID,
			"error": aiRetestFailureAction(err),
			"state": "retest_failed",
		})
		return
	}
	sample := strings.TrimSpace(resp.Text)
	if rs := []rune(sample); len(rs) > 40 {
		sample = string(rs[:40])
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_test", snap.Spec.Name, "ok")
	writeJSON(w, map[string]any{
		"ok": true, "latency_ms": latency, "revision": revision,
		"epoch": strconv.FormatInt(em.Epoch, 10), "attempt": retest.Attempt,
		"destination": map[string]any{"host": host, "model": snap.Spec.ModelID},
		"provider":    snap.Spec.Name, "model": snap.Spec.ModelID, "sample": sample,
		"state":   "retested",
		"message": fmt.Sprintf("此连接已通过复测 · %s / %s / 修订 %d", host, snap.Spec.ModelID, revision),
	})
}

func writeAITestStale(w http.ResponseWriter, revision int64) {
	writeJSON(w, map[string]any{
		"ok": false, "revision": revision, "state": "stale_retest",
		"error": "急停状态、配置或复测请求已变化，请重新复测",
	})
}

// writeAITestEgressDenied 返回与运行期同一套 aiconfig 判定原因（state + display）。
func writeAITestEgressDenied(w http.ResponseWriter, status aiconfig.EgressStatus) {
	writeJSON(w, map[string]any{
		"ok": false, "error": "测试连接已拒绝：" + status.Display,
		"state": status.State, "egress": status,
	})
}

// writeAITestEgressError 把出口检查错误还原成 aiconfig 的判定原因。
func writeAITestEgressError(w http.ResponseWriter, err error) {
	var denied *aibridge.EgressDeniedError
	if errors.As(err, &denied) {
		writeAITestEgressDenied(w, aiconfig.EgressStatus{State: denied.State, Display: denied.Display})
		return
	}
	writeJSON(w, map[string]any{"ok": false, "error": "测试连接已拒绝：外发判定失败"})
}

// writeAITestHostDenied 返回传输层拒绝原因（非 HTTPS、userinfo、
// 解析到非公网地址等）。部署主机名单已不再参与放行判断。
func writeAITestHostDenied(w http.ResponseWriter, err error) {
	state, display := aiconfig.StateEgressHostDenied, "关（出口地址被拒绝）"
	var denied *aiconfig.HostPolicyError
	if errors.As(err, &denied) {
		state, display = denied.State, denied.Display
	}
	writeJSON(w, map[string]any{
		"ok": false, "error": "复测已拒绝：" + display, "state": state,
	})
}

// aiTestUnsavedInput 核对页面字段没有超出已保存配置。返回空串表示可以继续；
// 任何未保存的地址/模型/Key 都直接拒绝，绝不用它扩大本次外发的范围。
func aiTestUnsavedInput(baseURL, modelID, apiKey string, spec aibridge.ProviderSpec) string {
	if v := strings.TrimSpace(baseURL); v != "" && v != strings.TrimSpace(spec.BaseURL) {
		return "页面上的厂商地址尚未保存：请先保存厂商配置，再测试连接"
	}
	if v := strings.TrimSpace(modelID); v != "" && v != strings.TrimSpace(spec.ModelID) {
		return "页面上的模型 ID 尚未保存：请先保存厂商配置，再测试连接"
	}
	if v := strings.TrimSpace(apiKey); v != "" &&
		subtle.ConstantTimeCompare([]byte(v), []byte(spec.APIKey)) != 1 {
		return "页面输入的 API Key 与已保存的不一致：请先保存密钥，再测试连接"
	}
	return ""
}

func aiRetestFailureAction(err error) string {
	switch {
	case strings.HasPrefix(err.Error(), "ai: 厂商返回 401:"), strings.HasPrefix(err.Error(), "ai: 厂商返回 403:"):
		return "厂商拒绝鉴权，请核对该厂商的测试 Key 和模型权限，再保存并复测"
	case strings.HasPrefix(err.Error(), "ai: 厂商返回 429:"):
		return "厂商限流，请稍后复测已保存连接"
	default:
		return "连接测试失败，请检查网络和模型 ID，再复测已保存连接"
	}
}

func validAgentKey(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// handleAIAgentCreate 先建 Agent（角色 + 系统提示词），默认不接入游戏。
func (s *Server) handleAIAgentCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key             string   `json:"key"`
		Display         string   `json:"display"`
		Role            string   `json:"role"`
		Prompt          string   `json:"prompt"`
		Tools           []string `json:"tools"`
		ThinkingEnabled bool     `json:"thinking_enabled"`
		ThinkingEffort  string   `json:"thinking_effort"`
		ContactUID      int64    `json:"contact_uid"`
		ReplyMode       string   `json:"reply_mode"`
		Talkativeness   *int     `json:"talkativeness"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	if req.ContactUID != 0 {
		// B-103：新增只由服务端分配联系人 UID；手填会让「恢复旧角色」与
		// 历史私聊对不上号。旧脚本带该字段一律拒绝，而不是悄悄覆盖。
		httpError(w, 400, "联系人 UID 由服务端自动分配，新增时不需要填写")
		return
	}
	if !validAgentKey(req.Key) {
		httpError(w, 400, "key 只能用小写字母、数字、- 和 _")
		return
	}
	if strings.TrimSpace(req.Display) == "" {
		httpError(w, 400, "请填写展示名")
		return
	}
	switch req.ThinkingEffort {
	case "off", "low", "medium", "high", "max":
	default:
		req.ThinkingEffort = "off"
	}
	if req.Tools == nil {
		req.Tools = []string{}
	}
	toolsJSON, _ := json.Marshal(req.Tools)
	talk := 50
	if req.Talkativeness != nil {
		talk = *req.Talkativeness
	}
	ctx := r.Context()
	uid, revived, err := s.db.AICreateAgent(ctx, store.AIAgent{
		Key: req.Key, Display: strings.TrimSpace(req.Display),
		Role: strings.TrimSpace(req.Role), ContactUID: req.ContactUID,
		Prompt: req.Prompt, ThinkingEnabled: req.ThinkingEnabled,
		ThinkingEffort: req.ThinkingEffort,
		ReplyMode:      req.ReplyMode, Talkativeness: talk,
	}, string(toolsJSON))
	if errors.Is(err, store.ErrAIAgentExists) {
		httpError(w, 400, "这个 Key 已经有一个在用的 Agent")
		return
	}
	if errors.Is(err, store.ErrAIContactUIDUsed) {
		httpError(w, 400, "这个联系人 UID 已经被占用")
		return
	}
	if errors.Is(err, store.ErrAIContactUIDInvalid) {
		httpError(w, 400, "联系人 UID 必须留空或使用 2147480000-2147483645")
		return
	}
	if err != nil {
		httpError(w, 500, "Agent 创建失败")
		return
	}
	action := "created"
	if revived {
		action = "revived"
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_create", req.Key,
		action+" uid="+strconv.FormatInt(uid, 10))
	writeJSON(w, map[string]any{
		"ok": true, "key": req.Key, "contact_uid": uid, "revived": revived,
		"history_preserved": revived, "uid_auto": true,
	})
}

// handleAIAgentJoin 接入/移出游戏；接入时才分配联系人 UID。
// B-103：join 只接受 key/joined，UID 一律由服务端在首次接入时分配。
func (s *Server) handleAIAgentJoin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key    string `json:"key"`
		Joined bool   `json:"joined"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	ctx := r.Context()
	a, err := s.db.AIAgentByKey(ctx, req.Key)
	if err != nil || a == nil {
		httpError(w, 404, "Agent 不存在")
		return
	}
	uid := a.ContactUID
	if req.Joined && uid == 0 {
		next, err := s.db.AINextContactUID(ctx)
		if err != nil {
			httpError(w, 500, "没有可用的联系人 UID")
			return
		}
		uid = next
	}
	if err := s.db.AISetAgentJoined(ctx, req.Key, req.Joined, uid); err != nil {
		if errors.Is(err, store.ErrAIContactUIDInvalid) {
			httpError(w, 400, "联系人 UID 必须使用 2147480000-2147483645")
			return
		}
		httpError(w, 500, "接入状态保存失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_join", req.Key,
		"joined="+strconv.FormatBool(req.Joined)+" uid="+strconv.FormatInt(uid, 10))
	// 200 直接写库：接入不是运行命令，不能伪造 202；是否玩家可见由后续游戏端证据决定。
	writeJSON(w, map[string]any{
		"ok": true, "saved": true, "joined": req.Joined, "contact_uid": uid,
	})
}

// handleAIAgentUpdate 改展示名、角色与系统提示词。
func (s *Server) handleAIAgentUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string    `json:"key"`
		Display string    `json:"display"`
		Role    string    `json:"role"`
		Prompt  string    `json:"prompt"`
		Tools   *[]string `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	if strings.TrimSpace(req.Display) == "" {
		httpError(w, 400, "请填写展示名")
		return
	}
	ctx := r.Context()
	// 没传 tools 时保留原白名单，避免「只改提示词却把工具清空」。
	toolsJSON := ""
	if req.Tools != nil {
		b, _ := json.Marshal(*req.Tools)
		toolsJSON = string(b)
	} else {
		var cur string
		_ = s.db.SQL.QueryRowContext(ctx, `SELECT tools_json FROM ai_agent WHERE key=?`, req.Key).Scan(&cur)
		toolsJSON = cur
	}
	if err := s.db.AIUpdateAgent(ctx, req.Key, strings.TrimSpace(req.Display),
		strings.TrimSpace(req.Role), req.Prompt, toolsJSON); err != nil {
		httpError(w, 500, "Agent 保存失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_update", req.Key, "updated")
	writeJSON(w, map[string]any{"ok": true})
}

// handleAIAgentDelete 逻辑删除 Agent：聊天对象与历史保留，key 与 UID 继续占位。
func (s *Server) handleAIAgentDelete(w http.ResponseWriter, r *http.Request) {
	ctx := s.bindAIAdminIntent(r.Context())
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	if a, err := s.db.AIAgentByKey(ctx, req.Key); err != nil || a == nil {
		httpError(w, 404, "Agent 不存在或已删除")
		return
	}
	cmd, ok := s.enqueueAIIntent(ctx, w, store.AIRuntimeEnqueueRequest{
		Source:   store.AIRuntimeSourceAdmin,
		Kind:     store.AIRuntimeKindAgentDelete,
		AgentKey: req.Key,
	})
	if !ok {
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_delete", req.Key, "accepted")
	writeAICommandAccepted(w, cmd)
}

// aiBridgeHealth 探测 aibridge 的观测端口，让后台能看到 AI 进程是否在线。
func (s *Server) aiBridgeHealth() map[string]any {
	addr := aiBridgeAddr
	if v := os.Getenv("RF4_AI_OBS_ADDR"); v != "" {
		addr = v
	}
	// 配置里可能写 ":9504" 或 "0.0.0.0:9504"，探测时统一走本机回环。
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	} else if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return map[string]any{"online": false}
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	body["online"] = resp.StatusCode == 200
	return body
}

// handleAIMemoryClear 清空某个 Agent 的战绩记忆。
func (s *Server) handleAIMemoryClear(w http.ResponseWriter, r *http.Request) {
	ctx := s.bindAIAdminIntent(r.Context())
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	cmd, ok := s.enqueueAIIntent(ctx, w, store.AIRuntimeEnqueueRequest{
		Source:   store.AIRuntimeSourceAdmin,
		Kind:     store.AIRuntimeKindMemoryClear,
		AgentKey: req.Key,
	})
	if !ok {
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_memory_clear", req.Key, "accepted")
	writeAICommandAccepted(w, cmd)
}

// handleAIAgentBehavior 保存「什么时候开口」：主动/点名/沉默，以及活跃度。
func (s *Server) handleAIAgentBehavior(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key           string `json:"key"`
		ReplyMode     string `json:"reply_mode"`
		Talkativeness *int   `json:"talkativeness"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	if req.ReplyMode != "social" && req.ReplyMode != "mention" && req.ReplyMode != "silent" {
		httpError(w, 400, "回复方式不合法")
		return
	}
	talk := 50
	if req.Talkativeness != nil {
		talk = *req.Talkativeness
	}
	ctx := r.Context()
	if a, err := s.db.AIAgentByKey(ctx, req.Key); err != nil || a == nil {
		httpError(w, 404, "Agent 不存在")
		return
	}
	if err := s.db.AISetAgentBehavior(ctx, req.Key, req.ReplyMode, talk); err != nil {
		httpError(w, 500, "说话方式保存失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_behavior", req.Key,
		req.ReplyMode+" talkativeness="+strconv.Itoa(talk))
	writeJSON(w, map[string]any{"ok": true})
}

// handleAIAgentFishingTalk 开关某个 Agent 的「上鱼报话」。
// 全局范围决定在哪能见到它，这个决定它上了鱼能不能主动开口。
func (s *Server) handleAIAgentFishingTalk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string `json:"key"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	ctx := r.Context()
	if err := s.db.AISetAgentFishingTalk(ctx, req.Key, req.Enabled); err != nil {
		httpError(w, 500, "上鱼报话保存失败")
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_fishing_talk", req.Key,
		"enabled="+strconv.FormatBool(req.Enabled))
	writeJSON(w, map[string]any{"ok": true})
}

// handleAIAgentActor 开关某个 Agent 的「角色状态」（是否在钓鱼）。
// 关闭=永远不钓鱼；打开后由 GS 在运行态事务里写开关/状态。
// 管理端只入队意图并返回 202：真正生效与否通过 GET /api/ai/commands/{id} 查询。
func (s *Server) handleAIAgentActor(w http.ResponseWriter, r *http.Request) {
	ctx := s.bindAIAdminIntent(r.Context())
	var req struct {
		Key        string `json:"key"`
		Enabled    bool   `json:"enabled"`
		Reroll     bool   `json:"reroll"`
		Generation int64  `json:"generation"`
		Revision   int64  `json:"revision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		httpError(w, 400, "请求格式错误")
		return
	}
	ag, err := s.db.AIAgentByKey(ctx, req.Key)
	if err != nil || ag == nil {
		httpError(w, 404, "Agent 不存在")
		return
	}
	ctl, err := s.aiRuntimeControl(r, req.Key)
	if err != nil {
		httpError(w, 500, "Agent 状态读取失败")
		return
	}
	gen := req.Generation
	if gen <= 0 {
		gen = ctl.Generation
	}
	rev := req.Revision
	if rev <= 0 {
		rev = ctl.Revision
	}
	enabledPayload, _ := json.Marshal(map[string]bool{"enabled": req.Enabled})
	cmd, ok := s.enqueueAIIntent(ctx, w, store.AIRuntimeEnqueueRequest{
		Source:             store.AIRuntimeSourceAdmin,
		Kind:               store.AIRuntimeKindActorSetEnabled,
		AgentKey:           req.Key,
		ExpectedGeneration: gen,
		Payload:            enabledPayload,
	})
	if !ok {
		return
	}
	cmds := []*store.AIRuntimeCommand{cmd}
	// 「换个状态」是相对操作：只有已开启时才单独入队；刚开启的那次由下一 tick
	// 初始化状态（同样是随机一段活动），避免同一请求里先改 revision 再自相矛盾。
	if req.Enabled && req.Reroll && ag.ActorEnabled {
		reroll, ok := s.enqueueAIIntent(ctx, w, store.AIRuntimeEnqueueRequest{
			Source:             store.AIRuntimeSourceAdmin,
			Kind:               store.AIRuntimeKindActorReroll,
			AgentKey:           req.Key,
			ExpectedGeneration: gen,
			ExpectedRevision:   rev,
		})
		if !ok {
			return
		}
		cmds = append(cmds, reroll)
	}
	// 纯「换个状态」只是角色扮演；只有真正开启角色模拟时才留审计。
	if !ag.ActorEnabled && req.Enabled {
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_actor", req.Key,
			"actor_enabled=true reroll="+strconv.FormatBool(req.Reroll)+" accepted")
	} else if !req.Enabled {
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_agent_actor", req.Key, "actor_enabled=false accepted")
	}
	writeAICommandAccepted(w, cmds...)
}

// ---------------------------------------------------------------------------
// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
//
// 后台「AI 虚拟钓鱼」页的「发鱼测试」按钮。只做两件事：强制前置校验后写
// 一条 pending 请求；查询同 request_id 的消费结果。真正的入队由 gameserver
// 复用 maybeAIFishingTalk 的文案与投递路径完成，这里不碰 ai_task/mail/24-27。
// ---------------------------------------------------------------------------

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func (s *Server) handleAIFishingTalkTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentKey string `json:"agent_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "请求格式错误")
		return
	}
	agentKey := strings.TrimSpace(req.AgentKey)
	if agentKey == "" {
		httpError(w, 400, "请选择要测试的 Agent")
		return
	}
	ctx := r.Context()
	state, rej := gameserver.RequestAIFishingTalkTest(ctx, s.db, agentKey, newAIRequestID())
	if rej != nil {
		httpError(w, aiFishingTalkTestHTTPStatus(rej.Code), rej.Message)
		return
	}
	_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_fishing_talk_test", state.AgentKey,
		"request_id="+state.RequestID+" accepted")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"status":     state.Status,
		"request_id": state.RequestID,
		"agent_key":  state.AgentKey,
	})
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func (s *Server) handleAIFishingTalkTestGet(w http.ResponseWriter, r *http.Request) {
	requestID := strings.TrimSpace(r.URL.Query().Get("request_id"))
	if requestID == "" {
		httpError(w, 400, "缺少 request_id")
		return
	}
	state, ok, err := gameserver.GetAIFishingTalkTest(r.Context(), s.db, requestID)
	if err != nil {
		httpError(w, 500, "测试结果读取失败")
		return
	}
	if !ok {
		httpError(w, 404, "没有找到该次测试触发（可能已被新的触发覆盖）")
		return
	}
	writeJSON(w, state)
}

// T-027 测试脚手架：v2 删除（见 specs/changes/ai-fishing-talk-test-hook）
func aiFishingTalkTestHTTPStatus(code string) int {
	switch code {
	case "agent_missing":
		return http.StatusNotFound
	case "bad_agent_key", "bad_request_id":
		return http.StatusBadRequest
	case "db_error", "store_error":
		return http.StatusInternalServerError
	default:
		// 其余都是「当前状态不满足前置条件」：明确 409，不静默降级。
		return http.StatusConflict
	}
}
