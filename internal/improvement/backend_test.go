package improvement

import (
	"encoding/json"
	"testing"

	"work-assistant/internal/model"
)

func proposalFixture(kind string, patch any) (model.CandidateProposal, model.TaskProfile) {
	profile := model.TaskProfile{RootTaskID: "task-1", SourceType: "manual", TaskType: "code", Repository: "oceanbase/seekdb", RoleID: "developer", Workflow: "standard", RuntimeOS: "linux"}
	raw, _ := json.Marshal(patch)
	return model.CandidateProposal{
		Type: kind, Title: "Use the repository build entrypoint", Rationale: "Three failed runs bypassed the supported script.",
		Scope: model.ImprovementScope{SourceType: profile.SourceType, TaskType: profile.TaskType, Repository: profile.Repository, RoleID: profile.RoleID, Workflow: profile.Workflow, RuntimeOS: profile.RuntimeOS},
		Patch: raw, Evidence: []model.ExperienceEvidence{{TaskID: profile.RootTaskID, Signal: "repeated_failure"}},
	}, profile
}

func TestValidateProposalKeepsScopeAndImmutableKernelClosed(t *testing.T) {
	proposal, profile := proposalFixture("experience", map[string]any{
		"situation": "building seekdb", "recommended_action": "use build scripts", "avoid": "guessing raw CMake flags", "verification": "run the repository script",
	})
	if err := ValidateProposal(proposal, profile); err != nil {
		t.Fatal(err)
	}
	broader := proposal
	broader.Scope.RuntimeOS = ""
	if err := ValidateProposal(broader, profile); err == nil {
		t.Fatal("candidate broadened the evidence scope")
	}
	forbidden, source := proposalFixture("prompt", map[string]any{"overlay": "follow the evidence", "permission_override": true})
	if err := ValidateProposal(forbidden, source); err == nil {
		t.Fatal("candidate changed the immutable permission kernel")
	}
	workflow, source := proposalFixture("workflow", model.WorkflowPolicy{RequireAgentPlanReview: true, RequireHumanPlanReview: false, RequirePRReview: true, RequirePassingTests: true, RequireHumanAcceptance: true})
	if err := ValidateProposal(workflow, source); err == nil {
		t.Fatal("code workflow bypassed human plan approval")
	}
	legalWorkflow, source := proposalFixture("workflow", model.WorkflowPolicy{RequireAgentPlanReview: true, RequireHumanPlanReview: true, RequirePRReview: true, RequirePassingTests: true, RequireHumanAcceptance: true, PlanReviewRoundLimit: 6})
	if err := ValidateProposal(legalWorkflow, source); err != nil {
		t.Fatalf("legal bounded workflow threshold rejected: %v", err)
	}
	prompt, source := proposalFixture("prompt", model.PromptPolicy{RoleID: "developer", Stage: "development", Overlay: "continue until there is evidence"})
	if err := ValidateProposal(prompt, source); err != nil {
		t.Fatalf("role/stage scoped prompt rejected: %v", err)
	}
	prompt.Patch, _ = json.Marshal(model.PromptPolicy{RoleID: "reviewer", Stage: "development", Overlay: "wrong role"})
	if err := ValidateProposal(prompt, source); err == nil {
		t.Fatal("prompt escaped its evidence role")
	}
	prompt.Patch, _ = json.Marshal(model.PromptPolicy{RoleID: "developer", Stage: "arbitrary_state", Overlay: "wrong stage"})
	if err := ValidateProposal(prompt, source); err == nil {
		t.Fatal("prompt introduced an arbitrary workflow stage")
	}
}

func TestValidateExperienceAddReviseAndRetireShapes(t *testing.T) {
	for _, patch := range []model.ExperiencePatch{
		{Operation: "add", Situation: "building", Action: "use the supported script", Verification: "run the script"},
		{Operation: "revise", ExperienceID: "experience-1", ExpectedRevision: 3, Situation: "building", Action: "use the supported script", Verification: "run the script"},
		{Operation: "retire", ExperienceID: "experience-1", ExpectedRevision: 3},
	} {
		proposal, profile := proposalFixture("experience", patch)
		if err := ValidateProposal(proposal, profile); err != nil {
			t.Fatalf("valid %s experience patch rejected: %v", patch.Operation, err)
		}
	}
	bad, profile := proposalFixture("experience", model.ExperiencePatch{Operation: "revise", ExperienceID: "experience-1", ExpectedRevision: 0, Situation: "building", Action: "guess", Verification: "none"})
	if err := ValidateProposal(bad, profile); err == nil {
		t.Fatal("revision without an immutable expected revision was accepted")
	}
}

func TestProposalEvidenceMustComeFromDeterministicObservation(t *testing.T) {
	proposal, profile := proposalFixture("experience", map[string]any{
		"situation": "building seekdb", "recommended_action": "use build scripts", "avoid": "raw flags", "verification": "run supported script",
	})
	observation := model.ImprovementObservation{RootTaskID: profile.RootTaskID, Signals: []model.TaskEfficiencySignal{{Code: "repeated_failure"}}}
	if err := ValidateProposalEvidence(proposal, observation); err != nil {
		t.Fatal(err)
	}
	proposal.Evidence[0].Signal = "agent_says_this_is_better"
	if err := ValidateProposalEvidence(proposal, observation); err == nil {
		t.Fatal("unobserved Agent claim validated a candidate")
	}
	proposal.Evidence = nil
	if err := ValidateProposal(proposal, profile); err == nil {
		t.Fatal("candidate without evidence was accepted")
	}
}

func TestCompositeScoreUsesMeansMediansAndNeverInventsMissingTokens(t *testing.T) {
	tokens := func(v int64) *int64 { return &v }
	baseline := []model.TaskEvaluation{
		{QualityScore: 70, HumanInterventions: 10, CycleTimeMS: 10_000, TotalTokens: tokens(1000), NoProgressRuns: 5},
		{QualityScore: 90, HumanInterventions: 20, CycleTimeMS: 20_000, TotalTokens: tokens(1000), NoProgressRuns: 7},
	}
	candidate := []model.TaskEvaluation{
		{QualityScore: 90, HumanInterventions: 5, CycleTimeMS: 5_000, TotalTokens: tokens(500), NoProgressRuns: 2},
		{QualityScore: 100, HumanInterventions: 8, CycleTimeMS: 8_000, TotalTokens: tokens(500), NoProgressRuns: 3},
	}
	score, ok := CompositeScore(baseline, candidate)
	if !ok || score.QualityChange != 15 || score.TokenChange != 50 || score.Composite <= 8 {
		t.Fatalf("unexpected score: %#v, ok=%v", score, ok)
	}
	baseline[0].TotalTokens = nil
	withoutTokens, ok := CompositeScore(baseline, candidate)
	if !ok || withoutTokens.TokenChange != 0 || withoutTokens.Composite <= 8 {
		t.Fatalf("missing tokens were treated as zero cost: %#v", withoutTokens)
	}
}
