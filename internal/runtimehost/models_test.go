package runtimehost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
	"work-assistant/internal/rpcpeer"
)

type catalogAdapter struct {
	interactiveTestAdapter
	calls atomic.Int32
	fail  bool
}

func (a *catalogAdapter) ListModels(ctx context.Context) ([]model.ModelOption, error) {
	a.calls.Add(1)
	if a.fail {
		return nil, errors.New("private upstream error")
	}
	return []model.ModelOption{{ID: "test", Name: "Test"}}, nil
}

func TestModelRPCOptionalProviderCachingAndRetry(t *testing.T) {
	a := &catalogAdapter{}
	d := &Daemon{adapters: map[string]agent.Adapter{"listed": a, "manual": &interactiveTestAdapter{}}}
	request := rpcpeer.Request{Method: "models.list", Params: []byte(`{"adapter_id":"listed"}`)}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			result, err := d.handleRequest(context.Background(), request)
			if err != nil || result.(model.ModelCatalog).Status != "ready" {
				t.Error(result, err)
			}
		})
	}
	wg.Wait()
	if a.calls.Load() != 1 {
		t.Fatal("concurrent requests were not coalesced", a.calls.Load())
	}
	for _, id := range []string{"manual", "missing"} {
		result, err := d.handleRequest(context.Background(), rpcpeer.Request{Method: "models.list", Params: []byte(`{"adapter_id":"` + id + `"}`)})
		if id == "missing" && err == nil || id == "manual" && (err != nil || result.(model.ModelCatalog).Status != "unsupported") {
			t.Fatal(id, result, err)
		}
	}
	d.modelCache["listed"].expires = time.Now().Add(-time.Second)
	a.fail = true
	result, err := d.handleRequest(context.Background(), request)
	if err != nil || result.(model.ModelCatalog).Status != "unavailable" || len(result.(model.ModelCatalog).Models) != 0 {
		t.Fatal("failed query pretended to be ready", result, err)
	}
	a.fail = false
	d.modelCache["listed"].expires = time.Now().Add(-time.Second)
	result, err = d.handleRequest(context.Background(), request)
	if err != nil || result.(model.ModelCatalog).Status != "ready" || a.calls.Load() != 3 {
		t.Fatal("failed catalog could not recover", result, err)
	}
	caps := d.capabilities()["adapters"].(map[string]any)
	if caps["listed"].(map[string]any)["model_catalog"] != true || caps["manual"].(map[string]any)["model_catalog"] != false {
		t.Fatal(caps)
	}
}
