package agent

import "strings"

// Explicit control credentials must not be inherited by model/tool processes.
// This is environment hygiene, not OS isolation from the host user's files.
func isControlCredential(entry string) bool {
	key, _, _ := strings.Cut(entry, "=")
	return strings.HasPrefix(key, "ASSISTANT_") || key == "GITLAB_TOKEN" || key == "GITLAB_PRIVATE_TOKEN" || key == "WORK_ASSISTANT_GITLAB_TOKEN_FILE"
}
