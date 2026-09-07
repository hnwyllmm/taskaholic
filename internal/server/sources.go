package server

import (
	"context"
	"net/http"
	"time"

	"work-assistant/internal/model"
)

func (s *Server) registerSourceRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/sources", s.apiAuth(http.HandlerFunc(s.handleSources)))
	mux.Handle("PUT /api/v1/sources/{source_id}", s.apiAuth(http.HandlerFunc(s.handleSaveSource)))
	mux.Handle("POST /api/v1/work/tasks/{task_id}/pull-requests", s.apiAuth(http.HandlerFunc(s.handleRegisterPR)))
	mux.Handle("PUT /api/v1/source-targets/{target_id}", s.apiAuth(http.HandlerFunc(s.handleSourceTarget)))
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
