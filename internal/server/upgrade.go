package server

import (
	"fmt"
	"net/http"

	"work-assistant/internal/model"
)

func (s *Server) registerUpgradeRoutes(mux *http.ServeMux) {
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/upgrades":                       s.handleUpgradeList,
		"GET /api/v1/upgrades/{upgrade_id}":          s.handleUpgradeGet,
		"POST /api/v1/upgrades/{upgrade_id}/install": s.handleUpgradeInstall,
		"POST /api/v1/upgrades/{upgrade_id}/cancel":  s.handleUpgradeCancel,
	} {
		mux.Handle(pattern, s.apiAuth(handler))
	}
}

func (s *Server) handleUpgradeList(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListUpgrades(r.Context())
	// The homepage needs the full current candidate for review. Historical rows
	// remain addressable individually without returning every old patch on each poll.
	for i := 1; i < len(items); i++ {
		items[i].Patch = ""
		items[i].Log = ""
	}
	reply(w, 200, map[string]any{"enabled": s.config.UpgradeEnabled, "upgrades": items}, err)
}

func (s *Server) handleUpgradeGet(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetUpgrade(r.Context(), r.PathValue("upgrade_id"))
	reply(w, 200, u, err)
}

func (s *Server) handleUpgradeInstall(w http.ResponseWriter, r *http.Request) {
	if !s.config.UpgradeEnabled {
		writeStoreError(w, fmt.Errorf("%w: 当前启动方式没有升级守护进程，请通过启动脚本运行", model.ErrConflict))
		return
	}
	var req struct {
		Version int64  `json:"expected_version"`
		Digest  string `json:"candidate_sha256"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	u, err := s.store.RequestUpgradeInstall(r.Context(), r.PathValue("upgrade_id"), req.Version, req.Digest)
	reply(w, 202, u, err)
}

func (s *Server) handleUpgradeCancel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version int64 `json:"expected_version"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	u, err := s.store.CancelUpgrade(r.Context(), r.PathValue("upgrade_id"), req.Version)
	reply(w, 200, u, err)
}
