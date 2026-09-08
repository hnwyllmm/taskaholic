package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"work-assistant/internal/model"
)

func TestTaskConsultationIsDurableAndCannotInterruptWorker(t *testing.T) {
	ctx := context.Background()
	s, worker, task := workFixture(t)
	workerRun := startWork(t, s, worker, task)
	role := publishTestRole(t, s, "analysis")
	consultant, err := s.CreateAgent(ctx, model.AgentProfile{Name: "consultant", RoleID: role.ID, RuntimeID: "role-runtime", AdapterID: "codex-agent", ModelID: "test-model", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.CreateTaskConsultation(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := s.CreateTaskConsultation(ctx, task.ID)
	if again.ID != item.ID {
		t.Fatal("created duplicate consultation")
	}
	if _, err = s.CreateRun(ctx, CreateRunRequest{TaskID: item.ExecutionTaskID, AgentID: consultant.ID, RuntimeID: consultant.RuntimeID}); !errors.Is(err, model.ErrConflict) {
		t.Fatal("generic run bypassed consultation endpoint", err)
	}
	item, err = s.StartTaskConsultationRun(ctx, item.ID, item.Version, "现在做到哪里？", json.RawMessage(`{"task":{"state":"IN_PROGRESS"}}`), 42, CreateRunRequest{AgentID: consultant.ID, RuntimeID: consultant.RuntimeID, AdapterID: consultant.AdapterID, ModelID: consultant.ModelID, IdempotencyKey: "question-1"})
	if err != nil {
		t.Fatal(err)
	}
	consultationRun, _ := s.GetRun(ctx, item.LastRunID)
	if consultationRun.TaskID == task.ID || consultationRun.SessionID == workerRun.SessionID {
		t.Fatal("consultation reused the worker task or Session")
	}
	var raw []byte
	if err = s.db.QueryRow(`SELECT params_json FROM outbox_message WHERE json_extract(params_json,'$.run_id')=?`, consultationRun.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var spec model.RunSpec
	if err = json.Unmarshal(raw, &spec); err != nil || !spec.ReadOnly || len(spec.Command) != 0 || spec.WorkingDir != "" {
		t.Fatal("consultation run is not safely read-only", spec, err)
	}
	currentWorker, _ := s.GetRun(ctx, workerRun.ID)
	currentTask, _ := s.GetTask(ctx, task.ID)
	if currentWorker.State != model.RunStateQueued || currentTask.State != model.TaskStateInProgress {
		t.Fatalf("consultation changed worker: run=%s task=%s", currentWorker.State, currentTask.State)
	}
	if _, err = s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: consultant.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: consultationRun.ID, Type: "run.completed", Output: `{"message":"已记录到正在执行。"}`}); err != nil {
		t.Fatal(err)
	}
	item, err = s.GetTaskConsultation(ctx, task.ID)
	if err != nil || item.State != "IDLE" || len(item.Messages) != 2 || item.Messages[1].Speaker != "assistant" || item.SnapshotCursor != 42 {
		t.Fatalf("consultation reply missing: %#v %v", item, err)
	}
	if _, err = s.GetLatestTaskSummary(ctx, item.ExecutionTaskID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("consultation was recorded as business work", err)
	}
	work, err := s.ListWork(ctx)
	if err != nil || len(work) != 1 || work[0].ID != task.ID {
		t.Fatal("consultation polluted work list", work, err)
	}
}
