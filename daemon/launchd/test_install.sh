#!/bin/zsh

set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/install.sh"
default="$(AGENTFLARE_BINARY=/bin/echo "$script" --dry-run)"
custom="$(AGENTFLARE_BINARY=/bin/echo "$script" --dry-run --key-brightness 47 --underglow-brightness 19)"

[[ "$default" == *"<string>35</string>"* ]]
[[ "$default" == *"<string>25</string>"* ]]
[[ "$custom" == *"<string>47</string>"* ]]
[[ "$custom" == *"<string>19</string>"* ]]
if AGENTFLARE_BINARY=/bin/echo "$script" --dry-run --key-brightness 101 >/dev/null 2>&1; then
  print -u2 "invalid brightness was accepted"
  exit 1
fi
