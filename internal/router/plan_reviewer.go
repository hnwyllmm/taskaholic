package router

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"work-assistant/internal/model"
)

// PlanReviewer chooses a design specialist before implementation. Assignment
// still goes through the ordinary AgentSelector and preserves session affinity.
type PlanReviewer struct{}

func (PlanReviewer) Select(repository, author string, agents []model.AgentProfile) (string, error) {
	repo := strings.ToLower(strings.TrimPrefix(repository, strings.Split(repository, "/")[0]+"/"))
	type candidate struct {
		id    string
		score int
	}
	var pool []candidate
	for _, a := range agents {
		if a.State != "ACTIVE" || a.ID == author {
			continue
		}
		c := a.Role.Capabilities
		if !slices.Contains(c, "design.review") && !slices.Contains(c, "architecture.review") {
			continue
		}
		scoped, match := false, false
		for _, cap := range c {
			area, ok := strings.CutSuffix(cap, ".review")
			if ok && !slices.Contains(genericReviewAreas, area) {
				scoped = true
				match = match || area == repo
			}
		}
		if scoped && !match {
			continue
		}
		score := 0
		if match {
			score = 10
		}
		if slices.Contains(c, "design.review") {
			score++
		}
		pool = append(pool, candidate{a.RoleID, score})
	}
	if len(pool) == 0 {
		return "", fmt.Errorf("%w: 没有独立的 design.review / architecture.review 成员可评审 %s 的方案", model.ErrConflict, repository)
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].score != pool[j].score {
			return pool[i].score > pool[j].score
		}
		return pool[i].id < pool[j].id
	})
	return pool[0].id, nil
}
