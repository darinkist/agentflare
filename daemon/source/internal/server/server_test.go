package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
)

type runningServer struct {
	server *Server
	cancel context.CancelFunc
	done   chan error
}

func startTestServer(t *testing.T) runningServer {
	return startTestServerWithOptions(t, Options{})
}

func startTestServerWithOptions(t *testing.T, options Options) runningServer {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "af-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	stateCore := core.New(nil)
	coreContext, coreCancel := context.WithCancel(context.Background())
	go func() { _ = stateCore.Run(coreContext) }()
	options.SocketPath = filepath.Join(directory, "agentflare.sock")
	socketServer := New(stateCore, options)
	if err := socketServer.Listen(); err != nil {
		t.Fatal(err)
	}
	serverContext, serverCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- socketServer.Serve(serverContext) }()
	t.Cleanup(func() {
		serverCancel()
		coreCancel()
		if err := <-done; err != nil {
			t.Errorf("server shutdown: %v", err)
		}
	})
	return runningServer{server: socketServer, cancel: serverCancel, done: done}
}

func waitForConnectionCount(t *testing.T, server *Server, want int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		server.connectionsMu.Lock()
		count := len(server.connections)
		server.connectionsMu.Unlock()
		if count == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("connection count = %d, want %d", count, want)
		case <-ticker.C:
		}
	}
}

func request(t *testing.T, socketPath string, data string) protocol.Envelope {
	t.Helper()
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(data + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response protocol.Envelope
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestSocketAPIAndStatus(t *testing.T) {
	running := startTestServer(t)
	started := request(t, running.server.socketPath, `{"version":1,"type":"started","source":"demo","agent_id":"alpha"}`)
	if !started.OK {
		t.Fatalf("started response = %#v", started)
	}
	status := request(t, running.server.socketPath, `{"version":1,"type":"status"}`)
	if !status.OK {
		t.Fatalf("status response = %#v", status)
	}
	data, err := json.Marshal(status.Result)
	if err != nil {
		t.Fatal(err)
	}
	var result protocol.StatusResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Device.Status != "disconnected" || len(result.Slots) != core.SlotCount {
		t.Fatalf("status result = %#v", result)
	}
	if result.Slots[0].State != "active" || result.Slots[0].LEDIndex != 12 || result.Slots[0].AgentID != "alpha" {
		t.Fatalf("slot status = %#v", result.Slots[0])
	}
}

func TestLightsAPIAndResolvedStatus(t *testing.T) {
	running := startTestServer(t)
	set := request(t, running.server.socketPath, `{"version":1,"type":"lights_set","target":{"zone":"backglow"},"color":{"r":0,"g":0,"b":64},"ttl_ms":2000}`)
	if !set.OK {
		t.Fatalf("lights_set response = %#v", set)
	}
	setData, err := json.Marshal(set.Result)
	if err != nil {
		t.Fatal(err)
	}
	var lightResult protocol.LightResult
	if err := json.Unmarshal(setData, &lightResult); err != nil {
		t.Fatal(err)
	}
	if len(lightResult.LEDIndices) != 6 || lightResult.LEDIndices[0] != 21 || lightResult.ExpiresAt == "" {
		t.Fatalf("light result = %#v", lightResult)
	}

	status := request(t, running.server.socketPath, `{"version":1,"type":"status"}`)
	if !status.OK {
		t.Fatalf("status response = %#v", status)
	}
	statusData, err := json.Marshal(status.Result)
	if err != nil {
		t.Fatal(err)
	}
	var result protocol.StatusResult
	if err := json.Unmarshal(statusData, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Lights) != core.LEDCount {
		t.Fatalf("light status count = %d, want %d", len(result.Lights), core.LEDCount)
	}
	if result.Lights[21].Layer != string(core.LayerManualTTL) || result.Lights[21].Color.B != 64 {
		t.Fatalf("resolved backglow = %#v", result.Lights[21])
	}
}

func TestRequestValidationAndOneRequestPerConnection(t *testing.T) {
	running := startTestServer(t)
	for _, test := range []struct {
		data string
		code string
	}{
		{data: `{"version":3,"type":"status"}`, code: "unsupported_version"},
		{data: `{"version":1,"type":"unknown"}`, code: "unknown_event_type"},
		{data: `{"version":1,"type":"started"}`, code: "invalid_request"},
	} {
		response := request(t, running.server.socketPath, test.data)
		if response.OK || response.Error == nil || response.Error.Code != test.code {
			t.Fatalf("response = %#v, want %s", response, test.code)
		}
	}

	connection, err := net.DialTimeout("unix", running.server.socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(`{"version":1,"type":"status"}` + "\n" + `{"version":1,"type":"status"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	if _, err := reader.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadBytes('\n'); err == nil {
		t.Fatal("server accepted more than one request")
	}
}

func TestRequestSizeLimit(t *testing.T) {
	running := startTestServer(t)
	connection, err := net.DialTimeout("unix", running.server.socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte(strings.Repeat("x", protocol.MaxRequestBytes+1) + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response protocol.Envelope
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "invalid_request" {
		t.Fatalf("oversized response = %#v", response)
	}
}

func TestConnectionLimitRejectsAndCleansUp(t *testing.T) {
	running := startTestServerWithOptions(t, Options{
		MaxConnections: 1,
		ReadTimeout:    time.Second,
	})
	first, err := net.DialTimeout("unix", running.server.socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Write([]byte(`{"version":1`)); err != nil {
		t.Fatal(err)
	}
	waitForConnectionCount(t, running.server, 1)

	second, err := net.DialTimeout("unix", running.server.socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(second).ReadBytes('\n')
	_ = second.Close()
	if err != nil {
		t.Fatal(err)
	}
	var response protocol.Envelope
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Error == nil || response.Error.Code != "server_busy" {
		t.Fatalf("limited response = %#v", response)
	}

	first.Close()
	waitForConnectionCount(t, running.server, 0)
	status := request(t, running.server.socketPath, `{"version":1,"type":"status"}`)
	if !status.OK {
		t.Fatalf("request after cleanup = %#v", status)
	}
}

func TestStaleSocketAndSecondDaemon(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "agentflare.sock")
	staleListener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := staleListener.Close(); err != nil {
		t.Fatal(err)
	}

	stateCore := core.New(nil)
	first := New(stateCore, Options{SocketPath: socketPath})
	if err := first.Listen(); err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatal(err)
	}
	second := New(stateCore, Options{SocketPath: socketPath})
	if err := second.Listen(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second listen error = %v, want %v", err, ErrAlreadyRunning)
	}
	info, err := os.Stat(socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o, want 700", directoryInfo.Mode().Perm())
	}
}

func TestProbeFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		stale bool
	}{
		{name: "refused", err: &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}, stale: true},
		{name: "missing", err: &net.OpError{Op: "dial", Net: "unix", Err: syscall.ENOENT}, stale: true},
		{name: "timeout", err: &net.OpError{Op: "dial", Net: "unix", Err: os.ErrDeadlineExceeded}},
		{name: "permission", err: &net.OpError{Op: "dial", Net: "unix", Err: syscall.EACCES}},
		{name: "reset", err: &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNRESET}},
		{name: "other", err: errors.New("dial unix: unexpected probe failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if stale := isStaleSocketError(test.err); stale != test.stale {
				t.Fatalf("isStaleSocketError(%v) = %v, want %v", test.err, stale, test.stale)
			}
		})
	}
}

func bindStaleSocket(t *testing.T, socketPath string) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: socketPath}); err != nil {
		t.Fatal(err)
	}
}

func TestListenKeepsSocketOnAmbiguousProbeError(t *testing.T) {
	for _, probeErr := range []error{
		&net.OpError{Op: "dial", Net: "unix", Err: os.ErrDeadlineExceeded},
		&net.OpError{Op: "dial", Net: "unix", Err: syscall.EACCES},
		errors.New("dial unix: unexpected probe failure"),
	} {
		directory, err := os.MkdirTemp("/tmp", "af-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(directory) })
		socketPath := filepath.Join(directory, "agentflare.sock")
		bindStaleSocket(t, socketPath)
		previous := dialUnix
		dialUnix = func(string, string, time.Duration) (net.Conn, error) {
			return nil, probeErr
		}
		server := New(core.New(nil), Options{SocketPath: socketPath})
		err = server.Listen()
		dialUnix = previous
		if err == nil {
			_ = server.Close()
			t.Fatalf("Listen succeeded after inconclusive probe failure %v", probeErr)
		}
		if errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("Listen reported a running daemon after inconclusive probe failure %v", probeErr)
		}
		if _, statErr := os.Lstat(socketPath); statErr != nil {
			t.Fatalf("socket was removed after inconclusive probe failure %v: %v", probeErr, statErr)
		}
	}
}

func TestListenRemovesRefusedSocket(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "agentflare.sock")
	bindStaleSocket(t, socketPath)
	server := New(core.New(nil), Options{SocketPath: socketPath})
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen after refused socket = %v", err)
	}
	defer server.Close()
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatalf("dial new daemon = %v", err)
	}
	_ = connection.Close()
}

func TestListenTreatsVanishedSocketAsStale(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socketPath := filepath.Join(directory, "agentflare.sock")
	bindStaleSocket(t, socketPath)
	previous := dialUnix
	dialUnix = func(string, string, time.Duration) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.ENOENT}
	}
	defer func() { dialUnix = previous }()
	server := New(core.New(nil), Options{SocketPath: socketPath})
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen after vanished socket = %v", err)
	}
	defer server.Close()
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatalf("dial new daemon = %v", err)
	}
	_ = connection.Close()
}
