// Package adapter delivers normalized harness events to the AgentFlare V2
// display controller. It deliberately contains no HID or harness-specific code.
package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
)

const (
	Version          = "0.1.0"
	MaxOpenSubagents = 64
	MaxPendingEvents = 256
)

var (
	ErrUnknownRun      = errors.New("run is not known")
	ErrOutOfOrder      = errors.New("source event is out of order")
	ErrCapacity        = errors.New("subagent capacity is exhausted")
	ErrSourceLost      = errors.New("source connection was lost")
	ErrBindingLost     = errors.New("service binding was lost")
	ErrRequestTooLarge = errors.New("service request exceeds socket limit")
	// ErrResponseTooLarge indicates that a peer exceeded the bounded response
	// envelope before the adapter attempted to decode it.
	ErrResponseTooLarge = errors.New("service response exceeds socket limit")
)

// Capabilities records which semantics a source can establish without
// inference. Unsupported capabilities must be surfaced to the operator.
type Capabilities struct {
	Snapshot     bool `json:"snapshot"`
	LiveEvents   bool `json:"live_events"`
	RunIdentity  bool `json:"run_identity"`
	WaitingUser  bool `json:"waiting_user"`
	Terminal     bool `json:"terminal"`
	Cancellation bool `json:"cancellation"`
	Liveness     bool `json:"liveness"`
}

// Run is the normalized identity and current state of a harness run.
// ParentAgentID is required for subagents and omitted for the main run.
type Run struct {
	AgentID       string `json:"agent_id"`
	RunID         string `json:"run_id"`
	Role          string `json:"role"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	StartOrder    uint64 `json:"start_order,omitempty"`
}

// Snapshot is an authoritative active and waiting view at SourceOrder. It
// never contains terminal runs because sync represents present state only. A
// SourceOrder of 0 is only the initial state before the first event.
type Snapshot struct {
	SourceOrder uint64 `json:"source_order,omitempty"`
	Main        *Run   `json:"main"`
	Subagents   []Run  `json:"subagents"`
}

// Event is one normalized source transition. EventID is stable when the source
// provides it and is only an additional duplicate signal, never a substitute
// for ordering. SourceOrder is mandatory for live events and forms a strictly
// increasing source cursor.
type Event struct {
	EventID     string `json:"event_id,omitempty"`
	SourceOrder uint64 `json:"source_order,omitempty"`
	Run         Run    `json:"run"`
}

// Source opens one explicitly selected existing session. Implementations must
// not create or control a harness session as a side effect.
type Source interface {
	Name() string
	Open(ctx context.Context, sessionID string) (Stream, error)
}

// VersionedSource optionally supplies a source implementation version for
// privacy-safe diagnostics.
type VersionedSource interface {
	Source
	Version() string
}

// Stream exposes an atomic observation boundary: Open starts buffering live
// events, Snapshot returns the current state and cursor, and Events includes
// every later event with a strictly increasing SourceOrder. A live event with
// SourceOrder 0 is rejected as out of order. Events at or before
// Snapshot.SourceOrder were already covered by the snapshot and are harmlessly
// ignored by Runtime.
type Stream interface {
	Capabilities() Capabilities
	Snapshot(ctx context.Context) (Snapshot, error)
	Events() <-chan Event
	Errors() <-chan error
	Close() error
}

func validateRun(run Run, allowTerminal bool) error {
	if run.AgentID == "" || run.RunID == "" || (run.Role != "main" && run.Role != "subagent") {
		return errors.New("run identity is invalid")
	}
	if run.Role == "main" && run.ParentAgentID != "" {
		return errors.New("main run has a parent")
	}
	if run.Role == "subagent" && run.ParentAgentID == "" {
		return errors.New("subagent parent is required")
	}
	if !activeState(run.State) && (!allowTerminal || !terminalState(run.State)) {
		return fmt.Errorf("run state %q is invalid", run.State)
	}
	if run.Reason != "" && run.Reason != "approval" && run.Reason != "decision" && run.Reason != "usage_limit" && run.Reason != "execution_error" && run.Reason != "user_cancelled" && run.Reason != "other" {
		return fmt.Errorf("run reason %q is invalid", run.Reason)
	}
	return nil
}

func activeState(state string) bool { return state == "running" || state == "waiting_user" }

func terminalState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled"
}

func eventRequest(bindingID string, seq uint64, run Run) protocol.Request {
	return protocol.Request{Version: protocol.DisplayVersion, Type: protocol.DisplayEvent, V2: &protocol.DisplayRequest{
		Type:          protocol.DisplayEvent,
		BindingID:     bindingID,
		Seq:           seq,
		HasSeq:        true,
		Role:          run.Role,
		AgentID:       run.AgentID,
		RunID:         run.RunID,
		ParentAgentID: run.ParentAgentID,
		State:         run.State,
		Reason:        run.Reason,
	}}
}
