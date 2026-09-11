package server

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/store"
)

func TestImprovementAPIAuthenticationAndEmbeddedPage(t *testing.T) {
	state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := New(Config{APIToken: "test"}, state, nil)

	for _, path := range []string{"/api/v1/improvements/overview", "/api/v1/improvements/candidates", "/api/v1/improvements/experiences", "/api/v1/improvements/policies"} {
		unauthorized := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(unauthorized, httptest.NewRequest("GET", path, nil))
		if unauthorized.Code != 401 {
			t.Fatalf("%s exposed without authentication: %d", path, unauthorized.Code)
		}
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer test")
		response := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(response, req)
		if response.Code != 200 || !strings.Contains(response.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s unavailable: %d %s", path, response.Code, response.Body.String())
		}
	}
	for path, fragment := range map[string]string{
		"/improvements":                    "持续改进",
		"/assets/improvements.js":          "gross_token_saving",
		"/assets/improvements.css":         "metric-grid",
		"/api/v1/work/tasks/no/evaluation": "",
	} {
		req := httptest.NewRequest("GET", path, nil)
		if strings.HasPrefix(path, "/api/") {
			req.Header.Set("Authorization", "Bearer test")
		}
		response := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(response, req)
		want := 200
		if strings.HasPrefix(path, "/api/") {
			want = 404
		}
		if response.Code != want || (fragment != "" && !strings.Contains(response.Body.String(), fragment)) {
			t.Fatalf("page/API %s: %d %s", path, response.Code, response.Body.String())
		}
		if !strings.HasPrefix(path, "/api/") && response.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("page asset %s has no security policy", path)
		}
	}
	asset, err := roleUI.ReadFile("ui/improvements.js")
	if err != nil || strings.Contains(string(asset), "innerHTML") {
		t.Fatal("unsafe improvement UI", err)
	}
}
