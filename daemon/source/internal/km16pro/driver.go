// Package km16pro drives the single vendor HID interface of the KM16-Pro.
package km16pro

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/go-macos/iokit/hid"
	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
)

const (
	VendorID  uint16 = 0x28e9
	ProductID uint16 = 0x3145
	UsagePage uint16 = 0xff60
	Usage     uint16 = 0x61

	PauseEffect byte = 0
)

var (
	ErrProtocol    = errors.New("protocol error")
	ErrSessionLost = errors.New("HID session lost")
	ErrTransport   = errors.New("HID transport error")
)

// Status is the driver's connection status.
type Status string

const (
	Disconnected  Status = "disconnected"
	Connecting    Status = "connecting"
	Connected     Status = "connected"
	Ambiguous     Status = "ambiguous"
	ProtocolError Status = "protocol_error"
)

// DeviceSource enumerates retained HID device handles.
type DeviceSource interface {
	Devices() ([]Transport, error)
}

// Transport is the narrow HID boundary used by the driver and its tests.
type Transport interface {
	Info() hid.Info
	Open() error
	SetReport(hid.ReportKind, byte, []byte) error
	Stream(context.Context, func([]byte)) error
	Close() error
}

// StatusSink receives device state changes for the core.
type StatusSink func(context.Context, Status) error

type Options struct {
	Logger             *slog.Logger
	TransactionTimeout time.Duration
	DiscoveryInterval  time.Duration
	ShutdownTimeout    time.Duration
	StatusSink         StatusSink
}

// Driver owns discovery, one open session, one transaction executor and one
// input read-pump at a time.
type Driver struct {
	source             DeviceSource
	logger             *slog.Logger
	transactionTimeout time.Duration
	discoveryInterval  time.Duration
	shutdownTimeout    time.Duration
	statusSink         StatusSink
	status             Status
	restoreEffect      *byte
}

// New creates a driver with an injected HID source.
func New(source DeviceSource, options Options) *Driver {
	if source == nil {
		source = systemEnumerator{}
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if options.TransactionTimeout <= 0 {
		options.TransactionTimeout = 1500 * time.Millisecond
	}
	if options.DiscoveryInterval <= 0 {
		options.DiscoveryInterval = time.Second
	}
	if options.ShutdownTimeout <= 0 {
		options.ShutdownTimeout = 3 * time.Second
	}
	return &Driver{
		source:             source,
		logger:             logger,
		transactionTimeout: options.TransactionTimeout,
		discoveryInterval:  options.DiscoveryInterval,
		shutdownTimeout:    options.ShutdownTimeout,
		statusSink:         options.StatusSink,
		status:             Disconnected,
	}
}

// NewDefault creates the production macOS HID driver.
func NewDefault(options Options) *Driver { return New(systemEnumerator{}, options) }

// Run discovers and reconciles the keyboard until ctx is canceled.
func (d *Driver) Run(ctx context.Context, desired <-chan core.DesiredState) error {
	latest := core.InitialDesiredState()
	for {
		drainDesired(desired, &latest)
		devices, err := d.source.Devices()
		if err != nil {
			d.updateStatus(ctx, ProtocolError)
			d.logger.Error("device discovery failed", "error", err)
			if !waitForDiscovery(ctx, desired, &latest, d.discoveryInterval) {
				return nil
			}
			continue
		}
		if len(devices) == 0 {
			d.updateStatus(ctx, Disconnected)
			if !waitForDiscovery(ctx, desired, &latest, d.discoveryInterval) {
				return nil
			}
			continue
		}
		if len(devices) > 1 {
			closeDevices(devices)
			d.updateStatus(ctx, Ambiguous)
			if !waitForDiscovery(ctx, desired, &latest, d.discoveryInterval) {
				return nil
			}
			continue
		}

		device := devices[0]
		d.updateStatus(ctx, Connecting)
		if err := device.Open(); err != nil {
			_ = device.Close()
			d.updateStatus(ctx, Disconnected)
			d.logger.Error("device open failed", "error", err)
			if !waitForDiscovery(ctx, desired, &latest, d.discoveryInterval) {
				return nil
			}
			continue
		}

		session := newSession(device)
		startupStatus, err := d.startSessionWithDesired(ctx, session, desired, &latest)
		if err != nil {
			if ctx.Err() != nil {
				d.shutdownOrCloseSession(session)
				return nil
			}
			d.closeSession(session)
			d.updateStatus(ctx, startupStatus)
			d.logger.Error("device session failed", "status", startupStatus, "error", err)
			if !waitForDiscovery(ctx, desired, &latest, d.discoveryInterval) {
				return nil
			}
			continue
		}

		d.updateStatus(ctx, Connected)
		failureStatus, runErr := d.runSession(ctx, desired, &latest, session)
		if ctx.Err() != nil {
			d.shutdownOrCloseSession(session)
			return nil
		}
		d.closeSession(session)
		d.updateStatus(ctx, failureStatus)
		if runErr != nil {
			d.logger.Error("device session ended", "status", failureStatus, "error", runErr)
		}
		if !waitForDiscovery(ctx, desired, &latest, d.discoveryInterval) {
			return nil
		}
	}
}

// shutdownOrCloseSession writes best-effort cleanup only when no transaction
// has failed. A failed transaction leaves the input stream unsynchronized, so
// any further request could consume a delayed response for the prior request.
func (d *Driver) shutdownOrCloseSession(session *session) {
	if session.canWrite() {
		d.shutdownSession(session)
		return
	}
	d.closeSession(session)
}

// startSession preserves the small test seam used by package tests. Production
// startup uses startSessionWithDesired so snapshots arriving during startup are
// folded into the same convergence loop.
func (d *Driver) startSession(ctx context.Context, session *session, latest core.DesiredState) (Status, error) {
	return d.startSessionWithDesired(ctx, session, nil, &latest)
}

func (d *Driver) startSessionWithDesired(ctx context.Context, session *session, desired <-chan core.DesiredState, latest *core.DesiredState) (Status, error) {
	d.restoreEffect = nil
	if err := validateDesired(*latest); err != nil {
		return ProtocolError, err
	}
	if desired != nil {
		drainDesired(desired, latest)
		if err := validateDesired(*latest); err != nil {
			return ProtocolError, err
		}
	}
	response, err := d.transact(ctx, session, matrixGetRequest())
	if err != nil {
		return d.classifyFailure(ctx, err), err
	}
	effect := response[3]
	if effect != PauseEffect {
		restored := effect
		d.restoreEffect = &restored
	}
	if _, err := d.transact(ctx, session, matrixSetRequest(PauseEffect)); err != nil {
		return d.classifyFailure(ctx, err), err
	}
	if err := d.reconcileLatest(ctx, desired, latest, session); err != nil {
		return d.classifyFailure(ctx, err), err
	}
	return Connected, nil
}

func (d *Driver) runSession(ctx context.Context, desired <-chan core.DesiredState, latest *core.DesiredState, session *session) (Status, error) {
	for {
		select {
		case snapshot, ok := <-desired:
			if !ok {
				return Disconnected, errors.New("desired-state stream closed")
			}
			*latest = snapshot
			if err := d.reconcileLatest(ctx, desired, latest, session); err != nil {
				return d.classifyFailure(ctx, err), err
			}
		case <-session.pumpDone:
			return Disconnected, session.err()
		case <-ctx.Done():
			return Disconnected, ctx.Err()
		}
	}
}

func (d *Driver) reconcile(ctx context.Context, session *session, desired core.DesiredState) error {
	return d.reconcileLatest(ctx, nil, &desired, session)
}

func (d *Driver) reconcileLatest(ctx context.Context, desired <-chan core.DesiredState, latest *core.DesiredState, session *session) error {
	for {
		if desired != nil {
			closed := drainDesired(desired, latest)
			if closed {
				return errors.New("desired-state stream closed")
			}
		}
		if err := validateDesired(*latest); err != nil {
			return err
		}
		for index := 0; index < core.LEDCount; index++ {
			if desired != nil {
				closed := drainDesired(desired, latest)
				if closed {
					return errors.New("desired-state stream closed")
				}
			}
			if err := validateDesired(*latest); err != nil {
				return err
			}
			led := latest.LEDs[index]
			if session.valid[index] && session.cache[index] == led.Color {
				continue
			}
			request := ledRequest(led)
			response, err := d.transact(ctx, session, request)
			if err != nil {
				return err
			}
			if err := validateResponse(request[:], response[:]); err != nil {
				return err
			}
			session.cache[index] = led.Color
			session.valid[index] = true
		}
		if desired != nil {
			closed := drainDesired(desired, latest)
			if closed {
				return errors.New("desired-state stream closed")
			}
		}
		if err := validateDesired(*latest); err != nil {
			return err
		}
		if sessionMatches(session, *latest) {
			return nil
		}
	}
}

func validateDesired(desired core.DesiredState) error {
	for index, led := range desired.LEDs {
		if led.Index != index {
			return fmt.Errorf("invalid desired state: LED %d has index %d", index, led.Index)
		}
	}
	return nil
}

func sessionMatches(session *session, desired core.DesiredState) bool {
	for index, led := range desired.LEDs {
		if !session.valid[index] || session.cache[index] != led.Color {
			return false
		}
	}
	return true
}

func (d *Driver) transact(ctx context.Context, session *session, request [32]byte) ([32]byte, error) {
	transactionContext, cancel := context.WithTimeout(ctx, d.transactionTimeout)
	defer cancel()
	return session.transact(transactionContext, request)
}

func (d *Driver) classifyFailure(ctx context.Context, err error) Status {
	if errors.Is(err, ErrProtocol) {
		return ProtocolError
	}
	if errors.Is(err, ErrSessionLost) {
		return Disconnected
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return Disconnected
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTransport) {
		if d.devicePresent() {
			return ProtocolError
		}
		return Disconnected
	}
	return ProtocolError
}

func (d *Driver) devicePresent() bool {
	devices, err := d.source.Devices()
	if err != nil {
		return false
	}
	defer closeDevices(devices)
	return len(devices) == 1
}

func (d *Driver) shutdownSession(session *session) {
	shutdownContext, cancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
	defer cancel()
	shutdownDeadline := time.Now().Add(d.shutdownTimeout)
	offDuration := d.shutdownTimeout
	if offDuration > 1500*time.Millisecond {
		offDuration = 1500 * time.Millisecond
	}
	offDeadline := time.Now().Add(offDuration)
	healthy := true
	for index := 0; index < core.LEDCount; index++ {
		remaining := time.Until(offDeadline)
		if remaining <= 0 {
			break
		}
		if remaining > d.transactionTimeout {
			remaining = d.transactionTimeout
		}
		requestContext, requestCancel := context.WithTimeout(shutdownContext, remaining)
		_, err := session.transact(requestContext, ledRequest(core.LED{Index: index, Color: core.OffColor}))
		requestCancel()
		if err != nil {
			healthy = false
			d.logger.Error("LED cleanup failed", "index", index, "error", err)
			break
		}
	}
	if healthy && d.restoreEffect != nil {
		remaining := time.Until(shutdownDeadline)
		if remaining > 0 {
			if remaining > d.transactionTimeout {
				remaining = d.transactionTimeout
			}
			requestContext, requestCancel := context.WithTimeout(shutdownContext, remaining)
			_, err := session.transact(requestContext, matrixSetRequest(*d.restoreEffect))
			requestCancel()
			if err != nil {
				d.logger.Error("matrix effect restore failed", "error", err)
			}
		}
	}
	d.closeSessionWithContext(session, shutdownContext)
	d.restoreEffect = nil
}

func (d *Driver) closeSession(session *session) {
	closeContext, cancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
	defer cancel()
	d.closeSessionWithContext(session, closeContext)
}

func (d *Driver) closeSessionWithContext(session *session, ctx context.Context) {
	if err := session.close(ctx); err != nil {
		d.logger.Error("device close failed", "error", err)
	}
}

func (d *Driver) updateStatus(ctx context.Context, next Status) {
	if d.status == next {
		return
	}
	previous := d.status
	d.status = next
	d.logger.Info("device status changed", "from", previous, "to", next)
	if d.statusSink != nil {
		if err := d.statusSink(ctx, next); err != nil && ctx.Err() == nil {
			d.logger.Error("publish device status", "status", next, "error", err)
		}
	}
}

func closeDevices(devices []Transport) {
	for _, device := range devices {
		_ = device.Close()
	}
}

func drainDesired(desired <-chan core.DesiredState, latest *core.DesiredState) bool {
	for {
		select {
		case snapshot, ok := <-desired:
			if !ok {
				return true
			}
			*latest = snapshot
		default:
			return false
		}
	}
}

func waitForDiscovery(ctx context.Context, desired <-chan core.DesiredState, latest *core.DesiredState, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case snapshot, ok := <-desired:
			if !ok {
				return false
			}
			*latest = snapshot
		case <-timer.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
}

func matrixGetRequest() [32]byte {
	return [32]byte{0x08, 0x03, 0x02}
}

func matrixSetRequest(effect byte) [32]byte {
	return [32]byte{0x07, 0x03, 0x02, effect}
}

func ledRequest(led core.LED) [32]byte {
	return [32]byte{0x07, 0x00, 0x01, byte(led.Index), led.Color.R, led.Color.G, led.Color.B}
}

func validateResponse(request, response []byte) error {
	if len(response) != 32 {
		return fmt.Errorf("%w: response length is %d, want 32", ErrProtocol, len(response))
	}
	if !bytes.Equal(response[:3], request[:3]) {
		return fmt.Errorf("%w: response header % x does not confirm % x", ErrProtocol, response[:3], request[:3])
	}
	switch {
	case request[0] == 0x07 && request[1] == 0x03 && request[2] == 0x02:
		if response[3] != request[3] {
			return fmt.Errorf("%w: matrix effect response is %d, want %d", ErrProtocol, response[3], request[3])
		}
	case request[0] == 0x07 && request[1] == 0x00 && request[2] == 0x01:
		if !bytes.Equal(response[:7], request[:7]) {
			return fmt.Errorf("%w: LED response does not confirm request", ErrProtocol)
		}
	}
	return nil
}

type session struct {
	transport  Transport
	cache      [core.LEDCount]core.Color
	valid      [core.LEDCount]bool
	txMu       sync.Mutex
	unsafe     bool
	pendingMu  sync.Mutex
	pending    chan []byte
	pumpDone   chan struct{}
	pumpErr    error
	pumpMu     sync.Mutex
	pumpCancel context.CancelFunc
}

func newSession(transport Transport) *session {
	pumpContext, cancel := context.WithCancel(context.Background())
	s := &session{
		transport:  transport,
		pumpDone:   make(chan struct{}),
		pumpCancel: cancel,
	}
	go func() {
		err := transport.Stream(pumpContext, s.deliver)
		s.pumpMu.Lock()
		s.pumpErr = err
		s.pumpMu.Unlock()
		close(s.pumpDone)
	}()
	return s
}

func (s *session) deliver(data []byte) {
	response := append([]byte(nil), data...)
	s.pendingMu.Lock()
	pending := s.pending
	if pending != nil {
		select {
		case pending <- response:
		default:
		}
	}
	s.pendingMu.Unlock()
}

func (s *session) transact(ctx context.Context, request [32]byte) ([32]byte, error) {
	var empty [32]byte
	s.txMu.Lock()
	defer s.txMu.Unlock()

	responseChannel := make(chan []byte, 1)
	s.pendingMu.Lock()
	if s.pending != nil {
		s.pendingMu.Unlock()
		return empty, fmt.Errorf("%w: transaction already pending", ErrTransport)
	}
	s.pending = responseChannel
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		s.pending = nil
		s.pendingMu.Unlock()
	}()

	if err := s.transport.SetReport(hid.Output, 0, request[:]); err != nil {
		s.unsafe = true
		return empty, fmt.Errorf("%w: set report: %w", ErrTransport, err)
	}
	select {
	case response := <-responseChannel:
		if err := validateResponse(request[:], response); err != nil {
			s.unsafe = true
			return empty, err
		}
		var result [32]byte
		copy(result[:], response)
		return result, nil
	case <-s.pumpDone:
		s.unsafe = true
		return empty, fmt.Errorf("%w: %v", ErrSessionLost, s.err())
	case <-ctx.Done():
		s.unsafe = true
		return empty, ctx.Err()
	}
}

func (s *session) canWrite() bool {
	s.txMu.Lock()
	defer s.txMu.Unlock()
	return !s.unsafe
}

func (s *session) err() error {
	s.pumpMu.Lock()
	defer s.pumpMu.Unlock()
	if s.pumpErr != nil {
		return s.pumpErr
	}
	return errors.New("read pump stopped")
}

func (s *session) close(ctx context.Context) error {
	s.pumpCancel()
	var waitErr error
	select {
	case <-s.pumpDone:
	case <-ctx.Done():
		waitErr = ctx.Err()
	}
	closeErr := s.transport.Close()
	return errors.Join(waitErr, closeErr)
}
