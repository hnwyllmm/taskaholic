package router

import (
	"reflect"
	"testing"

	"work-assistant/internal/model"
)

func reviewAgent(id, role string, caps ...string) model.AgentProfile {
	return model.AgentProfile{ID: id, RoleID: role, State: "ACTIVE", MaxConcurrent: 1,
		Role: model.Role{ID: role, RoleSpec: model.RoleSpec{Capabilities: caps}}}
}

func TestReviewPlannerUsesCapabilitiesNotSourceOrMachine(t *testing.T) {
	agents := []model.AgentProfile{
		reviewAgent("author", "dev", "seekdb.develop"),
		reviewAgent("architecture", "architecture", "architecture.review", "seekdb.review", "seekdb-bindings.review"),
		reviewAgent("qa", "qa", "qa.review", "seekdb.review", "seekdb-bindings.review"),
		reviewAgent("code", "code", "code.review", "seekdb.review", "seekdb-bindings.review"),
		reviewAgent("same-role-another-machine", "code", "code.review", "seekdb.review"),
		reviewAgent("other-project", "other", "code.review", "other.review"),
		reviewAgent("generic", "generic", "code.review"),
		reviewAgent("disabled", "disabled", "seekdb.review"),
	}
	agents[1].ActiveRuns = 1 // All busy/offline specialists must still be planned.
	agents[2].RuntimeID = "offline"
	agents[7].State = "DISABLED"
	for _, repo := range []string{"seekdb", "seekdb-bindings"} {
		plan, err := (CapabilityReviews{}).PlanReviews(ReviewRequest{Repository: repo, AuthorAgentID: "author"}, agents)
		if err != nil || !reflect.DeepEqual(plan.RoleIDs, []string{"architecture", "code", "qa"}) {
			t.Fatal("scope, author, role grouping or capacity changed review responsibilities", plan, err)
		}
	}
	plan, err := (CapabilityReviews{}).PlanReviews(ReviewRequest{Repository: "unknown", AuthorAgentID: "author"}, agents)
	if err != nil || !reflect.DeepEqual(plan.RoleIDs, []string{"generic"}) {
		t.Fatal("unrelated specialists should not review an unknown repository", plan, err)
	}
}

func TestReviewPlannerRejectsNoIndependentCandidateAndOversizedOrInvalidPlans(t *testing.T) {
	request := ReviewRequest{Repository: "seekdb", AuthorAgentID: "author"}
	agents := []model.AgentProfile{reviewAgent("author", "author-role", "code.review"), reviewAgent("reviewer", "reviewer-role", "qa.review")}
	for _, ids := range [][]string{nil, {"author-role"}, {"missing"}, {"reviewer-role", "reviewer-role"}} {
		if err := ValidateReviewPlan(request, ReviewPlan{RoleIDs: ids, Reason: "test"}, agents); err == nil {
			t.Fatal("invalid plan accepted", ids)
		}
	}
	if _, err := (CapabilityReviews{}).PlanReviews(request, agents[:1]); err == nil {
		t.Fatal("author-only review silently skipped")
	}
}
