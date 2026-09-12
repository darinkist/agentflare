"""Runtime defaults and bounded protocol limits for the Codex bridge."""

import os
from pathlib import Path


APP_SUPPORT_DIR = Path.home() / "Library" / "Application Support" / "agentflare"
DEFAULT_SOCKET_PATH = str(APP_SUPPORT_DIR / "agentflare.sock")
DEFAULT_RUNTIME_DIR = APP_SUPPORT_DIR / "codex-hook"
SOCKET_PATH = os.environ.get("AGENTFLARE_SOCKET_PATH", DEFAULT_SOCKET_PATH)
OUTPUT_PATH = os.environ.get(
    "AGENTFLARE_CODEX_HOOK_LOG", str(DEFAULT_RUNTIME_DIR / "events.jsonl")
)
STATE_DIR = os.environ.get(
    "AGENTFLARE_CODEX_HOOK_STATE_DIR", str(DEFAULT_RUNTIME_DIR / "state")
)
SOURCE_FRESHNESS_GRACE = float(os.environ.get("AGENTFLARE_SOURCE_FRESHNESS_GRACE", "90"))
SOCKET_TIMEOUT = 0.25
HEARTBEAT_INTERVAL = 5.0
WORKER_START_TIMEOUT = 2.0
MAX_REQUEST_BYTES = 16 * 1024
MAX_RESPONSE_BYTES = 64 * 1024
MAX_IDENTIFIER_BYTES = 128
MAX_SUBAGENTS = 64
MAX_LOG_BYTES = 5 * 1024 * 1024
MAX_SAFE_INTEGER = 9_007_199_254_740_991
COMPACT_DIGEST_HEX = 48
