package model

// These are read-only workbench projections, not execution or ownership state.
type SubtaskProgress struct {
	Total          int   `json:"total"`
	Completed      int   `json:"completed"`
	NeedsAttention int   `json:"needs_attention"`
	Blocked        int   `json:"blocked"`
	WaitingReview  int   `json:"waiting_review"`
	WaitingInput   int   `json:"waiting_input"`
	Superseded     int   `json:"superseded"`
	UpdatedAtMS    int64 `json:"updated_at_ms"`
}

type WorkTaskItem struct {
	Task
	Subtasks          SubtaskProgress `json:"subtasks"`
	SourceReviewState string          `json:"source_review_state,omitempty"`
	ReviewHeadSHA     string          `json:"review_head_sha,omitempty"`
}

type WorkTaskLink struct {
	ID    string `json:"task_id"`
	Title string `json:"title"`
}

type WorkHierarchy struct {
	TaskID      string         `json:"task_id"`
	TaskVersion int64          `json:"task_version"`
	Parents     []WorkTaskLink `json:"parents"`
	Roots       []WorkTaskLink `json:"roots"`
	Children    []WorkTaskItem `json:"children"`
}
