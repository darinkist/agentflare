# Hook bridge instructions

- `agentflare_bridge.py` is the thin maintained hook entry point. Runtime code
  lives in the standard-library-only `src/agentflare_codex/` package and is
  developed through `uv sync --locked --project integrations/codex`. The
  adapter normalizes allowlisted lifecycle metadata, owns source identities,
  binding and sequence progression, heartbeats, snapshot resynchronization,
  and explicit recovery. The Go daemon remains responsible for server-side
  validation, accepted lifecycle state, capacity, timing, and HID output.
- Forward only the normalized, allowlisted lifecycle fields required by the
  AgentFlare adapter. Never persist prompts, tool arguments, responses, or
  transcript contents.
- Keep the bridge service-free in unit tests. Tests must not require a running
  daemon, a Codex task, a network connection, or physical keyboard hardware.
- Treat uncertain socket outcomes conservatively and preserve the existing
  AgentFlare transaction-safety rules.
- The installer is the supported activation path. Do not describe or introduce
  manual copying of hook files; changes to `hooks.json` or the bridge must be
  reflected in the local tests and reviewed under the project trust model.
- Installed hooks and workers hold the shared installation lock for their
  whole lifetime. Update and removal take its non-waiting exclusive lock, so
  document updates as a closed-Codex, idle-state operation.
