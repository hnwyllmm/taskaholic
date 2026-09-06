package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

func makeReadyUpgrade(t *testing.T, state *store.Store) model.Upgrade {
	t.Helper()
	u, err := state.CreateUpgrade(context.Background(), "Add feature", "implement and test")
	if err != nil {
		t.Fatal(err)
	}
	u.State = "BUILDING"
	u, err = state.ChangeUpgrade(context.Background(), u, u.Version, "build")
	if err != nil {
		t.Fatal(err)
	}
	u.State, u.CandidateSHA256, u.SourceSHA256 = "READY", "candidate", "source"
	u, err = state.ChangeUpgrade(context.Background(), u, u.Version, "ready")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUpgradeAPIRequiresEnabledSupervisorAndPinnedCandidate(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	u := makeReadyUpgrade(t, state)
	call := func(server *Server, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		recorder := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(recorder, req)
		return recorder
	}
	disabled := New(Config{APIToken: "test"}, state, nil)
	if got := call(disabled, "POST", "/api/v1/upgrades/"+u.ID+"/install", map[string]any{"expected_version": u.Version, "candidate_sha256": u.CandidateSHA256}); got.Code != 409 {
		t.Fatalf("disabled supervisor accepted install: %d %s", got.Code, got.Body.String())
	}
	enabled := New(Config{APIToken: "test", UpgradeEnabled: true}, state, nil)
	if got := call(enabled, "GET", "/api/v1/upgrades", nil); got.Code != 200 || !bytes.Contains(got.Body.Bytes(), []byte(`"enabled":true`)) {
		t.Fatalf("list upgrades: %d %s", got.Code, got.Body.String())
	}
	if got := call(enabled, "POST", "/api/v1/upgrades/"+u.ID+"/install", map[string]any{"expected_version": u.Version, "candidate_sha256": "wrong"}); got.Code != 409 {
		t.Fatalf("wrong candidate accepted: %d %s", got.Code, got.Body.String())
	}
	got := call(enabled, "POST", "/api/v1/upgrades/"+u.ID+"/install", map[string]any{"expected_version": u.Version, "candidate_sha256": u.CandidateSHA256})
	if got.Code != 202 || !bytes.Contains(got.Body.Bytes(), []byte(`"state":"WAITING_IDLE"`)) {
		t.Fatalf("install approval: %d %s", got.Code, got.Body.String())
	}
	var waiting model.Upgrade
	if err := json.Unmarshal(got.Body.Bytes(), &waiting); err != nil {
		t.Fatal(err)
	}
	if got = call(enabled, "POST", "/api/v1/upgrades/"+u.ID+"/cancel", map[string]any{"expected_version": waiting.Version}); got.Code != 200 || !bytes.Contains(got.Body.Bytes(), []byte(`"state":"CANCELLED"`)) {
		t.Fatalf("cancel draining upgrade: %d %s", got.Code, got.Body.String())
	}
}
