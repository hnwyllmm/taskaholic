package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"work-assistant/internal/model"
	"work-assistant/internal/router"
	"work-assistant/internal/workflow"
)

func TestSourceBoundaryMigrationAuditsConfigAndPreservesTaskSession(t *testing.T) {
	ctx := context.Background()
	s, author, task := workFixture(t)
	run := startWork(t, s, author, task)
	if _, err := s.ApplyRuntimeEvent(ctx, model.RuntimeEvent{RuntimeID: author.RuntimeID, Epoch: "epoch-role", RuntimeSeq: 1, RunID: run.ID, SessionID: run.SessionID, Type: "session.bound", AgentSessionRef: "codex:native-preserved"}); err != nil {
		t.Fatal(err)
	}
	finishWork(t, s, run, 2, "review", "before migration")
	source := saveTestSource(t, s, "github")
	// A schema-13 configuration, deliberately injected only in this temp DB.
	if _, err := s.db.Exec(`UPDATE task_source SET data_json=json_set(data_json,
		'$.config.role_id',?,'$.config.reviewer_role_ids',json(?),
		'$.config.project_id','old-team','$.config.defer_assignment',json('true')) WHERE source_id=?`,
		author.RoleID, `["historic-reviewer"]`, source.ID); err != nil {
		t.Fatal(err)
	}
	var oldRaw string
	if err := s.db.QueryRow(`SELECT data_json FROM task_source WHERE source_id=?`, source.ID).Scan(&oldRaw); err != nil {
		t.Fatal(err)
	}
	beforeTask, _ := s.GetTask(ctx, task.ID)
	beforeWork, _ := s.GetWorkDetail(ctx, task.ID)
	beforeSession, _ := s.GetTaskSession(ctx, task.ID)
	beforeRun, _ := s.GetRun(ctx, run.ID)
	if _, err := s.db.Exec(`DELETE FROM schema_version WHERE version>=14`); err != nil {
		t.Fatal(err)
	}
	if err := migrateV14(s.db); err != nil {
		t.Fatal(err)
	}
	if err := migrateV14(s.db); err != nil {
		t.Fatal(err)
	}
	afterTask, _ := s.GetTask(ctx, task.ID)
	afterWork, _ := s.GetWorkDetail(ctx, task.ID)
	afterSession, _ := s.GetTaskSession(ctx, task.ID)
	afterRun, _ := s.GetRun(ctx, run.ID)
	if !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeWork, afterWork) ||
		!reflect.DeepEqual(beforeSession, afterSession) || !reflect.DeepEqual(beforeRun, afterRun) {
		t.Fatal("migration changed existing work or native memory")
	}
	saved, err := s.GetTaskSource(ctx, source.ID)
	if err != nil || saved.Version != source.Version+1 {
		t.Fatal(saved, err)
	}
	var config, audit string
	if err = s.db.QueryRow(`SELECT data_json FROM task_source WHERE source_id=?`, source.ID).Scan(&config); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"role_id", "reviewer_role_ids", "project_id", "defer_assignment"} {
		if strings.Contains(config, `"`+key+`"`) {
			t.Fatal("legacy assignment remained active", key)
		}
	}
	if err = s.db.QueryRow(`SELECT payload_json FROM event_log WHERE event_type='SourceExecutionSettingsRetired'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	var record struct {
		Previous json.RawMessage `json:"previous_source"`
	}
	if err = json.Unmarshal([]byte(audit), &record); err != nil {
		t.Fatal(err)
	}
	var before, archived any
	if err = json.Unmarshal([]byte(oldRaw), &before); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(record.Previous, &archived); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, archived) {
		t.Fatal("old configuration not fully audited")
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE event_type='SourceExecutionSettingsRetired'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("migration repeated", count, err)
	}
}

type selectedReviewPlan struct {
	roleID string
	calls  int
}

func (p *selectedReviewPlan) PlanReviews(_ router.ReviewRequest, _ []model.AgentProfile) (router.ReviewPlan, error) {
	p.calls++
	return router.ReviewPlan{RoleIDs: []string{p.roleID}, Reason: "custom Router policy"}, nil
}

func TestManagerUsesReplaceableRouterPlanAndRetriesMissingReviewers(t *testing.T) {
	ctx := context.Background()
	s, author, task := workFixture(t)
	finishWork(t, s, startWork(t, s, author, task), 1, "review", "PR")
	source := saveTestSource(t, s, "github")
	target, err := s.RegisterPR(ctx, task.ID, source.ID, "https://github.com/o/seekdb/pull/12")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	event := model.SourceEvent{Key: "head:" + head, Kind: "github.head", HeadSHA: head, Message: "review this revision"}
	if err = s.CommitSourcePoll(ctx, source, target, json.RawMessage("{}"), head, []model.SourceEvent{event}, 0, false); err != nil {
		t.Fatal(err)
	}
	if err = s.ProcessSourceEvents(ctx); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListSourceEvents(ctx)
	if err != nil || len(events) != 1 || events[0].State != "PENDING" || !strings.Contains(events[0].Error, "Router") {
		t.Fatal("no-reviewer event lost or silently skipped", events, err)
	}
	reviews, err := s.ListSourceReviews(ctx)
	if err != nil || len(reviews) != 0 {
		t.Fatal("partial review fan-out", reviews, err)
	}
	role := publishTestRole(t, s, "code.review")
	reviewer, err := s.CreateAgent(ctx, model.AgentProfile{Name: "independent reviewer", RoleID: role.ID, RuntimeID: author.RuntimeID, AdapterID: author.AdapterID})
	if err != nil {
		t.Fatal(err)
	}
	// Retry with an injected Router policy; a successful plan is committed once.
	if _, err = s.db.Exec(`UPDATE source_event SET next_attempt_ms=0`); err != nil {
		t.Fatal(err)
	}
	planner := &selectedReviewPlan{roleID: role.ID}
	for range 2 {
		if err = s.ProcessSourceEvents(ctx, planner); err != nil {
			t.Fatal(err)
		}
	}
	reviews, err = s.ListSourceReviews(ctx)
	if err != nil || len(reviews) != 1 || reviews[0].RoleID != role.ID || planner.calls != 1 {
		t.Fatal("Router plugin or replay failed", reviews, planner.calls, err)
	}
	events, err = s.ListSourceEvents(ctx)
	if err != nil || events[0].Error != "" || events[0].State != "APPLIED" {
		t.Fatal(events, err)
	}
	var decisions int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM event_log WHERE event_type='PRReviewRouted'`).Scan(&decisions); err != nil || decisions != 1 {
		t.Fatal("missing routing audit", decisions, err)
	}
	child, err := s.GetTask(ctx, reviews[0].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.StartWorkRun(ctx, CreateRunRequest{TaskID: child.ID, AgentID: reviewer.ID, RuntimeID: reviewer.RuntimeID, AdapterID: reviewer.AdapterID}, workflow.JSONContract{})
	if err != nil {
		t.Fatal(err)
	}
	spec := outboxSpec(t, s, run.ID)
	if !spec.ReadOnly || !spec.NetworkAccess || spec.ExecutionGrant != nil || !strings.Contains(spec.Instructions, "已自动开启 Codex 网络访问") || !strings.Contains(spec.Instructions, "可只对该读取命令临时清除") || !strings.Contains(spec.Instructions, "不要再申请 network_access") {
		t.Fatalf("review network boundary missing: %#v", spec)
	}
}
