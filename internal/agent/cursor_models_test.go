package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCursorModelDiscoveryParsesCLIWithoutInventingIDs(t *testing.T) {
	models, err := parseCursorModels("Available models\n\n\x1b[32mauto\x1b[0m - Auto (current, default)\ncomposer-test - Composer Test\nauto - duplicate\ninvalid id - skip\nTip: use --model <id> to switch.\n")
	if err != nil || len(models) != 2 || models[0].ID != "auto" || models[0].Name != "Auto" || models[1].ID != "composer-test" {
		t.Fatal(models, err)
	}
	for _, output := range []string{"", "Please authenticate", "bad id - name", "../invalid - name", "model - "} {
		if _, err := parseCursorModels(output); err == nil {
			t.Fatal("invalid catalog accepted", output)
		}
	}
	w := &modelOutput{remaining: 2}
	if _, err := w.Write([]byte("123")); err == nil || w.Len() != 0 {
		t.Fatal("output limit ignored")
	}
}

func TestCursorModelsCommandIsReadOnlyBoundedAndHidesErrors(t *testing.T) {
	t.Setenv("ASSISTANT_API_TOKEN", "should-not-reach-cli")
	a, _, _ := fakeCursor(t, `test "$#" = 1 && test "$1" = models || exit 2
test -z "$ASSISTANT_API_TOKEN" || exit 3
printf 'auto - Auto (default)\n'
`)
	models, err := a.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "auto" {
		t.Fatal(models, err)
	}
	a, _, _ = fakeCursor(t, "echo private-token >&2\nexit 1\n")
	if _, err = a.ListModels(context.Background()); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("CLI error not sanitized", err)
	}
	a, _, _ = fakeCursor(t, "exec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err = a.ListModels(ctx); err == nil || time.Since(start) > 2*time.Second {
		t.Fatal("discovery ignored cancellation", err)
	}
}
