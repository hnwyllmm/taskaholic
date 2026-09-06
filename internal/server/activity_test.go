package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/store"
)

func activityServerFixture(t *testing.T) (*Server, *store.Store, model.Task, model.Run) {
	t.Helper()
	s, st, task, _, review := serverReviewFixture(t)
	ctx := context.Background()
	q, err := st.MessageReview(ctx, task.ID, review.ID, "Explain while I observe", "observe", review.DiscussionVersion)
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.StartReviewTurn(ctx, q.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "review-epoch", RuntimeSeq: 3, RunID: run.ID, Type: "run.started"}); err != nil {
		t.Fatal(err)
	}
	return s, st, task, run
}

func TestActivityAPIAuthValidationAndEmbeddedAssets(t *testing.T) {
	s, _, task, _ := activityServerFixture(t)
	base := "/api/v1/work/tasks/" + task.ID
	for _, c := range []struct {
		path, token string
		code        int
	}{
		{base + "/activities", "", 401}, {base + "/events", "", 401}, {base + "/activities?before=-1", "test", 400}, {base + "/events?after=invalid", "test", 400}, {base + "/events?after=9223372036854775808", "test", 400},
		{"/api/v1/work/tasks/missing/activities", "test", 404}, {"/api/v1/work/tasks/missing/events", "test", 404}, {base + "/activities", "test", 200}, {base + "?event_limit=30", "test", 200}, {"/assets/activity.js", "", 200},
	} {
		t.Run(c.path+"/"+c.token, func(t *testing.T) {
			req := httptest.NewRequest("GET", c.path, nil)
			if c.token != "" {
				req.Header.Set("Authorization", "Bearer "+c.token)
			}
			out := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(out, req)
			if out.Code != c.code {
				t.Fatal(out.Code, out.Body.String())
			}
		})
	}
	file, err := roleUI.ReadFile("ui/tasks.html")
	if err != nil || !strings.Contains(string(file), `id="activity-panel"`) || !strings.Contains(string(file), `/assets/activity.js`) {
		t.Fatal("activity not wired into task UI", err)
	}
	file, err = roleUI.ReadFile("ui/activity.js")
	if err != nil || strings.Contains(string(file), "innerHTML") || strings.Contains(string(file), "?token=") {
		t.Fatal("unsafe activity UI", err)
	}
}

func TestTaskEventStreamIsLiveFilteredAndResumesAfterSnapshot(t *testing.T) {
	s, st, task, run := activityServerFixture(t)
	server := httptest.NewServer(s.http.Handler)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	page, err := st.ListActivities(ctx, task.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	// Commit between snapshot and stream: it must appear, with no lost boundary.
	emit := func(seq int64, state, output string) {
		t.Helper()
		_, err := st.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "review-epoch", RuntimeSeq: seq, RunID: run.ID, Type: "run.progress", Activity: &model.Action{ID: "scope/item", Kind: "command", State: state, Command: "go test ./...", Output: output}})
		if err != nil {
			t.Fatal(err)
		}
	}
	emit(4, "RUNNING", "part one")
	if _, err = st.CreateWork(ctx, store.CreateWorkRequest{Title: "other task", Goal: "do not stream me"}); err != nil {
		t.Fatal(err)
	}
	open := func(after int64, header string) (*http.Response, *bufio.Scanner) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v1/work/tasks/%s/events?after=%d", server.URL, task.ID, after), nil)
		req.Header.Set("Authorization", "Bearer test")
		if header != "" {
			req.Header.Set("Last-Event-ID", header)
		}
		r, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 200 || r.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal(r.Status)
		}
		return r, bufio.NewScanner(r.Body)
	}
	next := func(scanner *bufio.Scanner) model.Event {
		t.Helper()
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var e model.Event
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
					t.Fatal(err)
				}
				if e.CorrelationID != task.ID {
					t.Fatal("unrelated task leaked")
				}
				return e
			}
		}
		t.Fatal("stream ended", scanner.Err())
		return model.Event{}
	}
	r, reader := open(page.Cursor, "")
	first := next(reader)
	if first.Type != "RunActivity" || first.GlobalSeq <= page.Cursor {
		t.Fatal(first)
	}
	current, err := st.GetRun(ctx, run.ID)
	if err != nil || current.State != "RUNNING" {
		t.Fatal("only delivered after completion", current, err)
	}
	emit(5, "RUNNING", "part one\npart two")
	second := next(reader)
	if second.GlobalSeq <= first.GlobalSeq || !strings.Contains(string(second.Payload), "part two") {
		t.Fatal(second)
	}
	r.Body.Close()
	emit(6, "COMPLETED", "done")
	r, reader = open(0, fmt.Sprint(second.GlobalSeq))
	defer r.Body.Close()
	third := next(reader)
	if third.GlobalSeq <= second.GlobalSeq || !strings.Contains(string(third.Payload), `"state":"COMPLETED"`) {
		t.Fatal("reconnect replayed/omitted action", third)
	}
	// Quiet connections stay alive, independently of an Agent producing output.
	heartbeat := false
	for reader.Scan() {
		if reader.Text() == ": heartbeat" {
			heartbeat = true
			break
		}
		if strings.HasPrefix(reader.Text(), "data: ") {
			t.Fatal("unexpected event after cursor", reader.Text())
		}
	}
	if !heartbeat {
		t.Fatal("missing SSE heartbeat", reader.Err())
	}
}
