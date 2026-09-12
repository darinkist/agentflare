# Codex integration

Shows what Codex is doing on your keyboard: lifecycle events (task started,
approval needed, finished, …) go to the local AgentFlare daemon, which owns
colours, timing, and HID. The bridge only taps events — it never controls
Codex, never touches HID, and never stores prompts, tool input, responses, or
transcripts.

## Install

1. Install `uv` 0.8.22 ([official docs](https://docs.astral.sh/uv/)) and Python 3.12:

```bash
curl -LsSf https://astral.sh/uv/0.8.22/install.sh | sh
test "$(uv --version | awk '{print $2}')" = "0.8.22"
uv python install 3.12
```

2. From the repository root, preview then install the hooks (global covers
all your projects):

```bash
uv sync --locked --project integrations/codex
uv run --locked --project integrations/codex python integrations/codex/install.py --mode global --preview
uv run --locked --project integrations/codex python integrations/codex/install.py --mode global
```

For a single repository instead: `--mode project --project /absolute/path/to/project`.
Never install both — Codex would fire every event twice.

3. In Codex, review and trust the new hook configuration when prompted.

The installer copies the bridge next to your `hooks.json`, preserves
unrelated hooks, records a hash for every managed file, and backs up your
existing config. For `--update` or `--remove`, close Codex first.

## If the display looks stuck

No manual bridge command is needed day-to-day — Codex fires the hooks on
its own, and the bridge may keep a background process for heartbeats. If the
display freezes after a daemon outage, first make sure the daemon is back,
then recover the session explicitly (drops the event whose reply was lost):

```bash
uv run --locked --project integrations/codex python integrations/codex/agentflare_bridge.py --recover --session-id '<session-id>'
```

Two things to know: `Stop` is a completion candidate, not proof of success.
And the display clears itself after ~90 seconds without hook activity — that
also happens during a long but valid quiet run, and says nothing about Codex
having exited.

## Configure and validate

State lives in `~/Library/Application Support/agentflare/codex-hook/`.
Diagnostics use one local `events.jsonl` file there, capped at 5 MiB. When
full, previous entries are discarded and logging continues in the same file.
There are no retained archives.
Override with `AGENTFLARE_SOCKET_PATH`, `AGENTFLARE_CODEX_HOOK_STATE_DIR`, or
`AGENTFLARE_CODEX_HOOK_LOG` before starting Codex.

```bash
uv run --locked --project integrations/codex python -m unittest discover -s integrations/codex/tests
uv build --project integrations/codex
uv run --locked --project integrations/codex python -m json.tool integrations/codex/hooks.json >/dev/null
```

These checks need no daemon, Codex task, network connection, or keyboard.
