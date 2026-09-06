//go:build activity_ui_fixture

package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"testing"
	"time"

	"work-assistant/internal/model"
)

// Opt-in isolated UI exercise: real SQLite + SSE, simulated public actions,
// no CLI/model invocation and no access to production databases.
func TestActivityBrowserFixture(t *testing.T) {
	addr := os.Getenv("WA_ACTIVITY_UI_ADDR")
	if addr == "" {
		t.Skip("set WA_ACTIVITY_UI_ADDR=127.0.0.1:17344")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("loopback only")
	}
	s, st, task, run := activityServerFixture(t)
	s.config.APIToken = ""
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: s.http.Handler, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	go server.Serve(listener)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	seq := int64(3)
	emit := func(a model.Action) {
		t.Helper()
		seq++
		_, err := st.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "review-epoch", RuntimeSeq: seq, RunID: run.ID, Type: "run.progress", Activity: &a})
		if err != nil && ctx.Err() == nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 55; i++ {
		emit(model.Action{ID: fmt.Sprintf("history/%d", i), Kind: "tool", State: "COMPLETED", Title: fmt.Sprintf("历史资料检查 %02d（模拟）", i), Details: "公开行动记录；不创建新 Session"})
	}
	emit(model.Action{ID: "plan", Kind: "plan", State: "COMPLETED", Title: "工作计划（模拟）", Details: "1. 阅读设计\n2. 检查实现\n3. 验证结果"})
	fmt.Printf("ISOLATED ACTIVITY UI: http://%s/tasks#%s session=%s\n", listener.Addr(), task.ID, run.SessionID)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	step := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := st.HeartbeatRuntime(ctx, run.RuntimeID, "review-epoch"); err != nil {
				if ctx.Err() != nil {
					return
				}
				t.Fatal(err)
			}
			item := model.Action{ID: fmt.Sprintf("live/%d", step/3), Kind: "command", State: "RUNNING", Title: fmt.Sprintf("检查测试结果 %02d（模拟）", step/3), Command: "go test ./internal/...", Output: "ok model\ntoken=fixture-secret-value\n<script>literal-text-not-executable</script>"}
			if step%3 >= 1 {
				item.Output += "\nok store\nok server"
			}
			if step%3 == 2 {
				item.State = "COMPLETED"
				zero := 0
				item.ExitCode = &zero
			}
			emit(item)
			step++
		}
	}
}
