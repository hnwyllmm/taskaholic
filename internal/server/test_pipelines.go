package server

import (
	"context"
	"fmt"
	"net/http"
	"time"
	"work-assistant/internal/model"
)

func (s *Server) handleResolveTestPipeline(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConfirmNotCreated bool `json:"confirm_not_created"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	if !req.ConfirmNotCreated {
		writeError(w, 400, fmt.Errorf("%w: 请先在 GitLab 核查并明确确认未创建", model.ErrValidation))
		return
	}
	p, err := s.store.GetTestPipeline(r.Context(), r.PathValue("request_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if p.TaskID != r.PathValue("task_id") || p.PipelineID > 0 || (p.State != "SUBMITTING" && p.State != "UNCERTAIN") || time.Now().UnixMilli()-p.SubmittedAtMS < 120000 {
		writeError(w, 409, fmt.Errorf("%w: 申请仍在提交中或状态已更新，请稍后刷新", model.ErrConflict))
		return
	}
	executor := s.testActions.Executors[p.Kind]
	if executor == nil {
		writeError(w, 409, fmt.Errorf("%w: 对账插件不可用，不能重置申请", model.ErrConflict))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	found, err := executor.Find(ctx, p)
	if err != nil {
		writeError(w, 409, fmt.Errorf("%w: 对账未完成，不允许重置：%s", model.ErrConflict, err))
		return
	}
	if found != nil {
		err = s.store.AttachTestPipeline(ctx, p.ID, *found)
		reply(w, 200, map[string]any{"status": "recovered", "pipeline_id": found.ID}, err)
		return
	}
	err = s.store.ConfirmTestPipelineNotCreated(ctx, p.TaskID, p.ID)
	reply(w, 200, map[string]any{"status": "confirmed_missing"}, err)
}
