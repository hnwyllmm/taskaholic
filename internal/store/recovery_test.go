package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/workflow"
)

func TestEnvironmentFeedbackKeepsFailureTail(t *testing.T) {
	message := "snapshot=start\n" + strings.Repeat("configuration output\n", 1500) + "native error 206\nPOLICY_RESTORED=0"
	excerpt := environmentEvidence(message)
	if !strings.HasPrefix(excerpt, "snapshot=start") || !strings.HasSuffix(excerpt, "POLICY_RESTORED=0") || !strings.Contains(excerpt, "native error 206") {
		t.Fatal("lost failure evidence")
	}
}

func TestRecoveryKeepsSessionApprovalAndHasNoAttemptCap(t *testing.T) {
	ctx := context.Background()
	s, dev, task, run := approvedEnvironmentFixture(t)
	before := *developmentState(t, s, task.ID)
	session := run.SessionID
	for i := 0; i < 7; i++ {
		developmentFinish(t, s, run, int64(3+i), workflow.Result{Outcome: "blocked", Message: "diagnosis continues", Artifacts: []workflow.File{}, RecoveryRequest: &workflow.RecoveryRequest{Evidence: "build script returned 1; saved stderr", NextStep: "inspect script and fix in authorized worktree"}})
		current, _ := s.GetTask(ctx, task.ID)
		if current.State != model.TaskStateQueued {
			t.Fatal(current.State)
		}
		var retry int64
		if err := s.db.QueryRow(`SELECT retry_at_ms FROM task_workflow WHERE task_id=?`, task.ID).Scan(&retry); err != nil || retry <= time.Now().UnixMilli() {
			t.Fatal("continuation not paced", err)
		}
		after := developmentState(t, s, task.ID)
		if after.PlanHash != before.PlanHash || after.ApprovedReviewID != before.ApprovedReviewID || after.Version != before.Version {
			t.Fatal("approval changed")
		}
		run = startWork(t, s, dev, task)
		if run.SessionID != session {
			t.Fatal("lost session")
		}
	}
}

func TestRecoveryRegistersPRBeforeContinuing(t *testing.T) {
	ctx := context.Background()
	s, _, task, run := approvedEnvironmentFixture(t)
	source := saveTestSource(t, s, "github")
	prURL := "https://github.com/oceanbase/seekdb/pull/123"
	developmentFinish(t, s, run, 3, workflow.Result{
		Outcome:         "blocked",
		Message:         "implementation continues",
		Artifacts:       []workflow.File{},
		RecoveryRequest: &workflow.RecoveryRequest{Evidence: "review feedback is being fixed", NextStep: "finish the approved implementation"},
		PullRequests:    []workflow.PullRequest{{URL: prURL, SourceID: source.ID}},
	})
	current, _ := s.GetTask(ctx, task.ID)
	if current.State != model.TaskStateQueued {
		t.Fatal("continuation was blocked by its PR reference", current.State)
	}
	targets, err := s.ListSourceTargets(ctx)
	if err != nil || len(targets) != 1 || targets[0].TaskID != task.ID || targets[0].Entity != prURL {
		t.Fatalf("PR was not registered before continuation: targets=%+v err=%v", targets, err)
	}
}

func TestRecoveryHonorsPauseAndUnapprovedPlan(t *testing.T) {
	for _, paused := range []bool{true, false} {
		t.Run(map[bool]string{true: "paused", false: "unapproved"}[paused], func(t *testing.T) {
			ctx := context.Background()
			s, _, task, run := approvedEnvironmentFixture(t)
			if paused {
				if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return pauseWorkTx(ctx, tx, task.ID) }); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := s.db.Exec(`UPDATE development SET data_json=json_set(data_json,'$.approved_review_id','') WHERE task_id=?`, task.ID); err != nil {
					t.Fatal(err)
				}
			}
			developmentFinish(t, s, run, 3, workflow.Result{Outcome: "blocked", Message: "repair", Artifacts: []workflow.File{}, RecoveryRequest: &workflow.RecoveryRequest{Evidence: "error", NextStep: "diagnose"}})
			var pending int
			s.db.QueryRow(`SELECT COUNT(*) FROM task_message WHERE task_id=? AND delivery='PENDING'`, task.ID).Scan(&pending)
			current, _ := s.GetTask(ctx, task.ID)
			if pending != 0 || current.State == model.TaskStateQueued {
				t.Fatal("unsafe continuation", current.State, pending)
			}
		})
	}
}

func TestTransientDisconnectResumesOnlyKnownTransportError(t *testing.T) {
	for _, message := range []string{"websocket: Connection reset without closing handshake", "git failed", "401 unauthorized", "command timeout"} {
		t.Run(message, func(t *testing.T) {
			ctx := context.Background()
			s, _, task, run := approvedEnvironmentFixture(t)
			if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: run.ID, TaskID: task.ID, Type: "run.failed", Error: message}); err != nil {
				t.Fatal(err)
			}
			current, _ := s.GetTask(ctx, task.ID)
			if (current.State == model.TaskStateQueued) != transientAgentDisconnect(message) {
				t.Fatal(current.State)
			}
		})
	}
}

func TestDiagnosisCanResumeAutomaticPauseButNotUserPause(t *testing.T) {
	ctx := context.Background()
	s, dev, task, run := approvedEnvironmentFixture(t)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: run.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 3, RunID: run.ID, TaskID: task.ID, Type: "run.failed", Error: "git failed"}); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetTask(ctx, task.ID)
	if err := s.ResumeDevelopmentDiagnosis(ctx, task.ID, current.Version, "Inspect the original failure; no claim that the environment is fixed."); err != nil {
		t.Fatal(err)
	}
	next := startWork(t, s, dev, task)
	if next.SessionID != run.SessionID {
		t.Fatal("lost session")
	}
	if err := s.sourceWrite(ctx, func(tx *sql.Tx) error { return pauseWorkTx(ctx, tx, task.ID) }); err != nil {
		t.Fatal(err)
	}
	current, _ = s.GetTask(ctx, task.ID)
	if err := s.ResumeDevelopmentDiagnosis(ctx, task.ID, current.Version, "continue"); err == nil {
		t.Fatal("user pause bypassed")
	}
}

func TestRecoveryRejectsPlanningAndUncertainPublication(t *testing.T) {
	t.Run("planning", func(t *testing.T) {
		s, dev, _, task := developmentFixture(t)
		run := startWork(t, s, dev, task)
		developmentFinish(t, s, run, 1, workflow.Result{Outcome: "blocked", Message: "repair", Artifacts: []workflow.File{}, RecoveryRequest: &workflow.RecoveryRequest{Evidence: "error", NextStep: "diagnose"}})
		current, _ := s.GetTask(context.Background(), task.ID)
		if current.State != model.TaskStateBlocked {
			t.Fatal(current.State)
		}
	})
	t.Run("uncertain-publication", func(t *testing.T) {
		s, _, task, run := approvedEnvironmentFixture(t)
		if _, err := s.db.Exec(`INSERT INTO publication VALUES('uncertain',?,'UNCERTAIN',0,'{}')`, task.ID); err != nil {
			t.Fatal(err)
		}
		developmentFinish(t, s, run, 3, workflow.Result{Outcome: "blocked", Message: "repair", Artifacts: []workflow.File{}, RecoveryRequest: &workflow.RecoveryRequest{Evidence: "error", NextStep: "diagnose"}})
		current, _ := s.GetTask(context.Background(), task.ID)
		if current.State != model.TaskStateBlocked {
			t.Fatal(current.State)
		}
	})
}

func TestUnavailableEnvironmentReturnsPrerequisiteWithoutDeadlockedChild(t *testing.T) {
	s, dev, task, run := approvedEnvironmentFixture(t)
	if _, err := s.db.Exec(`UPDATE runtime SET capabilities_json=json_set(capabilities_json,'$.executors.windows_seekdb_phase0.available',json('false'),'$.executors.windows_seekdb_phase0.reason','build entry not registered') WHERE runtime_id=?`, dev.RuntimeID); err != nil {
		t.Fatal(err)
	}
	developmentFinish(t, s, run, 3, workflow.Result{Outcome: "blocked", Message: "test", Artifacts: []workflow.File{}, EnvironmentRequest: &workflow.EnvironmentRequest{Profile: "windows_seekdb_phase0", Reason: "verify"}})
	var children int
	s.db.QueryRow(`SELECT COUNT(*) FROM environment_job WHERE parent_task_id=?`, task.ID).Scan(&children)
	current, _ := s.GetTask(context.Background(), task.ID)
	if children != 0 || current.State != model.TaskStateQueued {
		t.Fatal("deadlocked environment request", children, current.State)
	}
	next := startWork(t, s, dev, task)
	if next.SessionID != run.SessionID {
		t.Fatal("lost session")
	}
}
