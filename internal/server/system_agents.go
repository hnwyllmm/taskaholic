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

func (s *Server) registerSystemAgentRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/system/agents", s.apiAuth(http.HandlerFunc(s.handleSystemAgents)))
	mux.Handle("PUT /api/v1/system/agents/{slot}", s.apiAuth(http.HandlerFunc(s.handleSystemAgentUpdate)))
	mux.Handle("POST /api/v1/home/chats/{chat_id}/executor", s.apiAuth(http.HandlerFunc(s.handleHomeExecutor)))
}
func (s *Server) handleSystemAgents(w http.ResponseWriter, r *http.Request) {
	bindings, err := s.store.ListSystemBindings(r.Context())
	reply(w, 200, map[string]any{"slots": model.SystemSlots, "bindings": bindings, "local_runtime_id": s.config.LocalRuntimeID, "upgrade_enabled": s.config.UpgradeEnabled}, err)
}
func (s *Server) handleSystemAgentUpdate(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Mode    string `json:"mode"`
		AgentID string `json:"agent_id"`
		Version int64  `json:"expected_version"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err)
		return
	}
	local := ""
	if s.config.UpgradeEnabled {
		local = s.config.LocalRuntimeID
	}
	b, err := s.store.UpdateSystemBinding(r.Context(), model.SystemBinding{Slot: r.PathValue("slot"), Mode: input.Mode, AgentID: input.AgentID}, input.Version, local)
	reply(w, 200, b, err)
}

func (s *Server) systemExecutor(ctx context.Context, slot, taskID, key string) (store.CreateRunRequest, error) {
	req := store.CreateRunRequest{TaskID: taskID, IdempotencyKey: key}
	runtimes, err := s.connectedRuntimes(ctx)
	if err != nil {
		return req, err
	}
	if taskID != "" {
		if session, e := s.store.GetTaskSession(ctx, taskID); e == nil {
			req.SessionID, req.AgentID, req.RuntimeID, req.AdapterID, req.ModelID = session.ID, session.AgentID, session.RuntimeID, session.AdapterID, session.ModelID
			return req, checkSystemRuntime(req, runtimes)
		} else if e != sql.ErrNoRows {
			return req, e
		}
	}
	b, err := s.store.GetSystemBinding(ctx, slot)
	if err != nil {
		return req, err
	}
	req.SystemBinding = &b
	if b.Mode == "agent" {
		a, e := s.store.GetAgent(ctx, b.AgentID)
		if e != nil {
			return req, e
		}
		if a.State != "ACTIVE" || a.ActiveRuns >= a.MaxConcurrent {
			return req, fmt.Errorf("%w: 系统岗位成员 %s 已停用或正在忙；请稍后重试或更换岗位配置", model.ErrConflict, a.Name)
		}
		req.AgentID, req.RuntimeID, req.AdapterID, req.ModelID = a.ID, a.RuntimeID, a.AdapterID, a.ModelID
		return req, checkSystemRuntime(req, runtimes)
	}
	// Compatibility mode uses a normal available member when possible; older
	// installations without members can still bootstrap their first role.
	agents, err := s.store.ListAgents(ctx)
	if err != nil {
		return req, err
	}
	eligible := []model.AgentProfile{}
	for _, a := range agents {
		for _, rt := range runtimes {
			if rt.ID == a.RuntimeID && supportsRoleBuilder(rt, a.AdapterID) {
				eligible = append(eligible, a)
				break
			}
		}
	}
	selector := router.LeastLoaded{}
	a, err := selector.SelectAgent(ctx, router.AgentRequest{Requirements: model.TaskRequirements{Capabilities: []string{"analysis"}}}, eligible, runtimes)
	if err != nil {
		a, err = selector.SelectAgent(ctx, router.AgentRequest{}, eligible, runtimes)
	}
	if err == nil {
		req.AgentID, req.RuntimeID, req.AdapterID, req.ModelID = a.ID, a.RuntimeID, a.AdapterID, a.ModelID
		return req, nil
	}
	if len(agents) > 0 {
		return req, fmt.Errorf("%w: 没有在线且空闲的系统岗位成员", model.ErrConflict)
	}
	req.AgentID, req.AdapterID = "system-"+slot, "codex-agent"
	for _, rt := range runtimes {
		if rt.State == "ONLINE" && supportsRoleBuilder(rt, req.AdapterID) {
			req.RuntimeID = rt.ID
			return req, nil
		}
	}
	return req, fmt.Errorf("%w: 没有支持只读结构化输出的在线执行环境", model.ErrConflict)
}

func checkSystemRuntime(req store.CreateRunRequest, runtimes []model.Runtime) error {
	for _, rt := range runtimes {
		if rt.ID == req.RuntimeID && rt.State == "ONLINE" && supportsRoleBuilder(rt, req.AdapterID) {
			return nil
		}
	}
	return fmt.Errorf("%w: 系统岗位原执行机器不在线或不支持只读结构化输出；不会自动改派", model.ErrConflict)
}

func (s *Server) handleHomeExecutor(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version        int64 `json:"expected_version"`
		BindingVersion int64 `json:"binding_version"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err)
		return
	}
	req, err := s.systemExecutor(r.Context(), "home_chat", "", "")
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if req.SystemBinding.Version != input.BindingVersion {
		writeStoreError(w, fmt.Errorf("%w: 岗位配置已变化，请刷新后确认", model.ErrConflict))
		return
	}
	c, err := s.store.SwitchHomeExecutor(r.Context(), r.PathValue("chat_id"), input.Version, req)
	reply(w, 200, c, err)
}
