#!/bin/zsh
set -euo pipefail
wa_dir="$(cd -- "$(dirname -- "$0")" && pwd)"
wa_codex="${WA_CODEX_BINARY:-$HOME/.local/bin/codex}"
wa_go="${WA_GO_BINARY:-$wa_dir/data/role-studio/.toolchains/go/bin/go}"
if [[ ! -x "$wa_go" ]] && command -v go >/dev/null 2>&1; then
  wa_go="$(command -v go)"
fi
if [[ ! -x "$wa_dir/bin/assistant-supervisor" || ! -x "$wa_dir/bin/assistant-local" ]]; then
  print -u2 "请先构建：go build -o bin/ ./cmd/..."
  exit 1
fi
print "启动后访问 http://127.0.0.1:17343/；按 Ctrl+C 停止。"
exec "$wa_dir/bin/assistant-supervisor" \
  --root "$wa_dir" \
  --data "$wa_dir/data/role-studio" \
  --runtime-id local-role-studio \
  --listen 127.0.0.1:17343 \
  --codex-binary "$wa_codex" \
  --go-binary "$wa_go" \
  --model "${WA_MODEL:-gpt-5.6-sol}"
