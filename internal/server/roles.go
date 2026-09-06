package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"work-assistant/internal/model"
	"work-assistant/internal/router"
	"work-assistant/internal/store"
)

func (s *Server) registerRoleRoutes(mux *http.ServeMux) {
	for pattern, handler := range map[string]http.HandlerFunc{
		"GET /api/v1/roles":                            s.handleRoles,
		"GET /api/v1/roles/{role_id}":                  s.handleRole,
		"PUT /api/v1/roles/{role_id}":                  s.handleUpdateRole,
		"GET /api/v1/role-drafts":                      s.handleRoleDrafts,
		"POST /api/v1/role-drafts":                     s.handleCreateRoleDraft,
		"GET /api/v1/role-drafts/{draft_id}":           s.handleRoleDraft,
		"PUT /api/v1/role-drafts/{draft_id}":           s.handleUpdateRoleDraft,
		"POST /api/v1/role-drafts/{draft_id}/messages": s.handleRoleDraftMessage,
		"POST /api/v1/role-drafts/{draft_id}/publish":  s.handlePublishRoleDraft,
		"GET /api/v1/agents":                           s.handleAgents,
		"POST /api/v1/agents":                          s.handleCreateAgent,
		"GET /api/v1/agents/{agent_id}":                s.handleAgent,
		"PUT /api/v1/agents/{agent_id}":                s.handleUpdateAgent,
	} {
		mux.Handle(pattern, s.apiAuth(handler))
	}
}

func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version int64          `json:"expected_version"`
		Spec    model.RoleSpec `json:"spec"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	role, err := s.store.UpdateRole(r.Context(), r.PathValue("role_id"), req.Version, req.Spec)
	reply(w, 200, role, err)
}
func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	var req store.AgentUpdate
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	a, err := s.store.UpdateAgent(r.Context(), r.PathValue("agent_id"), req)
	reply(w, 200, a, err)
}

func reply(w http.ResponseWriter, status int, value any, err error) {
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, status, value)
}
func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := s.store.ListRoles(r.Context())
	reply(w, http.StatusOK, map[string]any{"roles": roles}, err)
}
func (s *Server) handleRole(w http.ResponseWriter, r *http.Request) {
	role, err := s.store.GetRole(r.Context(), r.PathValue("role_id"))
	reply(w, http.StatusOK, role, err)
}
func (s *Server) handleRoleDrafts(w http.ResponseWriter, r *http.Request) {
	drafts, err := s.store.ListRoleDrafts(r.Context())
	reply(w, http.StatusOK, map[string]any{"drafts": drafts}, err)
}
func (s *Server) handleRoleDraft(w http.ResponseWriter, r *http.Request) {
	draft, err := s.store.GetRoleDraft(r.Context(), r.PathValue("draft_id"))
	reply(w, http.StatusOK, draft, err)
}
func (s *Server) handleCreateRoleDraft(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Description  string `json:"description"`
		SourceRoleID string `json:"source_role_id"`
		Key          string `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.Key == "" {
		request.Key = r.Header.Get("Idempotency-Key")
	}
	draft, err := s.store.CreateRoleDraft(r.Context(), request.Description, request.SourceRoleID, request.Key)
	reply(w, http.StatusCreated, draft, err)
}
func (s *Server) handleUpdateRoleDraft(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Version int64          `json:"expected_version"`
		Spec    model.RoleSpec `json:"spec"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	draft, err := s.store.UpdateRoleDraft(r.Context(), r.PathValue("draft_id"), request.Version, request.Spec)
	reply(w, http.StatusOK, draft, err)
}
func (s *Server) handlePublishRoleDraft(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Version int64 `json:"expected_version"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	role, err := s.store.PublishRoleDraft(r.Context(), r.PathValue("draft_id"), request.Version)
	reply(w, http.StatusOK, role, err)
}

func (s *Server) handleRoleDraftMessage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ManualExecutor bool   `json:"manual_executor"`
		Version        int64  `json:"expected_version"`
		Message        string `json:"message"`
		RuntimeID      string `json:"runtime_id"`
		AdapterID      string `json:"adapter_id"`
		ModelID        string `json:"model_id"`
		Key            string `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.Key == "" {
		request.Key = r.Header.Get("Idempotency-Key")
	}
	draft, err := s.store.GetRoleDraft(r.Context(), r.PathValue("draft_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	// Replay before checking online presence: an accepted request remains accepted
	// even if the runtime has since disconnected or the draft was published.
	if request.Key != "" {
		run, err := s.store.GetRunByKey(r.Context(), draft.TaskID, request.Key)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"draft": draft, "run": run})
			return
		}
		if err != sql.ErrNoRows {
			writeStoreError(w, err)
			return
		}
	}
	if draft.Version != request.Version || draft.State == "GENERATING" || draft.State == "PUBLISHED" {
		writeStoreError(w, fmt.Errorf("%w: draft changed or is not editable; reload it", model.ErrConflict))
		return
	}
	binding, err := s.store.GetSystemBinding(r.Context(), "role_builder")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !request.ManualExecutor && (binding.Mode == "agent" || (request.RuntimeID == "" && request.ModelID == "" && request.AdapterID == "")) {
		resolved, e := s.systemExecutor(r.Context(), "role_builder", draft.TaskID, request.Key)
		if e != nil {
			writeStoreError(w, e)
			return
		}
		draft, run, e := s.store.StartRoleDraftRun(r.Context(), draft.ID, request.Version, request.Message, resolved, s.roleBuilder)
		reply(w, http.StatusAccepted, map[string]any{"draft": draft, "run": run}, e)
		return
	}
	if request.AdapterID == "" {
		if session, err := s.store.GetTaskSession(r.Context(), draft.TaskID); err == nil {
			request.AdapterID = session.AdapterID
		} else if err == sql.ErrNoRows {
			request.AdapterID = "codex-agent"
		} else {
			writeStoreError(w, err)
			return
		}
	}
	// Filter by builder features before choosing a machine. An older online
	// runtime must not hide another runtime that can actually generate drafts.
	if request.RuntimeID == "" {
		if session, err := s.store.GetTaskSession(r.Context(), draft.TaskID); err == nil {
			request.RuntimeID = session.RuntimeID
		} else if err == sql.ErrNoRows {
			runtimes, err := s.connectedRuntimes(r.Context())
			if err != nil {
				writeStoreError(w, err)
				return
			}
			candidates := runtimes[:0]
			for _, runtime := range runtimes {
				if supportsRoleBuilder(runtime, request.AdapterID) {
					candidates = append(candidates, runtime)
				}
			}
			request.RuntimeID, err = s.router.Select(r.Context(), router.Request{AdapterID: request.AdapterID}, candidates)
			if err != nil {
				writeStoreError(w, fmt.Errorf("%w: role builder needs an online adapter with role_instructions, structured_output and read_only_runs support", model.ErrConflict))
				return
			}
		} else {
			writeStoreError(w, err)
			return
		}
	}
	resolved, err := s.resolveRun(r.Context(), model.Task{ID: draft.TaskID}, store.CreateRunRequest{
		TaskID: draft.TaskID, RuntimeID: request.RuntimeID, AdapterID: request.AdapterID, ModelID: request.ModelID, IdempotencyKey: request.Key,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	runtimes, err := s.connectedRuntimes(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	supported := false
	for _, runtime := range runtimes {
		if runtime.ID == resolved.RuntimeID && runtime.State == "ONLINE" && supportsRoleBuilder(runtime, resolved.AdapterID) {
			supported = true
		}
	}
	if !supported {
		writeStoreError(w, fmt.Errorf("%w: role builder needs an online adapter with structured_output and read_only_runs support", model.ErrConflict))
		return
	}
	draft, run, err := s.store.StartRoleDraftRun(r.Context(), draft.ID, request.Version, request.Message, resolved, s.roleBuilder)
	reply(w, http.StatusAccepted, map[string]any{"draft": draft, "run": run}, err)
}

func supportsRoleBuilder(runtime model.Runtime, adapterID string) bool {
	return router.SupportsFeature(runtime, adapterID, "role_instructions") && router.SupportsFeature(runtime, adapterID, "structured_output") && router.SupportsFeature(runtime, adapterID, "read_only_runs")
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.store.ListAgents(r.Context())
	reply(w, http.StatusOK, map[string]any{"agents": agents}, err)
}
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	agent, err := s.store.GetAgent(r.Context(), r.PathValue("agent_id"))
	reply(w, http.StatusOK, agent, err)
}
func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name          string `json:"name"`
		RoleID        string `json:"role_id"`
		RuntimeID     string `json:"runtime_id"`
		AdapterID     string `json:"adapter_id"`
		ModelID       string `json:"model_id"`
		MaxConcurrent int    `json:"max_concurrent"`
	}
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.AdapterID == "" {
		request.AdapterID = "codex-agent"
	}
	agent, err := s.store.CreateAgent(r.Context(), model.AgentProfile{Name: request.Name, RoleID: request.RoleID, RuntimeID: request.RuntimeID, AdapterID: request.AdapterID, ModelID: request.ModelID, MaxConcurrent: request.MaxConcurrent})
	reply(w, http.StatusCreated, agent, err)
}

func (s *Server) connectedRuntimes(ctx context.Context) ([]model.Runtime, error) {
	runtimes, err := s.store.ListRuntimes(ctx)
	if err != nil {
		return nil, err
	}
	connected := make(map[string]bool)
	for _, connection := range s.hub.snapshot() {
		connected[connection.runtimeID] = true
	}
	candidates := []model.Runtime{}
	for _, runtime := range runtimes {
		if connected[runtime.ID] {
			candidates = append(candidates, runtime)
		}
	}
	return candidates, nil
}

func (s *Server) resolveRun(ctx context.Context, task model.Task, request store.CreateRunRequest) (store.CreateRunRequest, error) {
	var affinity *model.Session
	if session, err := s.store.GetTaskSession(ctx, task.ID); err == nil {
		if request.SessionID != "" && session.ID != request.SessionID {
			return request, fmt.Errorf("%w: task is already bound to another session", model.ErrConflict)
		}
		affinity = &session
	} else if err != sql.ErrNoRows {
		return request, err
	}
	if affinity == nil && request.SessionID != "" {
		session, err := s.store.GetSession(ctx, request.SessionID)
		if err != nil {
			return request, err
		}
		affinity = &session
	}
	if affinity != nil {
		for _, pair := range [][2]string{{request.AgentID, affinity.AgentID}, {request.RuntimeID, affinity.RuntimeID}, {request.AdapterID, affinity.AdapterID}, {request.ModelID, affinity.ModelID}} {
			if pair[0] != "" && pair[0] != pair[1] {
				return request, fmt.Errorf("%w: requested configuration conflicts with existing session ownership", model.ErrConflict)
			}
		}
		request.SessionID, request.AgentID, request.RuntimeID, request.AdapterID, request.ModelID = affinity.ID, affinity.AgentID, affinity.RuntimeID, affinity.AdapterID, affinity.ModelID
		return request, nil
	}
	if request.AdapterID == "" && (request.AgentID == "exec-agent" || request.AgentID == "stdio-agent" || request.AgentID == "codex-agent") {
		request.AdapterID, request.AgentID = request.AgentID, ""
	}
	roleRouting := !task.Requirements.IsEmpty()
	if request.AgentID != "" {
		if _, err := s.store.GetAgent(ctx, request.AgentID); err == nil {
			roleRouting = true
		} else if err != sql.ErrNoRows || request.AdapterID == "" || roleRouting {
			return request, err
		}
	}
	if roleRouting {
		agents, err := s.store.ListAgents(ctx)
		if err != nil {
			return request, err
		}
		runtimes, err := s.connectedRuntimes(ctx)
		if err != nil {
			return request, err
		}
		agent, err := s.agentRouter.SelectAgent(ctx, router.AgentRequest{Requirements: task.Requirements, AgentID: request.AgentID, RuntimeID: request.RuntimeID, AdapterID: request.AdapterID, ModelID: request.ModelID}, agents, runtimes)
		if err != nil {
			return request, err
		}
		request.AgentID, request.RuntimeID, request.AdapterID, request.ModelID = agent.ID, agent.RuntimeID, agent.AdapterID, agent.ModelID
		return request, nil
	}
	if request.AdapterID == "" {
		request.AdapterID = "exec-agent"
	}
	if request.RuntimeID == "" {
		runtimes, err := s.connectedRuntimes(ctx)
		if err != nil {
			return request, err
		}
		request.RuntimeID, err = s.router.Select(ctx, router.Request{Task: task, AdapterID: request.AdapterID}, runtimes)
		if err != nil {
			return request, fmt.Errorf("%w: %s", model.ErrConflict, err)
		}
	}
	return request, nil
}
