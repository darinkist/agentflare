#!/bin/zsh

# Build AgentFlare's software components without contacting the keyboard.
set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
required_go="$(awk '$1 == "go" { print $2; exit }' "$repo_root/go.mod")"

if [[ "$(uname -s)" != "Darwin" ]]; then
  print -u2 "AgentFlare currently supports macOS only"
  exit 1
fi
if ! command -v go >/dev/null; then
  print -u2 "Go $required_go is required"
  exit 1
fi
if ! command -v uv >/dev/null; then
  print -u2 "uv is required; install it from https://docs.astral.sh/uv/"
  exit 1
fi

required_uv="0.8.22"
installed_uv="$(uv --version | awk '{ print $2 }')"
if [[ "$installed_uv" != "$required_uv" ]]; then
  print -u2 "uv $required_uv is required; found $installed_uv"
  exit 1
fi

installed_go="$(go env GOVERSION | sed 's/^go//')"
if [[ "$installed_go" != "$required_go" ]]; then
  print -u2 "Go $required_go is required; found $installed_go"
  exit 1
fi

print "macOS: $(sw_vers -productVersion)"
print "Go: $(go version)"
print "uv: $(uv --version)"
cd "$repo_root"
uv sync --locked --project integrations/codex
print "Python: $(uv run --locked --project integrations/codex python --version)"
mkdir -p daemon/bin
go build -o daemon/bin/agentflare ./daemon/source/cmd/agentflare
go build -o daemon/bin/agentflare-adapter ./daemon/source/cmd/agentflare-adapter
print "Synced Codex bridge and built daemon/bin/agentflare and daemon/bin/agentflare-adapter"
