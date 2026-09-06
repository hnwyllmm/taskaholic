package main

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type healthRoundTripper struct {
	body string
}

func (r healthRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(r.body))}, nil
}

func TestSupervisorLockIsExclusiveAndRecoverable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervisor.lock")
	first, err := acquireSupervisorLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireSupervisorLock(path); err == nil {
		releaseSupervisorLock(second)
		t.Fatal("second supervisor acquired the same lock")
	}
	releaseSupervisorLock(first)
	third, err := acquireSupervisorLock(path)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	releaseSupervisorLock(third)
}

func TestWaitHealthyBindsTheChildInstance(t *testing.T) {
	client := &http.Client{Transport: healthRoundTripper{body: `{"status":"ready","instance_id":"expected"}`}}
	if err := waitHealthyWithClient(context.Background(), client, "http://health.invalid/ready", "expected", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitHealthyWithClient(context.Background(), client, "http://health.invalid/ready", "other", 25*time.Millisecond); err == nil {
		t.Fatal("health check accepted another process instance")
	}
}
