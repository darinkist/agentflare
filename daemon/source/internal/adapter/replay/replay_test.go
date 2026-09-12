package replay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/konstantinrink/agentflare/daemon/source/internal/adapter"
)

func TestLifecycleFixtureStreamsNormalizedEvents(t *testing.T) {
	fixture, err := Load(filepath.Join("testdata", "lifecycle.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.Events[0].DelayMS = 1
	source, err := NewSource(fixture)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := source.Open(context.Background(), "demo-session")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	snapshot, err := stream.Snapshot(context.Background())
	if err != nil || snapshot.Main == nil || snapshot.Main.State != "running" {
		t.Fatalf("snapshot = %#v err=%v", snapshot, err)
	}
	first := <-stream.Events()
	if first.EventID != "subagent-start" || first.Run.Role != "subagent" {
		t.Fatalf("first event = %#v", first)
	}
}

func TestSourceSnapshotAdvancesAcrossReconnect(t *testing.T) {
	main := adapter.Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "running"}
	source, err := NewSource(Fixture{
		Source: "replay", SessionID: "demo-session",
		Capabilities: adapter.Capabilities{Snapshot: true, LiveEvents: true, RunIdentity: true},
		Snapshot:     adapter.Snapshot{SourceOrder: 1, Main: &main},
		Events:       []TimedEvent{{Event: adapter.Event{SourceOrder: 2, Run: adapter.Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "succeeded"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := source.Open(context.Background(), "demo-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := <-stream.Events(); !ok {
		t.Fatal("missing terminal event")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	reconnected, err := source.Open(context.Background(), "demo-session")
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	snapshot, err := reconnected.Snapshot(context.Background())
	if err != nil || snapshot.Main != nil || snapshot.SourceOrder != 2 {
		t.Fatalf("reconnected snapshot = %#v err=%v", snapshot, err)
	}
}

func TestLoadRejectsEventWithoutSourceOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unordered.json")
	data := `{"source":"replay","session_id":"demo-session","capabilities":{"snapshot":true,"live_events":true,"run_identity":true},"snapshot":{},"events":[{"event":{"event_id":"unordered","run":{"agent_id":"main","run_id":"turn-1","role":"main","state":"running"}}}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for a live event without a source order")
	}
}

func TestResyncWithBufferedOlderEventsKeepsNewerSnapshot(t *testing.T) {
	source, err := NewSource(Fixture{
		Source: "replay", SessionID: "demo-session",
		Capabilities: adapter.Capabilities{Snapshot: true, LiveEvents: true, RunIdentity: true},
		Snapshot:     adapter.Snapshot{Main: &adapter.Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "running"}},
		Events: []TimedEvent{
			{Event: adapter.Event{EventID: "waiting", SourceOrder: 2, Run: adapter.Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "waiting_user", Reason: "approval"}}},
			{Event: adapter.Event{EventID: "subagent-start", SourceOrder: 3, Run: adapter.Run{AgentID: "worker", RunID: "child-1", Role: "subagent", ParentAgentID: "main", State: "running"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the first stream delivering its head event.
	source.apply(0, source.fixture.Events[0].Event)
	// A resync redelivers an older buffered event after the snapshot moved on.
	source.apply(1, adapter.Event{EventID: "stale", SourceOrder: 1, Run: adapter.Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "running"}})
	source.state.mu.RLock()
	order, next := source.state.snapshot.SourceOrder, source.state.nextEvent
	source.state.mu.RUnlock()
	if order != 2 || next != 1 {
		t.Fatalf("stale event moved the snapshot: order=%d next=%d, want 2 and 1", order, next)
	}
	reconnected, err := source.Open(context.Background(), "demo-session")
	if err != nil {
		t.Fatal(err)
	}
	defer reconnected.Close()
	event, ok := <-reconnected.Events()
	if !ok || event.SourceOrder != 3 {
		t.Fatalf("reconnected event = %#v ok=%v, want source order 3", event, ok)
	}
	snapshot, err := reconnected.Snapshot(context.Background())
	if err != nil || snapshot.SourceOrder != 3 || len(snapshot.Subagents) != 1 {
		t.Fatalf("reconnected snapshot = %#v err=%v", snapshot, err)
	}
}

func orderedFixture(snapshotOrder uint64, orders ...uint64) Fixture {
	events := make([]TimedEvent, len(orders))
	for index, order := range orders {
		events[index] = TimedEvent{Event: adapter.Event{
			EventID:     fmt.Sprintf("event-%d", index),
			SourceOrder: order,
			Run:         adapter.Run{AgentID: "main", RunID: "turn-1", Role: "main", State: "running"},
		}}
	}
	return Fixture{
		Source: "replay", SessionID: "demo-session",
		Capabilities: adapter.Capabilities{Snapshot: true, LiveEvents: true, RunIdentity: true},
		Snapshot:     adapter.Snapshot{SourceOrder: snapshotOrder},
		Events:       events,
	}
}

func TestNewSourceAcceptsOrderedEventsPastSnapshot(t *testing.T) {
	source, err := NewSource(orderedFixture(1, 2, 3))
	if err != nil {
		t.Fatalf("NewSource = %v", err)
	}
	if source == nil {
		t.Fatal("NewSource returned no source for ordered events")
	}
}

func TestNewSourceRejectsUnorderedEvents(t *testing.T) {
	for _, test := range []struct {
		name          string
		snapshotOrder uint64
		orders        []uint64
		want          string
	}{
		{name: "first without order", snapshotOrder: 1, orders: []uint64{0, 2}, want: "event 0 lacks a source order"},
		{name: "first at snapshot cursor", snapshotOrder: 2, orders: []uint64{2, 3}, want: "event 0 has source order 2 at or before cursor 2"},
		{name: "first before snapshot cursor", snapshotOrder: 5, orders: []uint64{3, 6}, want: "event 0 has source order 3 at or before cursor 5"},
		{name: "duplicate orders", snapshotOrder: 1, orders: []uint64{2, 2}, want: "event 1 has source order 2 at or before cursor 2"},
		{name: "falling orders", snapshotOrder: 1, orders: []uint64{3, 2}, want: "event 1 has source order 2 at or before cursor 3"},
		{name: "zero after valid prefix", snapshotOrder: 1, orders: []uint64{2, 0}, want: "event 1 lacks a source order"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, err := NewSource(orderedFixture(test.snapshotOrder, test.orders...))
			if err == nil {
				t.Fatal("expected an error for unordered fixture events")
			}
			if source != nil {
				t.Fatal("expected no source for unordered fixture events")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %q, want substring %q", err.Error(), test.want)
			}
		})
	}
}

func TestLoadRejectsFallingSourceOrders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "falling.json")
	data := `{"source":"replay","session_id":"demo-session","capabilities":{"snapshot":true,"live_events":true,"run_identity":true},"snapshot":{"source_order":1},"events":[{"event":{"event_id":"first","source_order":3,"run":{"agent_id":"main","run_id":"turn-1","role":"main","state":"running"}}},{"event":{"event_id":"second","source_order":2,"run":{"agent_id":"main","run_id":"turn-1","role":"main","state":"running"}}}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for falling fixture source orders")
	}
	if !strings.Contains(err.Error(), "event 1 has source order 2 at or before cursor 3") {
		t.Fatalf("error = %q, want the shared ordering diagnostic", err.Error())
	}
}
