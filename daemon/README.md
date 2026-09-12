# AgentFlare daemon

The daemon is the harness-independent part of AgentFlare. It owns the local
Unix socket, lifecycle state, LED layering, underglow behavior, leases, slot
assignment, and KM16-Pro HID output. It does not know whether events came from
Codex, Claude, OpenCode, replay, or another harness.

## Requirements

- macOS
- Go 1.27.1 for building from source
- A KM16-Pro in USB mode for physical LED output
- The documented Direct-LED firmware for the current Direct-LED path

Normal unit and integration tests do not require a keyboard. Do not run the
daemon or the HID preflight unless physical hardware interaction is intended.

Check the local toolchain before building:

~~~bash
go version
python3 --version
~~~

## Build

From the repository root:

~~~bash
go build -o daemon/bin/agentflare ./daemon/source/cmd/agentflare
go build -o daemon/bin/agentflare-adapter ./daemon/source/cmd/agentflare-adapter
go build ./...
~~~

The binaries are local build artifacts and are ignored by Git under daemon/bin.

## Run in the foreground

~~~bash
./daemon/bin/agentflare daemon
~~~

The default socket is:

~~~text
~/Library/Application Support/agentflare/agentflare.sock
~~~

Use another terminal for read-only status checks:

~~~bash
./daemon/bin/agentflare status --v2 --json
./daemon/bin/agentflare status
~~~

The daemon logs to stderr when run in the foreground. Press Ctrl-C to stop it
cleanly.

## Useful manual commands

These commands send lifecycle or display requests to an already running daemon:

~~~bash
./daemon/bin/agentflare emit started --source demo --agent main
./daemon/bin/agentflare lights set --zone backglow --rgb 0,0,64 --ttl 2s
./daemon/bin/agentflare lights clear --zone backglow
./daemon/bin/agentflare status --v2 --json
./daemon/bin/agentflare clear
~~~

The commands exercise the service API. They do not prove that an existing
external harness task is connected to the service.

The socket admits at most 64 concurrent requests. A client that arrives while
all slots are in use receives a bounded `server_busy` response and can retry
after an admitted request completes.

## Optional HID preflight

This command inspects the connected HID interface and sends a VIA query. It is
not a software-only check and does not prove support for every Direct-LED:

~~~bash
go run ./daemon/source/cmd/hidpreflight
~~~

## Start automatically on macOS login

Use the user-level launchd installer after building the binary:

~~~bash
cd /absolute/path/to/agentflare
daemon/launchd/install.sh
~~~

The installer starts the service and therefore changes macOS launch configuration. Its defaults are 35 percent key brightness and 25 percent underglow brightness. Select values explicitly when needed:

~~~bash
daemon/launchd/install.sh --key-brightness 35 --underglow-brightness 25
~~~

Generate and inspect a plist without installing or starting anything:

~~~bash
daemon/launchd/install.sh --dry-run
~~~

This installs com.agentflare.daemon into:

~~~text
~/Library/LaunchAgents/com.agentflare.daemon.plist
~~~

It starts the daemon when the user logs in and restarts it if it exits. A
LaunchAgent is intentional because the HID device belongs to the logged-in user
session. It does not run before user login.

Check the launchd service:

~~~bash
launchctl print "gui/$(id -u)/com.agentflare.daemon"
./daemon/bin/agentflare status --v2 --json
tail -f "$HOME/Library/Logs/AgentFlare/daemon.stderr.log"
~~~

Remove the autostart entry with:

~~~bash
cd /absolute/path/to/agentflare
daemon/launchd/uninstall.sh
~~~

The installer and uninstaller change macOS user launch configuration and start
or stop the daemon only when you explicitly run them.

## Adapter runtime

The Go package under daemon/source/internal/adapter is harness-neutral runtime
code for normalized sources, sessions, snapshots, sequencing, resync, and
delivery. Harness-specific ingress code belongs under integrations/.

For example, the current replay source can be run with:

~~~bash
./daemon/bin/agentflare-adapter \
  --source replay \
  --fixture daemon/source/internal/adapter/replay/testdata/lifecycle.json \
  --session demo-session \
  --diagnostics
~~~

Replay is useful for service testing, but it is not evidence of access to an
existing external harness task.
