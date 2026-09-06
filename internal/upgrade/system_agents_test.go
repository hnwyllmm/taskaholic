package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"work-assistant/internal/model"
)

func TestUpgradeUsesPinnedSystemMemberModel(t *testing.T) {
	root, data := testProject(t)
	config := testManagerConfig(root, data)
	config.RuntimeID = "local"
	config.ModelID = "default-model"
	called := false
	manager, err := New(config, fakeAdapter{change: func(dir string) error { return os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed"), 0600) }, inspect: func(spec model.RunSpec) {
		called = true
		if spec.ModelID != "chosen-model" || spec.AgentID != "chosen-agent" || !strings.Contains(spec.Instructions, "Builder instructions") || !strings.Contains(spec.Instructions, "隔离副本") {
			t.Fatal(spec)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := model.Upgrade{ID: "chosen-builder", Title: "docs", Instructions: "update", State: "BUILDING", Builder: &model.AgentProfile{ID: "chosen-agent", RuntimeID: "local", AdapterID: "codex-agent", ModelID: "chosen-model", Role: model.Role{RoleSpec: model.RoleSpec{Instructions: "Builder instructions"}}}}
	result := manager.Prepare(context.Background(), u)
	if !called || result.State != "READY" {
		t.Fatal(result, called)
	}
	called = false
	u.ID = "wrong-machine"
	u.Builder.RuntimeID = "remote"
	result = manager.Prepare(context.Background(), u)
	if called || result.State != "FAILED" {
		t.Fatal("remote silently ran locally", result, called)
	}
}
