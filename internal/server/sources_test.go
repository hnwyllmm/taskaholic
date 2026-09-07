package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

func TestSourceAPIAuthCASAndUI(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	server := New(Config{APIToken: "test"}, state, nil)
	for _, path := range []string{"/api/v1/sources", "/api/v1/source-targets/a"} {
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if path == "/api/v1/sources" && rec.Code != 401 {
			t.Fatal("source credentials exposed")
		}
	}
	source := model.TaskSource{Kind: "github", Name: "PR watcher", Enabled: true, IntervalSeconds: 5}
	call := func(expected int64, want int) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"source": source, "expected_version": expected})
		req := httptest.NewRequest("PUT", "http://example.com/api/v1/sources/github", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer test")
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	call(0, 200)
	call(0, 409)
	call(1, 200)
	for _, path := range []string{"/sources", "/assets/sources.js", "/assets/sources.css"} {
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Fatal("source page missing/security headers", path, rec.Code)
		}
	}
	req := httptest.NewRequest("PUT", "http://example.com/api/v1/sources/github", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("cross-origin mutation accepted")
	}
}
