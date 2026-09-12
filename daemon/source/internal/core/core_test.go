package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func (c *fakeClock) current() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(_ time.Duration) Timer {
	timer := &fakeTimer{channel: make(chan time.Time, 1)}
	c.mu.Lock()
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	return timer
}

func (c *fakeClock) lastTimer() *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.timers[len(c.timers)-1]
}

type fakeTimer struct {
	channel chan time.Time
	stopped bool
}

func (t *fakeTimer) Chan() <-chan time.Time { return t.channel }
func (t *fakeTimer) Stop() bool {
	wasRunning := !t.stopped
	t.stopped = true
	return wasRunning
}
func (t *fakeTimer) Fire() { t.channel <- time.Now() }

func startTestCore(t *testing.T, clock Clock) *Core {
	t.Helper()
	stateCore := New(clock)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = stateCore.Run(ctx) }()
	return stateCore
}

func submitEvent(t *testing.T, stateCore *Core, event Event) (Result, error) {
	t.Helper()
	return stateCore.Submit(context.Background(), event)
}

func identity(source, agentID string) AgentIdentity {
	return AgentIdentity{Source: source, AgentID: agentID}
}

func nextSnapshot(t *testing.T, stateCore *Core) DesiredState {
	t.Helper()
	return <-stateCore.Snapshots()
}

func TestSlotAssignmentColorsAndPhysicalIndexes(t *testing.T) {
	stateCore := startTestCore(t, newFakeClock())
	_ = nextSnapshot(t, stateCore)

	for index := 0; index < SlotCount; index++ {
		result, err := submitEvent(t, stateCore, Event{Type: EventStarted, Identity: identity("demo", string(rune('a'+index)))})
		if err != nil {
			t.Fatalf("start slot %d: %v", index, err)
		}
		if result.Slot != index {
			t.Fatalf("start slot = %d, want %d", result.Slot, index)
		}
		desired := nextSnapshot(t, stateCore)
		led := desired.LEDs[PhysicalLEDIndexes[index]]
		if led.Index != PhysicalLEDIndexes[index] {
			t.Fatalf("LED index = %d, want %d", led.Index, PhysicalLEDIndexes[index])
		}
		if led.Color != ActiveColors[index] {
			t.Fatalf("active color = %#v, want %#v", led.Color, ActiveColors[index])
		}
	}

	_, err := submitEvent(t, stateCore, Event{Type: EventStarted, Identity: identity("demo", "fifth")})
	if err != ErrSlotExhausted {
		t.Fatalf("fifth start error = %v, want %v", err, ErrSlotExhausted)
	}
}

func TestDifferentSourcesDoNotShareIdentity(t *testing.T) {
	stateCore := startTestCore(t, newFakeClock())
	_ = nextSnapshot(t, stateCore)
	first, err := submitEvent(t, stateCore, Event{Type: EventStarted, Identity: identity("one", "same")})
	if err != nil {
		t.Fatal(err)
	}
	_ = nextSnapshot(t, stateCore)
	second, err := submitEvent(t, stateCore, Event{Type: EventStarted, Identity: identity("two", "same")})
	if err != nil {
		t.Fatal(err)
	}
	if first.Slot == second.Slot {
		t.Fatalf("different sources shared slot %d", first.Slot)
	}
}

func TestParallelStartsAllocateAtMostFourSlots(t *testing.T) {
	stateCore := startTestCore(t, newFakeClock())
	_ = nextSnapshot(t, stateCore)
	const count = 12
	results := make(chan error, count)
	var waitGroup sync.WaitGroup
	for index := 0; index < count; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			_, err := stateCore.Submit(context.Background(), Event{Type: EventStarted, Identity: identity("parallel", string(rune('a'+index)))})
			results <- err
		}(index)
	}
	waitGroup.Wait()
	close(results)

	var successes, exhausted int
	for err := range results {
		switch err {
		case nil:
			successes++
		default:
			if err == ErrSlotExhausted {
				exhausted++
			} else {
				t.Errorf("parallel start: %v", err)
			}
		}
	}
	if successes != SlotCount || exhausted != count-SlotCount {
		t.Fatalf("successes=%d exhausted=%d, want %d and %d", successes, exhausted, SlotCount, count-SlotCount)
	}
}

func TestTerminalStatesAndReactivation(t *testing.T) {
	clock := newFakeClock()
	stateCore := startTestCore(t, clock)
	_ = nextSnapshot(t, stateCore)
	agent := identity("demo", "alpha")
	if _, err := submitEvent(t, stateCore, Event{Type: EventStarted, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	_ = nextSnapshot(t, stateCore)
	if _, err := submitEvent(t, stateCore, Event{Type: EventFinished, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	finished := nextSnapshot(t, stateCore)
	if finished.LEDs[12].Color != (Color{G: 255}) {
		t.Fatalf("finished color = %#v", finished.LEDs[12].Color)
	}
	other := identity("demo", "beta")
	result, err := submitEvent(t, stateCore, Event{Type: EventStarted, Identity: other})
	if err != nil || result.Slot != 1 {
		t.Fatalf("other start slot=%d err=%v, want slot 1", result.Slot, err)
	}
	_ = nextSnapshot(t, stateCore)
	result, err = submitEvent(t, stateCore, Event{Type: EventStarted, Identity: agent})
	if err != nil || result.Slot != 0 {
		t.Fatalf("reactivation slot=%d err=%v, want slot 0", result.Slot, err)
	}
	active := nextSnapshot(t, stateCore)
	if active.LEDs[12].Color != ActiveColors[0] {
		t.Fatalf("reactivated color = %#v", active.LEDs[12].Color)
	}
	if _, err := submitEvent(t, stateCore, Event{Type: EventFailed, Identity: other}); err != nil {
		t.Fatal(err)
	}
	failed := nextSnapshot(t, stateCore)
	if failed.LEDs[13].Color != (Color{R: 255}) {
		t.Fatalf("failed color = %#v", failed.LEDs[13].Color)
	}
}

func TestInvalidAndUnknownTransitions(t *testing.T) {
	stateCore := startTestCore(t, newFakeClock())
	_ = nextSnapshot(t, stateCore)
	unknown := identity("demo", "unknown")
	if _, err := submitEvent(t, stateCore, Event{Type: EventFinished, Identity: unknown}); err != ErrUnknownAgent {
		t.Fatalf("unknown error = %v, want %v", err, ErrUnknownAgent)
	}
	agent := identity("demo", "alpha")
	if _, err := submitEvent(t, stateCore, Event{Type: EventStarted, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	_ = nextSnapshot(t, stateCore)
	if _, err := submitEvent(t, stateCore, Event{Type: EventFailed, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	_ = nextSnapshot(t, stateCore)
	for _, eventType := range []EventType{EventHeartbeat, EventFinished, EventFailed, EventCancelled} {
		if _, err := submitEvent(t, stateCore, Event{Type: eventType, Identity: agent}); err != ErrInvalidTransition {
			t.Errorf("terminal %s error = %v, want %v", eventType, err, ErrInvalidTransition)
		}
	}
}

func TestLeaseAndTerminalTimers(t *testing.T) {
	clock := newFakeClock()
	stateCore := startTestCore(t, clock)
	_ = nextSnapshot(t, stateCore)
	agent := identity("demo", "alpha")
	if _, err := submitEvent(t, stateCore, Event{Type: EventStarted, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	_ = nextSnapshot(t, stateCore)
	clock.advance(LeaseDuration)
	clock.lastTimer().Fire()
	free := nextSnapshot(t, stateCore)
	if free.LEDs[12].Color != OffColor {
		t.Fatalf("lease expiry color = %#v, want off", free.LEDs[12].Color)
	}
	if result, err := submitEvent(t, stateCore, Event{Type: EventHeartbeat, Identity: agent}); err != ErrUnknownAgent || result.Slot != -1 {
		t.Fatalf("expired heartbeat result=%#v err=%v", result, err)
	}

	if _, err := submitEvent(t, stateCore, Event{Type: EventStarted, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	_ = nextSnapshot(t, stateCore)
	if _, err := submitEvent(t, stateCore, Event{Type: EventFailed, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	_ = nextSnapshot(t, stateCore)
	clock.advance(TerminalDuration)
	clock.mu.Lock()
	terminalTimer := clock.timers[len(clock.timers)-2]
	clock.mu.Unlock()
	terminalTimer.Fire()
	free = nextSnapshot(t, stateCore)
	if free.LEDs[12].Color != OffColor {
		t.Fatalf("terminal expiry color = %#v, want off", free.LEDs[12].Color)
	}
}

func TestStaleGenerationAndClearInvalidateTimers(t *testing.T) {
	clock := newFakeClock()
	stateCore := New(clock)
	agent := identity("demo", "alpha")
	if _, err := stateCore.handle(Event{Type: EventStarted, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	old := timerRef{slot: 0, generation: stateCore.slots[0].generation, kind: leaseTimer}
	if _, err := stateCore.handle(Event{Type: EventStarted, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	stateCore.handleTimer(old)
	if stateCore.slots[0].state != Active {
		t.Fatal("stale lease timer changed renewed active slot")
	}
	if _, err := stateCore.handle(Event{Type: EventClear}); err != nil {
		t.Fatal(err)
	}
	stateCore.handleTimer(timerRef{slot: 0, generation: stateCore.slots[0].generation - 1, kind: leaseTimer})
	if stateCore.slots[0].state != Free || stateCore.slots[0].hasIdentity {
		t.Fatal("stale timer changed cleared slot")
	}
}

func TestSnapshotsKeepOnlyLatestDesiredState(t *testing.T) {
	clock := newFakeClock()
	stateCore := New(clock)
	agent := identity("demo", "alpha")
	if _, err := stateCore.handle(Event{Type: EventStarted, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	if _, err := stateCore.handle(Event{Type: EventCancelled, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	latest := <-stateCore.Snapshots()
	for _, led := range latest.LEDs {
		if led.Color != OffColor {
			t.Fatalf("latest LED state = %#v, want off", latest)
		}
	}
	select {
	case <-stateCore.Snapshots():
		t.Fatal("snapshot channel retained an older state")
	default:
	}
}

func TestDesiredStateContainsAllLEDsInPhysicalOrder(t *testing.T) {
	desired := InitialDesiredState()
	for index, led := range desired.LEDs {
		if led.Index != index || led.Color != OffColor {
			t.Fatalf("LED %d = %#v, want index %d off", index, led, index)
		}
	}
}

func TestManualLayersAndTTLRecovery(t *testing.T) {
	clock := newFakeClock()
	stateCore := New(clock)
	if _, err := stateCore.handle(Event{Type: EventLightsSet, LEDIndices: []int{21}, Color: Color{B: 10}}); err != nil {
		t.Fatal(err)
	}
	if stateCore.status().Lights[21].Layer != LayerManualPersistent {
		t.Fatalf("persistent layer = %q", stateCore.status().Lights[21].Layer)
	}
	result, err := stateCore.handle(Event{Type: EventLightsSet, LEDIndices: []int{21}, Color: Color{R: 20}, TTL: 2 * time.Second, HasTTL: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Lights.HasExpiry || result.Lights.ExpiresAt != clock.current().Add(2*time.Second) {
		t.Fatalf("TTL result = %#v", result.Lights)
	}
	if stateCore.status().Lights[21].Color != (Color{R: 20}) || stateCore.status().Lights[21].Layer != LayerManualTTL {
		t.Fatalf("TTL resolution = %#v", stateCore.status().Lights[21])
	}
	if _, err := stateCore.handle(Event{Type: EventLightsSet, LEDIndices: []int{21}, Color: Color{G: 30}}); err != nil {
		t.Fatal(err)
	}
	if stateCore.status().Lights[21].Color != (Color{R: 20}) {
		t.Fatalf("persistent set replaced TTL = %#v", stateCore.status().Lights[21])
	}
	clock.mu.Lock()
	clock.now = result.Lights.ExpiresAt
	clock.mu.Unlock()
	status, err := stateCore.handle(Event{Type: EventStatus})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status.Lights[21].Color != (Color{G: 30}) || status.Status.Lights[21].Layer != LayerManualPersistent {
		t.Fatalf("TTL recovery = %#v", status.Status.Lights[21])
	}

	if _, err := stateCore.handle(Event{Type: EventLightsSet, LEDIndices: []int{21}, Color: OffColor, TTL: time.Second, HasTTL: true}); err != nil {
		t.Fatal(err)
	}
	if stateCore.status().Lights[21].Layer != LayerManualTTL || stateCore.status().Lights[21].Color != OffColor {
		t.Fatalf("temporary black = %#v", stateCore.status().Lights[21])
	}
	if _, err := stateCore.handle(Event{Type: EventLightsClear, LEDIndices: []int{21}}); err != nil {
		t.Fatal(err)
	}
	if stateCore.status().Lights[21].Layer != LayerOff {
		t.Fatalf("clear did not remove persistent layer = %#v", stateCore.status().Lights[21])
	}
}

func TestSlotAndLifecycleLayersHavePriority(t *testing.T) {
	clock := newFakeClock()
	stateCore := New(clock)
	if _, err := stateCore.handle(Event{Type: EventLightsSet, LEDIndices: []int{12, 21}, Color: Color{B: 40}}); err != nil {
		t.Fatal(err)
	}
	agent := identity("demo", "alpha")
	if _, err := stateCore.handle(Event{Type: EventStarted, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	status, err := stateCore.handle(Event{Type: EventStatus})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status.Lights[12].Layer != LayerAgentSlot || status.Status.Lights[12].Color != ActiveColors[0] {
		t.Fatalf("slot priority = %#v", status.Status.Lights[12])
	}
	if _, err := stateCore.handle(Event{Type: EventFinished, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	status, err = stateCore.handle(Event{Type: EventStatus})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status.Lights[21].Layer != LayerLifecycle || status.Status.Lights[21].Color != (Color{G: 64}) {
		t.Fatalf("lifecycle priority = %#v", status.Status.Lights[21])
	}
	if _, err := stateCore.handle(Event{Type: EventStarted, Identity: agent}); err != nil {
		t.Fatal(err)
	}
	status, err = stateCore.handle(Event{Type: EventStatus})
	if err != nil {
		t.Fatal(err)
	}
	if status.Status.Lights[21].Layer != LayerLifecycle {
		t.Fatalf("reactivation cleared lifecycle = %#v", status.Status.Lights[21])
	}
}

func TestInvalidLightRequestIsAtomic(t *testing.T) {
	stateCore := New(newFakeClock())
	if _, err := stateCore.handle(Event{Type: EventLightsSet, LEDIndices: []int{21}, Color: Color{B: 10}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stateCore.handle(Event{Type: EventLightsSet, LEDIndices: []int{21, 27}, Color: Color{R: 10}}); err == nil {
		t.Fatal("invalid light target was accepted")
	}
	if stateCore.status().Lights[21].Color != (Color{B: 10}) {
		t.Fatalf("invalid request mutated state = %#v", stateCore.status().Lights[21])
	}
}
