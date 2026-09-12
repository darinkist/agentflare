package adapter

import (
	"errors"
	"testing"
)

func TestRegistryRejectsForeignMainTerminalWithoutMutation(t *testing.T) {
	main := Run{AgentID: "main", RunID: "run-A", Role: "main", State: "running"}
	registry, err := newRegistry(Snapshot{SourceOrder: 1, Main: &main})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.apply(Event{SourceOrder: 2, Run: Run{AgentID: "main", RunID: "run-B", Role: "main", State: "succeeded"}})
	if !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("error = %v, want %v", err, ErrUnknownRun)
	}
	if registry.main == nil || registry.main.RunID != "run-A" || registry.main.State != "running" {
		t.Fatalf("main was changed: %#v", registry.main)
	}
}

func TestRegistryDeduplicatesAndKeepsSubagentOrder(t *testing.T) {
	registry, err := newRegistry(Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	first := Event{EventID: "event-1", SourceOrder: 1, Run: Run{AgentID: "worker", RunID: "one", Role: "subagent", ParentAgentID: "main", State: "running"}}
	run, err := registry.apply(first)
	if err != nil || run == nil || run.StartOrder != 1 {
		t.Fatalf("first run = %#v err=%v", run, err)
	}
	run, err = registry.apply(first)
	if err != nil || run != nil {
		t.Fatalf("duplicate run = %#v err=%v", run, err)
	}
	second := Event{EventID: "event-2", SourceOrder: 2, Run: Run{AgentID: "worker", RunID: "two", Role: "subagent", ParentAgentID: "main", State: "running"}}
	run, err = registry.apply(second)
	if err != nil || run == nil || run.StartOrder != 2 {
		t.Fatalf("second run = %#v err=%v", run, err)
	}
	snapshot := registry.snapshot()
	if len(snapshot.Subagents) != 2 || snapshot.Subagents[0].RunID != "one" || snapshot.Subagents[1].RunID != "two" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestRegistryEnforcesOpenSubagentCapacity(t *testing.T) {
	registry, err := newRegistry(Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < MaxOpenSubagents; index++ {
		run := Run{AgentID: "worker", RunID: string(rune('a' + index)), Role: "subagent", ParentAgentID: "main", State: "running"}
		if _, err := registry.apply(Event{SourceOrder: uint64(index + 1), Run: run}); err != nil {
			t.Fatalf("run %d: %v", index, err)
		}
	}
	_, err = registry.apply(Event{SourceOrder: MaxOpenSubagents + 1, Run: Run{AgentID: "worker", RunID: "overflow", Role: "subagent", ParentAgentID: "main", State: "running"}})
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("overflow error = %v, want %v", err, ErrCapacity)
	}
}

func TestRegistryTracksNestedSubagentsAndAgentIDReuse(t *testing.T) {
	registry, err := newRegistry(Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 6; index++ {
		run := Run{AgentID: "worker", RunID: "run-" + string(rune('0'+index)), Role: "subagent", ParentAgentID: "parent-" + string(rune('0'+index%2)), State: "running"}
		if _, err := registry.apply(Event{EventID: "start-" + run.RunID, SourceOrder: uint64(index), Run: run}); err != nil {
			t.Fatalf("start %d: %v", index, err)
		}
	}
	if len(registry.snapshot().Subagents) != 6 {
		t.Fatalf("open subagents = %d", len(registry.snapshot().Subagents))
	}
	finished := Run{AgentID: "worker", RunID: "run-1", Role: "subagent", ParentAgentID: "parent-1", State: "succeeded"}
	if _, err := registry.apply(Event{EventID: "finish-run-1", SourceOrder: 7, Run: finished}); err != nil {
		t.Fatal(err)
	}
	reused := Run{AgentID: "worker", RunID: "run-7", Role: "subagent", ParentAgentID: "parent-0", State: "running"}
	if _, err := registry.apply(Event{EventID: "start-run-7", SourceOrder: 8, Run: reused}); err != nil {
		t.Fatal(err)
	}
	if len(registry.snapshot().Subagents) != 6 {
		t.Fatalf("subagents after reuse = %d", len(registry.snapshot().Subagents))
	}
}

func TestRegistryIgnoresEventsIncludedInSnapshot(t *testing.T) {
	registry, err := newRegistry(Snapshot{SourceOrder: 2})
	if err != nil {
		t.Fatal(err)
	}
	run, err := registry.apply(Event{EventID: "included", SourceOrder: 2, Run: Run{AgentID: "main", RunID: "turn", Role: "main", State: "running"}})
	if err != nil || run != nil {
		t.Fatalf("included event run = %#v err=%v", run, err)
	}

	_, err = registry.apply(Event{EventID: "unknown-terminal", SourceOrder: 3, Run: Run{AgentID: "main", RunID: "turn", Role: "main", State: "failed"}})
	if !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("contradictory event error = %v", err)
	}
}

func TestRegistryRequestsResyncForContradictoryEvent(t *testing.T) {
	registry, err := newRegistry(Snapshot{SourceOrder: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.apply(Event{EventID: "unknown-terminal", SourceOrder: 3, Run: Run{AgentID: "main", RunID: "turn", Role: "main", State: "failed"}})
	if !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("contradictory event error = %v", err)
	}
}

func TestRegistryRejectsLiveEventWithoutSourceOrder(t *testing.T) {
	main := Run{AgentID: "main", RunID: "turn", Role: "main", State: "running"}
	registry, err := newRegistry(Snapshot{SourceOrder: 1, Main: &main})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.apply(Event{EventID: "unordered", Run: Run{AgentID: "main", RunID: "turn", Role: "main", State: "waiting_user", Reason: "approval"}})
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("error = %v, want %v", err, ErrOutOfOrder)
	}
	if registry.lastOrder != 1 || registry.main == nil || registry.main.State != "running" {
		t.Fatalf("registry was changed: order=%d main=%#v", registry.lastOrder, registry.main)
	}
}

func TestRegistryIgnoresOlderBufferedEventAfterResync(t *testing.T) {
	main := Run{AgentID: "main", RunID: "turn", Role: "main", State: "running"}
	registry, err := newRegistry(Snapshot{SourceOrder: 5, Main: &main})
	if err != nil {
		t.Fatal(err)
	}
	run, err := registry.apply(Event{EventID: "stale", SourceOrder: 3, Run: Run{AgentID: "main", RunID: "turn", Role: "main", State: "waiting_user", Reason: "approval"}})
	if err != nil || run != nil {
		t.Fatalf("stale event run = %#v err=%v", run, err)
	}
	if registry.lastOrder != 5 || registry.main == nil || registry.main.State != "running" {
		t.Fatalf("snapshot moved backwards: order=%d main=%#v", registry.lastOrder, registry.main)
	}
}
