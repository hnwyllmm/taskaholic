// Read-only provider probe: no Store, no imports, no PR registration, no Run.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"work-assistant/internal/model"
	"work-assistant/internal/tasksource"
)

func main() {
	kind := flag.String("kind", "", "antmultica or github")
	pr := flag.String("pr", "", "existing PR URL for read-only GitHub verification")
	pageSize := flag.Int("page-size", 100, "GitHub items per page (1-100), for live pagination verification")
	flag.Parse()
	if *pageSize < 1 || *pageSize > 100 {
		fmt.Fprintln(os.Stderr, "page-size must be between 1 and 100")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	s := model.TaskSource{ID: *kind, Kind: *kind}
	t := model.SourceTarget{Entity: *pr, CreatedAtMS: time.Now().UnixMilli(), Cursor: json.RawMessage("{}")}
	var p tasksource.Provider
	numericPages := 0
	switch *kind {
	case "antmultica":
		s.Config = model.SourceConfig{WorkspaceID: "44029359-53fc-4a6a-bd6f-aae91c2bd754", WorkspaceSlug: "seekdb", AssigneeID: "51410f21-5e32-490e-875d-eb3f28fb4095", IterationKey: "迭代", IterationValue: "1.5.0"}
		p = &tasksource.AntMultica{Run: tasksource.RunCLI, Binary: "/usr/local/bin/multica"}
	case "github":
		p = &tasksource.GitHub{Run: func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			last := len(args) - 1
			args[last] = strings.ReplaceAll(args[last], "per_page=100", fmt.Sprintf("per_page=%d", *pageSize))
			if strings.HasPrefix(args[last], "/repositories/") {
				numericPages++
			}
			return tasksource.RunCLI(ctx, binary, args...)
		}, Binary: "/usr/bin/gh"}
	default:
		fmt.Fprintln(os.Stderr, "choose --kind antmultica or github")
		os.Exit(2)
	}
	result, err := p.Poll(ctx, s, t)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	t.Cursor = result.Cursor
	replay, err := p.Poll(ctx, s, t)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	kinds := map[string]int{}
	titles := []string{}
	for _, e := range result.Events {
		kinds[e.Kind]++
		if e.Title != "" {
			titles = append(titles, e.Title)
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"kind": *kind, "events": kinds, "titles": titles, "head_sha": result.HeadSHA, "second_poll_events": len(replay.Events), "numeric_repository_pages": numericPages, "read_only": true})
}
