package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/taskaction"
)

func (s *Server) registerSourceRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/sources", s.apiAuth(http.HandlerFunc(s.handleSources)))
	mux.Handle("PUT /api/v1/sources/{source_id}", s.apiAuth(http.HandlerFunc(s.handleSaveSource)))
	mux.Handle("POST /api/v1/work/tasks/{task_id}/pull-requests", s.apiAuth(http.HandlerFunc(s.handleRegisterPR)))
	mux.Handle("POST /api/v1/work/tasks/{task_id}/origin-issue", s.apiAuth(http.HandlerFunc(s.handleBindOriginIssue)))
	mux.Handle("POST /api/v1/work/tasks/{task_id}/publications/{key}/resolve", s.apiAuth(http.HandlerFunc(s.handleResolvePublication)))
	mux.Handle("PUT /api/v1/source-targets/{target_id}", s.apiAuth(http.HandlerFunc(s.handleSourceTarget)))
}

func (s *Server) handleResolvePublication(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConfirmNotCreated bool `json:"confirm_not_created"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	if !req.ConfirmNotCreated {
		writeError(w, 400, model.ErrValidation)
		return
	}
	maintenance, err := s.store.Maintenance(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if maintenance != "" {
		writeStoreError(w, model.ErrConflict)
		return
	}
	all, err := s.store.ListPublications(r.Context(), r.PathValue("task_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var selected *model.Publication
	for _, p := range all {
		if p.Key == r.PathValue("key") && p.TaskID == r.PathValue("task_id") {
			copy := p
			selected = &copy
		}
	}
	if selected == nil {
		writeError(w, 404, errors.New("publication not found"))
		return
	}
	if selected.RemoteID != "" || (selected.State != "UNCERTAIN" && selected.State != "SUBMITTING") {
		writeStoreError(w, model.ErrConflict)
		return
	}
	publisher := s.publications.Publishers[selected.Platform]
	if publisher == nil {
		writeError(w, 400, model.ErrValidation)
		return
	}
	p, err := s.store.ClaimPublication(r.Context(), selected.Key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	p.State = "UNCERTAIN" // This reconciliation cannot create a fresh comment.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	receipt, publishErr := publisher.Publish(ctx, p)
	cancel()
	state, message, status := "SYNCED", "", "recovered"
	if publishErr != nil {
		state, message, status = "UNCERTAIN", "平台核对失败，未允许重新发布", "pending"
		var missing *taskaction.MissingPublicationError
		if errors.As(publishErr, &missing) {
			state, message, status = "QUEUED", "人工确认且平台查询未找到评论，允许重试", "retry_allowed"
		}
		var blocked *taskaction.PublicationError
		if errors.As(publishErr, &blocked) && blocked.State == "BLOCKED" {
			state, message, status = "BLOCKED", blocked.Message, "blocked"
		}
	}
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	err = s.store.FinishPublication(saveCtx, p, receipt.ID, receipt.URL, state, message)
	saveCancel()
	reply(w, 200, map[string]string{"status": status, "message": message}, err)
}

func (s *Server) handleBindOriginIssue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		SourceID string `json:"source_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	err := s.store.BindGitHubIssue(r.Context(), r.PathValue("task_id"), req.SourceID, req.URL)
	reply(w, 200, map[string]any{"url": req.URL}, err)
}

func (s *Server) publicationLoop(ctx context.Context) {
	if os.Getenv("WORK_ASSISTANT_PUBLICATIONS_DISABLED") == "1" {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			actionCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := s.publications.Tick(actionCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				s.log.Error("platform publication executor", "error", err)
			}
		}
	}
}
func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.ListTaskSources(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	targets, err := s.store.ListSourceTargets(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	events, err := s.store.ListSourceEvents(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	reviews, err := s.store.ListSourceReviews(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	reply(w, 200, map[string]any{"sources": sources, "targets": targets, "events": events, "reviews": reviews, "plugins": []string{"antmultica", "github", "gitlab"}}, nil)
}
func (s *Server) handleSaveSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source          model.TaskSource `json:"source"`
		ExpectedVersion int64            `json:"expected_version"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	req.Source.ID = r.PathValue("source_id")
	source, err := s.store.SaveTaskSource(r.Context(), req.Source, req.ExpectedVersion)
	reply(w, 200, source, err)
}
func (s *Server) handleRegisterPR(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		SourceID string `json:"source_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	target, err := s.store.RegisterPR(r.Context(), r.PathValue("task_id"), req.SourceID, req.URL)
	reply(w, 201, target, err)
}
func (s *Server) handleSourceTarget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	if req.Enabled == nil {
		writeError(w, 400, model.ErrValidation)
		return
	}
	err := s.store.SetSourceTargetEnabled(r.Context(), r.PathValue("target_id"), *req.Enabled)
	reply(w, 200, map[string]any{"enabled": *req.Enabled}, err)
}
func (s *Server) sourceLoop(ctx context.Context) {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := s.sources.Tick(ctx); err != nil && ctx.Err() == nil {
				s.log.Error("task source scheduler", "error", err)
			}
		}
	}
}

// Network-bound writes are isolated from the Router/Manager scheduling loop
// and from read-only source collection. The durable request survives shutdown.
func serverActionLoop(ctx context.Context, s *Server) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			actionCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := s.testActions.Tick(actionCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				s.log.Error("test action executor", "error", err)
			}
		}
	}
}
