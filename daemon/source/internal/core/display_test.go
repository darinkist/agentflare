package core

import (
	"testing"
)

func displayBindEvent() Event {
	return Event{Display: &DisplayEvent{
		Operation: DisplayBind,
		Source:    "codex",
		SessionID: "session-1",
		BindID:    "bind-1",
	}}
}

func displayWriteEvent(operation DisplayOperation, seq uint64) Event {
	return Event{Display: &DisplayEvent{
		Operation: operation,
		BindingID: "binding-1",
		Seq:       seq,
	}}
}

func TestDisplayBindStateMachineAndPalette(t *testing.T) {
	clock := newFakeClock()
	stateCore := New(clock)
	if _, err := stateCore.handle(Event{Type: EventLightsSet, LEDIndices: []int{12, 21}, Color: Color{R: 9, G: 8, B: 7}}); err != nil {
		t.Fatal(err)
	}
	bind, err := stateCore.handle(displayBindEvent())
	if err != nil {
		t.Fatal(err)
	}
	if bind.Display.BindingID == "" || bind.Display.NextSeq != 1 || bind.Display.LeaseMS != DisplayLeaseDuration.Milliseconds() {
		t.Fatalf("bind result = %#v", bind.Display)
	}
	if stateCore.status().Lights[12].Layer != LayerDisplayProtection || stateCore.status().Lights[12].Color != OffColor {
		t.Fatalf("bound idle key = %#v", stateCore.status().Lights[12])
	}
	if stateCore.status().Lights[21].Layer != LayerDisplayProtection || stateCore.status().Lights[21].Color != OffColor {
		t.Fatalf("bound idle underglow = %#v", stateCore.status().Lights[21])
	}

	running := displayWriteEvent(DisplayEventOperation, 1)
	running.Display.BindingID = bind.Display.BindingID
	running.Display.Role = DisplaySubagentRole
	running.Display.AgentID = "worker-a"
	running.Display.RunID = "run-a-1"
	running.Display.ParentAgentID = "main"
	running.Display.State = DisplayRunning
	result, err := stateCore.handle(running)
	if err != nil || !result.Display.HasSlot || result.Display.Slot != 0 {
		t.Fatalf("running result = %#v err=%v", result.Display, err)
	}
	if got, want := stateCore.status().Lights[12].Color, scaleColor(displaySubagentRunningColors[0], DefaultKeyBrightness); got != want {
		t.Fatalf("running color = %#v, want %#v", got, want)
	}
	if got := stateCore.status().Lights[21].Color; got != OffColor {
		t.Fatalf("running subagent changed underglow = %#v", got)
	}

	waiting := running
	waiting.Display = copyDisplayEvent(running.Display)
	waiting.Display.Seq = 2
	waiting.Display.State = DisplayWaitingUser
	if _, err := stateCore.handle(waiting); err != nil {
		t.Fatal(err)
	}
	if got, want := stateCore.status().Lights[12].Color, scaleColor(displaySubagentWaitingUserColor, DefaultKeyBrightness); got != want {
		t.Fatalf("waiting color = %#v, want %#v", got, want)
	}
	if got := stateCore.status().Lights[21].Color; got != OffColor {
		t.Fatalf("waiting subagent changed underglow = %#v", got)
	}

	finished := waiting
	finished.Display = copyDisplayEvent(waiting.Display)
	finished.Display.Seq = 3
	finished.Display.State = DisplaySucceeded
	result, err = stateCore.handle(finished)
	if err != nil || !result.Display.HasTerminalDeadline {
		t.Fatalf("finished result = %#v err=%v", result.Display, err)
	}
	if want := clock.current().Add(DisplayTerminalDuration); !result.Display.TerminalDeadline.Equal(want) {
		t.Fatalf("terminal deadline = %v, want %v", result.Display.TerminalDeadline, want)
	}
	if got, want := stateCore.status().Lights[12].Color, scaleColor(displaySucceededColor, DefaultKeyBrightness); got != want {
		t.Fatalf("success color = %#v, want %#v", got, want)
	}
	if stateCore.status().Lights[21].Layer != LayerDisplayProtection {
		t.Fatalf("subagent completion changed underglow = %#v", stateCore.status().Lights[21])
	}

	duplicate := finished
	duplicate.Display = copyDisplayEvent(finished.Display)
	duplicateResult, err := stateCore.handle(duplicate)
	if err != nil || duplicateResult.Display.TerminalDeadline != result.Display.TerminalDeadline {
		t.Fatalf("duplicate result = %#v err=%v", duplicateResult.Display, err)
	}
	duplicate.Display = copyDisplayEvent(finished.Display)
	duplicate.Display.State = DisplayFailed
	if _, err := stateCore.handle(duplicate); err != ErrSequenceConflict {
		t.Fatalf("sequence conflict error = %v", err)
	}
}

func TestDisplayMainUnderglowPaletteIsIndependentFromSubagentKeys(t *testing.T) {
	stateCore := New(newFakeClock())
	bind, err := stateCore.handle(displayBindEvent())
	if err != nil {
		t.Fatal(err)
	}

	main := displayWriteEvent(DisplayEventOperation, 1)
	main.Display.BindingID = bind.Display.BindingID
	main.Display.Role = DisplayMainRole
	main.Display.AgentID = "main"
	main.Display.RunID = "main-run"
	main.Display.State = DisplayRunning
	if _, err := stateCore.handle(main); err != nil {
		t.Fatal(err)
	}
	if got, want := stateCore.status().Lights[21].Color, scaleColor(displayMainRunningColor, DefaultUnderglowBrightness); got != want {
		t.Fatalf("running underglow = %#v, want %#v", got, want)
	}

	main.Display = copyDisplayEvent(main.Display)
	main.Display.Seq = 2
	main.Display.State = DisplayWaitingUser
	if _, err := stateCore.handle(main); err != nil {
		t.Fatal(err)
	}
	if got, want := stateCore.status().Lights[21].Color, scaleColor(displayMainWaitingUserColor, DefaultUnderglowBrightness); got != want {
		t.Fatalf("waiting underglow = %#v, want %#v", got, want)
	}
	if got := stateCore.status().Lights[12].Color; got != OffColor {
		t.Fatalf("main state changed subagent key = %#v", got)
	}
}

func TestDisplayQueuePromotesAfterTerminalDeadline(t *testing.T) {
	clock := newFakeClock()
	stateCore := New(clock)
	if _, err := stateCore.handle(displayBindEvent()); err != nil {
		t.Fatal(err)
	}
	seq := uint64(1)
	for index := 0; index < SlotCount; index++ {
		event := displayWriteEvent(DisplayEventOperation, seq)
		event.Display.BindingID = stateCore.display.bindingID
		seq++
		event.Display.Role = DisplaySubagentRole
		event.Display.AgentID = "worker"
		event.Display.RunID = string(rune('a' + index))
		event.Display.ParentAgentID = "main"
		event.Display.State = DisplayRunning
		if result, err := stateCore.handle(event); err != nil || !result.Display.HasSlot || result.Display.Slot != index {
			t.Fatalf("slot %d result=%#v err=%v", index, result.Display, err)
		}
	}
	for index, color := range displaySubagentRunningColors {
		if got, want := stateCore.status().Lights[12+index].Color, scaleColor(color, DefaultKeyBrightness); got != want {
			t.Fatalf("slot %d running color = %#v, want %#v", index, got, want)
		}
	}
	queued := displayWriteEvent(DisplayEventOperation, seq)
	queued.Display.BindingID = stateCore.display.bindingID
	seq++
	queued.Display.Role = DisplaySubagentRole
	queued.Display.AgentID = "worker"
	queued.Display.RunID = "queued"
	queued.Display.ParentAgentID = "main"
	queued.Display.State = DisplayRunning
	result, err := stateCore.handle(queued)
	if err != nil || result.Display.Assignment != "queued" || len(stateCore.display.queue) != 1 {
		t.Fatalf("queued result=%#v err=%v queue=%d", result.Display, err, len(stateCore.display.queue))
	}

	finish := displayWriteEvent(DisplayEventOperation, seq)
	finish.Display.BindingID = stateCore.display.bindingID
	seq++
	finish.Display.Role = DisplaySubagentRole
	finish.Display.AgentID = "worker"
	finish.Display.RunID = "a"
	finish.Display.ParentAgentID = "main"
	finish.Display.State = DisplaySucceeded
	if _, err := stateCore.handle(finish); err != nil {
		t.Fatal(err)
	}
	clock.advance(DisplayTerminalDuration)
	if !stateCore.expireDue(clock.current()) {
		t.Fatal("terminal expiry did not change state")
	}
	if stateCore.display.slots[0] == nil || stateCore.display.slots[0].identity.RunID != "queued" {
		t.Fatalf("queue was not promoted: %#v", stateCore.display.slots[0])
	}
	if stateCore.display.slots[0].state != DisplayRunning {
		t.Fatalf("promoted state = %s", stateCore.display.slots[0].state)
	}
}

func TestDisplaySyncPreservesVisibleSlotsAndClearsTerminalSignals(t *testing.T) {
	stateCore := New(newFakeClock())
	if _, err := stateCore.handle(displayBindEvent()); err != nil {
		t.Fatal(err)
	}
	bindingID := stateCore.display.bindingID
	for index, runID := range []string{"a", "b"} {
		event := displayWriteEvent(DisplayEventOperation, uint64(index+1))
		event.Display.BindingID = bindingID
		event.Display.Role = DisplaySubagentRole
		event.Display.AgentID = "worker"
		event.Display.RunID = runID
		event.Display.ParentAgentID = "main"
		event.Display.State = DisplayRunning
		if _, err := stateCore.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	terminal := displayWriteEvent(DisplayEventOperation, 3)
	terminal.Display.BindingID = bindingID
	terminal.Display.Role = DisplaySubagentRole
	terminal.Display.AgentID = "worker"
	terminal.Display.RunID = "a"
	terminal.Display.ParentAgentID = "main"
	terminal.Display.State = DisplaySucceeded
	if _, err := stateCore.handle(terminal); err != nil {
		t.Fatal(err)
	}

	sync := displayWriteEvent(DisplaySync, 4)
	sync.Display.BindingID = bindingID
	sync.Display.HasMain = true
	sync.Display.Subagents = []DisplaySnapshot{
		{AgentID: "worker", RunID: "b", ParentAgentID: "main", State: DisplayWaitingUser, StartOrder: 2},
		{AgentID: "worker", RunID: "c", ParentAgentID: "main", State: DisplayRunning, StartOrder: 3},
	}
	result, err := stateCore.handle(sync)
	if err != nil {
		t.Fatal(err)
	}
	if result.Display.AcceptedSeq != 4 || stateCore.display.main != nil {
		t.Fatalf("sync result=%#v main=%#v", result.Display, stateCore.display.main)
	}
	if stateCore.display.slots[1] == nil || stateCore.display.slots[1].identity.RunID != "b" {
		t.Fatalf("existing slot was not preserved: %#v", stateCore.display.slots)
	}
	if stateCore.display.slots[0] == nil || stateCore.display.slots[0].identity.RunID != "c" {
		t.Fatalf("new run was not assigned after preserved slots: %#v", stateCore.display.slots)
	}
	if stateCore.status().Lights[12].Color != scaleColor(displaySubagentRunningColors[0], DefaultKeyBrightness) {
		t.Fatalf("cleared terminal signal was not replaced by the new run: %#v", stateCore.status().Lights[12])
	}
}

func TestDisplayLeaseLossProtectsReservedLEDs(t *testing.T) {
	clock := newFakeClock()
	stateCore := New(clock)
	if _, err := stateCore.handle(Event{Type: EventLightsSet, LEDIndices: []int{12, 21}, Color: Color{R: 1, G: 2, B: 3}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stateCore.handle(displayBindEvent()); err != nil {
		t.Fatal(err)
	}
	clock.advance(DisplayLeaseDuration)
	if !stateCore.expireDue(clock.current()) {
		t.Fatal("lease expiry did not change state")
	}
	status := stateCore.displayStatus()
	if status.Health != DisplayStale || status.MainState != DisplayUnknown || status.Slots[0].State != DisplayUnknown {
		t.Fatalf("stale status = %#v", status)
	}
	if stateCore.status().Lights[12].Color != OffColor || stateCore.status().Lights[12].Layer != LayerDisplayProtection {
		t.Fatalf("stale key = %#v", stateCore.status().Lights[12])
	}
	if _, err := stateCore.handle(Event{Display: &DisplayEvent{
		Operation: DisplayBind,
		Source:    "codex",
		SessionID: "session-1",
		BindID:    "bind-2",
	}}); err != nil {
		t.Fatal(err)
	}
	if stateCore.status().Lights[12].Color != OffColor || stateCore.status().Lights[12].Layer != LayerDisplayProtection {
		t.Fatalf("rebound idle key exposed manual layer = %#v", stateCore.status().Lights[12])
	}
	if _, err := stateCore.handle(Event{Display: &DisplayEvent{
		Operation: DisplayUnbind,
		BindingID: stateCore.display.bindingID,
		Seq:       1,
	}}); err != nil {
		t.Fatal(err)
	}
	if stateCore.status().Lights[12].Color != (Color{R: 1, G: 2, B: 3}) || stateCore.displayStatus().Health != DisplayUnbound {
		t.Fatalf("unbind did not reveal manual layer: light=%#v status=%#v", stateCore.status().Lights[12], stateCore.displayStatus())
	}
}

func TestDisplayRejectsTerminalEventForDifferentMainRun(t *testing.T) {
	stateCore := New(newFakeClock())
	if _, err := stateCore.handle(displayBindEvent()); err != nil {
		t.Fatal(err)
	}
	bindingID := stateCore.display.bindingID

	running := displayWriteEvent(DisplayEventOperation, 1)
	running.Display.BindingID = bindingID
	running.Display.Role = DisplayMainRole
	running.Display.AgentID = "main"
	running.Display.RunID = "run-A"
	running.Display.State = DisplayRunning
	if _, err := stateCore.handle(running); err != nil {
		t.Fatal(err)
	}

	foreignTerminal := displayWriteEvent(DisplayEventOperation, 2)
	foreignTerminal.Display.BindingID = bindingID
	foreignTerminal.Display.Role = DisplayMainRole
	foreignTerminal.Display.AgentID = "main"
	foreignTerminal.Display.RunID = "run-B"
	foreignTerminal.Display.State = DisplaySucceeded
	if _, err := stateCore.handle(foreignTerminal); err != ErrUnknownAgent {
		t.Fatalf("foreign terminal error = %v, want %v", err, ErrUnknownAgent)
	}
	if stateCore.display.main == nil || stateCore.display.main.identity.RunID != "run-A" || stateCore.display.main.state != DisplayRunning {
		t.Fatalf("foreign terminal changed main run: %#v", stateCore.display.main)
	}
	if stateCore.display.expectedSeq != 2 {
		t.Fatalf("foreign terminal consumed sequence: next = %d", stateCore.display.expectedSeq)
	}

	running.Display = copyDisplayEvent(running.Display)
	running.Display.Seq = 2
	running.Display.State = DisplaySucceeded
	if _, err := stateCore.handle(running); err != nil {
		t.Fatalf("current terminal rejected: %v", err)
	}
}

func TestDisplayUnbindReplaysExactRetry(t *testing.T) {
	stateCore := New(newFakeClock())
	if _, err := stateCore.handle(displayBindEvent()); err != nil {
		t.Fatal(err)
	}
	unbind := displayWriteEvent(DisplayUnbind, 1)
	unbind.Display.BindingID = stateCore.display.bindingID
	result, err := stateCore.handle(unbind)
	if err != nil || !result.Display.Unbound || result.Display.AcceptedSeq != 1 {
		t.Fatalf("unbind result = %#v err=%v", result.Display, err)
	}

	retry := unbind
	retry.Display = copyDisplayEvent(unbind.Display)
	retried, err := stateCore.handle(retry)
	if err != nil || retried.Display != result.Display {
		t.Fatalf("unbind retry result = %#v err=%v, want %#v", retried.Display, err, result.Display)
	}

	retry.Display = copyDisplayEvent(unbind.Display)
	retry.Display.Seq = 2
	if _, err := stateCore.handle(retry); err != ErrUnknownBinding {
		t.Fatalf("different unbind request error = %v, want %v", err, ErrUnknownBinding)
	}
}

func copyDisplayEvent(event *DisplayEvent) *DisplayEvent {
	copy := *event
	copy.Fingerprint = nil
	return &copy
}
