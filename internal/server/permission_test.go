package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"work-assistant/internal/store"
)

func TestExecutionPermissionsAPIAndAssets(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(Config{APIToken: "test"}, st, nil)
	for _, tc := range []struct {
		method, path, token, body string
		status                    int
	}{
		{"GET", "/permissions", "", "", 200}, {"GET", "/assets/permissions.js", "", "", 200},
		{"GET", "/api/v1/execution-permissions", "", "", 401}, {"GET", "/api/v1/execution-permissions", "test", "", 200},
		{"POST", "/api/v1/work/tasks/missing/execution-permissions", "test", `{"expected_version":1,"capability":"sudo"}`, 400},
		{"POST", "/api/v1/work/tasks/missing/execution-permissions", "test", `{"expected_version":1,"capability":"network_access"}`, 404},
		{"PUT", "/api/v1/execution-permissions/policies", "test", `{"operation":"sudo","runtime_id":"dev","repository":"oceanbase/seekdb","effect":"allow"}`, 400},
		{"PUT", "/api/v1/execution-permissions/policies", "test", `{"operation":"windows_seekdb_phase0","runtime_id":"dev","repository":"oceanbase/seekdb","effect":"ask"}`, 200},
		{"PUT", "/api/v1/execution-permissions/policies", "test", `{"operation":"windows_seekdb_phase0","runtime_id":"dev","repository":"oceanbase/seekdb","effect":"allow"}`, 409},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		res := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(res, req)
		if res.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, res.Code, res.Body.String())
		}
	}
	policies, _ := st.ExecutionPolicies(context.Background())
	if len(policies) != 1 || policies[0].Effect != "ask" {
		t.Fatal("CAS did not preserve policy")
	}
}
