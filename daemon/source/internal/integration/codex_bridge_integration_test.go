package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
	"github.com/konstantinrink/agentflare/daemon/source/internal/km16pro"
	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
	"github.com/konstantinrink/agentflare/daemon/source/internal/server"
)

func TestCodexBridgeToSocketToFakeHID(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-codex-bridge-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })

	transport := newIntegrationTransport(2)
	source := &integrationSource{current: transport}
	stateCore := core.New(nil)
	coreContext, stopCore := context.WithCancel(context.Background())
	coreDone := make(chan error, 1)
	go func() { coreDone <- stateCore.Run(coreContext) }()

	driverContext, stopDriver := context.WithCancel(context.Background())
	driver := km16pro.New(source, km16pro.Options{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		DiscoveryInterval: time.Millisecond,
		StatusSink: func(statusContext context.Context, status km16pro.Status) error {
			return stateCore.SetDeviceStatus(statusContext, core.DeviceStatus(status))
		},
	})
	driverDone := make(chan error, 1)
	go func() { driverDone <- driver.Run(driverContext, stateCore.Snapshots()) }()

	socketServer := server.New(stateCore, server.Options{SocketPath: filepath.Join(directory, "agentflare.sock")})
	if err := socketServer.Listen(); err != nil {
		t.Fatal(err)
	}
	serverContext, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- socketServer.Serve(serverContext) }()
	t.Cleanup(func() {
		stopServer()
		stopDriver()
		stopCore()
		if err := <-serverDone; err != nil {
			t.Errorf("server shutdown: %v", err)
		}
		if err := <-driverDone; err != nil {
			t.Errorf("driver shutdown: %v", err)
		}
		if err := <-coreDone; err != nil {
			t.Errorf("core shutdown: %v", err)
		}
	})

	root := repositoryRoot(t)
	python := os.Getenv("AGENTFLARE_CODEX_PYTHON")
	if python == "" {
		t.Fatal("AGENTFLARE_CODEX_PYTHON must name the synchronized Codex bridge interpreter")
	}
	installTarget := filepath.Join(directory, "installed-codex-home")
	installer := exec.Command(
		python,
		filepath.Join(root, "integrations", "codex", "install.py"),
		"--mode", "global", "--codex-home", installTarget,
	)
	if output, installErr := installer.CombinedOutput(); installErr != nil {
		t.Fatalf("install bridge: %v: %s", installErr, output)
	}
	var hookConfig struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	configData, readErr := os.ReadFile(filepath.Join(installTarget, "hooks.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if unmarshalErr := json.Unmarshal(configData, &hookConfig); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	groups := hookConfig.Hooks["UserPromptSubmit"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command == "" {
		t.Fatal("installed hook command is missing")
	}
	hookCommand := groups[0].Hooks[0].Command
	stateDir := filepath.Join(directory, "bridge-state")
	environment := append(os.Environ(),
		"AGENTFLARE_SOCKET_PATH="+socketServer.SocketPath(),
		"AGENTFLARE_CODEX_HOOK_STATE_DIR="+stateDir,
		"AGENTFLARE_CODEX_HOOK_LOG="+filepath.Join(directory, "events.jsonl"),
		"AGENTFLARE_SOURCE_FRESHNESS_GRACE=1",
	)
	runBridge := func(payload map[string]any) []byte {
		t.Helper()
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		command := exec.Command("/bin/sh", "-c", hookCommand)
		command.Env = environment
		command.Stdin = bytes.NewReader(append(encoded, '\n'))
		output, runErr := command.Output()
		if runErr != nil {
			if exitErr, ok := runErr.(*exec.ExitError); ok {
				t.Fatalf("bridge failed: %v: %s", runErr, exitErr.Stderr)
			}
			t.Fatal(runErr)
		}
		return output
	}

	base := map[string]any{"session_id": "codex-session", "turn_id": "turn-1"}
	start := clonePayload(base)
	start["hook_event_name"] = "SessionStart"
	runBridge(start)

	before := len(transport.requestsSnapshot())
	running := clonePayload(base)
	running["hook_event_name"] = "UserPromptSubmit"
	runBridge(running)
	waitForNewLEDRequest(t, transport, before, 21, protocol.Color{R: 57, G: 59, B: 64})

	before = len(transport.requestsSnapshot())
	waiting := clonePayload(base)
	waiting["hook_event_name"] = "PermissionRequest"
	runBridge(waiting)
	waitForNewLEDRequest(t, transport, before, 21, protocol.Color{R: 12, G: 36, B: 64})

	before = len(transport.requestsSnapshot())
	subagent := clonePayload(base)
	subagent["hook_event_name"] = "SubagentStart"
	subagent["agent_id"] = "worker-a"
	subagent["agent_type"] = "worker"
	runBridge(subagent)
	waitForNewLEDRequest(t, transport, before, 12, protocol.Color{R: 58, G: 49, B: 88})

	before = len(transport.requestsSnapshot())
	subagentStop := clonePayload(subagent)
	subagentStop["hook_event_name"] = "SubagentStop"
	if output := runBridge(subagentStop); string(output) != "{\"continue\":true}\n" {
		t.Fatalf("subagent stop output = %q", output)
	}
	waitForNewLEDRequest(t, transport, before, 12, protocol.Color{})

	stop := clonePayload(base)
	stop["hook_event_name"] = "Stop"
	if output := runBridge(stop); string(output) != "{\"continue\":true}\n" {
		t.Fatalf("stop output = %q", output)
	}

	// A Stop hook is a completion candidate. If the same turn continues before
	// the terminal display expires, the bridge must renew the run identity so
	// the core can accept the waiting-user and running transitions.
	before = len(transport.requestsSnapshot())
	continuedWaiting := clonePayload(base)
	continuedWaiting["hook_event_name"] = "PermissionRequest"
	runBridge(continuedWaiting)
	waitForNewLEDRequest(t, transport, before, 21, protocol.Color{R: 12, G: 36, B: 64})

	before = len(transport.requestsSnapshot())
	continuedRunning := clonePayload(base)
	continuedRunning["hook_event_name"] = "PostToolUse"
	runBridge(continuedRunning)
	waitForNewLEDRequest(t, transport, before, 21, protocol.Color{R: 57, G: 59, B: 64})

	// The renewed run keeps the daemon's terminal contract intact: a different
	// terminal state for that same renewed identity is still rejected.
	continuedStop := clonePayload(base)
	continuedStop["hook_event_name"] = "Stop"
	runBridge(continuedStop)

	interrupt := clonePayload(base)
	interrupt["hook_event_name"] = "Interrupt"
	if output := runBridge(interrupt); len(output) != 0 {
		t.Fatalf("interrupt output = %q", output)
	}
	logData, err := os.ReadFile(filepath.Join(directory, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(logData, []byte(`"display_delivery_failure":"invalid_transition"`)) {
		t.Fatalf("bridge log did not record server rejection: %s", logData)
	}

	end := clonePayload(base)
	end["hook_event_name"] = "SessionEnd"
	runBridge(end)
}

func clonePayload(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source)+1)
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate integration test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", ".."))
}

func waitForNewLEDRequest(t *testing.T, transport *integrationTransport, after int, index int, color protocol.Color) {
	t.Helper()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		requests := transport.requestsSnapshot()
		for _, request := range requests[after:] {
			if request[0] == 0x07 && request[1] == 0x00 && request[2] == 0x01 && request[3] == byte(index) && request[4] == color.R && request[5] == color.G && request[6] == color.B {
				return
			}
		}
		select {
		case <-transport.requestWritten:
		case <-timeout.C:
			t.Fatalf("did not observe new LED %d with rgb=%d,%d,%d", index, color.R, color.G, color.B)
		}
	}
}
