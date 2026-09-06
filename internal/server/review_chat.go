package server

import (
	"context"
	"fmt"
	"net/http"

	"work-assistant/internal/model"
	"work-assistant/internal/router"
)

func (s *Server) handleReviewMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
		Key     string `json:"idempotency_key"`
		Version int64  `json:"expected_discussion_version"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	if req.Key == "" {
		req.Key = r.Header.Get("Idempotency-Key")
	}
	turn, err := s.store.MessageReview(r.Context(), r.PathValue("task_id"), r.PathValue("review_id"), req.Message, req.Key, req.Version)
	reply(w, 202, turn, err)
}
func (s *Server) handleReviewStop(w http.ResponseWriter, r *http.Request) {
	err := s.store.StopReviewTurn(r.Context(), r.PathValue("task_id"), r.PathValue("review_id"), r.PathValue("turn_id"))
	reply(w, 202, map[string]any{"status": "stop_requested"}, err)
}

func (s *Server) scheduleReviewTurns(ctx context.Context) {
	turns, err := s.store.PendingReviewTurns(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("read review discussion queue", "error", err)
		}
		return
	}
	for _, turn := range turns {
		if err := s.scheduleReviewTurn(ctx, turn); err != nil {
			if e := s.store.DeferWork(ctx, turn.TaskID, err); e != nil && ctx.Err() == nil {
				s.log.Error("defer review discussion", "error", e)
			}
		}
	}
}
func (s *Server) scheduleReviewTurn(ctx context.Context, turn model.ReviewTurn) error {
	runtimes, err := s.connectedRuntimes(ctx)
	if err != nil {
		return err
	}
	available := false
	for _, rt := range runtimes {
		if rt.ID == turn.RuntimeID && rt.State == "ONLINE" && supportsRoleBuilder(rt, turn.AdapterID) && router.SupportsFeature(rt, turn.AdapterID, "native_session") {
			available = true
			break
		}
	}
	if !available {
		return fmt.Errorf("%w: 验收问题已保存，等待原机器 %s 的原生 Session；不会交给首页助理或其它 Agent", model.ErrConflict, turn.RuntimeID)
	}
	_, err = s.store.StartReviewTurn(ctx, turn.ID, s.config.ReviewContract)
	return err
}
