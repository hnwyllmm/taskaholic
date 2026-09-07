package agent

import "testing"

func TestPipelineCredentialsNotInheritedByAgentCommands(t *testing.T) {
	for _, entry := range []string{"GITLAB_TOKEN=secret", "GITLAB_PRIVATE_TOKEN=secret", "WORK_ASSISTANT_GITLAB_TOKEN_FILE=/private/file", "ASSISTANT_RUNTIME_TOKEN=secret"} {
		if !isControlCredential(entry) {
			t.Fatal("leaked credential", entry[:3])
		}
	}
	for _, entry := range []string{"PATH=/bin", "CURSOR_API_KEY=cursor-key", "OPENAI_API_KEY=model-key", "HOME=/home/user"} {
		if isControlCredential(entry) {
			t.Fatal("removed agent auth or normal environment")
		}
	}
}
