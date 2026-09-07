package model

import "fmt"

// This policy is intentionally narrow. Additional test suites should be
// registered as actions, not arbitrary URLs, branches, or shell commands.
const (
	SeekDBTestKind    = "seekdb_regression"
	SeekDBTestSource  = "gitlab-seekdb-tests"
	SeekDBTestHost    = "https://gitlab.oceanbase-dev.com"
	SeekDBTestProject = "obqa/seekdb_test"
	SeekDBTestRef     = "master-pipeline"
)

type TestPipeline struct {
	ID                string        `json:"request_id"`
	TaskID            string        `json:"task_id"`
	RequestedByTaskID string        `json:"requested_by_task_id"`
	RunID             string        `json:"run_id"`
	PRTargetID        string        `json:"pr_target_id"`
	PRURL             string        `json:"pr_url"`
	HeadSHA           string        `json:"head_sha"`
	Kind              string        `json:"kind"`
	Reason            string        `json:"reason"`
	RetryOf           string        `json:"retry_of,omitempty"`
	Attempt           int           `json:"attempt"`
	State             string        `json:"state"`
	PipelineID        int64         `json:"pipeline_id,omitempty"`
	URL               string        `json:"url,omitempty"`
	ConfigSHA         string        `json:"config_sha,omitempty"`
	PollTargetID      string        `json:"poll_target_id,omitempty"`
	Error             string        `json:"error,omitempty"`
	Jobs              []PipelineJob `json:"failed_jobs,omitempty"`
	CreatedAtMS       int64         `json:"created_at_ms"`
	SubmittedAtMS     int64         `json:"submitted_at_ms,omitempty"`
	UpdatedAtMS       int64         `json:"updated_at_ms"`
	NextAttemptMS     int64         `json:"next_attempt_ms,omitempty"`
	// Filled at read time, not a persisted assertion about the latest PR.
	Current      bool   `json:"current"`
	PollError    string `json:"poll_error,omitempty"`
	NextPollMS   int64  `json:"next_poll_ms,omitempty"`
	LastPolledMS int64  `json:"last_polled_ms,omitempty"`
}

type PipelineJob struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
	URL           string `json:"web_url"`
}

type PipelineObservation struct {
	ID     int64         `json:"id"`
	Status string        `json:"status"`
	Ref    string        `json:"ref"`
	SHA    string        `json:"sha"`
	URL    string        `json:"web_url"`
	Jobs   []PipelineJob `json:"failed_jobs,omitempty"`
}

func PipelineURL(id int64) string {
	return fmt.Sprintf("%s/%s/-/pipelines/%d", SeekDBTestHost, SeekDBTestProject, id)
}

func PipelineFinished(status string) bool {
	switch status {
	case "success", "failed", "canceled", "skipped":
		return true
	}
	return false
}

func PipelineRetryable(status string) bool {
	return status == "ERROR" || (PipelineFinished(status) && status != "success")
}
