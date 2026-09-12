#!/bin/zsh

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
binary="${AGENTFLARE_BINARY:-$repo_root/daemon/bin/agentflare}"
label="com.agentflare.daemon"
domain="gui/$(id -u)"
launch_agents_dir="$HOME/Library/LaunchAgents"
plist_path="$launch_agents_dir/$label.plist"
log_dir="$HOME/Library/Logs/AgentFlare"
key_brightness=35
underglow_brightness=25
dry_run=false

usage() {
  print "usage: $0 [--key-brightness 0..100] [--underglow-brightness 0..100] [--dry-run]"
}

while (( $# > 0 )); do
  case "$1" in
    --key-brightness)
      key_brightness="${2:-}"; shift 2 ;;
    --underglow-brightness)
      underglow_brightness="${2:-}"; shift 2 ;;
    --dry-run)
      dry_run=true; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      usage; exit 2 ;;
  esac
done

for value in "$key_brightness" "$underglow_brightness"; do
  if [[ ! "$value" =~ '^[0-9]+$' ]] || (( value < 0 || value > 100 )); then
    print -u2 "brightness must be an integer from 0 through 100"
    exit 2
  fi
done

if [[ ! -x "$binary" ]]; then
  print -u2 "missing executable: $binary"
  print -u2 "build it first with: go build -o daemon/bin/agentflare ./daemon/source/cmd/agentflare"
  exit 1
fi

render_plist() {
python3 - "$repo_root" "$binary" "$log_dir" "$label" "$key_brightness" "$underglow_brightness" <<'PY'
import plistlib
import sys
from pathlib import Path

repo_root, binary, log_dir, label, key_brightness, underglow_brightness = sys.argv[1:]
payload = {
    "Label": label,
    "ProgramArguments": [binary, "daemon", "--key-brightness", key_brightness, "--underglow-brightness", underglow_brightness],
    "WorkingDirectory": repo_root,
    "RunAtLoad": True,
    "KeepAlive": True,
    "ThrottleInterval": 5,
    "ProcessType": "Interactive",
    "StandardOutPath": str(Path(log_dir) / "daemon.stdout.log"),
    "StandardErrorPath": str(Path(log_dir) / "daemon.stderr.log"),
}
plistlib.dump(payload, sys.stdout.buffer, sort_keys=False)
PY
}

if [[ "$dry_run" == true ]]; then
  render_plist
  exit 0
fi

mkdir -p "$launch_agents_dir" "$log_dir"
render_plist > "$plist_path"

launchctl bootout "$domain/$label" 2>/dev/null || true
launchctl bootstrap "$domain" "$plist_path"
launchctl enable "$domain/$label"
launchctl kickstart -k "$domain/$label"

print "Installed and started $label"
print "Inspect with: launchctl print $domain/$label"
