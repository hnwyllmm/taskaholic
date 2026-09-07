package router

import (
	"testing"
	"work-assistant/internal/model"
)

func TestPlanReviewerPrefersIndependentDesignSpecialist(t *testing.T) {
	a := func(id, role string, caps ...string) model.AgentProfile {
		return model.AgentProfile{ID: id, RoleID: role, State: "ACTIVE", Role: model.Role{RoleSpec: model.RoleSpec{Capabilities: caps}}}
	}
	agents := []model.AgentProfile{a("author", "self", "design.review", "seekdb.review"), a("generic", "general", "design.review"), a("qa", "qa", "qa.review", "seekdb.review"), a("design", "specialist", "design.review", "seekdb.review"), a("other", "other", "design.review", "different.review")}
	got, err := (PlanReviewer{}).Select("oceanbase/seekdb", "author", agents)
	if err != nil || got != "specialist" {
		t.Fatal(got, err)
	}
	if _, err = (PlanReviewer{}).Select("oceanbase/seekdb", "author", agents[:1]); err == nil {
		t.Fatal("self-review accepted")
	}
}
