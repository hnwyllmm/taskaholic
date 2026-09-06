package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"work-assistant/internal/backup"
	"work-assistant/internal/store"
)

func TestBackupAPIAuthenticationAndVerifiedSnapshot(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "control.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := backup.New(backup.Config{Directory: t.TempDir(), Sources: map[string]backup.Source{"control.sqlite": s}})
	if err != nil {
		t.Fatal(err)
	}
	server := New(Config{APIToken: "test", Backups: m}, s, nil)
	for _, method := range []string{"GET", "POST"} {
		w := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(w, httptest.NewRequest(method, "http://example.com/api/v1/admin/backups", nil))
		if w.Code != 401 {
			t.Fatal("missing authentication", method, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "http://example.com/api/v1/admin/backups", nil)
	r.Header.Set("Authorization", "Bearer test")
	r.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin backup accepted", w.Code)
	}
	r = httptest.NewRequest("POST", "http://example.com/api/v1/admin/backups", nil)
	r.Header.Set("Authorization", "Bearer test")
	w = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var snapshot backup.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Verify(context.Background(), filepath.Join(m.Status().Directory, snapshot.ID)); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/admin/backups", "/api/v1/system"} {
		r = httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer test")
		w = httptest.NewRecorder()
		server.http.Handler.ServeHTTP(w, r)
		if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
