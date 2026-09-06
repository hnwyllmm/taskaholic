package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	if s.config.Backups == nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "snapshots": []any{}})
		return
	}
	writeJSON(w, 200, s.config.Backups.Status())
}

func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	if s.config.Backups == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("automatic backup manager is unavailable"))
		return
	}
	// A request cannot choose arbitrary local paths or overwrite an existing
	// recovery point. Cancellation leaves only a disposable partial directory.
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	snapshot, err := s.config.Backups.Capture(ctx, "manual")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusCreated, snapshot)
}
