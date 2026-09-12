package core

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// DisplayOptions controls the semantic V2 display brightness.
type DisplayOptions struct {
	KeyBrightness       int
	UnderglowBrightness int
}

var (
	// displayMainRunningColor makes ongoing main-agent work visible without
	// competing with the accent-colored subagent keys.
	displayMainRunningColor     = Color{R: 228, G: 235, B: 255}
	displayMainWaitingUserColor = Color{R: 48, G: 144, B: 255}
	// displaySubagentRunningColors gives each physical subagent slot a distinct
	// accent color, while preserving its stable position in the four-slot row.
	displaySubagentRunningColors = [SlotCount]Color{
		{R: 167, G: 139, B: 250},
		{R: 163, G: 230, B: 53},
		{R: 16, G: 185, B: 129},
		{R: 251, G: 191, B: 36},
	}
	displaySubagentWaitingUserColor = Color{R: 249, G: 115, B: 22}
	displaySucceededColor           = Color{R: 52, G: 211, B: 153}
	displayFailedColor              = Color{R: 239, G: 68, B: 68}
)

// DisplayOperation identifies a V2 controller operation.
type DisplayOperation string

const (
	DisplayBind           DisplayOperation = "display_bind"
	DisplayEventOperation DisplayOperation = "display_event"
	DisplaySync           DisplayOperation = "display_sync"
	DisplayHeartbeat      DisplayOperation = "display_heartbeat"
	DisplayUnbind         DisplayOperation = "display_unbind"
	DisplayStatus         DisplayOperation = "status"
)

// DisplayRole identifies the main agent or a subagent.
type DisplayRole string

const (
	DisplayMainRole     DisplayRole = "main"
	DisplaySubagentRole DisplayRole = "subagent"
)

// DisplayState is the V2 lifecycle state visible to the controller.
type DisplayState string

const (
	DisplayRunning     DisplayState = "running"
	DisplayWaitingUser DisplayState = "waiting_user"
	DisplaySucceeded   DisplayState = "succeeded"
	DisplayFailed      DisplayState = "failed"
	DisplayCancelled   DisplayState = "cancelled"
	DisplayIdle        DisplayState = "idle"
	DisplayUnknown     DisplayState = "unknown"
)

// DisplayHealth describes whether a controller can still be trusted.
type DisplayHealth string

const (
	DisplayHealthy DisplayHealth = "healthy"
	DisplayStale   DisplayHealth = "stale"
	DisplayUnbound DisplayHealth = "unbound"
)

// DisplayIdentity is the complete identity of one V2 run.
type DisplayIdentity struct {
	Source        string
	SessionID     string
	AgentID       string
	RunID         string
	ParentAgentID string
}

// DisplaySnapshot is an active or waiting subagent entry in a sync request.
type DisplaySnapshot struct {
	AgentID       string
	RunID         string
	ParentAgentID string
	State         DisplayState
	StartOrder    uint64
}

// DisplayEvent is the core representation of one V2 request.
// Fingerprint is a normalized request representation used for exact retries.
type DisplayEvent struct {
	Operation     DisplayOperation
	Source        string
	SessionID     string
	BindID        string
	Takeover      bool
	BindingID     string
	Seq           uint64
	Role          DisplayRole
	AgentID       string
	RunID         string
	ParentAgentID string
	State         DisplayState
	Reason        string
	HasMain       bool
	Main          *DisplaySnapshot
	Subagents     []DisplaySnapshot
	Fingerprint   []byte
}

// DisplayResult is returned for a V2 operation.
type DisplayResult struct {
	BindingID           string
	NextSeq             uint64
	LeaseMS             int64
	AcceptedSeq         uint64
	HasAcceptedSeq      bool
	Assignment          string
	Slot                int
	HasSlot             bool
	QueueLength         int
	TerminalDeadline    time.Time
	HasTerminalDeadline bool
	LeaseEnd            time.Time
	HasLeaseEnd         bool
	Unbound             bool
}

// DisplayRunView is a read-only V2 run representation.
type DisplayRunView struct {
	Identity            DisplayIdentity
	Role                DisplayRole
	State               DisplayState
	Slot                int
	HasSlot             bool
	StartOrder          uint64
	HasStartOrder       bool
	TerminalDeadline    time.Time
	HasTerminalDeadline bool
}

// DisplaySlotView describes one of the four reserved subagent positions.
type DisplaySlotView struct {
	Number   int
	LEDIndex int
	State    DisplayState
	Run      *DisplayRunView
}

// DisplayStatusView is the V2 controller status. Device and LED data remain
// in StatusView so V1 and V2 status share the same resolved snapshot.
type DisplayStatusView struct {
	Health              DisplayHealth
	BindingID           string
	Source              string
	SessionID           string
	LeaseEnd            time.Time
	HasLeaseEnd         bool
	Main                *DisplayRunView
	MainState           DisplayState
	Slots               [SlotCount]DisplaySlotView
	Queue               []DisplayRunView
	QueueLength         int
	KeyBrightness       int
	UnderglowBrightness int
}

type displayRun struct {
	identity         DisplayIdentity
	role             DisplayRole
	state            DisplayState
	slot             int
	startOrder       uint64
	terminalDeadline time.Time
	generation       uint64
	timer            Timer
}

type displayController struct {
	bound           bool
	stale           bool
	source          string
	sessionID       string
	bindID          string
	bindingID       string
	leaseEnd        time.Time
	leaseTimer      Timer
	leaseGeneration uint64

	expectedSeq uint64
	lastSeq     uint64
	lastRequest []byte
	lastResult  DisplayResult
	hasLast     bool
	bindResult  DisplayResult

	// An unbind tears down the live binding, but its exact response must remain
	// replayable when the client lost that response.
	unboundBindingID string
	unboundSeq       uint64
	unboundRequest   []byte
	unboundResult    DisplayResult
	hasUnboundRetry  bool

	main           *displayRun
	slots          [SlotCount]*displayRun
	runs           map[string]*displayRun
	queue          []*displayRun
	nextStartOrder uint64
}

func (d *displayController) owned() bool { return d.bound || d.stale }

func (d *displayController) ownsReservedLED(index int) bool {
	return d.owned() && ((index >= 12 && index <= 15) || (index >= 21 && index <= 26))
}

func (d *displayController) resolve(index, keyBrightness, underglowBrightness int) (Color, Layer, time.Time, bool) {
	if d.stale {
		return OffColor, LayerDisplayProtection, time.Time{}, false
	}
	if index >= 12 && index <= 15 {
		run := d.slots[index-12]
		if run == nil {
			return OffColor, LayerDisplayProtection, time.Time{}, false
		}
		color := displaySubagentColor(run.state, keyBrightness, index-12)
		if isDisplayTerminal(run.state) {
			return color, LayerDisplaySlot, run.terminalDeadline, true
		}
		return color, LayerDisplaySlot, time.Time{}, false
	}
	if index >= 21 && index <= 26 {
		if d.main == nil {
			return OffColor, LayerDisplayProtection, time.Time{}, false
		}
		color := displayMainColor(d.main.state, underglowBrightness)
		if isDisplayTerminal(d.main.state) {
			return color, LayerDisplayMain, d.main.terminalDeadline, true
		}
		return color, LayerDisplayMain, time.Time{}, false
	}
	return OffColor, LayerDisplayProtection, time.Time{}, false
}

func displayMainColor(state DisplayState, brightness int) Color {
	var base Color
	switch state {
	case DisplayRunning:
		base = displayMainRunningColor
	case DisplayWaitingUser:
		base = displayMainWaitingUserColor
	case DisplaySucceeded:
		base = displaySucceededColor
	case DisplayFailed, DisplayCancelled:
		base = displayFailedColor
	default:
		return OffColor
	}
	return scaleColor(base, brightness)
}

func displaySubagentColor(state DisplayState, brightness, slot int) Color {
	var base Color
	switch state {
	case DisplayRunning:
		base = displaySubagentRunningColors[slot]
	case DisplayWaitingUser:
		base = displaySubagentWaitingUserColor
	case DisplaySucceeded:
		base = displaySucceededColor
	case DisplayFailed, DisplayCancelled:
		base = displayFailedColor
	default:
		return OffColor
	}
	return scaleColor(base, brightness)
}

func scaleColor(color Color, brightness int) Color {
	return Color{
		R: scaleChannel(color.R, brightness),
		G: scaleChannel(color.G, brightness),
		B: scaleChannel(color.B, brightness),
	}
}

func scaleChannel(channel uint8, brightness int) uint8 {
	value := (int(channel)*brightness + 50) / 100
	if value > 255 {
		return 255
	}
	return uint8(value)
}

func isDisplayActive(state DisplayState) bool {
	return state == DisplayRunning || state == DisplayWaitingUser
}

func isDisplayTerminal(state DisplayState) bool {
	return state == DisplaySucceeded || state == DisplayFailed || state == DisplayCancelled
}

func (c *Core) handleDisplay(event DisplayEvent) (Result, error) {
	switch event.Operation {
	case DisplayStatus:
		return Result{Status: c.status(), DisplayStatus: c.displayStatus(), HasDisplay: true}, nil
	case DisplayBind:
		return c.handleDisplayBind(event)
	case DisplayEventOperation, DisplaySync, DisplayHeartbeat, DisplayUnbind:
		return c.handleDisplayWrite(event)
	default:
		return Result{}, &APIError{Code: "invalid_request", Message: "unknown display operation"}
	}
}

func (c *Core) handleDisplayBind(event DisplayEvent) (Result, error) {
	if event.Source == "" || event.SessionID == "" || event.BindID == "" {
		return Result{}, &APIError{Code: "invalid_request", Message: "display binding is invalid"}
	}
	if c.display.bound {
		if c.display.source == event.Source && c.display.sessionID == event.SessionID && c.display.bindID == event.BindID {
			return Result{Display: c.display.bindResult, HasDisplay: true}, nil
		}
		if !event.Takeover {
			return Result{}, ErrDisplayBusy
		}
		c.invalidateDisplayLease()
	}

	c.resetDisplayRuns()
	c.clearLegacyAgents()
	bindingID, err := newBindingID()
	if err != nil {
		return Result{}, fmt.Errorf("create display binding: %w", err)
	}
	c.display.bound = true
	c.display.stale = false
	c.display.source = event.Source
	c.display.sessionID = event.SessionID
	c.display.bindID = event.BindID
	c.display.bindingID = bindingID
	c.display.expectedSeq = 1
	c.display.lastSeq = 0
	c.display.lastRequest = nil
	c.display.hasLast = false
	c.display.unboundBindingID = ""
	c.display.unboundSeq = 0
	c.display.unboundRequest = nil
	c.display.unboundResult = DisplayResult{}
	c.display.hasUnboundRetry = false
	c.display.nextStartOrder = 1
	c.armDisplayLease()
	c.display.bindResult = DisplayResult{BindingID: bindingID, NextSeq: 1, LeaseMS: DisplayLeaseDuration.Milliseconds()}
	c.publishDesired()
	return Result{Display: c.display.bindResult, HasDisplay: true}, nil
}

func newBindingID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func (c *Core) handleDisplayWrite(event DisplayEvent) (Result, error) {
	duplicate, result, err := c.prepareDisplayWrite(event)
	if err != nil {
		return Result{}, err
	}
	if duplicate {
		return Result{Display: result, HasDisplay: true}, nil
	}

	changed := false
	switch event.Operation {
	case DisplayHeartbeat:
		result = DisplayResult{AcceptedSeq: event.Seq, HasAcceptedSeq: true}
	case DisplayUnbind:
		result = DisplayResult{AcceptedSeq: event.Seq, HasAcceptedSeq: true, Unbound: true}
		c.commitDisplayWrite(event, result)
		c.resetDisplayAfterUnbind(event, result)
		c.publishDesired()
		return Result{Display: result, HasDisplay: true}, nil
	case DisplayEventOperation:
		result, changed, err = c.applyDisplayEvent(event)
		if err != nil {
			return Result{}, err
		}
		result.AcceptedSeq = event.Seq
		result.HasAcceptedSeq = true
	case DisplaySync:
		if err := validateDisplaySync(event); err != nil {
			return Result{}, err
		}
		changed = c.applyDisplaySync(event)
		result = DisplayResult{AcceptedSeq: event.Seq, HasAcceptedSeq: true, QueueLength: len(c.display.queue)}
	default:
		return Result{}, &APIError{Code: "invalid_request", Message: "unknown display operation"}
	}
	c.commitDisplayWrite(event, result)
	if event.Operation == DisplayHeartbeat {
		result.HasLeaseEnd = true
		result.LeaseEnd = c.display.leaseEnd
		c.display.lastResult = result
	}
	if changed {
		c.publishDesired()
	}
	return Result{Display: result, HasDisplay: true}, nil
}

func (c *Core) prepareDisplayWrite(event DisplayEvent) (bool, DisplayResult, error) {
	if event.Seq == 0 {
		return false, DisplayResult{}, &APIError{Code: "invalid_request", Message: "sequence is invalid"}
	}
	fingerprint := event.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = displayEventFingerprint(event)
	}
	if !c.display.bound || c.display.bindingID == "" || event.BindingID != c.display.bindingID {
		if c.display.hasUnboundRetry && event.Operation == DisplayUnbind && event.BindingID == c.display.unboundBindingID && event.Seq == c.display.unboundSeq && bytes.Equal(fingerprint, c.display.unboundRequest) {
			return true, c.display.unboundResult, nil
		}
		return false, DisplayResult{}, ErrUnknownBinding
	}
	if c.display.hasLast && event.Seq == c.display.lastSeq {
		if bytes.Equal(fingerprint, c.display.lastRequest) {
			return true, c.display.lastResult, nil
		}
		return false, DisplayResult{}, ErrSequenceConflict
	}
	if event.Seq < c.display.expectedSeq {
		return false, DisplayResult{}, ErrStaleEvent
	}
	if event.Seq > c.display.expectedSeq {
		return false, DisplayResult{}, ErrSequenceGap
	}
	return false, DisplayResult{}, nil
}

func (c *Core) commitDisplayWrite(event DisplayEvent, result DisplayResult) {
	fingerprint := event.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = displayEventFingerprint(event)
	}
	c.display.lastSeq = event.Seq
	c.display.expectedSeq = event.Seq + 1
	c.display.lastRequest = append(c.display.lastRequest[:0], fingerprint...)
	c.display.lastResult = result
	c.display.hasLast = true
	if c.display.bound && event.Operation != DisplayUnbind {
		c.armDisplayLease()
	}
}

func displayEventFingerprint(event DisplayEvent) []byte {
	return []byte(fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s|%s|%s|%s|%t|%v|%v", event.Operation, event.BindingID, event.Role, event.AgentID, event.Seq, event.RunID, event.ParentAgentID, event.State, event.Reason, event.BindID, event.HasMain, event.Main, event.Subagents))
}

func (c *Core) applyDisplayEvent(event DisplayEvent) (DisplayResult, bool, error) {
	if event.Role != DisplayMainRole && event.Role != DisplaySubagentRole {
		return DisplayResult{}, false, &APIError{Code: "invalid_request", Message: "display role is invalid"}
	}
	if event.AgentID == "" || event.RunID == "" || !isDisplayEventState(event.State) {
		return DisplayResult{}, false, &APIError{Code: "invalid_request", Message: "display event is invalid"}
	}
	if event.Role == DisplayMainRole {
		if event.ParentAgentID != "" {
			return DisplayResult{}, false, &APIError{Code: "invalid_request", Message: "main agent has no parent"}
		}
		return c.applyMainEvent(event)
	}
	if event.ParentAgentID == "" {
		return DisplayResult{}, false, &APIError{Code: "invalid_request", Message: "subagent parent is required"}
	}
	return c.applySubagentEvent(event)
}

func isDisplayEventState(state DisplayState) bool {
	return isDisplayActive(state) || isDisplayTerminal(state)
}

func (c *Core) applyMainEvent(event DisplayEvent) (DisplayResult, bool, error) {
	identity := DisplayIdentity{Source: c.display.source, SessionID: c.display.sessionID, AgentID: event.AgentID, RunID: event.RunID}
	current := c.display.main
	if current != nil && current.identity.AgentID == identity.AgentID && current.identity.RunID == identity.RunID {
		if current.state == event.State {
			return displayResultForRun(current, "main"), false, nil
		}
		if isDisplayTerminal(current.state) {
			return DisplayResult{}, false, ErrInvalidTransition
		}
		if isDisplayTerminal(event.State) {
			c.setDisplayTerminal(current, event.State)
			return displayResultForRun(current, "main"), true, nil
		}
		current.state = event.State
		return displayResultForRun(current, "main"), true, nil
	}

	if current != nil && isDisplayTerminal(event.State) {
		return DisplayResult{}, false, ErrUnknownAgent
	}
	if current != nil {
		stopDisplayRunTimer(current)
	}
	if isDisplayTerminal(event.State) && current == nil {
		return DisplayResult{}, false, ErrUnknownAgent
	}
	run := &displayRun{identity: identity, role: DisplayMainRole, state: event.State, slot: -1}
	c.display.main = run
	if isDisplayTerminal(event.State) {
		c.setDisplayTerminal(run, event.State)
	}
	return displayResultForRun(run, "main"), true, nil
}

func (c *Core) applySubagentEvent(event DisplayEvent) (DisplayResult, bool, error) {
	identity := DisplayIdentity{
		Source:        c.display.source,
		SessionID:     c.display.sessionID,
		AgentID:       event.AgentID,
		RunID:         event.RunID,
		ParentAgentID: event.ParentAgentID,
	}
	key := displayRunKey(identity.AgentID, identity.RunID)
	if current, ok := c.display.runs[key]; ok {
		if current.identity.ParentAgentID != identity.ParentAgentID {
			return DisplayResult{}, false, &APIError{Code: "invalid_request", Message: "subagent identity changed"}
		}
		if current.state == event.State {
			result := displayResultForSubagent(current)
			result.QueueLength = len(c.display.queue)
			return result, false, nil
		}
		if isDisplayTerminal(current.state) {
			return DisplayResult{}, false, ErrInvalidTransition
		}
		if isDisplayTerminal(event.State) {
			if current.slot < 0 {
				c.removeQueuedDisplayRun(current)
				delete(c.display.runs, key)
				return DisplayResult{Assignment: "queued", QueueLength: len(c.display.queue)}, true, nil
			}
			c.setDisplayTerminal(current, event.State)
			result := displayResultForSubagent(current)
			result.QueueLength = len(c.display.queue)
			return result, true, nil
		}
		current.state = event.State
		result := displayResultForSubagent(current)
		result.QueueLength = len(c.display.queue)
		return result, true, nil
	}
	if isDisplayTerminal(event.State) {
		return DisplayResult{}, false, ErrUnknownAgent
	}
	if len(c.display.runs) >= MaxDisplaySubagentRuns {
		return DisplayResult{}, false, ErrCapacityExceeded
	}
	if c.display.runs == nil {
		c.display.runs = make(map[string]*displayRun)
	}
	run := &displayRun{
		identity:   identity,
		role:       DisplaySubagentRole,
		state:      event.State,
		slot:       -1,
		startOrder: c.display.nextStartOrder,
	}
	c.display.nextStartOrder++
	c.display.runs[key] = run
	if slot := c.firstFreeDisplaySlot(); slot >= 0 {
		c.assignDisplayRun(run, slot)
		if isDisplayTerminal(event.State) {
			c.setDisplayTerminal(run, event.State)
		}
		result := displayResultForSubagent(run)
		result.QueueLength = len(c.display.queue)
		return result, true, nil
	}
	c.display.queue = append(c.display.queue, run)
	result := displayResultForSubagent(run)
	result.QueueLength = len(c.display.queue)
	return result, true, nil
}

func displayRunKey(agentID, runID string) string { return agentID + "\x00" + runID }

func displayResultForRun(run *displayRun, assignment string) DisplayResult {
	result := DisplayResult{Assignment: assignment}
	if run.slot >= 0 {
		result.Slot = run.slot
		result.HasSlot = true
	}
	if run.timer != nil {
		result.TerminalDeadline = run.terminalDeadline
		result.HasTerminalDeadline = true
	}
	return result
}

func displayResultForSubagent(run *displayRun) DisplayResult {
	assignment := "queued"
	if run.slot >= 0 {
		assignment = "slot"
	}
	return displayResultForRun(run, assignment)
}

func (c *Core) firstFreeDisplaySlot() int {
	for index, run := range c.display.slots {
		if run == nil {
			return index
		}
	}
	return -1
}

func (c *Core) assignDisplayRun(run *displayRun, slot int) {
	if run.slot >= 0 {
		return
	}
	removeDisplayRunFromQueue(&c.display.queue, run)
	run.slot = slot
	c.display.slots[slot] = run
}

func (c *Core) setDisplayTerminal(run *displayRun, state DisplayState) {
	stopDisplayRunTimer(run)
	run.state = state
	run.generation++
	run.terminalDeadline = c.clock.Now().Add(DisplayTerminalDuration)
	run.timer = c.clock.NewTimer(DisplayTerminalDuration)
}

func stopDisplayRunTimer(run *displayRun) {
	if run == nil {
		return
	}
	if run.timer != nil {
		run.timer.Stop()
		run.timer = nil
	}
	run.terminalDeadline = time.Time{}
	run.generation++
}

func (c *Core) removeQueuedDisplayRun(run *displayRun) {
	removeDisplayRunFromQueue(&c.display.queue, run)
}

func removeDisplayRunFromQueue(queue *[]*displayRun, wanted *displayRun) {
	items := *queue
	for index, run := range items {
		if run == wanted {
			copy(items[index:], items[index+1:])
			*queue = items[:len(items)-1]
			return
		}
	}
}

func (c *Core) applyDisplaySync(event DisplayEvent) bool {
	oldSlots := make(map[*displayRun]int, SlotCount)
	for index, run := range c.display.slots {
		if run != nil {
			oldSlots[run] = index
		}
	}
	oldRuns := c.display.runs
	for _, run := range oldRuns {
		stopDisplayRunTimer(run)
	}
	if c.display.main != nil {
		stopDisplayRunTimer(c.display.main)
	}
	c.display.slots = [SlotCount]*displayRun{}
	c.display.queue = nil
	c.display.runs = make(map[string]*displayRun, len(event.Subagents))

	ordered := append([]DisplaySnapshot(nil), event.Subagents...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].StartOrder < ordered[j].StartOrder })
	maxOrder := uint64(0)
	for _, snapshot := range ordered {
		identity := DisplayIdentity{
			Source:        c.display.source,
			SessionID:     c.display.sessionID,
			AgentID:       snapshot.AgentID,
			RunID:         snapshot.RunID,
			ParentAgentID: snapshot.ParentAgentID,
		}
		key := displayRunKey(snapshot.AgentID, snapshot.RunID)
		run := oldRuns[key]
		if run == nil {
			run = &displayRun{identity: identity, role: DisplaySubagentRole, slot: -1}
		} else {
			run.identity = identity
			run.role = DisplaySubagentRole
			run.slot = -1
		}
		run.state = snapshot.State
		run.startOrder = snapshot.StartOrder
		run.terminalDeadline = time.Time{}
		run.timer = nil
		c.display.runs[key] = run
		if snapshot.StartOrder > maxOrder {
			maxOrder = snapshot.StartOrder
		}
	}
	c.display.nextStartOrder = maxOrder + 1

	for _, snapshot := range ordered {
		run := c.display.runs[displayRunKey(snapshot.AgentID, snapshot.RunID)]
		if oldSlot, ok := oldSlots[oldRuns[displayRunKey(snapshot.AgentID, snapshot.RunID)]]; ok && c.display.slots[oldSlot] == nil {
			run.slot = oldSlot
			c.display.slots[oldSlot] = run
		}
	}
	for _, snapshot := range ordered {
		run := c.display.runs[displayRunKey(snapshot.AgentID, snapshot.RunID)]
		if run.slot >= 0 {
			continue
		}
		if slot := c.firstFreeDisplaySlot(); slot >= 0 {
			c.assignDisplayRun(run, slot)
		} else {
			c.display.queue = append(c.display.queue, run)
		}
	}

	if event.HasMain && event.Main != nil {
		identity := DisplayIdentity{
			Source:    c.display.source,
			SessionID: c.display.sessionID,
			AgentID:   event.Main.AgentID,
			RunID:     event.Main.RunID,
		}
		c.display.main = &displayRun{identity: identity, role: DisplayMainRole, state: event.Main.State, slot: -1}
	} else {
		c.display.main = nil
	}
	return true
}

func validateDisplaySync(event DisplayEvent) error {
	if !event.HasMain {
		return &APIError{Code: "invalid_request", Message: "main snapshot is required"}
	}
	if len(event.Subagents) > MaxDisplaySubagentRuns {
		return ErrCapacityExceeded
	}
	seen := make(map[string]struct{}, len(event.Subagents))
	orders := make(map[uint64]struct{}, len(event.Subagents))
	for _, snapshot := range event.Subagents {
		if snapshot.AgentID == "" || snapshot.RunID == "" || snapshot.ParentAgentID == "" || !isDisplayActive(snapshot.State) || snapshot.StartOrder == 0 {
			return &APIError{Code: "invalid_request", Message: "display sync entry is invalid"}
		}
		key := displayRunKey(snapshot.AgentID, snapshot.RunID)
		if _, ok := seen[key]; ok {
			return &APIError{Code: "invalid_request", Message: "display sync contains duplicate runs"}
		}
		seen[key] = struct{}{}
		if _, ok := orders[snapshot.StartOrder]; ok {
			return &APIError{Code: "invalid_request", Message: "display sync contains duplicate start orders"}
		}
		orders[snapshot.StartOrder] = struct{}{}
	}
	if event.Main != nil {
		if event.Main.AgentID == "" || event.Main.RunID == "" || !isDisplayActive(event.Main.State) || event.Main.ParentAgentID != "" {
			return &APIError{Code: "invalid_request", Message: "main sync entry is invalid"}
		}
	}
	return nil
}

func (c *Core) armDisplayLease() {
	if c.display.leaseTimer != nil {
		c.display.leaseTimer.Stop()
	}
	c.display.leaseGeneration++
	c.display.leaseEnd = c.clock.Now().Add(DisplayLeaseDuration)
	c.display.leaseTimer = c.clock.NewTimer(DisplayLeaseDuration)
}

func (c *Core) handleDisplayTimer(ref timerRef) bool {
	now := c.clock.Now()
	switch ref.kind {
	case displayLeaseTimer:
		if !c.display.bound || c.display.leaseGeneration != ref.generation || c.display.leaseTimer == nil {
			return false
		}
		if now.Before(c.display.leaseEnd) {
			c.display.leaseTimer = c.clock.NewTimer(c.display.leaseEnd.Sub(now))
			return false
		}
		c.invalidateDisplayLease()
		return true
	case displayMainTerminalTimer:
		if c.display.main == nil || c.display.main.generation != ref.generation || c.display.main.timer == nil {
			return false
		}
		if now.Before(c.display.main.terminalDeadline) {
			c.display.main.timer = c.clock.NewTimer(c.display.main.terminalDeadline.Sub(now))
			return false
		}
		stopDisplayRunTimer(c.display.main)
		c.display.main = nil
		return true
	case displaySubagentTerminalTimer:
		if ref.index < 0 || ref.index >= SlotCount {
			return false
		}
		run := c.display.slots[ref.index]
		if run == nil || run.generation != ref.generation || run.timer == nil {
			return false
		}
		if now.Before(run.terminalDeadline) {
			run.timer = c.clock.NewTimer(run.terminalDeadline.Sub(now))
			return false
		}
		stopDisplayRunTimer(run)
		c.display.slots[ref.index] = nil
		run.slot = -1
		delete(c.display.runs, displayRunKey(run.identity.AgentID, run.identity.RunID))
		c.promoteDisplayQueue()
		return true
	}
	return false
}

func (c *Core) promoteDisplayQueue() {
	for {
		slot := c.firstFreeDisplaySlot()
		if slot < 0 || len(c.display.queue) == 0 {
			return
		}
		run := c.display.queue[0]
		c.display.queue = c.display.queue[1:]
		if c.display.runs[displayRunKey(run.identity.AgentID, run.identity.RunID)] != run {
			continue
		}
		c.assignDisplayRun(run, slot)
	}
}

func (c *Core) expireDisplayDue(now time.Time) bool {
	changed := false
	if c.display.bound && c.display.leaseTimer != nil && !now.Before(c.display.leaseEnd) {
		c.invalidateDisplayLease()
		changed = true
	}
	if c.display.main != nil && c.display.main.timer != nil && !now.Before(c.display.main.terminalDeadline) {
		stopDisplayRunTimer(c.display.main)
		c.display.main = nil
		changed = true
	}
	for index, run := range c.display.slots {
		if run == nil || run.timer == nil || now.Before(run.terminalDeadline) {
			continue
		}
		stopDisplayRunTimer(run)
		c.display.slots[index] = nil
		run.slot = -1
		delete(c.display.runs, displayRunKey(run.identity.AgentID, run.identity.RunID))
		changed = true
	}
	if changed {
		c.promoteDisplayQueue()
	}
	return changed
}

func (c *Core) invalidateDisplayLease() {
	c.stopDisplayTimers()
	c.resetDisplayRuns()
	c.display.bound = false
	c.display.stale = true
	c.display.bindingID = ""
	c.display.bindID = ""
	c.display.expectedSeq = 0
	c.display.lastRequest = nil
	c.display.hasLast = false
}

func (c *Core) resetDisplayRuns() {
	if c.display.main != nil {
		stopDisplayRunTimer(c.display.main)
	}
	for _, run := range c.display.runs {
		stopDisplayRunTimer(run)
	}
	if c.display.leaseTimer != nil {
		c.display.leaseTimer.Stop()
		c.display.leaseTimer = nil
	}
	c.display.main = nil
	c.display.slots = [SlotCount]*displayRun{}
	c.display.runs = nil
	c.display.queue = nil
}

func (c *Core) resetDisplayUnbound() {
	c.resetDisplayRuns()
	c.display = displayController{}
}

func (c *Core) resetDisplayAfterUnbind(event DisplayEvent, result DisplayResult) {
	fingerprint := event.Fingerprint
	if len(fingerprint) == 0 {
		fingerprint = displayEventFingerprint(event)
	}
	c.resetDisplayUnbound()
	c.display.unboundBindingID = event.BindingID
	c.display.unboundSeq = event.Seq
	c.display.unboundRequest = append([]byte(nil), fingerprint...)
	c.display.unboundResult = result
	c.display.hasUnboundRetry = true
}

func (c *Core) stopDisplayTimers() {
	if c.display.leaseTimer != nil {
		c.display.leaseTimer.Stop()
		c.display.leaseTimer = nil
	}
	if c.display.main != nil {
		stopDisplayRunTimer(c.display.main)
	}
	for _, run := range c.display.runs {
		stopDisplayRunTimer(run)
	}
	for _, run := range c.display.slots {
		if run != nil {
			stopDisplayRunTimer(run)
		}
	}
}

func (c *Core) clearLegacyAgents() {
	for index := range c.slots {
		c.freeSlot(index)
	}
	c.stopLifecycle()
}

func (c *Core) displayStatus() DisplayStatusView {
	status := DisplayStatusView{
		Health:              DisplayUnbound,
		Source:              c.display.source,
		SessionID:           c.display.sessionID,
		MainState:           DisplayIdle,
		QueueLength:         len(c.display.queue),
		KeyBrightness:       c.keyBrightness,
		UnderglowBrightness: c.underglowBrightness,
	}
	if c.display.bound {
		status.Health = DisplayHealthy
		status.BindingID = c.display.bindingID
		status.LeaseEnd = c.display.leaseEnd
		status.HasLeaseEnd = true
	} else if c.display.stale {
		status.Health = DisplayStale
		if !c.display.leaseEnd.IsZero() {
			status.LeaseEnd = c.display.leaseEnd
			status.HasLeaseEnd = true
		}
	}
	if c.display.main != nil && !c.display.stale {
		view := displayRunView(c.display.main)
		status.Main = &view
		status.MainState = c.display.main.state
	} else if c.display.stale {
		status.MainState = DisplayUnknown
	}
	for index := range status.Slots {
		status.Slots[index] = DisplaySlotView{Number: index, LEDIndex: PhysicalLEDIndexes[index], State: DisplayIdle}
		if c.display.stale {
			status.Slots[index].State = DisplayUnknown
			continue
		}
		if run := c.display.slots[index]; run != nil {
			view := displayRunView(run)
			status.Slots[index].State = run.state
			status.Slots[index].Run = &view
		}
	}
	if !c.display.stale {
		status.Queue = make([]DisplayRunView, 0, len(c.display.queue))
		for _, run := range c.display.queue {
			status.Queue = append(status.Queue, displayRunView(run))
		}
	}
	return status
}

func displayRunView(run *displayRun) DisplayRunView {
	view := DisplayRunView{
		Identity:      run.identity,
		Role:          run.role,
		State:         run.state,
		Slot:          run.slot,
		HasSlot:       run.slot >= 0,
		StartOrder:    run.startOrder,
		HasStartOrder: run.role == DisplaySubagentRole,
	}
	if run.timer != nil {
		view.TerminalDeadline = run.terminalDeadline
		view.HasTerminalDeadline = true
	}
	return view
}
