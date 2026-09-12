package adapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
)

var heartbeatInterval = 5 * time.Second

// Options configures one adapter for one explicitly selected session.
type Options struct {
	Source     Source
	SessionID  string
	SocketPath string
	Logger     *slog.Logger
}

// Diagnostics is a privacy-safe view of the adapter's current delivery state.
type Diagnostics struct {
	AdapterVersion   string       `json:"adapter_version"`
	Source           string       `json:"source"`
	SourceVersion    string       `json:"source_version"`
	SessionID        string       `json:"session_id"`
	Capabilities     Capabilities `json:"capabilities"`
	SourceHealthy    bool         `json:"source_healthy"`
	ServiceConnected bool         `json:"service_connected"`
	LastAcceptedSeq  uint64       `json:"last_accepted_seq"`
	QueueLength      int          `json:"queue_length"`
	Resyncs          uint64       `json:"resyncs"`
	DroppedEvents    uint64       `json:"dropped_events"`
	LimitedCoverage  bool         `json:"limited_coverage"`
}

// Runtime serializes delivery to a single V2 binding.
type Runtime struct {
	options       Options
	client        socketClient
	diagMu        sync.RWMutex
	diag          Diagnostics
	bindingID     string
	nextSeq       uint64
	pendingBindID string
	ids           *idMapper
	sourceID      string
	sessionID     string
}

func New(options Options) (*Runtime, error) {
	if options.Source == nil || options.SessionID == "" || options.SocketPath == "" {
		return nil, errors.New("source, session ID and socket path are required")
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	ids := newIDMapper()
	sourceID, err := ids.mapID(options.Source.Name())
	if err != nil {
		return nil, fmt.Errorf("source ID: %w", err)
	}
	sessionID, err := ids.mapID(options.SessionID)
	if err != nil {
		return nil, fmt.Errorf("session ID: %w", err)
	}
	sourceVersion := "unknown"
	if versioned, ok := options.Source.(VersionedSource); ok && versioned.Version() != "" {
		sourceVersion = versioned.Version()
	}
	return &Runtime{options: options, client: socketClient{path: options.SocketPath}, ids: ids, sourceID: sourceID, sessionID: sessionID, diag: Diagnostics{AdapterVersion: Version, Source: options.Source.Name(), SourceVersion: sourceVersion, SessionID: options.SessionID}}, nil
}

// Diagnostics returns a copy safe to expose through a CLI or log.
func (r *Runtime) Diagnostics() Diagnostics {
	r.diagMu.RLock()
	defer r.diagMu.RUnlock()
	return r.diag
}

func (r *Runtime) updateDiagnostics(update func(*Diagnostics)) {
	r.diagMu.Lock()
	defer r.diagMu.Unlock()
	update(&r.diag)
}

// Run observes the chosen source session until cancellation. Source loss stops
// heartbeats so the service lease naturally clears the display.
func (r *Runtime) Run(ctx context.Context) error {
	defer func() {
		if ctx.Err() != nil {
			r.unbind(context.Background())
		}
	}()
	for delay := 100 * time.Millisecond; ; delay = min(delay*2, 5*time.Second) {
		err := r.runSourceStream(ctx)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if !errors.Is(err, ErrSourceLost) {
			return err
		}
		if err := wait(ctx, delay); err != nil {
			return nil
		}
	}
}

func (r *Runtime) runSourceStream(ctx context.Context) error {
	stream, err := r.options.Source.Open(ctx, r.options.SessionID)
	if err != nil {
		return fmt.Errorf("open source session: %w", err)
	}
	defer stream.Close()
	capabilities := stream.Capabilities()
	r.updateDiagnostics(func(diag *Diagnostics) {
		diag.Capabilities = capabilities
		diag.LimitedCoverage = !capabilities.Snapshot || !capabilities.LiveEvents || !capabilities.RunIdentity || !capabilities.Liveness
		diag.SourceHealthy = true
	})
	if !capabilities.Snapshot || !capabilities.LiveEvents || !capabilities.RunIdentity {
		return errors.New("source lacks required snapshot, live-event or run-identity capability")
	}

	registry, err := r.resync(ctx, stream)
	if err != nil {
		return err
	}

	queue := make(chan Event, MaxPendingEvents)
	overflow := make(chan struct{}, 1)
	streamContext, stopStream := context.WithCancel(ctx)
	defer stopStream()
	go r.collect(streamContext, stream.Events(), queue, overflow)
	var heartbeatC <-chan time.Time
	var heartbeat *time.Ticker
	if capabilities.Liveness {
		heartbeat = time.NewTicker(heartbeatInterval)
		heartbeatC = heartbeat.C
		defer heartbeat.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-stream.Errors():
			r.updateDiagnostics(func(diag *Diagnostics) { diag.SourceHealthy = false; diag.ServiceConnected = false })
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return ErrSourceLost
			}
			if err != nil {
				return fmt.Errorf("%w: %v", ErrSourceLost, err)
			}
		case <-overflow:
			r.updateDiagnostics(func(diag *Diagnostics) { diag.DroppedEvents++ })
			registry, err = r.resync(ctx, stream)
			if err != nil {
				return err
			}
		case event, ok := <-queue:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return ErrSourceLost
			}
			r.updateDiagnostics(func(diag *Diagnostics) { diag.QueueLength = len(queue) })
			run, applyErr := registry.apply(event)
			if applyErr != nil {
				r.updateDiagnostics(func(diag *Diagnostics) { diag.Resyncs++ })
				registry, err = r.resync(ctx, stream)
				if err != nil {
					return err
				}
				continue
			}
			if run == nil {
				continue
			}
			if err := r.writeEvent(ctx, *run); err != nil {
				r.updateDiagnostics(func(diag *Diagnostics) { diag.Resyncs++ })
				registry, err = r.resync(ctx, stream)
				if err != nil {
					return err
				}
			}
		case <-heartbeatC:
			if err := r.writeHeartbeat(ctx); err != nil {
				r.updateDiagnostics(func(diag *Diagnostics) { diag.Resyncs++ })
				registry, err = r.resync(ctx, stream)
				if err != nil {
					return err
				}
			}
		}
	}
}

func (r *Runtime) collect(ctx context.Context, events <-chan Event, queue chan<- Event, overflow chan<- struct{}) {
	defer close(queue)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			select {
			case queue <- event:
				r.updateDiagnostics(func(diag *Diagnostics) { diag.QueueLength = len(queue) })
			default:
				select {
				case overflow <- struct{}{}:
				default:
				}
			}
		}
	}
}

func (r *Runtime) resync(ctx context.Context, stream Stream) (*registry, error) {
	snapshot, err := stream.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("read source snapshot: %w", err)
	}
	registry, err := newRegistry(snapshot)
	if err != nil {
		return nil, fmt.Errorf("validate source snapshot: %w", err)
	}
	if err := r.bindAndSync(ctx, registry); err != nil {
		return nil, err
	}
	r.updateDiagnostics(func(diag *Diagnostics) { diag.Resyncs++ })
	return registry, nil
}

func (r *Runtime) bindAndSync(ctx context.Context, registry *registry) error {
	if r.bindingID != "" {
		if err := r.writeSync(ctx, registry); err == nil {
			return nil
		} else if !errors.Is(err, ErrBindingLost) {
			return err
		}
		r.bindingID, r.nextSeq = "", 0
		r.updateDiagnostics(func(diag *Diagnostics) { diag.ServiceConnected = false })
	}
	if r.pendingBindID == "" {
		bindID, err := randomID()
		if err != nil {
			return err
		}
		r.pendingBindID = bindID
	}
	request := protocol.Request{Version: protocol.DisplayVersion, Type: protocol.DisplayBind, V2: &protocol.DisplayRequest{Type: protocol.DisplayBind, Source: r.sourceID, SessionID: r.sessionID, BindID: r.pendingBindID}}
	for delay := 100 * time.Millisecond; ; delay = min(delay*2, 5*time.Second) {
		var result protocol.DisplayBindResult
		apiErr, err := r.client.send(ctx, request, &result)
		if err == nil && apiErr == nil {
			if result.BindingID == "" || result.NextSeq == 0 {
				return errors.New("service returned an invalid bind result")
			}
			r.bindingID, r.nextSeq = result.BindingID, result.NextSeq
			r.pendingBindID = ""
			r.updateDiagnostics(func(diag *Diagnostics) { diag.ServiceConnected = true })
			if err := r.writeSync(ctx, registry); err == nil {
				return nil
			} else if !errors.Is(err, ErrBindingLost) {
				return err
			}
			r.bindingID, r.nextSeq = "", 0
			r.updateDiagnostics(func(diag *Diagnostics) { diag.ServiceConnected = false })
			continue
		}
		if apiErr != nil && apiErr.Code != "display_busy" {
			return fmt.Errorf("bind service: %s", apiErr.Code)
		}
		r.updateDiagnostics(func(diag *Diagnostics) { diag.ServiceConnected = false })
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (r *Runtime) writeSync(ctx context.Context, registry *registry) error {
	request, err := r.syncRequest(registry)
	if err != nil {
		return err
	}
	return r.write(ctx, request)
}

func (r *Runtime) syncRequest(registry *registry) (protocol.Request, error) {
	snapshot := registry.snapshot()
	request := protocol.Request{Version: protocol.DisplayVersion, Type: protocol.DisplaySync, V2: &protocol.DisplayRequest{Type: protocol.DisplaySync, BindingID: r.bindingID, Seq: r.nextSeq, HasSeq: true, HasMain: true, Subagents: make([]protocol.DisplaySnapshot, 0, len(snapshot.Subagents))}}
	if snapshot.Main != nil {
		main, err := r.ids.mapRun(*snapshot.Main)
		if err != nil {
			return protocol.Request{}, err
		}
		request.V2.Main = &protocol.DisplaySnapshot{AgentID: main.AgentID, RunID: main.RunID, State: main.State}
	}
	for _, run := range snapshot.Subagents {
		mapped, err := r.ids.mapRun(run)
		if err != nil {
			return protocol.Request{}, err
		}
		request.V2.Subagents = append(request.V2.Subagents, protocol.DisplaySnapshot{AgentID: mapped.AgentID, RunID: mapped.RunID, ParentAgentID: mapped.ParentAgentID, State: mapped.State, StartOrder: mapped.StartOrder})
	}
	return request, nil
}
func (r *Runtime) writeEvent(ctx context.Context, run Run) error {
	mapped, err := r.ids.mapRun(run)
	if err != nil {
		return err
	}
	return r.write(ctx, eventRequest(r.bindingID, r.nextSeq, mapped))
}
func (r *Runtime) writeHeartbeat(ctx context.Context) error {
	return r.write(ctx, protocol.Request{Version: protocol.DisplayVersion, Type: protocol.DisplayHeartbeat, V2: &protocol.DisplayRequest{Type: protocol.DisplayHeartbeat, BindingID: r.bindingID, Seq: r.nextSeq, HasSeq: true}})
}

func (r *Runtime) write(ctx context.Context, request protocol.Request) error {
	for delay := 100 * time.Millisecond; ; delay = min(delay*2, 5*time.Second) {
		var result protocol.DisplayWriteResult
		apiErr, err := r.client.send(ctx, request, &result)
		if err == nil && apiErr == nil {
			if result.AcceptedSeq != request.V2.Seq {
				return errors.New("service accepted an unexpected sequence")
			}
			r.updateDiagnostics(func(diag *Diagnostics) { diag.LastAcceptedSeq = result.AcceptedSeq })
			r.nextSeq++
			return nil
		}
		if apiErr != nil {
			if apiErr.Code == "unknown_binding" {
				r.updateDiagnostics(func(diag *Diagnostics) { diag.ServiceConnected = false })
				return ErrBindingLost
			}
			return fmt.Errorf("write to service: %s", apiErr.Code)
		}
		if errors.Is(err, ErrRequestTooLarge) {
			return err
		}
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (r *Runtime) unbind(ctx context.Context) {
	if r.bindingID == "" || r.nextSeq == 0 {
		return
	}
	limited, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_ = r.write(limited, protocol.Request{Version: protocol.DisplayVersion, Type: protocol.DisplayUnbind, V2: &protocol.DisplayRequest{Type: protocol.DisplayUnbind, BindingID: r.bindingID, Seq: r.nextSeq, HasSeq: true}})
}

func randomID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("create bind ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
