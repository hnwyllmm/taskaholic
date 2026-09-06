package server

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/store"
)

func TestAPIAuthConfigurationAndAnonymousAccessKeepRuntimeAndOriginChecks(t *testing.T) {
	for _, token := range []string{"", "control-test-secret"} {
		t.Run("token="+token, func(t *testing.T) {
			state, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			s := New(Config{APIToken: token, RuntimeToken: "runtime-test-secret"}, state, nil)
			call := func(method, path, origin, authorization, body string) *httptest.ResponseRecorder {
				t.Helper()
				r := httptest.NewRequest(method, "http://work.test"+path, strings.NewReader(body))
				r.Header.Set("Origin", origin)
				r.Header.Set("Authorization", authorization)
				w := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, r)
				return w
			}
			config := call("GET", "/api/v1/auth/config", "", "", "")
			want := `{"api_token_required":false}`
			if token != "" {
				want = `{"api_token_required":true}`
			}
			if config.Code != 200 || strings.TrimSpace(config.Body.String()) != want || config.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("public auth configuration leaked data or misreported policy", config.Code, config.Body.String())
			}
			wantRead, wantWrite := 200, 201
			if token != "" {
				wantRead, wantWrite = 401, 401
			}
			if w := call("GET", "/api/v1/system", "", "", ""); w.Code != wantRead {
				t.Fatal("anonymous read", w.Code, w.Body.String())
			}
			if w := call("POST", "/api/v1/projects", "http://work.test", "", `{"name":"Test material","context":"Test only"}`); w.Code != wantWrite {
				t.Fatal("same-origin write", w.Code, w.Body.String())
			}
			if w := call("POST", "/api/v1/projects", "http://untrusted.test", "Bearer "+token, `{}`); w.Code != 403 {
				t.Fatal("cross-origin protection was removed", w.Code)
			}
			if w := call("GET", "/runtime/ws", "", "", ""); w.Code != 401 {
				t.Fatal("runtime authentication was removed", w.Code)
			}
			if w := call("GET", "/api/v1/system", "", "Bearer "+token, ""); w.Code != 200 {
				t.Fatal("configured access no longer works", w.Code)
			}
		})
	}
}
