# Contributing to AgentFlare

Develop on macOS with the Go version in `go.mod`, `uv 0.8.22`, and Python 3.12. Run `scripts/setup.sh` to synchronize the locked Codex package and build local binaries without contacting hardware. Update `uv.lock` deliberately with `uv lock --project integrations/codex`; CI rejects an outdated lockfile.

Keep the daemon harness-independent. Integrations belong in `integrations/` and
normalize allowlisted lifecycle metadata into V2 client requests. An integration
owns its client-side binding ID, request sequence, heartbeat, resynchronisation
after uncertainty, and stable run identities. The daemon validates that V2
contract and exclusively owns accepted display state, colours, timing, slots and
HID access. Tests must use a fake daemon or injected HID transport, never a
physical keyboard.

Run these checks before a pull request:

```bash
gofmt -w $(find daemon -name '*.go' -type f)
go test ./...
go vet ./...
go test -race ./...
go build ./...
uv sync --locked --project integrations/codex
uv run --locked --project integrations/codex python -m unittest discover -s integrations/codex/tests
uv build --project integrations/codex
uv run --locked --project integrations/codex python -B firmware/tools/test_build_direct_led.py
uv run --locked --project integrations/codex python -B firmware/tools/test_control_direct_led.py
zsh daemon/launchd/test_install.sh
```

Keep changes focused, update relevant documentation and include regression tests. Pull requests should describe the user-visible change, checks run, and any hardware verification explicitly requested and performed.

### Claude Code example

A Claude Code contribution should be an isolated adapter under
`integrations/claude/`. It should normalize lifecycle events, create stable
identities, and implement the V2 client protocol: binding, ordered requests,
heartbeats and full resynchronisation after uncertainty. Test it against a fake
daemon. Do not put Claude-specific rules, LED colours, timers, slots, accepted
state, or HID access in the daemon.
