package integration_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/adapter"
	"github.com/konstantinrink/agentflare/daemon/source/internal/adapter/replay"
	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
	"github.com/konstantinrink/agentflare/daemon/source/internal/km16pro"
	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
	"github.com/konstantinrink/agentflare/daemon/source/internal/server"
)

func TestReplayAdapterToSocketToFakeHID(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-adapter-integration-")
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
	driver := km16pro.New(source, km16pro.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), DiscoveryInterval: time.Millisecond, StatusSink: func(context.Context, km16pro.Status) error { return nil }})
	driverDone := make(chan error, 1)
	go func() { driverDone <- driver.Run(driverContext, stateCore.Snapshots()) }()
	socketServer := server.New(stateCore, server.Options{SocketPath: filepath.Join(directory, "agentflare.sock")})
	if err := socketServer.Listen(); err != nil {
		t.Fatal(err)
	}
	serverContext, stopServer := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- socketServer.Serve(serverContext) }()

	main := adapter.Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "running"}
	replaySource, err := replay.NewSource(replay.Fixture{Source: "replay", SessionID: "session-1", Capabilities: adapter.Capabilities{Snapshot: true, LiveEvents: true, RunIdentity: true, Liveness: true}, Snapshot: adapter.Snapshot{Main: &main}})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := adapter.New(adapter.Options{Source: replaySource, SessionID: "session-1", SocketPath: socketServer.SocketPath()})
	if err != nil {
		t.Fatal(err)
	}
	adapterContext, stopAdapter := context.WithCancel(context.Background())
	adapterDone := make(chan error, 1)
	go func() { adapterDone <- runtime.Run(adapterContext) }()
	waitForLEDRequest(t, transport, 21, protocol.Color{R: 57, G: 59, B: 64})

	stopAdapter()
	if err := <-adapterDone; err != nil {
		t.Fatal(err)
	}
	stopServer()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	stopDriver()
	if err := <-driverDone; err != nil {
		t.Fatal(err)
	}
	stopCore()
	if err := <-coreDone; err != nil {
		t.Fatal(err)
	}
}
