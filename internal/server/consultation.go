package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"work-assistant/internal/model"
)

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
	clip := func(value string, size int) string {
		clean, _, _ := model.ObservationText(value, size)
		return clean
	}
	detail.Task.Goal = clip(detail.Task.Goal, 6000)
	if len(work.Messages) > 15 {
		work.Messages = work.Messages[len(work.Messages)-15:]
	}
	for i := range work.Messages {
		work.Messages[i].Content = clip(work.Messages[i].Content, 2400)
	}
	if len(work.Artifacts) > 4 {
		work.Artifacts = work.Artifacts[:4]
	}
	for i := range work.Artifacts {
		work.Artifacts[i].Content = clip(work.Artifacts[i].Content, 2500)
	}
	if len(activities.Items) > 20 {
		activities.Items = activities.Items[:20]
	}
	for i := range activities.Items {
		activities.Items[i].Command = clip(activities.Items[i].Command, 500)
		activities.Items[i].Details = clip(activities.Items[i].Details, 600)
		activities.Items[i].Output = clip(activities.Items[i].Output, 1200)
		activities.Items[i].Error = clip(activities.Items[i].Error, 500)
	}
	if len(work.Reviews) > 12 {
		work.Reviews = work.Reviews[len(work.Reviews)-12:]
	}
	for i := range work.Reviews {
		work.Reviews[i].Comment = clip(work.Reviews[i].Comment, 1000)
	}
	if len(work.ReviewTurns) > 12 {
		work.ReviewTurns = work.ReviewTurns[len(work.ReviewTurns)-12:]
	}
	for i := range work.ReviewTurns {
		work.ReviewTurns[i].Question = clip(work.ReviewTurns[i].Question, 900)
		work.ReviewTurns[i].Answer = clip(work.ReviewTurns[i].Answer, 1200)
		work.ReviewTurns[i].Error = clip(work.ReviewTurns[i].Error, 500)
	}
	if len(work.TestPipelines) > 10 {
		work.TestPipelines = work.TestPipelines[:10]
	}
	for i := range work.TestPipelines {
		work.TestPipelines[i].Reason = clip(work.TestPipelines[i].Reason, 800)
		work.TestPipelines[i].Error = clip(work.TestPipelines[i].Error, 500)
		work.TestPipelines[i].PollError = clip(work.TestPipelines[i].PollError, 500)
	}
	publications := make([]map[string]any, 0, min(10, len(work.Publications)))
	for _, publication := range work.Publications[:min(10, len(work.Publications))] {
		publications = append(publications, map[string]any{"platform": publication.Platform, "url": publication.URL, "state": publication.State, "verdict": publication.Verdict, "desired_status": publication.DesiredStatus, "remote_url": publication.RemoteURL, "error": clip(publication.Error, 500), "updated_at_ms": publication.UpdatedAtMS})
	}
	type runObservation struct {
		ID           string `json:"run_id"`
		State        string `json:"state"`
		AgentID      string `json:"agent_id"`
		ModelID      string `json:"model_id,omitempty"`
		CreatedAtMS  int64  `json:"created_at_ms"`
		StartedAtMS  *int64 `json:"started_at_ms,omitempty"`
		FinishedAtMS *int64 `json:"finished_at_ms,omitempty"`
		Error        string `json:"error,omitempty"`
	}
	runs := make([]runObservation, 0, 12)
	start := 0
	if len(detail.Runs) > 12 {
		start = len(detail.Runs) - 12
	}
	for _, run := range detail.Runs[start:] {
		runs = append(runs, runObservation{run.ID, run.State, run.AgentID, run.ModelID, run.CreatedAtMS, run.StartedAtMS, run.FinishedAtMS, clip(run.Error, 1200)})
	}
	payload := map[string]any{
		"snapshot_at":             time.Now().Format(time.RFC3339),
		"snapshot_cursor":         activities.Cursor,
		"scope":                   "only persisted records; no hidden reasoning or unreported working-tree state",
		"task":                    detail.Task,
		"session_public_metadata": detail.Session,
		"development":             work.Development,
		"source_references":       work.References,
		"messages_latest":         work.Messages,
		"activities_latest":       activities.Items,
		"artifacts_latest":        work.Artifacts,
		"reviews":                 work.Reviews,
		"review_discussion":       work.ReviewTurns,
		"test_pipelines":          work.TestPipelines,
		"platform_publications":   publications,
		"runs_latest":             runs,
		"summary":                 detail.Summary,
	}
	raw, err := json.Marshal(payload)
	if err == nil && len(raw) > 78000 {
		for i := range work.Artifacts {
			work.Artifacts[i].Content = "（内容因快照大小未附带）"
		}
		for i := range activities.Items {
			activities.Items[i].Output = clip(activities.Items[i].Output, 300)
			activities.Items[i].Details = clip(activities.Items[i].Details, 200)
		}
		payload["artifacts_latest"] = work.Artifacts
		payload["activities_latest"] = activities.Items
		payload["snapshot_truncated"] = true
		raw, err = json.Marshal(payload)
	}
	if err == nil && len(raw) > 78000 {
		detail.Task.Goal = clip(detail.Task.Goal, 3000)
		for i := range activities.Items {
			activities.Items[i].Command, activities.Items[i].Details, activities.Items[i].Output = "", "", ""
		}
		raw, err = json.Marshal(map[string]any{
			"snapshot_at": time.Now().Format(time.RFC3339), "snapshot_cursor": activities.Cursor,
			"scope": "compact persisted snapshot; large evidence omitted", "snapshot_truncated": true,
			"task": detail.Task, "session_public_metadata": detail.Session, "development": work.Development,
			"source_references": work.References, "messages_latest": work.Messages[max(0, len(work.Messages)-8):],
			"activities_latest": activities.Items[:min(10, len(activities.Items))], "artifacts_latest": work.Artifacts,
			"reviews": work.Reviews, "test_pipelines": work.TestPipelines, "runs_latest": runs,
		})
	}
	return raw, activities.Cursor, err
}
