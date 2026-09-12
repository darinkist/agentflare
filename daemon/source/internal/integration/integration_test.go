package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-macos/iokit/hid"
	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
	"github.com/konstantinrink/agentflare/daemon/source/internal/km16pro"
	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
	"github.com/konstantinrink/agentflare/daemon/source/internal/server"
)

type integrationTransport struct {
	mu             sync.Mutex
	info           hid.Info
	responses      chan []byte
	removed        chan struct{}
	removeOnce     sync.Once
	requests       [][32]byte
	requestWritten chan struct{}
	currentEffect  byte
}

func newIntegrationTransport(effect byte) *integrationTransport {
	return &integrationTransport{
		info:           hid.Info{VendorID: km16pro.VendorID, ProductID: km16pro.ProductID, UsagePage: km16pro.UsagePage, Usage: km16pro.Usage},
		responses:      make(chan []byte, 32),
		requestWritten: make(chan struct{}, 128),
		removed:        make(chan struct{}),
		currentEffect:  effect,
	}
}

func (t *integrationTransport) Info() hid.Info { return t.info }
func (t *integrationTransport) Open() error    { return nil }
func (t *integrationTransport) SetReport(kind hid.ReportKind, id byte, data []byte) error {
	if kind != hid.Output || id != 0 || len(data) != 32 {
		return errors.New("unexpected report parameters")
	}
	var request [32]byte
	copy(request[:], data)
	t.mu.Lock()
	t.requests = append(t.requests, request)
	effect := t.currentEffect
	t.mu.Unlock()
	t.requestWritten <- struct{}{}
	response := request
	if request[0] == 0x08 {
		response[3] = effect
	}
	t.responses <- response[:]
	return nil
}

func waitForLEDRequest(t *testing.T, transport *integrationTransport, index int, color protocol.Color) {
	t.Helper()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		for _, request := range transport.requestsSnapshot() {
			if request[0] == 0x07 && request[1] == 0x00 && request[2] == 0x01 && request[3] == byte(index) && request[4] == color.R && request[5] == color.G && request[6] == color.B {
				return
			}
		}
		select {
		case <-transport.requestWritten:
		case <-timeout.C:
			t.Fatalf("did not observe LED %d with rgb=%d,%d,%d", index, color.R, color.G, color.B)
		}
	}
}

func (t *integrationTransport) Stream(ctx context.Context, deliver func([]byte)) error {
	for {
		select {
		case response := <-t.responses:
			deliver(response)
		case <-t.removed:
			return errors.New("device removed")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *integrationTransport) Close() error { return nil }
func (t *integrationTransport) remove()      { t.removeOnce.Do(func() { close(t.removed) }) }

func (t *integrationTransport) requestsSnapshot() [][32]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([][32]byte(nil), t.requests...)
}

type integrationSource struct {
	mu      sync.Mutex
	current km16pro.Transport
}

func (s *integrationSource) Devices() ([]km16pro.Transport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil, nil
	}
	return []km16pro.Transport{s.current}, nil
}

func (s *integrationSource) setCurrent(transport km16pro.Transport) {
	s.mu.Lock()
	s.current = transport
	s.mu.Unlock()
}

func sendRequest(t *testing.T, socketPath string, request protocol.Request) protocol.Envelope {
	t.Helper()
	connection, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if _, err := connection.Write(data); err != nil {
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

func waitForStatus(t *testing.T, statuses <-chan km16pro.Status, want km16pro.Status) {
	t.Helper()
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		select {
		case status := <-statuses:
			if status == want {
				return
			}
		case <-timeout.C:
			t.Fatalf("did not receive device status %q", want)
		}
	}
}

func TestSocketCoreDriverReconnectIntegration(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "af-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	first := newIntegrationTransport(2)
	source := &integrationSource{current: first}
	statuses := make(chan km16pro.Status, 16)
	stateCore := core.New(nil)
	coreContext, coreCancel := context.WithCancel(context.Background())
	defer coreCancel()
	go func() { _ = stateCore.Run(coreContext) }()
	driver := km16pro.New(source, km16pro.Options{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		DiscoveryInterval: time.Millisecond,
		StatusSink: func(statusContext context.Context, status km16pro.Status) error {
			statuses <- status
			return stateCore.SetDeviceStatus(statusContext, core.DeviceStatus(status))
		},
	})
	driverContext, driverCancel := context.WithCancel(context.Background())
	defer driverCancel()
	driverDone := make(chan error, 1)
	go func() { driverDone <- driver.Run(driverContext, stateCore.Snapshots()) }()
	waitForStatus(t, statuses, km16pro.Connected)

	socketServer := server.New(stateCore, server.Options{SocketPath: filepath.Join(directory, "agentflare.sock")})
	if err := socketServer.Listen(); err != nil {
		t.Fatal(err)
	}
	serverContext, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- socketServer.Serve(serverContext) }()

	started := sendRequest(t, socketServer.SocketPath(), protocol.Request{Version: protocol.Version, Type: protocol.Started, Source: "demo", AgentID: "alpha"})
	if !started.OK {
		t.Fatalf("started response = %#v", started)
	}
	waitForLEDRequest(t, first, 12, protocol.Color{B: 255})

	manual := sendRequest(t, socketServer.SocketPath(), protocol.Request{Version: protocol.Version, Type: protocol.LightsSet, Target: &protocol.Target{Zone: "backglow"}, Color: &protocol.Color{R: 1, G: 2, B: 3}})
	if !manual.OK {
		t.Fatalf("lights set response = %#v", manual)
	}
	waitForLEDRequest(t, first, 21, protocol.Color{R: 1, G: 2, B: 3})
	lightStatus := sendRequest(t, socketServer.SocketPath(), protocol.Request{Version: protocol.Version, Type: protocol.Status})
	if !lightStatus.OK {
		t.Fatalf("online status response = %#v", lightStatus)
	}
	statusBytes, err := json.Marshal(lightStatus.Result)
	if err != nil {
		t.Fatal(err)
	}
	var onlineStatus protocol.StatusResult
	if err := json.Unmarshal(statusBytes, &onlineStatus); err != nil {
		t.Fatal(err)
	}
	if len(onlineStatus.Lights) != core.LEDCount {
		t.Fatalf("light count = %d, want %d", len(onlineStatus.Lights), core.LEDCount)
	}
	clearedLights := sendRequest(t, socketServer.SocketPath(), protocol.Request{Version: protocol.Version, Type: protocol.LightsClear, Target: &protocol.Target{Zone: "backglow"}})
	if !clearedLights.OK {
		t.Fatalf("lights clear response = %#v", clearedLights)
	}
	waitForLEDRequest(t, first, 21, protocol.Color{})

	socketServerPath := socketServer.SocketPath()
	first.remove()
	source.setCurrent(nil)
	waitForStatus(t, statuses, km16pro.Disconnected)
	finished := sendRequest(t, socketServerPath, protocol.Request{Version: protocol.Version, Type: protocol.Finished, Source: "demo", AgentID: "alpha"})
	if !finished.OK {
		t.Fatalf("offline finished response = %#v", finished)
	}
	status := sendRequest(t, socketServerPath, protocol.Request{Version: protocol.Version, Type: protocol.Status})
	if !status.OK {
		t.Fatalf("offline status response = %#v", status)
	}
	statusData, err := json.Marshal(status.Result)
	if err != nil {
		t.Fatal(err)
	}
	var offlineStatus protocol.StatusResult
	if err := json.Unmarshal(statusData, &offlineStatus); err != nil {
		t.Fatal(err)
	}
	if offlineStatus.Device.Status != string(km16pro.Disconnected) || offlineStatus.Slots[0].State != string(core.Finished) {
		t.Fatalf("offline status result = %#v", offlineStatus)
	}

	second := newIntegrationTransport(2)
	source.setCurrent(second)
	waitForStatus(t, statuses, km16pro.Connected)
	secondRequests := second.requestsSnapshot()
	if len(secondRequests) < 15 || secondRequests[14][3] != 12 || !bytes.Equal(secondRequests[14][4:7], []byte{0, 255, 0}) {
		t.Fatalf("reconnect requests = % x", secondRequests)
	}

	cleared := sendRequest(t, socketServerPath, protocol.Request{Version: protocol.Version, Type: protocol.Clear})
	if !cleared.OK {
		t.Fatalf("clear response = %#v", cleared)
	}
	driverCancel()
	serverCancel()
	if err := <-driverDone; err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
