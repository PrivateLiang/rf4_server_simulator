package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"rf4server/internal/aiconfig"
	"rf4server/internal/store"
)

func (s *Server) handleAILocalKnowledgeGet(w http.ResponseWriter, r *http.Request) {
	snap, err := s.db.AIGetLocalKnowledge(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "读取本地资料失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, snap)
}

func (s *Server) handleAILocalKnowledgeSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseRevision *int64                   `json:"base_revision"`
		Document     *aiconfig.LocalKnowledge `json:"document"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*aiconfig.MaxLocalKnowledgeBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err == nil && dec.Decode(new(json.RawMessage)) != io.EOF {
		err = fmt.Errorf("只接受一个 JSON 对象")
	}
	if err != nil || req.BaseRevision == nil || *req.BaseRevision < 0 || req.Document == nil {
		httpError(w, http.StatusBadRequest, "资料格式或修订号有误，请检查 JSON 字段与内容大小")
		return
	}
	ctx := r.Context()
	snap, err := s.db.AISaveLocalKnowledge(ctx, *req.BaseRevision, *req.Document)
	switch {
	case errors.Is(err, aiconfig.ErrLocalKnowledgeInvalid):
		httpError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrAILocalKnowledgeConflict):
		httpError(w, http.StatusConflict, err.Error())
	case err != nil:
		httpError(w, http.StatusInternalServerError, "保存本地资料失败")
	default:
		_ = s.db.AuditLogWrite(ctx, adminOf(r), "ai_local_knowledge", "", strconv.FormatInt(snap.Revision, 10))
		writeJSON(w, snap)
	}
}
