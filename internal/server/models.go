package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/router"
)

// Validate explicit changes with the selected machine. Keeping an unchanged
// member setting or choosing native defaults does not require an online CLI.
func (s *Server) validateEffort(ctx context.Context, runtimeID, adapterID, modelID, effort string) error {
	if err := model.ValidateReasoningEffort(effort); err != nil {
		return err
	}
	if effort == "" {
		return nil
	}
	if modelID == "" {
		return fmt.Errorf("%w: 请先选择明确的模型，再设置推理强度", model.ErrValidation)
	}
	runtimes, err := s.store.ListRuntimes(ctx)
	if err != nil {
		return err
	}
	for _, runtime := range runtimes {
		if runtime.ID != runtimeID {
			continue
		}
		if !router.SupportsFeature(runtime, adapterID, "reasoning_effort") {
			return fmt.Errorf("%w: 此 Agent 暂不支持独立推理强度", model.ErrValidation)
		}
		for _, connection := range s.hub.snapshot() {
			if connection.runtimeID != runtimeID || connection.epoch != runtime.Epoch || connection.peer == nil {
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var catalog model.ModelCatalog
			if err := connection.peer.Call(callCtx, "models.list", map[string]string{"adapter_id": adapterID}, &catalog); err == nil && catalog.Status == "ready" {
				_, err := model.ResolveReasoningEffort(catalog.Models, modelID, effort)
				return err
			}
		}
	}
	return fmt.Errorf("%w: 暂时无法验证推理强度；请连接执行机器并刷新模型列表，或选择运行环境默认", model.ErrConflict)
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	adapterID := r.URL.Query().Get("adapter_id")
	if adapterID == "" || len(adapterID) > 200 {
		writeError(w, http.StatusBadRequest, errors.New("adapter_id is required"))
		return
	}
	runtimes, err := s.store.ListRuntimes(r.Context())
	if err != nil {
		writeStoreError(w, err)
		return
	}
	for _, runtime := range runtimes {
		if runtime.ID != r.PathValue("runtime_id") {
			continue
		}
		var capabilities struct {
			Adapters map[string]map[string]any `json:"adapters"`
		}
		if err := json.Unmarshal(runtime.Capabilities, &capabilities); err != nil {
			writeError(w, http.StatusServiceUnavailable, errors.New("runtime capabilities are unavailable"))
			return
		}
		caps, exists := capabilities.Adapters[adapterID]
		if !exists {
			writeError(w, http.StatusBadRequest, errors.New("adapter is not available on this machine"))
			return
		}
		if caps["model_catalog"] != true {
			writeJSON(w, http.StatusOK, model.ModelCatalog{Status: "unsupported", Models: []model.ModelOption{}})
			return
		}
		for _, connection := range s.hub.snapshot() {
			if connection.runtimeID != runtime.ID || connection.epoch != runtime.Epoch || connection.peer == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			var catalog model.ModelCatalog
			if err := connection.peer.Call(ctx, "models.list", map[string]string{"adapter_id": adapterID}, &catalog); err != nil {
				writeError(w, http.StatusServiceUnavailable, errors.New("model list is temporarily unavailable; manual model entry is still supported"))
				return
			}
			writeJSON(w, http.StatusOK, catalog)
			return
		}
		writeError(w, http.StatusServiceUnavailable, errors.New("execution machine is offline; manual model entry is still supported"))
		return
	}
	writeError(w, http.StatusNotFound, errors.New("execution machine not found"))
}
