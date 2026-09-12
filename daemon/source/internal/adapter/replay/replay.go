// Package replay provides a deterministic adapter source for fixtures, local
// diagnostics and integration tests. It is not a harness integration.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/adapter"
)

// Fixture is a privacy-safe recording of normalized metadata only.
type Fixture struct {
	Version      string               `json:"version,omitempty"`
	Source       string               `json:"source"`
	SessionID    string               `json:"session_id"`
	Capabilities adapter.Capabilities `json:"capabilities"`
	Snapshot     adapter.Snapshot     `json:"snapshot"`
	Events       []TimedEvent         `json:"events"`
}

// TimedEvent is emitted after DelayMS. It contains no prompts or tool payloads.
type TimedEvent struct {
	DelayMS int64         `json:"delay_ms,omitempty"`
	Event   adapter.Event `json:"event"`
}

// Load reads and validates a replay fixture.
func Load(path string) (Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Fixture{}, fmt.Errorf("read replay fixture: %w", err)
	}
	var fixture Fixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return Fixture{}, fmt.Errorf("decode replay fixture: %w", err)
	}
	if fixture.Source == "" || fixture.SessionID == "" {
		return Fixture{}, errors.New("replay fixture source and session ID are required")
	}
	if !fixture.Capabilities.Snapshot || !fixture.Capabilities.LiveEvents || !fixture.Capabilities.RunIdentity {
		return Fixture{}, errors.New("replay fixture lacks required source capabilities")
	}
	if err := validateFixture(fixture); err != nil {
		return Fixture{}, err
	}
	return fixture, nil
}

// validateFixture enforces the live-event ordering contract shared by file
// and programmatic fixtures: every event carries a nonzero source cursor,
// the first event sits past the snapshot cursor, and every later event moves
// strictly forward.
func validateFixture(fixture Fixture) error {
	previous := fixture.Snapshot.SourceOrder
	for index, timed := range fixture.Events {
		order := timed.Event.SourceOrder
		if order == 0 {
			return fmt.Errorf("replay fixture event %d lacks a source order", index)
		}
		if order <= previous {
			return fmt.Errorf("replay fixture event %d has source order %d at or before cursor %d", index, order, previous)
		}
		previous = order
	}
	return nil
}

// Source implements adapter.Source from one fixture. It retains the observed
// fixture state across reconnects, just as a real source retains its session.
type Source struct {
	fixture Fixture
	state   sourceState
}

type sourceState struct {
	mu        sync.RWMutex
	snapshot  adapter.Snapshot
	nextEvent int
}

// NewSource constructs a replay source with one persistent session timeline.
// It rejects fixtures whose live events violate the source ordering contract.
func NewSource(fixture Fixture) (*Source, error) {
	if err := validateFixture(fixture); err != nil {
		return nil, err
	}
	return &Source{fixture: fixture, state: sourceState{snapshot: cloneSnapshot(fixture.Snapshot)}}, nil
}

func (s *Source) Name() string { return s.fixture.Source }

func (s *Source) Version() string {
	if s.fixture.Version == "" {
		return "replay-v1"
	}
	return s.fixture.Version
}

func (s *Source) Open(ctx context.Context, sessionID string) (adapter.Stream, error) {
	if sessionID != s.fixture.SessionID {
		return nil, errors.New("selected replay session is not present")
	}
	s.state.mu.RLock()
	start := s.state.nextEvent
	s.state.mu.RUnlock()
	events := make(chan adapter.Event, len(s.fixture.Events)-start)
	errorsChannel := make(chan error)
	stream := &stream{source: s, start: start, events: events, errors: errorsChannel, done: make(chan struct{})}
	go stream.emit(ctx)
	return stream, nil
}

type stream struct {
	source *Source
	start  int
	events chan adapter.Event
	errors chan error
	done   chan struct{}
}

func (s *stream) Capabilities() adapter.Capabilities { return s.source.fixture.Capabilities }
func (s *stream) Snapshot(context.Context) (adapter.Snapshot, error) {
	s.source.state.mu.RLock()
	defer s.source.state.mu.RUnlock()
	return cloneSnapshot(s.source.state.snapshot), nil
}
func (s *stream) Events() <-chan adapter.Event { return s.events }
func (s *stream) Errors() <-chan error         { return s.errors }
func (s *stream) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

func (s *stream) emit(ctx context.Context) {
	defer close(s.events)
	defer close(s.errors)
	for index := s.start; index < len(s.source.fixture.Events); index++ {
		timed := s.source.fixture.Events[index]
		if timed.DelayMS > 0 {
			timer := time.NewTimer(time.Duration(timed.DelayMS) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.done:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		s.source.apply(index, timed.Event)
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case s.events <- timed.Event:
		}
	}
	select {
	case <-ctx.Done():
	case <-s.done:
	}
}

func (s *Source) apply(index int, event adapter.Event) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	// Live events without a source cursor carry no ordering proof, and older
	// buffered events must never move a newer snapshot backwards.
	if event.SourceOrder == 0 {
		return
	}
	if index < s.state.nextEvent || event.SourceOrder <= s.state.snapshot.SourceOrder {
		return
	}
	s.state.nextEvent = index + 1
	s.state.snapshot.SourceOrder = event.SourceOrder
	run := event.Run
	if run.Role == "main" {
		if run.State == "succeeded" || run.State == "failed" || run.State == "cancelled" {
			if s.state.snapshot.Main != nil && s.state.snapshot.Main.AgentID == run.AgentID && s.state.snapshot.Main.RunID == run.RunID {
				s.state.snapshot.Main = nil
			}
			return
		}
		copy := run
		s.state.snapshot.Main = &copy
		return
	}
	key := run.AgentID + "\x00" + run.RunID
	for index, current := range s.state.snapshot.Subagents {
		if current.AgentID+"\x00"+current.RunID != key {
			continue
		}
		if run.State == "succeeded" || run.State == "failed" || run.State == "cancelled" {
			s.state.snapshot.Subagents = append(s.state.snapshot.Subagents[:index], s.state.snapshot.Subagents[index+1:]...)
			return
		}
		if run.StartOrder == 0 {
			run.StartOrder = current.StartOrder
		}
		s.state.snapshot.Subagents[index] = run
		return
	}
	if run.State != "succeeded" && run.State != "failed" && run.State != "cancelled" {
		s.state.snapshot.Subagents = append(s.state.snapshot.Subagents, run)
	}
}

func cloneSnapshot(snapshot adapter.Snapshot) adapter.Snapshot {
	copy := snapshot
	if snapshot.Main != nil {
		main := *snapshot.Main
		copy.Main = &main
	}
	copy.Subagents = append([]adapter.Run(nil), snapshot.Subagents...)
	return copy
}
