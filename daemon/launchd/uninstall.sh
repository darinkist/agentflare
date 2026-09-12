#!/bin/zsh

set -euo pipefail

label="com.agentflare.daemon"
domain="gui/$(id -u)"
plist_path="$HOME/Library/LaunchAgents/$label.plist"

launchctl bootout "$domain/$label" 2>/dev/null || true
if [[ -f "$plist_path" ]]; then
  rm "$plist_path"
fi

print "Removed $label"
