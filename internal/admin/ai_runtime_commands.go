package admin

// AI 运行态命令的管理入口（T-020 / DEV-004）。
//
// 管理动作只写意图：入队一条 ai_runtime_command 并返回 202 pending，
// 不直接改 actor/channel_agent/memory，也不返回伪造的 actor 结果。
// 终态由唯一活动 gameserver 在同一事务里应用后写入，页面通过
// GET /api/ai/commands/{id} 轮询。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

// aiIntentTTL 是后台操作的寿命：用户会离开页面，10 分钟足够。
const aiIntentTTL = 10 * time.Minute

// newAIRequestID 生成一次用户操作的稳定重试键（时间戳 + 随机数）。
// 网络重试必须原样复用；同一 request_id 的不同 payload 会被服务端拒绝。
func newAIRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ad-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("ad-%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

func (s *Server) bindAIAdminIntent(ctx context.Context) context.Context {
	dec := aiconfig.EvaluateEmergency(s.db.AIEmergencyState(ctx))
	return store.AIIntentContext(ctx, dec.Epoch)
}

// enqueueAIIntent 入队一条意图，失败时按语义写 HTTP 错误并返回 ok=false。
func (s *Server) enqueueAIIntent(ctx context.Context, w http.ResponseWriter, req store.AIRuntimeEnqueueRequest) (*store.AIRuntimeCommand, bool) {
	if req.RequestID == "" {
		req.RequestID = newAIRequestID()
	}
	if req.TTL <= 0 {
		req.TTL = aiIntentTTL
	}
	cmd, _, err := s.db.AIEnqueueRuntimeCommand(ctx, req)
	if err == nil {
		return cmd, true
	}
	switch {
	case errors.Is(err, store.ErrAIRuntimeInvalidCommand):
		httpError(w, 400, "操作参数不合法")
	case errors.Is(err, store.ErrAIRuntimeCommandConflict):
		httpError(w, 409, "同一请求已用于不同操作，请刷新后重试")
	case errors.Is(err, store.ErrAIRuntimeQueueFull):
		httpError(w, 429, "AI 运行态命令队列已满，请稍后再试")
	default:
		httpError(w, 500, "操作入队失败")
	}
	return nil, false
}

// writeAICommandAccepted 写 202 + 命令状态，不带 payload 与伪造结果。
func writeAICommandAccepted(w http.ResponseWriter, cmds ...*store.AIRuntimeCommand) {
	if len(cmds) == 0 || cmds[0] == nil {
		httpError(w, 500, "操作入队失败")
		return
	}
	ids := make([]int64, 0, len(cmds))
	for _, c := range cmds {
		if c != nil {
			ids = append(ids, c.ID)
		}
	}
	body := map[string]any{
		"ok":         true,
		"status":     store.AIRuntimeStatusPending,
		"command_id": ids[0],
		"request_id": cmds[0].RequestID,
	}
	if len(ids) > 1 {
		body["command_ids"] = ids
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(body)
}

// handleAICommandGet 返回脱敏命令状态：不含 payload、不含入口参数。
func (s *Server) handleAICommandGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpError(w, 400, "命令 id 不合法")
		return
	}
	cmd, err := s.db.AIGetRuntimeCommand(r.Context(), id)
	if errors.Is(err, store.ErrAIRuntimeCommandNotFound) {
		httpError(w, 404, "命令不存在")
		return
	}
	if err != nil {
		httpError(w, 500, "命令查询失败")
		return
	}
	body := map[string]any{
		"id":              cmd.ID,
		"kind":            cmd.Kind,
		"agent_key":       cmd.AgentKey,
		"status":          cmd.Status,
		"reason":          cmd.Reason,
		"result_revision": cmd.ResultRevision,
		"applied_ts":      cmd.AppliedTS,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if cmd.Status == store.AIRuntimeStatusPending {
		w.WriteHeader(http.StatusAccepted)
	}
	_ = json.NewEncoder(w).Encode(body)
}

// aiRuntimeControl 读 Agent 的代次/版本；缺行按 1/0。
func (s *Server) aiRuntimeControl(r *http.Request, key string) (*store.AIRuntimeControl, error) {
	return s.db.AIGetRuntimeControl(r.Context(), key)
}
