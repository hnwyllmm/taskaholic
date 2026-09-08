package server

import (
	"net/http"
	"work-assistant/internal/model"
)

func (s *Server) registerPermissionRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/execution-permissions", s.apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policies, err := s.store.ExecutionPolicies(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		requests, err := s.store.PermissionRequests(r.Context())
		if err != nil {
			writeStoreError(w, err)
			return
		}
		runtimes, err := s.store.ListRuntimes(r.Context())
		reply(w, 200, map[string]any{"policies": policies, "requests": requests, "runtimes": runtimes}, err)
	})))
	mux.Handle("PUT /api/v1/execution-permissions/policies", s.apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p model.ExecutionPolicy
		if err := decodeJSON(w, r, &p); err != nil {
			writeError(w, 400, err)
			return
		}
		p, err := s.store.SaveExecutionPolicy(r.Context(), p)
		reply(w, 200, p, err)
	})))
	mux.Handle("POST /api/v1/execution-permissions/requests/{request_id}", s.apiAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Version  int64  `json:"expected_version"`
			Decision string `json:"decision"`
			Remember bool   `json:"remember"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			writeError(w, 400, err)
			return
		}
		var err error
		if req.Decision == "recheck" {
			err = s.store.RecheckPermission(r.Context(), r.PathValue("request_id"), req.Version)
		} else {
			err = s.store.DecidePermission(r.Context(), r.PathValue("request_id"), req.Version, req.Decision, req.Remember)
		}
		reply(w, 200, map[string]any{"status": "recorded"}, err)
	})))
}
