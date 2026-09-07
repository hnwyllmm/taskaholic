package router

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"work-assistant/internal/model"
)

// ReviewPlanner determines independent review responsibilities. It never
// receives source configuration and cannot create tasks or start executions.
// Implementations must be pure/bounded: the Manager commits the plan and tasks
// atomically. Each resulting task uses the normal (rule or AI) AgentSelector.
type ReviewPlanner interface {
	PlanReviews(ReviewRequest, []model.AgentProfile) (ReviewPlan, error)
}

type ReviewRequest struct {
	Repository    string
	AuthorAgentID string
}

type ReviewPlan struct {
	RoleIDs []string `json:"role_ids"`
	Reason  string   `json:"reason"`
}

// CapabilityReviews prefers repository specialists, one task per distinct
// role. Multiple employees sharing that role compete at assignment time.
// Generic reviewers are used only when no repository specialist is configured.
// Capacity and connectivity do not change the review scope: busy/offline
// reviewers remain queued instead of silently dropping a discipline.
type CapabilityReviews struct{}

var genericReviewAreas = []string{"architecture", "code", "delivery", "design", "performance", "product", "qa", "security", "test", "repository"}

func (CapabilityReviews) PlanReviews(request ReviewRequest, agents []model.AgentProfile) (ReviewPlan, error) {
	specialists, general := map[string]bool{}, map[string]bool{}
	for _, a := range agents {
		if a.State != "ACTIVE" || a.ID == request.AuthorAgentID {
			continue
		}
		matching, scoped, review := false, false, false
		for _, capability := range a.Role.Capabilities {
			area, ok := strings.CutSuffix(capability, ".review")
			if !ok {
				continue
			}
			review = true
			if !slices.Contains(genericReviewAreas, area) {
				scoped = true
				matching = matching || area == strings.ToLower(request.Repository)
			}
		}
		if matching {
			specialists[a.RoleID] = true
		} else if review && !scoped {
			general[a.RoleID] = true
		}
	}
	pool := specialists
	reason := "Router 根据仓库匹配 <repository>.review 能力，为每个独立评审角色创建任务；具体成员由任务路由选择。"
	if len(pool) == 0 {
		pool = general
		reason = "Router 未找到仓库专属评审成员，使用通用评审能力；具体成员由任务路由选择。"
	}
	plan := ReviewPlan{RoleIDs: []string{}, Reason: reason}
	for roleID := range pool {
		plan.RoleIDs = append(plan.RoleIDs, roleID)
	}
	sort.Strings(plan.RoleIDs)
	return plan, ValidateReviewPlan(request, plan, agents)
}

// Recheck even replaceable planners. A plugin cannot bypass author separation
// or reference a disabled/nonexistent role. No candidates is a visible pending
// routing problem, never an implicitly approved or skipped review.
func ValidateReviewPlan(request ReviewRequest, plan ReviewPlan, agents []model.AgentProfile) error {
	if len(plan.RoleIDs) == 0 {
		return fmt.Errorf("%w: Router 未找到 %s 的独立评审成员；请在成员管理中配置评审能力", model.ErrConflict, request.Repository)
	}
	if len(plan.RoleIDs) > 8 || strings.TrimSpace(plan.Reason) == "" || len(plan.Reason) > 4000 {
		return fmt.Errorf("%w: Router 评审计划必须有说明且最多包含 8 个角色", model.ErrValidation)
	}
	seen := map[string]bool{}
	for _, roleID := range plan.RoleIDs {
		found := false
		for _, a := range agents {
			if a.State == "ACTIVE" && a.ID != request.AuthorAgentID && a.RoleID == roleID {
				found = true
			}
		}
		if roleID == "" || seen[roleID] || !found {
			return fmt.Errorf("%w: Router 评审计划包含重复、无成员或非独立的角色", model.ErrValidation)
		}
		seen[roleID] = true
	}
	return nil
}
