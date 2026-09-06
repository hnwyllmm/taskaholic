package store

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
)

func TestActivityProjectionIdentityReplayBackupAndTaskIsolation(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	run := startWork(t, s, a, task)
	event := model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RunID: run.ID, TaskID: task.ID, SessionID: run.SessionID, RuntimeSeq: 1, Type: "run.started"}
	if _, err := s.ApplyRuntimeEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetTask(ctx, task.ID)
	observedAt := time.Now().Add(-time.Minute).UnixMilli()
	event.Type = "run.progress"
	event.RuntimeSeq = 2
	event.OccurredAt = observedAt
	event.Activity = &model.Action{ID: "invocation/item_0", Kind: "command", State: "RUNNING", Command: "go test ./...", Output: "token=do-not-persist"}
	if _, err := s.ApplyRuntimeEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	first, err := s.ListActivities(ctx, task.ID, 0, 50)
	if err != nil || len(first.Items) != 1 {
		t.Fatal(first, err)
	}
	item := first.Items[0]
	if item.RunID != run.ID || item.TaskID != task.ID || item.AgentID != a.ID || item.SessionID != run.SessionID || item.RuntimeID != a.RuntimeID || item.ModelID != run.ModelID || item.StartedAtMS != observedAt || item.FirstSeq != item.LastSeq || !item.Redacted {
		t.Fatal(item)
	}
	if duplicate, err := s.ApplyRuntimeEvent(ctx, event); err != nil || !duplicate {
		t.Fatal("duplicate not acknowledged", duplicate, err)
	}
	unchanged, _ := s.ListActivities(ctx, task.ID, 0, 50)
	if !reflect.DeepEqual(first, unchanged) {
		t.Fatal("duplicate changed projection")
	}
	for _, field := range []string{"task", "session", "runtime"} {
		bad := event
		bad.RuntimeSeq = 3
		switch field {
		case "task":
			bad.TaskID = "another"
		case "session":
			bad.SessionID = "another"
		case "runtime":
			bad.RuntimeID = "another"
		}
		if _, err = s.ApplyRuntimeEvent(ctx, bad); err == nil {
			t.Fatal("forged identity accepted", field)
		}
	}
	event.RuntimeSeq = 3
	event.OccurredAt = observedAt + 1000
	event.Activity = &model.Action{ID: item.ID, Kind: "command", State: "COMPLETED", Command: item.Command, Output: "ok one\nok two"}
	if _, err = s.ApplyRuntimeEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	after, err := s.ListActivities(ctx, task.ID, 0, 50)
	if err != nil || len(after.Items) != 1 || after.Items[0].FirstSeq != item.FirstSeq || after.Items[0].LastSeq <= first.Cursor || after.Items[0].Output != "ok one\nok two" {
		t.Fatal(after, err)
	}
	events, err := s.EventsAfter(ctx, first.Cursor, 100, task.ID)
	if err != nil || len(events) != 1 {
		t.Fatal(events, err)
	}
	var streamed model.Activity
	if err = json.Unmarshal(events[0].Payload, &streamed); err != nil || !reflect.DeepEqual(streamed, after.Items[0]) {
		t.Fatal("stream/snapshot mismatch", streamed, after, err)
	}
	// Late active snapshots must not erase a terminal result or resurrect its card.
	event.RuntimeSeq = 4
	event.Activity.State = "RUNNING"
	event.Activity.Output = "stale"
	if _, err = s.ApplyRuntimeEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	late, _ := s.ListActivities(ctx, task.ID, 0, 50)
	if !reflect.DeepEqual(after, late) {
		t.Fatal("late progress replaced completed evidence", late)
	}
	afterTask, _ := s.GetTask(ctx, task.ID)
	if !reflect.DeepEqual(before, afterTask) {
		t.Fatal("observing changed task state/version", before, afterTask)
	}
	work, _ := s.GetWorkDetail(ctx, task.ID)
	if len(work.Artifacts) != 0 || len(work.Reviews) != 0 {
		t.Fatal("activity became a delivery", work)
	}
	other, err := s.CreateWork(ctx, CreateWorkRequest{Title: "unrelated", Goal: "must not leak"})
	if err != nil {
		t.Fatal(err)
	}
	otherEvents, _ := s.EventsAfter(ctx, 0, 100, other.ID)
	for _, e := range otherEvents {
		if e.CorrelationID != other.ID || e.AggregateID == run.ID {
			t.Fatal("cross task leak", e)
		}
	}
	limited, err := s.GetTaskDetail(ctx, task.ID, 2)
	if err != nil || len(limited.Events) != 2 || limited.Events[0].GlobalSeq >= limited.Events[1].GlobalSeq {
		t.Fatal(limited.Events, err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err = s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.ListActivities(ctx, task.ID, 0, 50)
	if err != nil || !reflect.DeepEqual(after.Items, restored.Items) {
		t.Fatal("lost activity on backup", restored, err)
	}
	var secrets int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE payload_json LIKE '%do-not-persist%'`).Scan(&secrets); err != nil || secrets != 0 {
		t.Fatal("secret persisted", secrets, err)
	}
}

func TestActivityPaginationTerminalClosureAndReplayAfterRuntimeRestart(t *testing.T) {
	ctx := context.Background()
	for _, terminal := range []string{"run.completed", "run.interrupted", "run.failed"} {
		t.Run(terminal, func(t *testing.T) {
			s, a, task := workFixture(t)
			run := startWork(t, s, a, task)
			seq := int64(0)
			emit := func(kind string, action *model.Action) {
				t.Helper()
				seq++
				_, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RunID: run.ID, RuntimeSeq: seq, Type: kind, Activity: action})
				if err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 7; i++ {
				emit("run.progress", &model.Action{ID: fmt.Sprintf("scope/%d", i), Kind: "tool", State: "RUNNING", Title: "observe"})
			}
			page, err := s.ListActivities(ctx, task.ID, 0, 3)
			if err != nil || len(page.Items) != 3 || !page.HasMore {
				t.Fatal(page, err)
			}
			next, err := s.ListActivities(ctx, task.ID, page.NextBefore, 3)
			if err != nil || len(next.Items) != 3 || !next.HasMore || next.Items[0].FirstSeq >= page.NextBefore {
				t.Fatal(next, err)
			}
			last, err := s.ListActivities(ctx, task.ID, next.NextBefore, 3)
			if err != nil || len(last.Items) != 1 || last.HasMore {
				t.Fatal(last, err)
			}
			if terminal == "run.completed" {
				seq++
				finishWork(t, s, run, seq, "review", "finished file")
			} else {
				emit(terminal, nil)
			}
			closed, err := s.ListActivities(ctx, task.ID, 0, 50)
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range closed.Items {
				want := "UNKNOWN"
				if terminal == "run.interrupted" {
					want = "INTERRUPTED"
				}
				if x.State != want || x.FinishedAtMS == 0 || x.Error == "" {
					t.Fatal("incomplete action misreported", x)
				}
			}
			// A current authenticated connection may replay old-epoch durable actions.
			if err = s.RegisterRuntime(ctx, model.RuntimeHello{RuntimeID: run.RuntimeID, Epoch: "new-epoch"}); err != nil {
				t.Fatal(err)
			}
			seq++
			ev := model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: seq, RunID: run.ID, Type: "run.progress", Activity: &model.Action{ID: "scope/late", Kind: "tool", State: "RUNNING"}}
			if _, err = s.ApplyRuntimeEvent(ctx, ev); err == nil {
				t.Fatal("fenced connection accepted")
			}
			if _, err = s.ApplyRuntimeEventFrom(ctx, "new-epoch", ev); err != nil {
				t.Fatal("durable replay rejected", err)
			}
			closed, _ = s.ListActivities(ctx, task.ID, 0, 50)
			if closed.Items[0].State != "UNKNOWN" {
				t.Fatal("terminal run revived a late action", closed.Items[0])
			}
		})
	}
}

func TestMalformedActivityRollsBackAndActionIDsAreRunScoped(t *testing.T) {
	ctx := context.Background()
	s, a, task := workFixture(t)
	run := startWork(t, s, a, task)
	ev := model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, Type: "run.progress", Activity: &model.Action{ID: strings.Repeat("x", 300), Kind: "command", State: "COMPLETED"}}
	if _, err := s.ApplyRuntimeEvent(ctx, ev); err == nil {
		t.Fatal("invalid action accepted")
	}
	ev.Activity.ID = "scope/reused"
	if duplicate, err := s.ApplyRuntimeEvent(ctx, ev); err != nil || duplicate {
		t.Fatal("failed event consumed dedupe key", duplicate, err)
	}
	finishWork(t, s, run, 2, "review", "one")
	work, _ := s.GetWorkDetail(ctx, task.ID)
	if _, err := s.DecideReview(ctx, task.ID, work.Reviews[0].ID, "CHANGES_REQUESTED", "revise"); err != nil {
		t.Fatal(err)
	}
	run2 := startWork(t, s, a, task)
	ev.RunID = run2.ID
	ev.RuntimeSeq = 3
	if _, err := s.ApplyRuntimeEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListActivities(ctx, task.ID, 0, 50)
	if err != nil || len(page.Items) != 2 || page.Items[0].RunID == page.Items[1].RunID || page.Items[0].SessionID != page.Items[1].SessionID {
		t.Fatal(page, err)
	}
}
