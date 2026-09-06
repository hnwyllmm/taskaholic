//go:build review_ui_fixture

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"testing"
	"time"

	"work-assistant/internal/model"
)

// Opt-in loopback-only UI fixture. It has its own temporary DB, no real model
// process, and no production data or external side effects.
func TestReviewChatBrowserFixture(t *testing.T) {
	addr := os.Getenv("WA_REVIEW_UI_ADDR")
	if addr == "" {
		t.Skip("set WA_REVIEW_UI_ADDR=127.0.0.1:17344 to run the isolated UI fixture")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("fixture must listen on 127.0.0.1")
	}
	s, state, task, delivery, _ := serverReviewFixture(t)
	s.config.APIToken = ""
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Handler: s.http.Handler, ReadHeaderTimeout: 5 * time.Second}
	defer httpServer.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	fmt.Printf("ISOLATED REVIEW UI: http://%s/ task=%s session=%s\n", listener.Addr(), task.ID, delivery.SessionID)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	started := map[string]time.Time{}
	seq := int64(2)
	apply := func(event model.RuntimeEvent) {
		t.Helper()
		seq++
		event.RuntimeID, event.Epoch, event.RuntimeSeq = "review-machine", "review-epoch", seq
		if _, err := state.ApplyRuntimeEvent(ctx, event); err != nil {
			if ctx.Err() != nil {
				return
			}
			t.Fatal(err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-serveErr:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Fatal(err)
			}
			return
		case <-ticker.C:
			s.scheduleReviewTurns(ctx)
			w, err := state.GetWorkDetail(ctx, task.ID)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				t.Fatal(err)
			}
			detail, err := state.GetTaskDetail(ctx, task.ID)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				t.Fatal(err)
			}
			for _, turn := range w.ReviewTurns {
				if turn.State != "RUNNING" {
					continue
				}
				if turn.SessionID != delivery.SessionID || turn.AgentID != delivery.AgentID {
					t.Fatal("fixture discussion changed its original delivering session")
				}
				if _, ok := started[turn.RunID]; !ok {
					started[turn.RunID] = time.Now()
					apply(model.RuntimeEvent{RunID: turn.RunID, Type: "run.started"})
					apply(model.RuntimeEvent{RunID: turn.RunID, SessionID: turn.SessionID, Type: "session.bound", AgentSessionRef: "codex:review-fixture-session"})
				}
				interrupted := false
				for _, d := range detail.Directives {
					if d.RunID == turn.RunID && d.Kind == model.DirectiveKindInterrupt {
						apply(model.RuntimeEvent{RunID: turn.RunID, Type: "directive.applied", DirectiveID: d.ID})
						apply(model.RuntimeEvent{RunID: turn.RunID, Type: "run.interrupted"})
						interrupted = true
						break
					}
				}
				if !interrupted && time.Since(started[turn.RunID]) >= 6*time.Second {
					output, _ := json.Marshal(map[string]string{"message": "我仍在交付架构说明的同一个 Session 中。针对“" + turn.Question + "”：Manager 只维护任务映射，工作过程由原 Agent 的原生会话保留。文件版本没有改变，仍需你明确验收。（此页为模拟验证，未调用真实模型。）"})
					apply(model.RuntimeEvent{RunID: turn.RunID, Type: "run.completed", Output: string(output)})
				}
			}
		}
	}
}
