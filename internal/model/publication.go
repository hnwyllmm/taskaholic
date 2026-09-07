package model

import (
	"fmt"
	"strings"
)

func ParseGitHubIssue(raw string) (owner, repo string, number int, canonical string, err error) {
	if strings.Count(raw, "/issues/") != 1 {
		err = fmt.Errorf("%w: invalid GitHub issue URL", ErrValidation)
		return
	}
	owner, repo, number, canonical, err = ParseGitHubPR(strings.Replace(raw, "/issues/", "/pull/", 1))
	canonical = strings.Replace(canonical, "/pull/", "/issues/", 1)
	if err == nil && canonical != raw {
		err = fmt.Errorf("%w: non-canonical GitHub issue URL", ErrValidation)
	}
	return
}

// Publication is a durable, restricted platform write, not an arbitrary HTTP
// request from an Agent. Key identifies one sticky review or one milestone.
type Publication struct {
	Key            string `json:"key"`
	TaskID         string `json:"task_id"`
	Platform       string `json:"platform"`
	URL            string `json:"url"`
	WorkspaceID    string `json:"workspace_id,omitempty"`
	IssueID        string `json:"issue_id,omitempty"`
	ActorID        string `json:"actor_id,omitempty"`
	HeadSHA        string `json:"head_sha,omitempty"`
	ReportHash     string `json:"report_hash,omitempty"`
	Verdict        string `json:"verdict,omitempty"`
	Body           string `json:"body"`
	Sticky         bool   `json:"sticky"`
	DesiredStatus  string `json:"desired_status,omitempty"`
	Version        int64  `json:"version"`
	AppliedVersion int64  `json:"applied_version"`
	RemoteID       string `json:"remote_id,omitempty"`
	RemoteURL      string `json:"remote_url,omitempty"`
	AppliedBody    string `json:"applied_body,omitempty"`
	State          string `json:"state"`
	Error          string `json:"error,omitempty"`
	NextAttemptMS  int64  `json:"next_attempt_ms"`
	UpdatedAtMS    int64  `json:"updated_at_ms"`
}
