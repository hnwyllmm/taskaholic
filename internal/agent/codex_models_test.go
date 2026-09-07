package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCodexCatalogHandshakePaginationAndVisibleModelIDs(t *testing.T) {
	responses := `{"id":1,"result":{}}
{"method":"notice","params":{}}
{"id":2,"result":{"data":[{"id":"picker-a","model":"actual-a","displayName":"A"},{"id":"hidden","hidden":true},{"id":"bad id"}],"nextCursor":"page2"}}
{"id":3,"result":{"data":[{"id":"duplicate","model":"actual-a"},{"id":"fallback-b"}],"nextCursor":null}}
`
	var input bytes.Buffer
	models, err := readCodexModels(&input, strings.NewReader(responses))
	if err != nil || len(models) != 2 || models[0].ID != "actual-a" || models[1].ID != "fallback-b" {
		t.Fatal(models, err)
	}
	lines := strings.Split(strings.TrimSpace(input.String()), "\n")
	if len(lines) != 4 {
		t.Fatal(input.String())
	}
	for i, method := range []string{"initialize", "initialized", "model/list", "model/list"} {
		var msg map[string]any
		if json.Unmarshal([]byte(lines[i]), &msg) != nil || msg["method"] != method {
			t.Fatal(lines[i])
		}
		if i >= 2 && msg["params"].(map[string]any)["includeHidden"] != false {
			t.Fatal("hidden models requested")
		}
		if i == 3 && msg["params"].(map[string]any)["cursor"] != "page2" {
			t.Fatal("pagination cursor lost")
		}
	}
}

func TestCodexCatalogRejectsErrorsEmptyMalformedAndLoops(t *testing.T) {
	for _, response := range []string{
		`{"id":2,"error":{"message":"private-token"}}`,
		`{"id":2,"result":{"data":[]}}`,
		`{"id":2,"result":{"data":[{"id":"valid","displayName":"bad\nname"}]}}`,
		`{"id":2,"method":"approval/request","params":{}}`,
		`{"id":3,"result":{}}`,
		`{"id":2,"result":null}`, "private-token\n", strings.Repeat("x", 300<<10),
		"{\"id\":2,\"result\":{\"data\":[],\"nextCursor\":\"x\"}}\n{\"id\":3,\"result\":{\"data\":[],\"nextCursor\":\"x\"}}",
	} {
		var input bytes.Buffer
		if _, err := readCodexModels(&input, strings.NewReader("{\"id\":1,\"result\":{}}\n"+response)); err == nil || strings.Contains(err.Error(), "private-token") {
			t.Fatal("invalid/private model response accepted", err)
		}
	}
}

func TestCodexModelProcessIsBoundedAndDoesNotReceiveControlCredentials(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	t.Setenv("ASSISTANT_RUNTIME_TOKEN", "must-not-reach-codex")
	t.Setenv("ASSISTANT_API_TOKEN", "must-not-reach-codex")
	fake := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
test "$*" = 'app-server --listen stdio://' || exit 2
test -z "$ASSISTANT_RUNTIME_TOKEN" && test -z "$ASSISTANT_API_TOKEN" || exit 3
read init
printf '%s\n' '{"id":1,"result":{}}'
read initialized
read models
printf '%s\n' '{"id":2,"result":{"data":[{"model":"real-model","displayName":"Real"}]}}'
sleep 30 &
wait
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := NewCodexAdapter(fake, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	models, err := a.ListModels(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "real-model" || time.Since(start) > 3*time.Second {
		t.Fatal(models, err)
	}
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho private-token >&2\nsleep 30 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := a.ListModels(ctx); err == nil || strings.Contains(err.Error(), "private-token") || time.Since(start) > 3*time.Second {
		t.Fatal("discovery not canceled or error leaked", err)
	}
}
