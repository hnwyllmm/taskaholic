package model

import (
	"fmt"
	"regexp"
	"strings"
)

// Development is a durable lifecycle, not native session memory. Every plan
// revision and human decision remains in events/artifacts after a restart.
type Development struct {
	TaskID           string `json:"task_id"`
	Phase            string `json:"phase"`
	Version          int64  `json:"version"`
	Rounds           int    `json:"rounds"`
	PlanRunID        string `json:"plan_run_id"`
	PlanHash         string `json:"plan_hash"`
	ReviewerTaskID   string `json:"reviewer_task_id"`
	ReviewerRunID    string `json:"reviewer_run_id"`
	ApprovedReviewID string `json:"approved_review_id"`
	Repository       string `json:"repository"`
	BaseBranch       string `json:"base_branch"`
}

// ExecutionGrant is issued only by the Manager after a version-bound human
// approval. It never comes directly from an Agent or an external task source.
type ExecutionGrant struct {
	ReviewID   string `json:"review_id"`
	PlanHash   string `json:"plan_hash"`
	Repository string `json:"repository"`
	BaseBranch string `json:"base_branch"`
}

var githubRepository = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var branchName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]{0,199}$`)

func ValidateDevelopmentRepository(repository, branch string) error {
	if !githubRepository.MatchString(repository) || strings.HasSuffix(repository, ".git") || !branchName.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, ".lock") {
		return fmt.Errorf("%w: development requires GitHub owner/repository and an explicit base branch", ErrValidation)
	}
	return nil
}
