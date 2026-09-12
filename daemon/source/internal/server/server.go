// Package server exposes the core through the local Unix socket API.
package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
)

var ErrAlreadyRunning = errors.New("daemon already running")

const (
	DefaultReadTimeout    = 5 * time.Second
	DefaultWriteTimeout   = 5 * time.Second
	DefaultRequestTimeout = 5 * time.Second
	DefaultMaxConnections = 64
)

// DefaultSocketPath returns the per-user daemon socket location.
func DefaultSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "agentflare", "agentflare.sock"), nil
}

type Options struct {
	SocketPath      string
	Logger          *slog.Logger
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	RequestTimeout  time.Duration
	MaxRequestBytes int
	MaxConnections  int
}

// Server accepts exactly one request per connection.
type Server struct {
	core            *core.Core
	socketPath      string
	logger          *slog.Logger
	readTimeout     time.Duration
	writeTimeout    time.Duration
	requestTimeout  time.Duration
	maxRequestBytes int
	maxConnections  int
	connectionSlots chan struct{}
	listener        net.Listener
	connectionsMu   sync.Mutex
	connections     map[net.Conn]struct{}
	connectionsWG   sync.WaitGroup
}

// New creates a Unix socket server for c.
func New(c *core.Core, options Options) *Server {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if options.ReadTimeout <= 0 {
		options.ReadTimeout = DefaultReadTimeout
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = DefaultWriteTimeout
	}
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = DefaultRequestTimeout
	}
	if options.MaxRequestBytes <= 0 {
		options.MaxRequestBytes = protocol.MaxRequestBytes
	}
	if options.MaxConnections <= 0 {
		options.MaxConnections = DefaultMaxConnections
	}
	return &Server{
		core:            c,
		socketPath:      options.SocketPath,
		logger:          logger,
		readTimeout:     options.ReadTimeout,
		writeTimeout:    options.WriteTimeout,
		requestTimeout:  options.RequestTimeout,
		maxRequestBytes: options.MaxRequestBytes,
		maxConnections:  options.MaxConnections,
		connectionSlots: make(chan struct{}, options.MaxConnections),
		connections:     make(map[net.Conn]struct{}),
	}
}

// SocketPath returns the configured Unix socket path.
func (s *Server) SocketPath() string { return s.socketPath }

// dialUnix probes whether a daemon answers behind an existing socket. It is a
// variable so tests can simulate inconclusive probe failures.
var dialUnix = func(network, address string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout(network, address, timeout)
}

// isStaleSocketError reports whether a failed liveness probe proves that no
// daemon answers behind the socket. Only connection-refused and missing-file
// failures are conclusive: the kernel reports ECONNREFUSED for a socket file
// without a listener and ENOENT when the file vanished between inspection and
// probe. Timeouts, permission errors and every other failure leave the daemon
// state unknown, so the socket must be preserved and no second daemon started.
func isStaleSocketError(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Timeout() {
		return false
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// Listen prepares the secure socket directory and removes only an unreachable
// stale Unix socket.
func (s *Server) Listen() error {
	if s.socketPath == "" {
		return errors.New("socket path is required")
	}
	directory := filepath.Dir(s.socketPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create socket directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure socket directory: %w", err)
	}
	if info, err := os.Lstat(s.socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("socket path exists and is not a Unix socket")
		}
		connection, dialErr := dialUnix("unix", s.socketPath, 100*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return ErrAlreadyRunning
		}
		if !isStaleSocketError(dialErr) {
			return fmt.Errorf("probe daemon socket: %w", dialErr)
		}
		if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect socket: %w", err)
	}

	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on socket: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(s.socketPath)
		return fmt.Errorf("secure socket: %w", err)
	}
	s.listener = listener
	s.logger.Info("socket listening", "path", s.socketPath)
	return nil
}

// Serve accepts requests until ctx is canceled or the listener fails.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("server is not listening")
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = s.listener.Close()
			s.closeConnections()
		case <-stop:
		}
	}()
	defer close(stop)
	defer func() {
		s.closeConnections()
		s.connectionsWG.Wait()
		_ = os.Remove(s.socketPath)
	}()

	for {
		connection, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept socket connection: %w", err)
		}
		select {
		case s.connectionSlots <- struct{}{}:
			// The connection is admitted below.
		default:
			// Reject promptly. No goroutine or connection-map entry is made,
			// and the caller can retry after an admitted request completes.
			s.write(connection, protocol.Failure(protocol.APIError{Code: "server_busy", Message: "connection limit reached"}))
			_ = connection.Close()
			continue
		}
		s.connectionsMu.Lock()
		s.connections[connection] = struct{}{}
		s.connectionsWG.Add(1)
		s.connectionsMu.Unlock()
		go func() {
			defer s.connectionsWG.Done()
			defer func() { <-s.connectionSlots }()
			defer func() {
				s.connectionsMu.Lock()
				delete(s.connections, connection)
				s.connectionsMu.Unlock()
			}()
			s.handle(ctx, connection)
		}()
	}
}

// Close stops accepting new connections.
func (s *Server) Close() error {
	if s.listener == nil {
		return nil
	}
	err := s.listener.Close()
	s.closeConnections()
	return err
}

func (s *Server) closeConnections() {
	s.connectionsMu.Lock()
	defer s.connectionsMu.Unlock()
	for connection := range s.connections {
		_ = connection.Close()
	}
}

func (s *Server) handle(serverContext context.Context, connection net.Conn) {
	defer connection.Close()
	_ = connection.SetReadDeadline(time.Now().Add(s.readTimeout))
	line, err := readLineLimited(bufio.NewReader(connection), s.maxRequestBytes)
	if err != nil {
		s.write(connection, protocol.Failure(protocol.APIError{Code: "invalid_request", Message: "request is invalid"}))
		return
	}
	request, apiErr := protocol.ParseRequest(line)
	if apiErr != nil {
		s.write(connection, protocol.Failure(*apiErr))
		return
	}

	requestContext, cancel := context.WithTimeout(serverContext, s.requestTimeout)
	defer cancel()
	event, err := eventFromRequest(request)
	if err != nil {
		s.write(connection, protocol.Failure(protocol.APIError{Code: "invalid_request", Message: "request is invalid"}))
		return
	}
	result, err := s.core.Submit(requestContext, event)
	if err != nil {
		var coreErr *core.APIError
		if errors.As(err, &coreErr) {
			s.write(connection, protocol.Failure(protocol.APIError{Code: coreErr.Code, Message: coreErr.Message}))
			return
		}
		s.write(connection, protocol.Failure(protocol.APIError{Code: "invalid_request", Message: "request could not be processed"}))
		return
	}

	var response protocol.Envelope
	if request.Version == protocol.DisplayVersion {
		response = displayResponse(request.Type, result)
	} else if request.Type == protocol.Status {
		response = protocol.Success(statusResult(result.Status))
	} else if request.Type == protocol.Clear {
		response = protocol.Success(map[string]any{"cleared": true})
	} else if request.Type == protocol.LightsSet || request.Type == protocol.LightsClear {
		response = protocol.Success(lightResult(result.Lights))
	} else {
		response = protocol.Success(map[string]int{"slot": result.Slot})
	}
	s.write(connection, response)
}

func eventFromRequest(request protocol.Request) (core.Event, error) {
	if request.Version == protocol.DisplayVersion {
		return displayEventFromRequest(request)
	}
	event := core.Event{
		Type:     core.EventType(request.Type),
		Identity: core.AgentIdentity{Source: request.Source, AgentID: request.AgentID},
	}
	if request.Type != protocol.LightsSet && request.Type != protocol.LightsClear {
		return event, nil
	}
	if request.Target == nil {
		return core.Event{}, errors.New("light target is required")
	}
	indices, ok := request.Target.Indices()
	if !ok {
		return core.Event{}, errors.New("light target is invalid")
	}
	event.LEDIndices = indices
	if request.Type == protocol.LightsSet {
		if request.Color == nil {
			return core.Event{}, errors.New("light color is required")
		}
		event.Color = core.Color{R: request.Color.R, G: request.Color.G, B: request.Color.B}
		if request.TTLMS != nil {
			event.HasTTL = true
			event.TTL = time.Duration(*request.TTLMS) * time.Millisecond
		}
	}
	return event, nil
}

func displayEventFromRequest(request protocol.Request) (core.Event, error) {
	if request.V2 == nil {
		return core.Event{}, errors.New("display request is missing")
	}
	displayRequest := request.V2
	event := core.DisplayEvent{
		Operation:     core.DisplayOperation(displayRequest.Type),
		Source:        displayRequest.Source,
		SessionID:     displayRequest.SessionID,
		BindID:        displayRequest.BindID,
		Takeover:      displayRequest.Takeover,
		BindingID:     displayRequest.BindingID,
		Seq:           displayRequest.Seq,
		Role:          core.DisplayRole(displayRequest.Role),
		AgentID:       displayRequest.AgentID,
		RunID:         displayRequest.RunID,
		ParentAgentID: displayRequest.ParentAgentID,
		State:         core.DisplayState(displayRequest.State),
		Reason:        displayRequest.Reason,
		HasMain:       displayRequest.HasMain,
		Fingerprint:   append([]byte(nil), displayRequest.Fingerprint...),
	}
	if displayRequest.Main != nil {
		event.Main = &core.DisplaySnapshot{
			AgentID:       displayRequest.Main.AgentID,
			RunID:         displayRequest.Main.RunID,
			ParentAgentID: displayRequest.Main.ParentAgentID,
			State:         core.DisplayState(displayRequest.Main.State),
			StartOrder:    displayRequest.Main.StartOrder,
		}
	}
	event.Subagents = make([]core.DisplaySnapshot, 0, len(displayRequest.Subagents))
	for _, subagent := range displayRequest.Subagents {
		event.Subagents = append(event.Subagents, core.DisplaySnapshot{
			AgentID:       subagent.AgentID,
			RunID:         subagent.RunID,
			ParentAgentID: subagent.ParentAgentID,
			State:         core.DisplayState(subagent.State),
			StartOrder:    subagent.StartOrder,
		})
	}
	return core.Event{Display: &event}, nil
}

func statusResult(status core.StatusView) protocol.StatusResult {
	result := protocol.StatusResult{
		Device: protocol.DeviceStatus{Status: string(status.Device)},
		Slots:  make([]protocol.SlotStatus, 0, core.SlotCount),
	}
	for _, slot := range status.Slots {
		view := protocol.SlotStatus{
			Slot:     slot.Number,
			LEDIndex: slot.LEDIndex,
			State:    string(slot.State),
			Color:    protocol.Color{R: slot.Color.R, G: slot.Color.G, B: slot.Color.B},
		}
		if slot.HasIdentity {
			view.Source = slot.Identity.Source
			view.AgentID = slot.Identity.AgentID
			view.LeaseEnd = slot.LeaseEnd.Format(time.RFC3339Nano)
		}
		result.Slots = append(result.Slots, view)
	}
	result.Lights = make([]protocol.LightStatus, 0, core.LEDCount)
	for _, light := range status.Lights {
		view := protocol.LightStatus{
			LEDIndex: light.Index,
			Zone:     string(light.Zone),
			Color:    protocol.Color{R: light.Color.R, G: light.Color.G, B: light.Color.B},
			Layer:    string(light.Layer),
		}
		if light.HasExpiry {
			view.ExpiresAt = light.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		result.Lights = append(result.Lights, view)
	}
	return result
}

func displayResponse(requestType protocol.EventType, result core.Result) protocol.Envelope {
	switch requestType {
	case protocol.EventType("display_bind"):
		return protocol.Success(protocol.DisplayBindResult{
			BindingID: result.Display.BindingID,
			NextSeq:   result.Display.NextSeq,
			LeaseMS:   result.Display.LeaseMS,
		})
	case protocol.Status:
		return protocol.Success(displayStatusResult(result.Status, result.DisplayStatus))
	default:
		response := protocol.DisplayWriteResult{
			AcceptedSeq: result.Display.AcceptedSeq,
			Assignment:  result.Display.Assignment,
			QueueLength: result.Display.QueueLength,
			Unbound:     result.Display.Unbound,
		}
		if result.Display.HasSlot {
			slot := result.Display.Slot
			response.Slot = &slot
		}
		if result.Display.HasTerminalDeadline {
			response.TerminalDeadline = result.Display.TerminalDeadline.UTC().Format(time.RFC3339Nano)
		}
		if result.Display.HasLeaseEnd {
			response.LeaseEnd = result.Display.LeaseEnd.UTC().Format(time.RFC3339Nano)
		}
		return protocol.Success(response)
	}
}

func displayStatusResult(status core.StatusView, display core.DisplayStatusView) protocol.DisplayStatusResult {
	result := protocol.DisplayStatusResult{
		Health:      string(display.Health),
		BindingID:   display.BindingID,
		Source:      display.Source,
		SessionID:   display.SessionID,
		MainState:   string(display.MainState),
		Queue:       make([]protocol.DisplayRunStatus, 0, len(display.Queue)),
		QueueLength: display.QueueLength,
		Brightness: protocol.DisplayBrightnessStatus{
			Keys:      display.KeyBrightness,
			Underglow: display.UnderglowBrightness,
		},
		Device: protocol.DeviceStatus{Status: string(status.Device)},
		Slots:  make([]protocol.DisplaySlotStatus, 0, core.SlotCount),
	}
	if display.HasLeaseEnd {
		result.LeaseEnd = display.LeaseEnd.UTC().Format(time.RFC3339Nano)
	}
	if display.Main != nil {
		main := displayRunStatus(*display.Main)
		result.Main = &main
	}
	for _, slot := range display.Slots {
		view := protocol.DisplaySlotStatus{
			Slot:     slot.Number,
			LEDIndex: slot.LEDIndex,
			State:    string(slot.State),
		}
		if slot.Run != nil {
			run := displayRunStatus(*slot.Run)
			view.Run = &run
			view.Role = run.Role
			view.Source = run.Source
			view.SessionID = run.SessionID
			view.AgentID = run.AgentID
			view.RunID = run.RunID
			view.ParentAgentID = run.ParentAgentID
			view.TerminalDeadline = run.TerminalDeadline
		}
		result.Slots = append(result.Slots, view)
	}
	for _, run := range display.Queue {
		result.Queue = append(result.Queue, displayRunStatus(run))
	}
	legacy := statusResult(status)
	result.Lights = legacy.Lights
	return result
}

func displayRunStatus(run core.DisplayRunView) protocol.DisplayRunStatus {
	result := protocol.DisplayRunStatus{
		Role:          string(run.Role),
		Source:        run.Identity.Source,
		SessionID:     run.Identity.SessionID,
		AgentID:       run.Identity.AgentID,
		RunID:         run.Identity.RunID,
		ParentAgentID: run.Identity.ParentAgentID,
		State:         string(run.State),
	}
	if run.HasSlot {
		slot := run.Slot
		result.Slot = &slot
	}
	if run.HasStartOrder {
		result.StartOrder = run.StartOrder
	}
	if run.HasTerminalDeadline {
		result.TerminalDeadline = run.TerminalDeadline.UTC().Format(time.RFC3339Nano)
	}
	return result
}

func lightResult(result core.LightResult) protocol.LightResult {
	response := protocol.LightResult{LEDIndices: append([]int(nil), result.LEDIndices...)}
	if result.HasExpiry {
		response.ExpiresAt = result.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return response
}

func (s *Server) write(connection net.Conn, response protocol.Envelope) {
	_ = connection.SetWriteDeadline(time.Now().Add(s.writeTimeout))
	data, err := protocol.EncodeResponse(response)
	if err != nil {
		s.logger.Error("encode response", "error", err)
		return
	}
	for len(data) > 0 {
		n, err := connection.Write(data)
		if err != nil {
			return
		}
		data = data[n:]
	}
}

func readLineLimited(reader *bufio.Reader, max int) ([]byte, error) {
	line := make([]byte, 0, min(max, 1024))
	for {
		byteValue, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		if byteValue == '\n' {
			return line, nil
		}
		if len(line) >= max {
			return nil, errors.New("request exceeds size limit")
		}
		line = append(line, byteValue)
	}
}

func min(first, second int) int {
	if first < second {
		return first
	}
	return second
}
