package km16pro

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/go-macos/iokit/hid"
	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
)

type fakeSource struct {
	mu      sync.Mutex
	devices []Transport
	queries int
}

func (s *fakeSource) Devices() ([]Transport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries++
	return append([]Transport(nil), s.devices...), nil
}

type fakeTransport struct {
	mu             sync.Mutex
	info           hid.Info
	responses      chan []byte
	streamStarted  chan struct{}
	streamOnce     sync.Once
	requests       [][32]byte
	currentEffect  byte
	malformed      bool
	noResponse     bool
	opened         bool
	closed         bool
	activeSetCalls int
	maxSetCalls    int
}

func newFakeTransport(effect byte) *fakeTransport {
	return &fakeTransport{
		info:          hid.Info{VendorID: VendorID, ProductID: ProductID, UsagePage: UsagePage, Usage: Usage},
		responses:     make(chan []byte, 32),
		streamStarted: make(chan struct{}),
		currentEffect: effect,
	}
}

func (t *fakeTransport) Info() hid.Info { return t.info }
func (t *fakeTransport) Open() error {
	t.mu.Lock()
	t.opened = true
	t.mu.Unlock()
	return nil
}
func (t *fakeTransport) SetReport(kind hid.ReportKind, id byte, data []byte) error {
	if kind != hid.Output || id != 0 || len(data) != 32 {
		return errors.New("unexpected report parameters")
	}
	var request [32]byte
	copy(request[:], data)
	t.mu.Lock()
	t.requests = append(t.requests, request)
	t.activeSetCalls++
	if t.activeSetCalls > t.maxSetCalls {
		t.maxSetCalls = t.activeSetCalls
	}
	effect := t.currentEffect
	malformed := t.malformed
	noResponse := t.noResponse
	t.activeSetCalls--
	t.mu.Unlock()

	response := request
	if request[0] == 0x08 {
		response[3] = effect
	}
	if malformed {
		response[0] = 0xff
	}
	if !noResponse {
		t.responses <- response[:]
	}
	return nil
}

func (t *fakeTransport) Stream(ctx context.Context, deliver func([]byte)) error {
	t.streamOnce.Do(func() { close(t.streamStarted) })
	for {
		select {
		case response := <-t.responses:
			deliver(response)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (t *fakeTransport) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return nil
}

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func validDesired() core.DesiredState {
	desired := core.InitialDesiredState()
	desired.LEDs[12].Color = core.ActiveColors[0]
	return desired
}

func requestsOf(transport *fakeTransport) [][32]byte {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([][32]byte(nil), transport.requests...)
}

func TestValidateResponse(t *testing.T) {
	request := ledRequest(core.LED{Index: 12, Color: core.Color{R: 1, G: 2, B: 3}})
	response := request
	if err := validateResponse(request[:], response[:]); err != nil {
		t.Fatal(err)
	}
	response[5]++
	if err := validateResponse(request[:], response[:]); !errors.Is(err, ErrProtocol) {
		t.Fatalf("mismatched LED response error = %v", err)
	}
	if err := validateResponse(request[:], make([]byte, 31)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("short response error = %v", err)
	}
	query := matrixGetRequest()
	queryResponse := query
	queryResponse[3] = 7
	if err := validateResponse(query[:], queryResponse[:]); err != nil {
		t.Fatal(err)
	}
}

func TestTransactionsAreSerialized(t *testing.T) {
	transport := newFakeTransport(2)
	session := newSession(transport)
	driver := New(&fakeSource{devices: []Transport{transport}}, Options{Logger: silentLogger(), TransactionTimeout: time.Second})
	var waitGroup sync.WaitGroup
	for index := 0; index < 8; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			request := ledRequest(core.LED{Index: 12 + index%4, Color: core.Color{R: uint8(index)}})
			if _, err := driver.transact(context.Background(), session, request); err != nil {
				t.Errorf("transaction %d: %v", index, err)
			}
		}(index)
	}
	waitGroup.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.close(ctx); err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	maxSetCalls := transport.maxSetCalls
	transport.mu.Unlock()
	if maxSetCalls != 1 {
		t.Fatalf("maximum concurrent SetReport calls = %d, want 1", maxSetCalls)
	}
}

func TestDriverReconcilesAndRestoresEffectOnShutdown(t *testing.T) {
	transport := newFakeTransport(2)
	source := &fakeSource{devices: []Transport{transport}}
	statusChanges := make(chan Status, 8)
	driver := New(source, Options{
		Logger:             silentLogger(),
		DiscoveryInterval:  time.Millisecond,
		TransactionTimeout: time.Second,
		StatusSink: func(_ context.Context, status Status) error {
			statusChanges <- status
			return nil
		},
	})
	desired := make(chan core.DesiredState, 1)
	desired <- validDesired()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- driver.Run(ctx, desired) }()

	deadline := time.After(time.Second)
	for {
		select {
		case status := <-statusChanges:
			if status == Connected {
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				requests := requestsOf(transport)
				if len(requests) != 57 {
					t.Fatalf("request count = %d, want startup 29 plus shutdown 28", len(requests))
				}
				if requests[0] != matrixGetRequest() || requests[1] != matrixSetRequest(PauseEffect) {
					t.Fatalf("startup requests = % x, % x", requests[0], requests[1])
				}
				for index := 0; index < core.LEDCount; index++ {
					if requests[2+index][3] != byte(index) {
						t.Fatalf("LED %d request = % x", index, requests[2+index])
					}
				}
				if requests[56] != matrixSetRequest(2) {
					t.Fatalf("restore request = % x", requests[56])
				}
				return
			}
		case <-deadline:
			t.Fatal("did not connect")
		}
	}
}

func TestTimeoutWithPresentDeviceIsProtocolError(t *testing.T) {
	transport := newFakeTransport(2)
	// Keep the response off the read path to exercise the transaction timeout.
	transport.noResponse = true
	source := &fakeSource{devices: []Transport{transport}}
	statuses := make(chan Status, 8)
	driver := New(source, Options{
		Logger:             silentLogger(),
		DiscoveryInterval:  time.Millisecond,
		TransactionTimeout: 5 * time.Millisecond,
		StatusSink: func(_ context.Context, status Status) error {
			statuses <- status
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	desired := make(chan core.DesiredState)
	done := make(chan error, 1)
	go func() { done <- driver.Run(ctx, desired) }()
	deadline := time.After(time.Second)
	for {
		select {
		case status := <-statuses:
			if status == ProtocolError {
				cancel()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if requests := requestsOf(transport); len(requests) != 1 {
					t.Fatalf("requests after timeout = %d, want 1 with no shutdown writes", len(requests))
				}
				return
			}
		case <-deadline:
			t.Fatal("did not report protocol error")
		}
	}
}

func TestCanceledTransactionDoesNotWriteShutdownRequests(t *testing.T) {
	transport := newFakeTransport(2)
	transport.noResponse = true
	session := newSession(transport)
	driver := New(&fakeSource{devices: []Transport{transport}}, Options{Logger: silentLogger(), TransactionTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	request := ledRequest(core.LED{Index: 12, Color: core.ActiveColors[0]})
	go func() {
		_, err := driver.transact(ctx, session, request)
		done <- err
	}()
	deadline := time.After(time.Second)
	for len(requestsOf(transport)) == 0 {
		select {
		case <-deadline:
			t.Fatal("transaction was not sent")
		default:
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("transaction error = %v, want context canceled", err)
	}
	// Deliver the old response after cancellation. It must not trigger an off
	// write or a matrix restore on the now-unsynchronized session.
	transport.responses <- request[:]
	driver.shutdownOrCloseSession(session)
	if requests := requestsOf(transport); len(requests) != 1 {
		t.Fatalf("requests after canceled transaction = %d, want 1", len(requests))
	}
}

func TestSessionCloseClosesTransportAfterDeadline(t *testing.T) {
	transport := newFakeTransport(2)
	session := newSession(transport)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("close error = %v, want context canceled", err)
	}
	transport.mu.Lock()
	closed := transport.closed
	transport.mu.Unlock()
	if !closed {
		t.Fatal("transport was not closed after deadline")
	}
}

func TestAmbiguousDiscoveryDoesNotOpenDevices(t *testing.T) {
	first := newFakeTransport(2)
	second := newFakeTransport(2)
	source := &fakeSource{devices: []Transport{first, second}}
	statuses := make(chan Status, 4)
	driver := New(source, Options{
		Logger:            silentLogger(),
		DiscoveryInterval: time.Millisecond,
		StatusSink: func(_ context.Context, status Status) error {
			statuses <- status
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- driver.Run(ctx, make(chan core.DesiredState)) }()
	select {
	case status := <-statuses:
		if status != Ambiguous {
			t.Fatalf("status = %q, want ambiguous", status)
		}
	case <-time.After(time.Second):
		t.Fatal("did not report ambiguous discovery")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	first.mu.Lock()
	firstOpened := first.opened
	first.mu.Unlock()
	second.mu.Lock()
	secondOpened := second.opened
	second.mu.Unlock()
	if firstOpened || secondOpened {
		t.Fatalf("ambiguous devices opened: first=%t second=%t", firstOpened, secondOpened)
	}
}

func TestRestoreEffectUsesLatestExternalEffect(t *testing.T) {
	source := &fakeSource{}
	driver := New(source, Options{Logger: silentLogger(), TransactionTimeout: time.Second})
	initial := core.InitialDesiredState()
	first := newFakeTransport(2)
	if err := first.Open(); err != nil {
		t.Fatal(err)
	}
	firstSession := newSession(first)
	if _, err := driver.startSession(context.Background(), firstSession, initial); err != nil {
		t.Fatal(err)
	}
	if driver.restoreEffect == nil || *driver.restoreEffect != 2 {
		t.Fatalf("first restore effect = %v, want 2", driver.restoreEffect)
	}
	if err := firstSession.close(context.Background()); err != nil {
		t.Fatal(err)
	}

	second := newFakeTransport(5)
	if err := second.Open(); err != nil {
		t.Fatal(err)
	}
	secondSession := newSession(second)
	if _, err := driver.startSession(context.Background(), secondSession, initial); err != nil {
		t.Fatal(err)
	}
	if driver.restoreEffect == nil || *driver.restoreEffect != 5 {
		t.Fatalf("second restore effect = %v, want 5", driver.restoreEffect)
	}
	if err := secondSession.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
