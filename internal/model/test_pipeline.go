package model

import (
	"fmt"
	"strings"
)

// This policy is intentionally narrow. Additional test suites should be
// registered as actions, not arbitrary URLs, branches, or shell commands.
const (
	SeekDBTestKind    = "seekdb_regression"
	SeekDBTestSource  = "gitlab-seekdb-tests"
	SeekDBTestHost    = "https://gitlab.oceanbase-dev.com"
	SeekDBTestProject = "obqa/seekdb_test"
	SeekDBTestRef     = "master-pipeline"

	// A small non-mysqltest failure fan-out is commonly caused by flaky workers
	// or shared infrastructure. Three or fewer are eligible for an Agent to
	// request an in-place retry after it has inspected the evidence. The
	// Manager never starts that retry by itself.
	PipelineQuickRetryMaxJobs = 3
	// This is a limit on Agent-approved retries of the same failed job set, not
	// a limit on the Agent's normal repair/recovery turns.
	PipelineQuickRetryLimit = 2
)

type TestPipeline struct {
	ID                 string                     `json:"request_id"`
	TaskID             string                     `json:"task_id"`
	RequestedByTaskID  string                     `json:"requested_by_task_id"`
	RunID              string                     `json:"run_id"`
	PRTargetID         string                     `json:"pr_target_id"`
	PRURL              string                     `json:"pr_url"`
	HeadSHA            string                     `json:"head_sha"`
	Kind               string                     `json:"kind"`
	Reason             string                     `json:"reason"`
	RetryOf            string                     `json:"retry_of,omitempty"`
	Attempt            int                        `json:"attempt"`
	State              string                     `json:"state"`
	PipelineID         int64                      `json:"pipeline_id,omitempty"`
	URL                string                     `json:"url,omitempty"`
	ConfigSHA          string                     `json:"config_sha,omitempty"`
	PollTargetID       string                     `json:"poll_target_id,omitempty"`
	Error              string                     `json:"error,omitempty"`
	EvidenceError      string                     `json:"evidence_error,omitempty"`
	Jobs               []PipelineJob              `json:"failed_jobs,omitempty"`
	FailureSummary     PipelineFailureSummary     `json:"failure_summary,omitempty"`
	FailureAssessment  *PipelineFailureAssessment `json:"failure_assessment,omitempty"`
	QuickRetries       int                        `json:"quick_retries,omitempty"`
	RetrySubmittedAtMS int64                      `json:"retry_submitted_at_ms,omitempty"`
	AnalysisRequired   bool                       `json:"analysis_required,omitempty"`
	AnalysisDelivered  bool                       `json:"analysis_delivered,omitempty"`
	FailureHistory     []PipelineFailure          `json:"failure_history,omitempty"`
	CreatedAtMS        int64                      `json:"created_at_ms"`
	SubmittedAtMS      int64                      `json:"submitted_at_ms,omitempty"`
	UpdatedAtMS        int64                      `json:"updated_at_ms"`
	NextAttemptMS      int64                      `json:"next_attempt_ms,omitempty"`
	// Filled at read time, not a persisted assertion about the latest PR.
	Current      bool   `json:"current"`
	PollError    string `json:"poll_error,omitempty"`
	NextPollMS   int64  `json:"next_poll_ms,omitempty"`
	LastPolledMS int64  `json:"last_polled_ms,omitempty"`
}

// PipelineFailure is intentionally compact. Full, redacted job excerpts stay
// on the latest observation while this history lets the work Agent see whether
// failures persist or move between jobs across automatic retries.
type PipelineFailure struct {
	Retry          int                        `json:"retry"`
	Fingerprint    string                     `json:"fingerprint"`
	JobIDs         []int64                    `json:"job_ids"`
	JobNames       []string                   `json:"job_names"`
	Jobs           []PipelineFailureJob       `json:"jobs"`
	FailureSummary PipelineFailureSummary     `json:"failure_summary,omitempty"`
	Assessment     *PipelineFailureAssessment `json:"assessment,omitempty"`
	ObservedAtMS   int64                      `json:"observed_at_ms"`
}

type PipelineFailureJob struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	FailureReason string `json:"failure_reason,omitempty"`
	LogExcerpt    string `json:"log_excerpt,omitempty"`
}

type PipelineJob struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	FailureReason   string `json:"failure_reason,omitempty"`
	URL             string `json:"web_url"`
	LogExcerpt      string `json:"log_excerpt,omitempty"`
	LogTruncated    bool   `json:"log_truncated,omitempty"`
	LogCollected    bool   `json:"log_collected,omitempty"`
	LogCollectError string `json:"log_collect_error,omitempty"`
}

// PipelineFailureSummary is produced by the trusted GitLab reader. It keeps
// accurate counts even when only a bounded job-detail/log subset is stored in
// the task record. CollectionComplete is false when the reader could not
// enumerate every failed job, in which case no automatic retry is permitted.
type PipelineFailureSummary struct {
	TotalFailures        int    `json:"total_failures,omitempty"`
	MySQLTestFailures    int    `json:"mysqltest_failures,omitempty"`
	NonMySQLTestFailures int    `json:"non_mysqltest_failures,omitempty"`
	CollectionComplete   bool   `json:"collection_complete,omitempty"`
	DetailTruncated      bool   `json:"detail_truncated,omitempty"`
	CollectionError      string `json:"collection_error,omitempty"`
	// MySQLTestDetailsKnown distinguishes records written before per-mysqltest
	// trace collection existed from a fresh collection that has no mysqltest
	// failures. Complete is true only when every mysqltest failed job is present
	// in Jobs and has a trace collection attempt.
	MySQLTestDetailsKnown    bool `json:"mysqltest_details_known,omitempty"`
	MySQLTestDetailsComplete bool `json:"mysqltest_details_complete,omitempty"`
}

// MySQLTestJobAssessment is the Agent's result after reading a mysqltest job's
// trusted, redacted log. FailedCaseCount counts test cases, not GitLab jobs.
type MySQLTestJobAssessment struct {
	JobID           int64    `json:"job_id"`
	JobName         string   `json:"job_name"`
	FailedCaseCount int      `json:"failed_case_count"`
	FailedCaseNames []string `json:"failed_case_names,omitempty"`
}

// PipelineFailureAssessment is the original development Agent's bounded,
// auditable conclusion for a failed regression. The Agent, rather than the
// Manager, decides whether the evidence warrants a retry. The Manager only
// validates that request and performs the constrained original-pipeline retry.
type PipelineFailureAssessment struct {
	Relation           string                   `json:"relation"`
	Decision           string                   `json:"decision"`
	Reason             string                   `json:"reason"`
	Evidence           string                   `json:"evidence"`
	MySQLTestCaseCount int                      `json:"mysqltest_case_count,omitempty"`
	MySQLTestJobs      []MySQLTestJobAssessment `json:"mysqltest_jobs,omitempty"`
	RunID              string                   `json:"run_id"`
	RecordedAtMS       int64                    `json:"recorded_at_ms"`
}

type PipelineObservation struct {
	ID             int64                  `json:"id"`
	Status         string                 `json:"status"`
	Ref            string                 `json:"ref"`
	SHA            string                 `json:"sha"`
	URL            string                 `json:"web_url"`
	Jobs           []PipelineJob          `json:"failed_jobs,omitempty"`
	FailureSummary PipelineFailureSummary `json:"failure_summary,omitempty"`
}

func IsMySQLTestPipelineJob(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"mysqltest", "mysql-test", "mysql_test", "mysql test"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// SummarizePipelineFailures is also used for old persisted records that were
// created before the reader reported a separate full failure count. A synthetic
// or incomplete job item makes the inferred summary ineligible for retry.
func SummarizePipelineFailures(jobs []PipelineJob) PipelineFailureSummary {
	summary := PipelineFailureSummary{CollectionComplete: len(jobs) > 0}
	for _, job := range jobs {
		if job.ID <= 0 || (job.Status != "failed" && job.Status != "canceled") {
			summary.CollectionComplete = false
			continue
		}
		summary.TotalFailures++
		if IsMySQLTestPipelineJob(job.Name) {
			summary.MySQLTestFailures++
		} else {
			summary.NonMySQLTestFailures++
		}
	}
	if summary.TotalFailures == 0 {
		summary.CollectionComplete = false
	}
	return summary
}

// EffectivePipelineFailureSummary preserves reader-supplied full counts. It
// only infers a summary for older records that did not contain one.
func EffectivePipelineFailureSummary(summary PipelineFailureSummary, jobs []PipelineJob) PipelineFailureSummary {
	if PipelineFailureSummaryKnown(summary) {
		return summary
	}
	return SummarizePipelineFailures(jobs)
}

func PipelineFailureSummaryKnown(summary PipelineFailureSummary) bool {
	return summary.TotalFailures > 0 || summary.CollectionComplete || summary.CollectionError != "" || summary.DetailTruncated || summary.MySQLTestDetailsKnown
}

func (s PipelineFailureSummary) AllowsQuickRetry() bool {
	return s.CollectionComplete && s.MySQLTestFailures == 0 && s.NonMySQLTestFailures > 0 && s.NonMySQLTestFailures <= PipelineQuickRetryMaxJobs && s.TotalFailures == s.NonMySQLTestFailures
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

// PipelineFailureEvidenceReady distinguishes a terminal pipeline status from
// actionable failure evidence. Work Agents never receive the GitLab token;
// they resume only after the trusted client has attempted every real failed
// job and persisted a bounded, redacted log excerpt (or a terminal reason why
// that specific trace is unavailable).
func PipelineFailureEvidenceReady(jobs []PipelineJob) bool {
	if len(jobs) == 0 {
		return false
	}
	for _, job := range jobs {
		if !job.LogCollected {
			return false
		}
	}
	return true
}
