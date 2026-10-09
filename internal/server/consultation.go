package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"work-assistant/internal/model"
)

// Leave room under the Store's 80 KB hard boundary for the consultation's
// recent conversation, the user's current question and the output contract.
// Persisted observations are useful evidence, but an old test log must never
// make the read-only sidecar unavailable.
const taskConsultationSnapshotMaxBytes = 56 * 1024

type consultationSnapshotInput struct {
	At         string
	Cursor     int64
	Detail     model.TaskDetail
	Work       model.WorkDetail
	Activities []model.Activity
}

type consultationRunObservation struct {
	ID           string `json:"run_id"`
	State        string `json:"state"`
	AgentID      string `json:"agent_id"`
	ModelID      string `json:"model_id,omitempty"`
	CreatedAtMS  int64  `json:"created_at_ms"`
	StartedAtMS  *int64 `json:"started_at_ms,omitempty"`
	FinishedAtMS *int64 `json:"finished_at_ms,omitempty"`
	Error        string `json:"error,omitempty"`
}

type consultationPipelineJob struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	FailureReason   string `json:"failure_reason,omitempty"`
	URL             string `json:"web_url,omitempty"`
	LogExcerpt      string `json:"log_excerpt,omitempty"`
	LogTruncated    bool   `json:"log_truncated,omitempty"`
	LogCollected    bool   `json:"log_collected,omitempty"`
	LogCollectError string `json:"log_collect_error,omitempty"`
}

type consultationPipelineObservation struct {
	ID           string                    `json:"request_id"`
	State        string                    `json:"state"`
	PipelineID   int64                     `json:"pipeline_id,omitempty"`
	URL          string                    `json:"url,omitempty"`
	HeadSHA      string                    `json:"head_sha,omitempty"`
	Kind         string                    `json:"kind,omitempty"`
	Reason       string                    `json:"reason,omitempty"`
	Attempt      int                       `json:"attempt,omitempty"`
	Current      bool                      `json:"current"`
	Error        string                    `json:"error,omitempty"`
	PollError    string                    `json:"poll_error,omitempty"`
	UpdatedAtMS  int64                     `json:"updated_at_ms"`
	NextPollMS   int64                     `json:"next_poll_ms,omitempty"`
	LastPolledMS int64                     `json:"last_polled_ms,omitempty"`
	FailedJobs   []consultationPipelineJob `json:"failed_jobs,omitempty"`
}

func (s *Server) handleConsultationGet(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.GetTaskConsultation(r.Context(), r.PathValue("task_id"))
	if err == sql.ErrNoRows {
		reply(w, 200, map[string]any{"consultation": nil}, nil)
		return
	}
	reply(w, 200, map[string]any{"consultation": item}, err)
}

func (s *Server) handleConsultationCreate(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.CreateTaskConsultation(r.Context(), r.PathValue("task_id"))
	reply(w, http.StatusCreated, item, err)
}

func (s *Server) handleConsultationMessage(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Message string `json:"message"`
		Version int64  `json:"expected_version"`
		Key     string `json:"idempotency_key"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, 400, err)
		return
	}
	item, err := s.store.GetTaskConsultation(r.Context(), r.PathValue("task_id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if input.Key != "" {
		if _, err = s.store.GetRunByKey(r.Context(), item.ExecutionTaskID, input.Key); err == nil {
			reply(w, 200, item, nil)
			return
		} else if err != sql.ErrNoRows {
			writeStoreError(w, err)
			return
		}
	}
	snapshot, cursor, err := s.taskConsultationSnapshot(r, item.SubjectTaskID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	req, err := s.systemExecutor(r.Context(), "task_consultation", item.ExecutionTaskID, input.Key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	item, err = s.store.StartTaskConsultationRun(r.Context(), item.ID, input.Version, input.Message, snapshot, cursor, req)
	reply(w, http.StatusAccepted, item, err)
}

func (s *Server) handleConsultationStop(w http.ResponseWriter, r *http.Request) {
	err := s.store.InterruptTaskConsultation(r.Context(), r.PathValue("task_id"))
	reply(w, http.StatusAccepted, map[string]string{"status": "consultation_interrupt_requested"}, err)
}

func (s *Server) taskConsultationSnapshot(r *http.Request, taskID string) (json.RawMessage, int64, error) {
	detail, err := s.store.GetTaskDetail(r.Context(), taskID, 30)
	if err != nil {
		return nil, 0, err
	}
	work, err := s.store.GetWorkDetail(r.Context(), taskID)
	if err != nil {
		return nil, 0, err
	}
	activities, err := s.store.ListActivities(r.Context(), taskID, 0, 40)
	if err != nil {
		return nil, 0, err
	}
	raw, err := marshalTaskConsultationSnapshot(consultationSnapshotInput{
		At:         time.Now().Format(time.RFC3339),
		Cursor:     activities.Cursor,
		Detail:     detail,
		Work:       work,
		Activities: activities.Items,
	})
	return raw, activities.Cursor, err
}

func consultationClip(value string, size int) string {
	clean, _, _ := model.ObservationText(value, size)
	return clean
}

func consultationPipelineSnapshot(items []model.TestPipeline, includeLogs bool) []consultationPipelineObservation {
	items = items[:min(10, len(items))]
	result := make([]consultationPipelineObservation, 0, len(items))
	for _, item := range items {
		jobs := make([]consultationPipelineJob, 0, min(4, len(item.Jobs)))
		for _, job := range item.Jobs[:min(4, len(item.Jobs))] {
			entry := consultationPipelineJob{
				ID: job.ID, Name: consultationClip(job.Name, 240), Status: job.Status,
				FailureReason: consultationClip(job.FailureReason, 300), URL: consultationClip(job.URL, 1000),
				LogTruncated: job.LogTruncated || len(item.Jobs) > 4, LogCollected: job.LogCollected,
				LogCollectError: consultationClip(job.LogCollectError, 300),
			}
			if includeLogs {
				entry.LogExcerpt = consultationClip(job.LogExcerpt, 700)
			}
			jobs = append(jobs, entry)
		}
		result = append(result, consultationPipelineObservation{
			ID: item.ID, State: item.State, PipelineID: item.PipelineID, URL: consultationClip(item.URL, 1000),
			HeadSHA: item.HeadSHA, Kind: item.Kind, Reason: consultationClip(item.Reason, 800), Attempt: item.Attempt,
			Current: item.Current, Error: consultationClip(item.Error, 500), PollError: consultationClip(item.PollError, 500),
			UpdatedAtMS: item.UpdatedAtMS, NextPollMS: item.NextPollMS, LastPolledMS: item.LastPolledMS, FailedJobs: jobs,
		})
	}
	return result
}

func marshalTaskConsultationSnapshot(input consultationSnapshotInput) (json.RawMessage, error) {
	detail, work := input.Detail, input.Work
	detail.Task.Goal = consultationClip(detail.Task.Goal, 6000)

	work.Messages = append([]model.TaskMessage(nil), work.Messages[max(0, len(work.Messages)-15):]...)
	for i := range work.Messages {
		work.Messages[i].Content = consultationClip(work.Messages[i].Content, 2400)
	}
	work.Artifacts = append([]model.Artifact(nil), work.Artifacts[:min(4, len(work.Artifacts))]...)
	for i := range work.Artifacts {
		work.Artifacts[i].Content = consultationClip(work.Artifacts[i].Content, 2500)
	}
	activities := append([]model.Activity(nil), input.Activities[:min(20, len(input.Activities))]...)
	for i := range activities {
		activities[i].Command = consultationClip(activities[i].Command, 500)
		activities[i].Details = consultationClip(activities[i].Details, 600)
		activities[i].Output = consultationClip(activities[i].Output, 1200)
		activities[i].Error = consultationClip(activities[i].Error, 500)
	}
	work.Reviews = append([]model.Review(nil), work.Reviews[max(0, len(work.Reviews)-12):]...)
	for i := range work.Reviews {
		work.Reviews[i].Comment = consultationClip(work.Reviews[i].Comment, 1000)
	}
	work.ReviewTurns = append([]model.ReviewTurn(nil), work.ReviewTurns[max(0, len(work.ReviewTurns)-12):]...)
	for i := range work.ReviewTurns {
		work.ReviewTurns[i].Question = consultationClip(work.ReviewTurns[i].Question, 900)
		work.ReviewTurns[i].Answer = consultationClip(work.ReviewTurns[i].Answer, 1200)
		work.ReviewTurns[i].Error = consultationClip(work.ReviewTurns[i].Error, 500)
	}
	pipelines := consultationPipelineSnapshot(work.TestPipelines, true)
	publications := make([]map[string]any, 0, min(10, len(work.Publications)))
	for _, publication := range work.Publications[:min(10, len(work.Publications))] {
		publications = append(publications, map[string]any{"platform": publication.Platform, "url": publication.URL, "state": publication.State, "verdict": publication.Verdict, "desired_status": publication.DesiredStatus, "remote_url": publication.RemoteURL, "error": consultationClip(publication.Error, 500), "updated_at_ms": publication.UpdatedAtMS})
	}
	runs := make([]consultationRunObservation, 0, 12)
	for _, run := range detail.Runs[max(0, len(detail.Runs)-12):] {
		runs = append(runs, consultationRunObservation{run.ID, run.State, run.AgentID, run.ModelID, run.CreatedAtMS, run.StartedAtMS, run.FinishedAtMS, consultationClip(run.Error, 1200)})
	}
	payload := map[string]any{
		"snapshot_at":             input.At,
		"snapshot_cursor":         input.Cursor,
		"scope":                   "only persisted records; no hidden reasoning or unreported working-tree state",
		"task":                    detail.Task,
		"session_public_metadata": detail.Session,
		"development":             work.Development,
		"source_references":       work.References,
		"messages_latest":         work.Messages,
		"activities_latest":       activities,
		"artifacts_latest":        work.Artifacts,
		"reviews":                 work.Reviews,
		"review_discussion":       work.ReviewTurns,
		"test_pipelines":          pipelines,
		"platform_publications":   publications,
		"runs_latest":             runs,
		"summary":                 detail.Summary,
	}
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) <= taskConsultationSnapshotMaxBytes {
		return raw, err
	}

	// Preserve identities and status first; large artifact bodies and command
	// output are available elsewhere on the task page and are the first details
	// removed when the complete observation would crowd out the conversation.
	for i := range work.Artifacts {
		work.Artifacts[i].Content = "（内容因快照大小未附带）"
	}
	for i := range activities {
		activities[i].Output = consultationClip(activities[i].Output, 300)
		activities[i].Details = consultationClip(activities[i].Details, 200)
	}
	payload["artifacts_latest"] = work.Artifacts
	payload["activities_latest"] = activities
	payload["snapshot_truncated"] = true
	raw, err = json.Marshal(payload)
	if err != nil || len(raw) <= taskConsultationSnapshotMaxBytes {
		return raw, err
	}

	// The compact form deliberately drops native session metadata and validation
	// matrices. Those may grow with the task but are not required to answer where
	// work is, why it is blocked, or what evidence has been recorded.
	detail.Task.Goal = consultationClip(detail.Task.Goal, 3000)
	for i := range work.Messages {
		work.Messages[i].Content = consultationClip(work.Messages[i].Content, 1200)
	}
	for i := range activities {
		activities[i].Command = consultationClip(activities[i].Command, 240)
		activities[i].Details = consultationClip(activities[i].Details, 240)
		activities[i].Output = consultationClip(activities[i].Output, 600)
		activities[i].Error = consultationClip(activities[i].Error, 300)
	}
	var session any
	if detail.Session != nil {
		session = map[string]any{"session_id": detail.Session.ID, "agent_id": detail.Session.AgentID, "adapter_id": detail.Session.AdapterID, "model_id": detail.Session.ModelID, "runtime_id": detail.Session.RuntimeID, "state": detail.Session.State, "last_run_id": detail.Session.LastRunID, "updated_at_ms": detail.Session.UpdatedAtMS}
	}
	var development any
	if work.Development != nil {
		development = map[string]any{"phase": work.Development.Phase, "version": work.Development.Version, "rounds": work.Development.Rounds, "plan_run_id": work.Development.PlanRunID, "plan_hash": work.Development.PlanHash, "reviewer_task_id": work.Development.ReviewerTaskID, "approved_review_id": work.Development.ApprovedReviewID, "repository": work.Development.Repository, "base_branch": work.Development.BaseBranch}
	}
	raw, err = json.Marshal(map[string]any{
		"snapshot_at": input.At, "snapshot_cursor": input.Cursor,
		"scope": "compact persisted snapshot; large evidence omitted", "snapshot_truncated": true,
		"task": detail.Task, "session_public_metadata": session, "development": development,
		"source_references": work.References[:min(10, len(work.References))],
		"messages_latest":   work.Messages[max(0, len(work.Messages)-8):],
		"activities_latest": activities[:min(10, len(activities))], "artifacts_latest": work.Artifacts,
		"reviews": work.Reviews[max(0, len(work.Reviews)-8):], "test_pipelines": consultationPipelineSnapshot(work.TestPipelines, false),
		"platform_publications": publications, "runs_latest": runs,
	})
	if err != nil || len(raw) <= taskConsultationSnapshotMaxBytes {
		return raw, err
	}

	// A final fixed-shape projection makes the size invariant independent of any
	// future model fields. It still retains the latest human/Agent messages,
	// observable actions, review verdicts, external references and gate states.
	return marshalEssentialTaskConsultationSnapshot(input, detail.Task, work, activities, runs, publications)
}

func marshalEssentialTaskConsultationSnapshot(input consultationSnapshotInput, task model.Task, work model.WorkDetail, activities []model.Activity, runs []consultationRunObservation, publications []map[string]any) (json.RawMessage, error) {
	taskView := map[string]any{
		"task_id": task.ID, "title": consultationClip(task.Title, 500), "goal": consultationClip(task.Goal, 2400),
		"state": task.State, "version": task.Version, "assigned_agent_id": task.AssignedAgentID,
		"created_at_ms": task.CreatedAtMS, "updated_at_ms": task.UpdatedAtMS,
	}
	var session any
	if input.Detail.Session != nil {
		s := input.Detail.Session
		session = map[string]any{"session_id": s.ID, "agent_id": s.AgentID, "adapter_id": s.AdapterID, "model_id": s.ModelID, "runtime_id": s.RuntimeID, "state": s.State, "last_run_id": s.LastRunID, "updated_at_ms": s.UpdatedAtMS}
	}
	var development any
	if work.Development != nil {
		d := work.Development
		development = map[string]any{"phase": d.Phase, "version": d.Version, "rounds": d.Rounds, "plan_hash": d.PlanHash, "reviewer_task_id": d.ReviewerTaskID, "approved_review_id": d.ApprovedReviewID, "repository": d.Repository, "base_branch": d.BaseBranch}
	}
	references := make([]map[string]any, 0, min(8, len(work.References)))
	for _, ref := range work.References[:min(8, len(work.References))] {
		references = append(references, map[string]any{"kind": ref.Kind, "label": consultationClip(ref.Label, 300), "url": consultationClip(ref.URL, 1000), "revision": consultationClip(ref.Revision, 160)})
	}
	messages := append([]model.TaskMessage(nil), work.Messages[max(0, len(work.Messages)-6):]...)
	for i := range messages {
		messages[i].Content = consultationClip(messages[i].Content, 900)
	}
	actions := make([]map[string]any, 0, min(8, len(activities)))
	for _, activity := range activities[:min(8, len(activities))] {
		outputLimit := 400
		if activity.Kind == "message" {
			outputLimit = 1000
		}
		actions = append(actions, map[string]any{
			"action_id": activity.ID, "run_id": activity.RunID, "kind": activity.Kind, "state": activity.State,
			"title": consultationClip(activity.Title, 240), "command": consultationClip(activity.Command, 300),
			"details": consultationClip(activity.Details, 300), "output": consultationClip(activity.Output, outputLimit),
			"error": consultationClip(activity.Error, 300), "updated_at_ms": activity.UpdatedAtMS,
		})
	}
	artifacts := make([]map[string]any, 0, min(4, len(work.Artifacts)))
	for _, artifact := range work.Artifacts[:min(4, len(work.Artifacts))] {
		artifacts = append(artifacts, map[string]any{"artifact_id": artifact.ID, "name": consultationClip(artifact.Name, 300), "version": artifact.Version, "sha256": artifact.SHA256, "created_at_ms": artifact.CreatedAtMS, "content_omitted": true})
	}
	reviews := make([]map[string]any, 0, min(6, len(work.Reviews)))
	for _, review := range work.Reviews[max(0, len(work.Reviews)-6):] {
		reviews = append(reviews, map[string]any{"review_id": review.ID, "kind": review.Kind, "state": review.State, "plan_hash": review.PlanHash, "comment": consultationClip(review.Comment, 700), "created_at_ms": review.CreatedAtMS, "decided_at_ms": review.DecidedAtMS})
	}
	pipelines := make([]map[string]any, 0, min(8, len(input.Work.TestPipelines)))
	for _, pipeline := range input.Work.TestPipelines[:min(8, len(input.Work.TestPipelines))] {
		jobs := make([]map[string]any, 0, min(4, len(pipeline.Jobs)))
		for _, job := range pipeline.Jobs[:min(4, len(pipeline.Jobs))] {
			jobs = append(jobs, map[string]any{"id": job.ID, "name": consultationClip(job.Name, 200), "status": job.Status, "failure_reason": consultationClip(job.FailureReason, 220)})
		}
		pipelines = append(pipelines, map[string]any{
			"request_id": pipeline.ID, "state": pipeline.State, "pipeline_id": pipeline.PipelineID,
			"url": consultationClip(pipeline.URL, 1000), "head_sha": pipeline.HeadSHA, "current": pipeline.Current,
			"attempt": pipeline.Attempt, "reason": consultationClip(pipeline.Reason, 400),
			"error": consultationClip(pipeline.Error, 300), "poll_error": consultationClip(pipeline.PollError, 300),
			"failed_jobs": jobs, "updated_at_ms": pipeline.UpdatedAtMS,
		})
	}
	compactPublications := make([]map[string]any, 0, min(8, len(publications)))
	for _, publication := range publications[:min(8, len(publications))] {
		compactPublications = append(compactPublications, map[string]any{
			"platform": publication["platform"], "state": publication["state"], "verdict": publication["verdict"],
			"desired_status": publication["desired_status"], "url": consultationClip(asString(publication["url"]), 1000),
			"remote_url": consultationClip(asString(publication["remote_url"]), 1000), "error": consultationClip(asString(publication["error"]), 300),
			"updated_at_ms": publication["updated_at_ms"],
		})
	}
	raw, err := json.Marshal(map[string]any{
		"snapshot_at": input.At, "snapshot_cursor": input.Cursor,
		"scope": "essential persisted snapshot; large evidence omitted", "snapshot_truncated": true,
		"task": taskView, "session_public_metadata": session, "development": development,
		"source_references": references, "messages_latest": messages, "activities_latest": actions,
		"artifacts_latest": artifacts, "reviews": reviews, "test_pipelines": pipelines,
		"platform_publications": compactPublications, "runs_latest": runs[max(0, len(runs)-8):],
	})
	if err != nil || len(raw) <= taskConsultationSnapshotMaxBytes {
		return raw, err
	}

	// This projection is intentionally tiny and therefore cannot inherit a new,
	// unexpectedly large field when persisted models evolve.
	for i := range messages {
		messages[i].Content = consultationClip(messages[i].Content, 500)
	}
	states := make([]map[string]any, 0, len(pipelines))
	for _, pipeline := range pipelines {
		states = append(states, map[string]any{"request_id": pipeline["request_id"], "state": pipeline["state"], "pipeline_id": pipeline["pipeline_id"], "url": pipeline["url"], "current": pipeline["current"], "updated_at_ms": pipeline["updated_at_ms"]})
	}
	return json.Marshal(map[string]any{
		"snapshot_at": input.At, "snapshot_cursor": input.Cursor,
		"scope":              "minimal persisted snapshot; details omitted because the task history is large",
		"snapshot_truncated": true, "task": taskView, "source_references": references,
		"messages_latest": messages[max(0, len(messages)-3):], "activities_latest": actions[:min(5, len(actions))],
		"test_pipelines": states, "runs_latest": runs[max(0, len(runs)-5):],
	})
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}
