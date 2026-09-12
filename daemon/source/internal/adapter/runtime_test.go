package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
	"github.com/konstantinrink/agentflare/daemon/source/internal/server"
)

func TestRuntimeDeliversReplayStyleLifecycleToV2Socket(t *testing.T) {
	stateCore := core.New(nil)
	coreContext, stopCore := context.WithCancel(context.Background())
	defer stopCore()
	coreDone := make(chan error, 1)
	go func() { coreDone <- stateCore.Run(coreContext) }()

	directory, err := os.MkdirTemp("/tmp", "af-adapter-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketServer := server.New(stateCore, server.Options{SocketPath: filepath.Join(directory, "agentflare.sock")})
	if err := socketServer.Listen(); err != nil {
		t.Fatal(err)
	}
	serverContext, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	serverDone := make(chan error, 1)
	go func() { serverDone <- socketServer.Serve(serverContext) }()

	main := Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "running"}
	stream := newTestStream(Snapshot{SourceOrder: 1, Main: &main})
	runtime, err := New(Options{Source: testSource{stream: stream}, SessionID: "session-1", SocketPath: socketServer.SocketPath()})
	if err != nil {
		t.Fatal(err)
	}
	runContext, stopRuntime := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runtime.Run(runContext) }()

	waitForMainState(t, socketServer.SocketPath(), "running")
	stream.events <- Event{EventID: "waiting", SourceOrder: 2, Run: Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "waiting_user", Reason: "approval"}}
	stream.events <- Event{EventID: "finished", SourceOrder: 3, Run: Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "succeeded"}}
	waitForMainState(t, socketServer.SocketPath(), "succeeded")
	stopRuntime()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	stopServer()
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	stopCore()
	if err := <-coreDone; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRetriesIdenticalWriteAfterLostResponse(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-adapter-retry-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	listener, err := net.Listen("unix", filepath.Join(directory, "agentflare.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	retried := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		var firstSync []byte
		for requestNumber := 0; requestNumber < 4; requestNumber++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverDone <- acceptErr
				return
			}
			line, readErr := bufio.NewReader(connection).ReadBytes('\n')
			if readErr != nil {
				_ = connection.Close()
				serverDone <- readErr
				return
			}
			switch requestNumber {
			case 0:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"binding_id\":\"binding-1\",\"next_seq\":1,\"lease_ms\":15000}}\n")
			case 1:
				firstSync = append([]byte(nil), line...)
				_ = connection.Close()
				continue
			case 2:
				if !bytes.Equal(firstSync, line) {
					_ = connection.Close()
					serverDone <- fmt.Errorf("retry changed request: first=%s retry=%s", firstSync, line)
					return
				}
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"accepted_seq\":1}}\n")
				close(retried)
			case 3:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"accepted_seq\":2,\"unbound\":true}}\n")
			}
			_ = connection.Close()
		}
		serverDone <- nil
	}()

	stream := newTestStream(Snapshot{})
	runtime, err := New(Options{Source: testSource{stream: stream}, SessionID: "session-1", SocketPath: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runtime.Run(ctx) }()
	select {
	case <-retried:
	case <-time.After(time.Second):
		t.Fatal("adapter did not retry the lost response")
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRetriesIdenticalBindAfterLostResponse(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-adapter-bind-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	listener, err := net.Listen("unix", filepath.Join(directory, "agentflare.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	synced := make(chan struct{})
	go func() {
		var firstBind []byte
		for requestNumber := 0; requestNumber < 4; requestNumber++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverDone <- acceptErr
				return
			}
			line, readErr := bufio.NewReader(connection).ReadBytes('\n')
			if readErr != nil {
				_ = connection.Close()
				serverDone <- readErr
				return
			}
			switch requestNumber {
			case 0:
				firstBind = append([]byte(nil), line...)
				_ = connection.Close()
				continue
			case 1:
				if !bytes.Equal(firstBind, line) {
					serverDone <- fmt.Errorf("retry changed bind request: first=%s retry=%s", firstBind, line)
					_ = connection.Close()
					return
				}
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"binding_id\":\"binding-1\",\"next_seq\":1,\"lease_ms\":15000}}\n")
			case 2:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"accepted_seq\":1}}\n")
				close(synced)
			case 3:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"accepted_seq\":2,\"unbound\":true}}\n")
			}
			_ = connection.Close()
		}
		serverDone <- nil
	}()
	runtime, err := New(Options{Source: testSource{stream: newTestStream(Snapshot{})}, SessionID: "session-1", SocketPath: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runtime.Run(ctx) }()
	select {
	case <-synced:
	case <-time.After(time.Second):
		t.Fatal("adapter did not finish bind and sync")
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeDoesNotRetryOversizeRequest(t *testing.T) {
	runtime := &Runtime{client: socketClient{path: "/does/not/matter"}}
	request := protocol.Request{Version: protocol.DisplayVersion, Type: protocol.DisplayEvent, V2: &protocol.DisplayRequest{Type: protocol.DisplayEvent, BindingID: strings.Repeat("x", protocol.MaxRequestBytes), Seq: 1, HasSeq: true}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := runtime.write(ctx, request); !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("write error = %v, want %v", err, ErrRequestTooLarge)
	}
}

func TestSocketClientRejectsOversizedResponse(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-response-limit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	listener, err := net.Listen("unix", filepath.Join(directory, "agentflare.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = bufio.NewReader(connection).ReadBytes('\n')
		_, _ = connection.Write([]byte(strings.Repeat("x", protocol.MaxResponseBytes+1) + "\n"))
	}()

	_, err = (socketClient{path: listener.Addr().String()}).send(
		context.Background(),
		protocol.Request{Version: protocol.DisplayVersion, Type: protocol.Status},
		nil,
	)
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("response error = %v, want %v", err, ErrResponseTooLarge)
	}
	<-serverDone
}

func TestRuntimeSkipsHeartbeatsWithoutSourceLiveness(t *testing.T) {
	previousInterval := heartbeatInterval
	heartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = previousInterval })
	directory, err := os.MkdirTemp("/tmp", "af-adapter-liveness-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	listener, err := net.Listen("unix", filepath.Join(directory, "agentflare.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	types := make(chan string, 3)
	serverDone := make(chan error, 1)
	go func() {
		for number := 0; number < 3; number++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverDone <- acceptErr
				return
			}
			var request struct {
				Type string `json:"type"`
			}
			line, readErr := bufio.NewReader(connection).ReadBytes('\n')
			if readErr == nil {
				readErr = json.Unmarshal(line, &request)
			}
			if readErr != nil {
				_ = connection.Close()
				serverDone <- readErr
				return
			}
			types <- request.Type
			switch number {
			case 0:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"binding_id\":\"binding-1\",\"next_seq\":1,\"lease_ms\":15000}}\n")
			case 1:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"accepted_seq\":1}}\n")
			case 2:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"accepted_seq\":2,\"unbound\":true}}\n")
			}
			_ = connection.Close()
		}
		serverDone <- nil
	}()
	stream := newTestStream(Snapshot{})
	stream.capabilities.Liveness = false
	runtime, err := New(Options{Source: testSource{stream: stream}, SessionID: "session-1", SocketPath: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runtime.Run(ctx) }()
	select {
	case <-time.After(30 * time.Millisecond):
	case err := <-runDone:
		t.Fatalf("adapter stopped before cancellation: %v", err)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	got := []string{<-types, <-types, <-types}
	want := []string{"display_bind", "display_sync", "display_unbind"}
	if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("request types = %v, want %v", got, want)
	}
}

func TestCollectBoundsPendingEventsAndSignalsOverflow(t *testing.T) {
	runtime := &Runtime{}
	events := make(chan Event, MaxPendingEvents+1)
	for index := 0; index <= MaxPendingEvents; index++ {
		events <- Event{EventID: fmt.Sprintf("event-%d", index)}
	}
	close(events)
	queue := make(chan Event, MaxPendingEvents)
	overflow := make(chan struct{}, 1)
	runtime.collect(context.Background(), events, queue, overflow)
	if len(queue) != MaxPendingEvents {
		t.Fatalf("queued events = %d, want %d", len(queue), MaxPendingEvents)
	}
	select {
	case <-overflow:
	default:
		t.Fatal("overflow was not signaled")
	}
	if runtime.Diagnostics().QueueLength != MaxPendingEvents {
		t.Fatalf("diagnostic queue length = %d", runtime.Diagnostics().QueueLength)
	}
}

func TestAdapterGoldenV2EventJSON(t *testing.T) {
	request := eventRequest("binding-1", 7, Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "waiting_user", Reason: "approval"})
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"agent_id":"main","binding_id":"binding-1","reason":"approval","role":"main","run_id":"turn-1","seq":7,"state":"waiting_user","type":"display_event","version":2}`
	if string(payload) != want {
		t.Fatalf("event JSON = %s, want %s", payload, want)
	}
}

func TestRuntimeReopensSourceAndResynchronizesAfterSourceLoss(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-adapter-source-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	listener, err := net.Listen("unix", filepath.Join(directory, "agentflare.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	resynced := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		for requestNumber := 0; requestNumber < 4; requestNumber++ {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				serverDone <- acceptErr
				return
			}
			line, readErr := bufio.NewReader(connection).ReadBytes('\n')
			if readErr != nil {
				_ = connection.Close()
				serverDone <- readErr
				return
			}
			switch requestNumber {
			case 0:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"binding_id\":\"binding-old\",\"next_seq\":1,\"lease_ms\":15000}}\n")
			case 1:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"accepted_seq\":1}}\n")
			case 2:
				if !bytes.Contains(line, []byte("\"type\":\"display_sync\"")) || !bytes.Contains(line, []byte("\"binding_id\":\"binding-old\"")) {
					serverDone <- fmt.Errorf("resync did not reuse current binding: %s", line)
					_ = connection.Close()
					return
				}
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"accepted_seq\":2}}\n")
				close(resynced)
			case 3:
				_, _ = fmt.Fprint(connection, "{\"ok\":true,\"result\":{\"accepted_seq\":3,\"unbound\":true}}\n")
			}
			_ = connection.Close()
		}
		serverDone <- nil
	}()
	first := newTestStream(Snapshot{})
	second := newTestStream(Snapshot{})
	source := &reopeningSource{streams: []*testStream{first, second}}
	runtime, err := New(Options{Source: source, SessionID: "session-1", SocketPath: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- runtime.Run(ctx) }()
	first.errors <- errors.New("source disconnected")
	select {
	case <-resynced:
	case <-time.After(time.Second):
		t.Fatal("adapter did not resynchronize after source loss")
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if source.opens != 2 {
		t.Fatalf("source opens = %d, want 2", source.opens)
	}
}

func waitForMainState(t *testing.T, socketPath, want string) {
	t.Helper()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		var status protocol.DisplayStatusResult
		apiErr, err := (socketClient{path: socketPath}).send(context.Background(), protocol.Request{Version: protocol.DisplayVersion, Type: protocol.Status}, &status)
		if err == nil && apiErr == nil && status.MainState == want {
			return
		}
		select {
		case <-timeout.C:
			t.Fatalf("main run did not reach state %q", want)
		case <-tick.C:
		}
	}
}

type testSource struct{ stream *testStream }

func (s testSource) Name() string                                 { return "replay" }
func (s testSource) Open(context.Context, string) (Stream, error) { return s.stream, nil }

type testStream struct {
	snapshot     Snapshot
	events       chan Event
	errors       chan error
	capabilities Capabilities
	once         sync.Once
}

func newTestStream(snapshot Snapshot) *testStream {
	return &testStream{snapshot: snapshot, events: make(chan Event, 8), errors: make(chan error), capabilities: Capabilities{Snapshot: true, LiveEvents: true, RunIdentity: true, WaitingUser: true, Terminal: true, Cancellation: true, Liveness: true}}
}
func (s *testStream) Capabilities() Capabilities {
	return s.capabilities
}
func (s *testStream) Snapshot(context.Context) (Snapshot, error) { return s.snapshot, nil }
func (s *testStream) Events() <-chan Event                       { return s.events }
func (s *testStream) Errors() <-chan error                       { return s.errors }
func (s *testStream) Close() error {
	s.once.Do(func() { close(s.events); close(s.errors) })
	return nil
}

type reopeningSource struct {
	streams []*testStream
	opens   int
}

func (s *reopeningSource) Name() string { return "replay" }
func (s *reopeningSource) Open(context.Context, string) (Stream, error) {
	if s.opens >= len(s.streams) {
		return nil, errors.New("no more streams")
	}
	stream := s.streams[s.opens]
	s.opens++
	return stream, nil
}
