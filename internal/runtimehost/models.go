package runtimehost

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
	"work-assistant/internal/rpcpeer"
)

type modelCacheEntry struct {
	ready   chan struct{}
	expires time.Time
	catalog model.ModelCatalog
}

func (d *Daemon) handleListModels(ctx context.Context, request rpcpeer.Request) (any, *rpcpeer.Error) {
	var params struct {
		AdapterID string `json:"adapter_id"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return nil, rpcInvalidParams(err)
	}
	a := d.adapters[params.AdapterID]
	if a == nil {
		return nil, rpcInvalidParams(errors.New("unknown adapter"))
	}
	provider, ok := a.(agent.ModelProvider)
	if !ok {
		return model.ModelCatalog{Status: "unsupported", Models: []model.ModelOption{}}, nil
	}
	// Coalesce requests per adapter. Discovery never holds the active-run lock
	// or blocks heartbeats, cancellation, dispatch or another adapter's catalog.
	d.modelMu.Lock()
	if d.modelCache == nil {
		d.modelCache = make(map[string]*modelCacheEntry)
	}
	if entry := d.modelCache[params.AdapterID]; entry != nil && time.Now().Before(entry.expires) {
		d.modelMu.Unlock()
		select {
		case <-entry.ready:
			return entry.catalog, nil
		case <-ctx.Done():
			return nil, rpcInternal(errors.New("model discovery canceled"))
		}
	}
	entry := &modelCacheEntry{ready: make(chan struct{}), expires: time.Now().Add(time.Minute)}
	d.modelCache[params.AdapterID] = entry
	d.modelMu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	models, err := provider.ListModels(callCtx)
	cancel()
	catalog := model.ModelCatalog{Status: "ready", Models: models, CheckedAtMS: time.Now().UnixMilli()}
	ttl := 5 * time.Minute
	if err != nil || len(models) == 0 {
		catalog.Status, catalog.Models = "unavailable", []model.ModelOption{}
		ttl = 30 * time.Second
	}
	d.modelMu.Lock()
	entry.catalog, entry.expires = catalog, time.Now().Add(ttl)
	close(entry.ready)
	d.modelMu.Unlock()
	return catalog, nil
}
