package adapter

import "sort"

type registry struct {
	main      *Run
	subagents map[string]Run
	seen      map[string]struct{}
	seenOrder []string
	lastOrder uint64
	nextOrder uint64
}

func newRegistry(snapshot Snapshot) (*registry, error) {
	r := &registry{subagents: make(map[string]Run), seen: make(map[string]struct{}), nextOrder: 1}
	if err := r.replace(snapshot); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *registry) replace(snapshot Snapshot) error {
	if len(snapshot.Subagents) > MaxOpenSubagents {
		return ErrCapacity
	}
	if snapshot.Main != nil {
		if err := validateRun(*snapshot.Main, false); err != nil {
			return err
		}
		if snapshot.Main.Role != "main" {
			return ErrUnknownRun
		}
		main := *snapshot.Main
		r.main = &main
	} else {
		r.main = nil
	}
	r.subagents = make(map[string]Run, len(snapshot.Subagents))
	maxOrder := uint64(0)
	for _, run := range snapshot.Subagents {
		if err := validateRun(run, false); err != nil {
			return err
		}
		if run.Role != "subagent" || run.StartOrder == 0 {
			return ErrUnknownRun
		}
		key := runKey(run)
		if _, exists := r.subagents[key]; exists {
			return ErrOutOfOrder
		}
		r.subagents[key] = run
		if run.StartOrder > maxOrder {
			maxOrder = run.StartOrder
		}
	}
	r.lastOrder = snapshot.SourceOrder
	r.nextOrder = maxOrder + 1
	if r.nextOrder == 0 {
		r.nextOrder = 1
	}
	return nil
}

// apply updates the authoritative local state before returning a state change
// for delivery. A caller can safely resync after a transport failure without
// replaying terminal history.
func (r *registry) apply(event Event) (*Run, error) {
	// Events buffered before a snapshot are expected during resynchronization.
	// The snapshot already represents them, so they must not trigger another
	// resync or mutate the local state. Live events without a source cursor
	// carry no ordering proof and are rejected instead.
	if event.SourceOrder == 0 {
		return nil, ErrOutOfOrder
	}
	if event.SourceOrder <= r.lastOrder {
		return nil, nil
	}
	if event.EventID != "" {
		if _, exists := r.seen[event.EventID]; exists {
			return nil, nil
		}
		r.seen[event.EventID] = struct{}{}
		r.seenOrder = append(r.seenOrder, event.EventID)
		if len(r.seenOrder) > MaxPendingEvents*2 {
			delete(r.seen, r.seenOrder[0])
			r.seenOrder = r.seenOrder[1:]
		}
	}
	if err := validateRun(event.Run, true); err != nil {
		return nil, err
	}
	r.lastOrder = event.SourceOrder
	run := event.Run
	if run.Role == "main" {
		if terminalState(run.State) {
			if r.main == nil || r.main.AgentID != run.AgentID || r.main.RunID != run.RunID {
				return nil, ErrUnknownRun
			}
			r.main = nil
			return &run, nil
		}
		if r.main != nil && r.main.AgentID == run.AgentID && r.main.RunID == run.RunID && r.main.State == run.State {
			return nil, nil
		}
		r.main = &run
		return &run, nil
	}
	key := runKey(run)
	current, exists := r.subagents[key]
	if terminalState(run.State) {
		if !exists {
			return nil, ErrUnknownRun
		}
		delete(r.subagents, key)
		return &run, nil
	}
	if exists && current.State == run.State {
		return nil, nil
	}
	if !exists {
		if len(r.subagents) >= MaxOpenSubagents {
			return nil, ErrCapacity
		}
		if run.StartOrder == 0 {
			run.StartOrder = r.nextOrder
			r.nextOrder++
		}
	}
	r.subagents[key] = run
	return &run, nil
}

func (r *registry) snapshot() Snapshot {
	snapshot := Snapshot{SourceOrder: r.lastOrder, Subagents: make([]Run, 0, len(r.subagents))}
	if r.main != nil {
		main := *r.main
		snapshot.Main = &main
	}
	for _, run := range r.subagents {
		snapshot.Subagents = append(snapshot.Subagents, run)
	}
	sort.Slice(snapshot.Subagents, func(i, j int) bool { return snapshot.Subagents[i].StartOrder < snapshot.Subagents[j].StartOrder })
	return snapshot
}

func runKey(run Run) string { return run.AgentID + "\x00" + run.RunID }
