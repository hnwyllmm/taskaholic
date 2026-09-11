// Package improvement contains the replaceable analysis boundary and the
// deterministic rules that remain owned by the local Manager.
package improvement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"work-assistant/internal/model"
)

// ImprovementBackend may be implemented by local system Agents or a future
// provider. It
// cannot assign experiments, promote a candidate, or change safety settings;
// those decisions always remain in the Manager/store layer.
type ImprovementBackend interface {
	Analyze(context.Context, model.ImprovementObservation) ([]model.CandidateProposal, error)
	Recall(context.Context, model.TaskProfile) ([]model.Experience, error)
	Judge(context.Context, model.EvaluationInput) (model.SemanticJudgement, error)
}

// Backend keeps the short pre-v22 name source-compatible for any local
// provider prototype while the public extension point uses the design name.
type Backend = ImprovementBackend

var allowedCandidateTypes = map[string]bool{
	"experience": true, "prompt": true, "routing": true,
	"recovery": true, "test": true, "workflow": true,
}

// ValidateProposal is intentionally strict. Candidate patches are typed data,
// never executable source, arbitrary JSON state transitions, or credentials.
func ValidateProposal(p model.CandidateProposal, source model.TaskProfile) error {
	if !allowedCandidateTypes[p.Type] {
		return fmt.Errorf("unsupported candidate type %q", p.Type)
	}
	if strings.TrimSpace(p.Title) == "" || len(p.Title) > 240 || strings.TrimSpace(p.Rationale) == "" || len(p.Rationale) > 4000 {
		return errors.New("candidate title and rationale are required")
	}
	if len(p.Patch) == 0 || len(p.Patch) > 32*1024 || !json.Valid(p.Patch) {
		return errors.New("candidate patch must be valid JSON up to 32 KiB")
	}
	if !scopeWithin(p.Scope, source) {
		return errors.New("candidate scope is broader than its source evidence")
	}
	if len(p.Evidence) == 0 || len(p.Evidence) > 20 {
		return errors.New("candidate requires 1..20 evidence references")
	}
	for _, evidence := range p.Evidence {
		if evidence.TaskID != source.RootTaskID || (strings.TrimSpace(evidence.Signal) == "" && strings.TrimSpace(evidence.RunID) == "" && strings.TrimSpace(evidence.Review) == "") || len(evidence.RunID) > 240 || len(evidence.Review) > 240 || len(evidence.Signal) > 240 {
			return errors.New("candidate evidence must reference its source task and a bounded signal, run, or review")
		}
	}
	var raw any
	if err := json.Unmarshal(p.Patch, &raw); err != nil {
		return err
	}
	if forbiddenField(raw) != "" {
		return fmt.Errorf("candidate changes immutable field %q", forbiddenField(raw))
	}
	switch p.Type {
	case "experience":
		var patch model.ExperiencePatch
		if err := strictJSON(p.Patch, &patch); err != nil {
			return err
		}
		if patch.Operation == "" {
			patch.Operation = "add"
		}
		if !oneOf(patch.Operation, "add", "revise", "retire") {
			return errors.New("experience operation must be add, revise, or retire")
		}
		if patch.Operation == "add" && (patch.ExperienceID != "" || patch.ExpectedRevision != 0) {
			return errors.New("new experience cannot name an existing revision")
		}
		if patch.Operation != "add" && (strings.TrimSpace(patch.ExperienceID) == "" || patch.ExpectedRevision < 1) {
			return errors.New("experience revision or retirement requires experience_id and expected_revision")
		}
		if patch.Operation != "retire" && (strings.TrimSpace(patch.Situation) == "" || strings.TrimSpace(patch.Action) == "" || strings.TrimSpace(patch.Verification) == "") {
			return errors.New("experience add or revision requires situation, recommended_action and verification")
		}
		if patch.Operation == "retire" && (patch.Situation != "" || patch.Action != "" || patch.Avoid != "" || patch.Verification != "") {
			return errors.New("experience retirement cannot smuggle replacement content")
		}
	case "prompt":
		var patch model.PromptPolicy
		if err := strictJSON(p.Patch, &patch); err != nil || strings.TrimSpace(patch.Overlay) == "" || len(patch.Overlay) > 6000 {
			return errors.New("prompt patch requires a non-empty overlay up to 6 KiB")
		}
		if patch.RoleID != "" && patch.RoleID != p.Scope.RoleID {
			return errors.New("prompt role must match the evidence scope")
		}
		if patch.Stage != "" && !oneOf(patch.Stage, "planning", "development", "review", "acceptance", "testing", "consultation", "other") {
			return errors.New("prompt stage is outside the predefined workflow stages")
		}
	case "routing":
		var patch model.RoutingPolicy
		if err := strictJSON(p.Patch, &patch); err != nil {
			return err
		}
		if !weight(patch.LoadWeight) || !weight(patch.SuccessWeight) || !weight(patch.CostWeight) || patch.LoadWeight+patch.SuccessWeight+patch.CostWeight != 100 {
			return errors.New("routing weights must be 0..100 and total 100")
		}
	case "recovery":
		var patch model.RecoveryPolicy
		if err := strictJSON(p.Patch, &patch); err != nil {
			return err
		}
		if patch.BackoffMS < 0 || patch.BackoffMS > 3_600_000 || patch.SplitEnvironmentAfter < 0 || patch.SplitEnvironmentAfter > 20 {
			return errors.New("recovery policy values are outside safe bounds")
		}
	case "test":
		var patch model.TestPolicy
		if err := strictJSON(p.Patch, &patch); err != nil {
			return err
		}
		if !oneOf(patch.RiskMode, "changed_scope", "full", "risk_based") || !oneOf(patch.RetestMode, "affected", "failed", "full") || patch.ReviewerCount < 1 || patch.ReviewerCount > 5 {
			return errors.New("test policy is outside the predefined state space")
		}
	case "workflow":
		var patch model.WorkflowPolicy
		if err := strictJSON(p.Patch, &patch); err != nil {
			return err
		}
		// The current legal graph always retains these gates. The only mutable
		// field in v22 is a bounded review-loop threshold; adding another legal
		// transition requires a future schema/code migration, not Agent JSON.
		if !patch.RequireAgentPlanReview || !patch.RequireHumanPlanReview || !patch.RequirePRReview || !patch.RequirePassingTests || !patch.RequireHumanAcceptance {
			return errors.New("workflow policy cannot bypass required workflow gates")
		}
		if patch.PlanReviewRoundLimit < 2 || patch.PlanReviewRoundLimit > 20 {
			return errors.New("workflow plan review round limit must be between 2 and 20")
		}
	}
	return nil
}

// ValidateProposalEvidence binds an Analyst proposal to facts in the
// Observation it actually received. Agent-authored learnings can inspire an
// explanation, but they cannot validate a candidate without one of these
// deterministic signals or the immutable completion source reference.
func ValidateProposalEvidence(p model.CandidateProposal, observation model.ImprovementObservation) error {
	knownSignals := map[string]bool{}
	for _, signal := range observation.Signals {
		knownSignals[signal.Code] = true
	}
	for _, fingerprint := range observation.Fingerprints {
		knownSignals[fingerprint.Kind] = true
		knownSignals[fingerprint.Fingerprint] = true
	}
	for _, evidence := range p.Evidence {
		if evidence.TaskID != observation.RootTaskID {
			continue
		}
		if knownSignals[evidence.Signal] {
			return nil
		}
		if observation.Summary != nil && ((observation.Summary.SourceType == "run" && evidence.RunID == observation.Summary.SourceID) || (observation.Summary.SourceType == "review" && evidence.Review == observation.Summary.SourceID)) {
			return nil
		}
	}
	return errors.New("candidate evidence is not present in the deterministic observation")
}

func strictJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) == nil {
		return errors.New("extra JSON content")
	}
	return nil
}

func weight(v int) bool { return v >= 0 && v <= 100 }
func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func scopeWithin(scope model.ImprovementScope, source model.TaskProfile) bool {
	checks := [][2]string{{scope.SourceType, source.SourceType}, {scope.TaskType, source.TaskType}, {scope.Repository, source.Repository}, {scope.RoleID, source.RoleID}, {scope.Workflow, source.Workflow}, {scope.RuntimeOS, source.RuntimeOS}}
	for _, check := range checks {
		// The first experiment is intentionally no broader than the task that
		// produced the evidence. A later human-reviewed candidate may be
		// generalized by creating a new proposal with new evidence.
		if check[0] != check[1] {
			return false
		}
	}
	return true
}

func forbiddenField(v any) string {
	forbidden := []string{"permission", "authorization", "execution_grant", "credential", "secret", "token", "backup", "retention", "migration", "external_write", "score_weight", "canary_ratio", "promotion_rule", "safety", "sql", "code", "command", "transition"}
	var visit func(any) string
	visit = func(node any) string {
		switch item := node.(type) {
		case map[string]any:
			for key, value := range item {
				lower := strings.ToLower(key)
				for _, blocked := range forbidden {
					if strings.Contains(lower, blocked) {
						return key
					}
				}
				if found := visit(value); found != "" {
					return found
				}
			}
		case []any:
			for _, value := range item {
				if found := visit(value); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return visit(v)
}

// CompositeScore compares candidate samples to a fixed baseline. Quality uses
// the mean; cost-like dimensions use medians. Missing token values remain
// missing and are not silently interpreted as zero.
func CompositeScore(baseline, candidate []model.TaskEvaluation) (model.ExperimentScore, bool) {
	if len(baseline) == 0 || len(candidate) == 0 {
		return model.ExperimentScore{}, false
	}
	qualityBase, qualityCandidate := meanQuality(baseline), meanQuality(candidate)
	humanBase, humanCandidate := medianInts(interventions(baseline)), medianInts(interventions(candidate))
	cycleBase, cycleCandidate := medianInt64(cycles(baseline)), medianInt64(cycles(candidate))
	progressBase, progressCandidate := medianInts(noProgress(baseline)), medianInts(noProgress(candidate))
	baseTokens, candidateTokens := tokens(baseline), tokens(candidate)
	result := model.ExperimentScore{
		QualityChange:           qualityCandidate - qualityBase,
		HumanInterventionChange: lowerIsBetter(humanBase, humanCandidate, 1),
		CycleTimeChange:         lowerIsBetter(cycleBase, cycleCandidate, 1000),
		NoProgressChange:        lowerIsBetter(progressBase, progressCandidate, 1),
	}
	tokenAvailable := len(baseTokens) == len(baseline) && len(candidateTokens) == len(candidate)
	if tokenAvailable {
		result.TokenChange = lowerIsBetter(medianInt64(baseTokens), medianInt64(candidateTokens), 1)
	}
	weighted := result.QualityChange*0.35 + result.HumanInterventionChange*0.20 + result.CycleTimeChange*0.20 + result.NoProgressChange*0.10
	if tokenAvailable {
		weighted += result.TokenChange * 0.15
	}
	// Keep the configured weights fixed. A missing Token component is absent,
	// not treated as zero usage and not redistributed to the other dimensions.
	result.Composite = round(weighted)
	return result, true
}

func meanQuality(items []model.TaskEvaluation) float64 {
	var sum float64
	for _, item := range items {
		sum += item.QualityScore
	}
	return sum / float64(len(items))
}
func interventions(items []model.TaskEvaluation) []int {
	result := make([]int, len(items))
	for i := range items {
		result[i] = items[i].HumanInterventions
	}
	return result
}
func cycles(items []model.TaskEvaluation) []int64 {
	result := make([]int64, len(items))
	for i := range items {
		result[i] = items[i].CycleTimeMS
	}
	return result
}
func noProgress(items []model.TaskEvaluation) []int {
	result := make([]int, len(items))
	for i := range items {
		result[i] = items[i].NoProgressRuns
	}
	return result
}
func tokens(items []model.TaskEvaluation) []int64 {
	result := []int64{}
	for i := range items {
		if items[i].TotalTokens != nil {
			result = append(result, *items[i].TotalTokens)
		}
	}
	return result
}

func medianInts(values []int) float64 {
	copy := append([]int{}, values...)
	sort.Ints(copy)
	if len(copy) == 0 {
		return 0
	}
	mid := len(copy) / 2
	if len(copy)%2 == 0 {
		return float64(copy[mid-1]+copy[mid]) / 2
	}
	return float64(copy[mid])
}
func medianInt64(values []int64) float64 {
	copy := append([]int64{}, values...)
	sort.Slice(copy, func(i, j int) bool { return copy[i] < copy[j] })
	if len(copy) == 0 {
		return 0
	}
	mid := len(copy) / 2
	if len(copy)%2 == 0 {
		return float64(copy[mid-1]+copy[mid]) / 2
	}
	return float64(copy[mid])
}
func lowerIsBetter(base, candidate, floor float64) float64 {
	return clamp((base-candidate)/math.Max(base, floor)*100, -100, 100)
}
func clamp(value, low, high float64) float64 { return math.Max(low, math.Min(high, value)) }
func round(value float64) float64            { return math.Round(value*100) / 100 }
