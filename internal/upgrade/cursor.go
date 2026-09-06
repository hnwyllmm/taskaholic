package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"work-assistant/internal/agent"
	"work-assistant/internal/model"
)

// Cursor stays in the normal read-only Ask mode. The trusted builder applies
// its declarative edits only to the private candidate; no Shell/Write/MCP or
// --force permission is granted to the CLI to make self-upgrades work.
type cursorBuilder struct{ reader agent.Adapter }

func (cursorBuilder) Name() string                   { return "cursor-agent" }
func (c cursorBuilder) Capabilities() map[string]any { return c.reader.Capabilities() }

var cursorEditsSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["summary","edits"],"properties":{"summary":{"type":"string"},"edits":{"type":"array","minItems":1,"maxItems":200,"items":{"type":"object","additionalProperties":false,"required":["path","old_text","new_text","create"],"properties":{"path":{"type":"string"},"old_text":{"type":"string"},"new_text":{"type":"string"},"create":{"type":"boolean"}}}}}}`)

type cursorEdits struct {
	Summary string       `json:"summary"`
	Edits   []cursorEdit `json:"edits"`
}

type cursorEdit struct {
	Path   string `json:"path"`
	Old    string `json:"old_text"`
	New    string `json:"new_text"`
	Create bool   `json:"create"`
}

func (c cursorBuilder) Run(ctx context.Context, spec model.RunSpec, candidate string, directives <-chan model.Directive, emit func(agent.Event)) agent.Result {
	// Keep CLI policy/session metadata outside the exhaustive installable tree.
	workspace := filepath.Join(filepath.Dir(candidate), "builder-workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return agent.Result{Err: err, ExitCode: -1}
	}
	spec.Instructions += "\n\nCursor 升级补丁协议（覆盖上面直接写文件/运行测试的要求）：你处于只读 Ask 模式，允许查看候选源码，但不要调用 Shell、Write 或 MCP。候选源码绝对路径是：" + candidate + `
请先阅读有关文件，再返回 summary 和 edits 数组。每条 edit 的 path 必须是相对候选根目录的源码路径；修改已有文件时 create=false，old_text 必须与当前文件中恰好一处文本逐字匹配，new_text 是替换内容。同一文件多条修改按数组顺序应用。创建新文件时 create=true、old_text=""、new_text 为完整文件。不要返回整份未修改文件，不要删除文件，不要修改受保护的基础设施。最终 JSON 不超过 120 KiB。
应用程序会检查全部补丁后写入隔离候选，并在外部隔离环境运行测试和构建。你不需要也不能自行执行测试，请不要声称已执行。`
	spec.OutputSchema = cursorEditsSchema
	result := c.reader.Run(ctx, spec, workspace, directives, emit)
	if result.Err != nil {
		return result
	}
	if len(result.Output) > 128<<10 {
		return agent.Result{Err: fmt.Errorf("Cursor upgrade patch exceeds 128 KiB"), ExitCode: -1}
	}
	if !json.Valid([]byte(result.Output)) {
		return agent.Result{Err: fmt.Errorf("Cursor upgrade patch is not one complete JSON value"), ExitCode: -1}
	}
	var edits cursorEdits
	decoder := json.NewDecoder(strings.NewReader(result.Output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&edits); err != nil {
		return agent.Result{Err: fmt.Errorf("decode Cursor upgrade edits: %w", err), ExitCode: -1}
	}
	if err := applyCursorEdits(candidate, edits); err != nil {
		return agent.Result{Err: err, ExitCode: -1}
	}
	output, _ := json.Marshal(preparationResult{Summary: edits.Summary})
	return agent.Result{Output: string(output)}
}

func applyCursorEdits(candidate string, answer cursorEdits) error {
	if strings.TrimSpace(answer.Summary) == "" || len(answer.Edits) == 0 || len(answer.Edits) > 200 {
		return fmt.Errorf("Cursor upgrade requires a summary and 1-200 edits")
	}
	type pendingFile struct {
		body string
		mode fs.FileMode
	}
	pending := map[string]pendingFile{}
	for _, edit := range answer.Edits {
		path := edit.Path
		if path == "" || filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || strings.ContainsAny(path, "\\\x00\r\n") || strings.HasPrefix(path, "../") {
			return fmt.Errorf("invalid Cursor upgrade path %q", path)
		}
		allowed := false
		for _, target := range sourceTargets {
			if path == target || ((target == "cmd" || target == "internal") && strings.HasPrefix(path, target+"/")) {
				allowed = true
			}
		}
		if !allowed || protected(path) {
			return fmt.Errorf("Cursor upgrade path is unsupported or protected: %s", path)
		}
		if !utf8.ValidString(edit.Old) || !utf8.ValidString(edit.New) || strings.ContainsRune(edit.Old+edit.New, '\x00') {
			return fmt.Errorf("Cursor upgrade edits must be UTF-8 text")
		}
		full := filepath.Join(candidate, filepath.FromSlash(path))
		// Reject every symlink component, not just the final file.
		for current := full; current != candidate; current = filepath.Dir(current) {
			info, err := os.Lstat(current)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && (!info.Mode().IsRegular() || hasMultipleHardLinks(info))) {
				return fmt.Errorf("unsafe Cursor upgrade path: %s", path)
			}
		}
		file, exists := pending[path]
		if !exists {
			info, err := os.Stat(full)
			if err == nil {
				if !info.Mode().IsRegular() {
					return fmt.Errorf("not a regular source file: %s", path)
				}
				body, err := os.ReadFile(full)
				if err != nil {
					return err
				}
				file, exists = pendingFile{body: string(body), mode: info.Mode().Perm()}, true
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if edit.Create {
			if exists || edit.Old != "" || edit.New == "" {
				return fmt.Errorf("invalid create edit: %s", path)
			}
			file = pendingFile{body: edit.New, mode: 0o600}
		} else {
			if !exists || edit.Old == "" || strings.Count(file.body, edit.Old) != 1 {
				return fmt.Errorf("old_text must match exactly once: %s", path)
			}
			file.body = strings.Replace(file.body, edit.Old, edit.New, 1)
		}
		if len(file.body) > maxSourceBytes {
			return fmt.Errorf("Cursor source file exceeds safety limit")
		}
		pending[path] = file
	}
	// No write happens until the complete proposal is validated. These files
	// are still unapproved; the normal manifest/sandbox/digest review follows.
	for path, file := range pending {
		if err := writeAtomic(filepath.Join(candidate, filepath.FromSlash(path)), []byte(file.body), file.mode); err != nil {
			return err
		}
	}
	return nil
}
