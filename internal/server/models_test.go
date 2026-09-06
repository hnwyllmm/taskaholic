package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"work-assistant/internal/model"
	"work-assistant/internal/rpcpeer"
	"work-assistant/internal/store"
)

func TestModelsAPIAuthRoutingAndOfflineFallback(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := New(Config{APIToken: "test"}, state, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hello := model.RuntimeHello{RuntimeID: "machine", Epoch: "epoch", Capabilities: map[string]any{"adapters": map[string]any{"listed": map[string]any{"model_catalog": true}, "old": map[string]any{}}}}
	if err := state.RegisterRuntime(ctx, hello); err != nil {
		t.Fatal(err)
	}
	call := func(path, token string, want int) model.ModelCatalog {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/v1/runtimes/"+path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var result model.ModelCatalog
		if want == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	call("machine/models?adapter_id=listed", "", 401)
	call("machine/models", "test", 400)
	call("missing/models?adapter_id=listed", "test", 404)
	call("machine/models?adapter_id=missing", "test", 400)
	call("machine/models?adapter_id=listed", "test", 503)
	if result := call("machine/models?adapter_id=old", "test", 200); result.Status != "unsupported" {
		t.Fatal(result)
	}
	// Exercise the real symmetric JSON-RPC WebSocket, not a stubbed HTTP reply.
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		peer := rpcpeer.New(conn, func(_ context.Context, request rpcpeer.Request) (any, *rpcpeer.Error) {
			if request.Method != "models.list" || string(request.Params) != `{"adapter_id":"listed"}` {
				t.Error("wrong RPC", request)
			}
			return model.ModelCatalog{Status: "ready", Models: []model.ModelOption{{ID: "remote-model", Name: "Remote Model"}}}, nil
		})
		_ = peer.Serve(ctx)
	}))
	defer remote.Close()
	callCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	conn, _, err := websocket.Dial(callCtx, remote.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	peer := rpcpeer.New(conn, nil)
	go peer.Serve(ctx)
	s.hub.register(&runtimeConnection{runtimeID: "machine", epoch: "epoch", peer: peer})
	if result := call("machine/models?adapter_id=listed", "test", 200); len(result.Models) != 1 || result.Models[0].ID != "remote-model" {
		t.Fatal(result)
	}
	s.hub.unregister("machine", peer)
	call("machine/models?adapter_id=listed", "test", 503)
}
