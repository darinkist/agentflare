// Package core owns the agent lifecycle and the complete desired LED state.
package core

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

const (
	SlotCount                  = 4
	LEDCount                   = 27
	LeaseDuration              = 10 * time.Minute
	TerminalDuration           = 2 * time.Second
	MaxLightTTL                = 24 * time.Hour
	DisplayLeaseDuration       = 15 * time.Second
	DisplayTerminalDuration    = 1500 * time.Millisecond
	MaxDisplaySubagentRuns     = 64
	DefaultKeyBrightness       = 35
	DefaultUnderglowBrightness = 25
)

var PhysicalLEDIndexes = [SlotCount]int{12, 13, 14, 15}

// AgentIdentity distinguishes the same agent ID used by different harnesses.
type AgentIdentity struct {
	Source  string
	AgentID string
}

// Color is an RGB color.
type Color struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
}

var (
	OffColor     = Color{}
	ActiveColors = [SlotCount]Color{
		{B: 255},
		{G: 255, B: 255},
		{R: 255, B: 255},
		{R: 255, G: 191},
	}
)

// Zone identifies one of the physical LED groups.
type Zone string

const (
	ZoneKeys     Zone = "keys"
	ZoneMMD      Zone = "mmd"
	ZoneBackglow Zone = "backglow"
	ZoneAll      Zone = "all"
)

// LEDIndicesForZone returns a fresh, ascending list of physical LED indices.
func LEDIndicesForZone(zone Zone) ([]int, bool) {
	var start, end int
	switch zone {
	case ZoneKeys:
		start, end = 0, 15
	case ZoneMMD:
		start, end = 16, 20
	case ZoneBackglow:
		start, end = 21, 26
	case ZoneAll:
		start, end = 0, 26
	default:
		return nil, false
	}
	indices := make([]int, 0, end-start+1)
	for index := start; index <= end; index++ {
		indices = append(indices, index)
	}
	return indices, true
}

// SlotState is the externally visible lifecycle state of a slot.
type SlotState string

const (
	Free     SlotState = "free"
	Active   SlotState = "active"
	Finished SlotState = "finished"
	Failed   SlotState = "failed"
)

// DeviceStatus describes the hardware connection independently of agent state.
type DeviceStatus string

const (
	DeviceDisconnected  DeviceStatus = "disconnected"
	DeviceConnecting    DeviceStatus = "connecting"
	DeviceConnected     DeviceStatus = "connected"
	DeviceAmbiguous     DeviceStatus = "ambiguous"
	DeviceProtocolError DeviceStatus = "protocol_error"
)

// Layer identifies the visible priority layer of one LED.
type Layer string

const (
	LayerAgentSlot         Layer = "agent_slot"
	LayerLifecycle         Layer = "lifecycle"
	LayerManualTTL         Layer = "manual_ttl"
	LayerManualPersistent  Layer = "manual_persistent"
	LayerDisplaySlot       Layer = "display_slot"
	LayerDisplayMain       Layer = "display_main"
	LayerDisplayProtection Layer = "display_protection"
	LayerOff               Layer = "off"
)

// EventType is an event accepted by the core.
type EventType string

const (
	EventStarted      EventType = "started"
	EventHeartbeat    EventType = "heartbeat"
	EventFinished     EventType = "finished"
	EventFailed       EventType = "failed"
	EventCancelled    EventType = "cancelled"
	EventClear        EventType = "clear"
	EventStatus       EventType = "status"
	EventDeviceStatus EventType = "device_status"
	EventLightsSet    EventType = "lights_set"
	EventLightsClear  EventType = "lights_clear"
)

// DesiredState is an immutable complete state for all 27 physical LEDs.
// LEDs are always ordered by their physical index, 0 through 26.
type DesiredState struct {
	LEDs [LEDCount]LED
}

// LED identifies one physical LED and its desired color.
type LED struct {
	Index int
	Color Color
}

// LightView is the resolved visible state of one LED.
type LightView struct {
	Index     int
	Zone      Zone
	Color     Color
	Layer     Layer
	ExpiresAt time.Time
	HasExpiry bool
}

// SlotView is a read-only representation used by status responses.
type SlotView struct {
	Number      int
	LEDIndex    int
	State       SlotState
	Color       Color
	Identity    AgentIdentity
	HasIdentity bool
	LeaseEnd    time.Time
}

// StatusView combines the core state with the latest device status.
type StatusView struct {
	Device DeviceStatus
	Slots  [SlotCount]SlotView
	Lights [LEDCount]LightView
}

// Event is submitted to the core event loop. Light requests must provide a
// non-empty list of unique indices in LEDIndices.
type Event struct {
	Type         EventType
	Identity     AgentIdentity
	DeviceStatus DeviceStatus
	LEDIndices   []int
	Color        Color
	TTL          time.Duration
	HasTTL       bool
	Display      *DisplayEvent
}

// LightResult confirms the target accepted by a light request.
type LightResult struct {
	LEDIndices []int
	ExpiresAt  time.Time
	HasExpiry  bool
}

// Result is returned after an event has been processed.
type Result struct {
	Slot          int
	Status        StatusView
	Lights        LightResult
	Display       DisplayResult
	DisplayStatus DisplayStatusView
	HasDisplay    bool
}

// APIError is a stable operational error returned by the core.
type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

var (
	ErrUnknownAgent      = &APIError{Code: "unknown_agent", Message: "agent is not known"}
	ErrSlotExhausted     = &APIError{Code: "slot_exhausted", Message: "all four agent slots are in use"}
	ErrInvalidTransition = &APIError{Code: "invalid_transition", Message: "event is not valid for the agent state"}
	ErrDisplayBusy       = &APIError{Code: "display_busy", Message: "display is bound to another session"}
	ErrUnknownBinding    = &APIError{Code: "unknown_binding", Message: "display binding is not known"}
	ErrSequenceConflict  = &APIError{Code: "sequence_conflict", Message: "sequence was already used for another request"}
	ErrSequenceGap       = &APIError{Code: "sequence_gap", Message: "sequence is ahead of the next expected sequence"}
	ErrStaleEvent        = &APIError{Code: "stale_event", Message: "sequence is older than the next expected sequence"}
	ErrCapacityExceeded  = &APIError{Code: "capacity_exceeded", Message: "subagent capacity is exhausted"}
)

// Timer is the small time seam used by the core. Timer channels deliver only
// lease, terminal, TTL and lifecycle expiration events.
type Timer interface {
	Chan() <-chan time.Time
	Stop() bool
}

// Clock provides current time and timer creation to the core.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type realClock struct{}

func (realClock) Now() time.Time                 { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer { return realTimer{timer: time.NewTimer(d)} }

type realTimer struct{ timer *time.Timer }

func (t realTimer) Chan() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop() bool             { return t.timer.Stop() }

// NewRealClock returns the production clock.
func NewRealClock() Clock { return realClock{} }

type timerKind uint8

const (
	noTimer timerKind = iota
	leaseTimer
	terminalTimer
	ttlTimer
	lifecycleTimer
	displayLeaseTimer
	displayMainTerminalTimer
	displaySubagentTerminalTimer
)

type slot struct {
	state       SlotState
	identity    AgentIdentity
	hasIdentity bool
	leaseEnd    time.Time
	generation  uint64
	timer       Timer
	timerKind   timerKind
}

type ttlState struct {
	active     bool
	color      Color
	expiresAt  time.Time
	generation uint64
	timer      Timer
}

type timerRef struct {
	kind       timerKind
	slot       int // retained as a descriptive alias for slot timer tests
	index      int
	generation uint64
	expiresAt  time.Time
}

type lifecycleState struct {
	active     bool
	color      Color
	expiresAt  time.Time
	generation uint64
	timer      Timer
}

// Core serializes all mutable state in one event loop.
type Core struct {
	clock               Clock
	events              chan coreCommand
	snapshots           chan DesiredState
	slots               [SlotCount]slot
	persistent          [LEDCount]Color
	persistentSet       [LEDCount]bool
	ttl                 [LEDCount]ttlState
	lifecycle           lifecycleState
	device              DeviceStatus
	display             displayController
	keyBrightness       int
	underglowBrightness int
}

type coreCommand struct {
	event Event
	reply chan coreReply
}

type coreReply struct {
	result Result
	err    error
}

// New initializes a core with four free slots, no manual colors and a
// disconnected device.
func New(clock Clock) *Core {
	return NewWithDisplayOptions(clock, DisplayOptions{
		KeyBrightness:       DefaultKeyBrightness,
		UnderglowBrightness: DefaultUnderglowBrightness,
	})
}

// NewWithDisplayOptions initializes a core with explicit V2 display brightness.
func NewWithDisplayOptions(clock Clock, options DisplayOptions) *Core {
	if clock == nil {
		clock = NewRealClock()
	}
	if options.KeyBrightness < 0 || options.KeyBrightness > 100 {
		options.KeyBrightness = DefaultKeyBrightness
	}
	if options.UnderglowBrightness < 0 || options.UnderglowBrightness > 100 {
		options.UnderglowBrightness = DefaultUnderglowBrightness
	}
	c := &Core{
		clock:               clock,
		events:              make(chan coreCommand),
		snapshots:           make(chan DesiredState, 1),
		device:              DeviceDisconnected,
		keyBrightness:       options.KeyBrightness,
		underglowBrightness: options.UnderglowBrightness,
	}
	for i := range c.slots {
		c.slots[i].state = Free
	}
	c.publishDesired()
	return c
}

// InitialDesiredState returns the all-off state used before the core starts.
func InitialDesiredState() DesiredState {
	var desired DesiredState
	for index := range desired.LEDs {
		desired.LEDs[index] = LED{Index: index, Color: OffColor}
	}
	return desired
}

// Snapshots returns the bounded latest-state stream consumed by the device driver.
func (c *Core) Snapshots() <-chan DesiredState { return c.snapshots }

// Submit sends one event to the core event loop and waits for its result.
func (c *Core) Submit(ctx context.Context, event Event) (Result, error) {
	reply := make(chan coreReply, 1)
	command := coreCommand{event: event, reply: reply}
	select {
	case c.events <- command:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	select {
	case response := <-reply:
		return response.result, response.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Run executes the single owner of all mutable core state until ctx is canceled.
func (c *Core) Run(ctx context.Context) error {
	for {
		if c.expireDue(c.clock.Now()) {
			c.publishDesired()
		}
		cases := []reflect.SelectCase{
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())},
			{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(c.events)},
		}
		refs := make([]timerRef, 0, SlotCount+LEDCount+1)
		for i, current := range c.slots {
			if current.timer == nil {
				continue
			}
			refs = append(refs, timerRef{kind: current.timerKind, index: i, generation: current.generation, expiresAt: current.leaseEnd})
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(current.timer.Chan())})
		}
		for i, current := range c.ttl {
			if current.timer == nil {
				continue
			}
			refs = append(refs, timerRef{kind: ttlTimer, index: i, generation: current.generation, expiresAt: current.expiresAt})
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(current.timer.Chan())})
		}
		if c.lifecycle.timer != nil {
			refs = append(refs, timerRef{kind: lifecycleTimer, generation: c.lifecycle.generation, expiresAt: c.lifecycle.expiresAt})
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(c.lifecycle.timer.Chan())})
		}
		if c.display.leaseTimer != nil {
			refs = append(refs, timerRef{kind: displayLeaseTimer, generation: c.display.leaseGeneration, expiresAt: c.display.leaseEnd})
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(c.display.leaseTimer.Chan())})
		}
		if c.display.main != nil && c.display.main.timer != nil {
			refs = append(refs, timerRef{kind: displayMainTerminalTimer, generation: c.display.main.generation, expiresAt: c.display.main.terminalDeadline})
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(c.display.main.timer.Chan())})
		}
		for index, run := range c.display.slots {
			if run == nil || run.timer == nil {
				continue
			}
			refs = append(refs, timerRef{kind: displaySubagentTerminalTimer, index: index, generation: run.generation, expiresAt: run.terminalDeadline})
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(run.timer.Chan())})
		}

		chosen, value, ok := reflect.Select(cases)
		switch chosen {
		case 0:
			c.stopTimers()
			return nil
		case 1:
			if !ok {
				return nil
			}
			command := value.Interface().(coreCommand)
			result, err := c.handle(command.event)
			command.reply <- coreReply{result: result, err: err}
		default:
			c.handleTimer(refs[chosen-2])
		}
	}
}

func (c *Core) handle(event Event) (Result, error) {
	if c.expireDue(c.clock.Now()) {
		c.publishDesired()
	}
	if event.Display != nil {
		return c.handleDisplay(*event.Display)
	}
	if event.Type == EventDeviceStatus {
		c.device = event.DeviceStatus
		return Result{Slot: -1, Status: c.status()}, nil
	}
	if event.Type == EventStatus {
		return Result{Slot: -1, Status: c.status()}, nil
	}
	if event.Type == EventClear {
		c.clearAll()
		c.publishDesired()
		return Result{Slot: -1, Status: c.status()}, nil
	}
	if event.Type == EventLightsSet {
		return c.handleLightsSet(event)
	}
	if event.Type == EventLightsClear {
		return c.handleLightsClear(event)
	}
	if event.Type == EventStarted {
		if c.display.owned() {
			return Result{Slot: -1, Status: c.status()}, ErrDisplayBusy
		}
		return c.handleStarted(event.Identity)
	}
	if c.display.owned() {
		return Result{Slot: -1, Status: c.status()}, ErrDisplayBusy
	}

	index := c.findIdentity(event.Identity)
	if index < 0 {
		return Result{Slot: -1, Status: c.status()}, ErrUnknownAgent
	}
	if c.slots[index].state != Active {
		return Result{Slot: index, Status: c.status()}, ErrInvalidTransition
	}

	switch event.Type {
	case EventHeartbeat:
		c.arm(index, leaseTimer, LeaseDuration)
		c.publishDesired()
		return Result{Slot: index, Status: c.status()}, nil
	case EventFinished, EventFailed:
		now := c.clock.Now()
		c.stopTimer(index)
		s := &c.slots[index]
		s.generation++
		s.state = Finished
		if event.Type == EventFailed {
			s.state = Failed
		}
		s.leaseEnd = now.Add(TerminalDuration)
		s.timerKind = terminalTimer
		s.timer = c.clock.NewTimer(TerminalDuration)
		c.startLifecycle(event.Type == EventFailed, now)
		c.publishDesired()
		return Result{Slot: index, Status: c.status()}, nil
	case EventCancelled:
		c.freeSlot(index)
		c.publishDesired()
		return Result{Slot: index, Status: c.status()}, nil
	default:
		return Result{Slot: index, Status: c.status()}, fmt.Errorf("unknown core event %q", event.Type)
	}
}

func (c *Core) handleLightsSet(event Event) (Result, error) {
	indices, err := normalizeIndices(event.LEDIndices)
	if err != nil || (event.HasTTL && (event.TTL < time.Millisecond || event.TTL > MaxLightTTL || event.TTL%time.Millisecond != 0)) {
		return Result{Slot: -1, Status: c.status()}, invalidLightRequest()
	}
	now := c.clock.Now()
	result := LightResult{LEDIndices: indices}
	if event.HasTTL {
		result.HasExpiry = true
		result.ExpiresAt = now.Add(event.TTL)
		for _, index := range indices {
			c.replaceTTL(index, event.Color, result.ExpiresAt, event.TTL)
		}
	} else {
		for _, index := range indices {
			c.persistent[index] = event.Color
			c.persistentSet[index] = true
		}
	}
	c.publishDesired()
	return Result{Slot: -1, Status: c.status(), Lights: result}, nil
}

func (c *Core) handleLightsClear(event Event) (Result, error) {
	indices, err := normalizeIndices(event.LEDIndices)
	if err != nil {
		return Result{Slot: -1, Status: c.status()}, invalidLightRequest()
	}
	for _, index := range indices {
		c.persistentSet[index] = false
		c.stopTTL(index)
	}
	c.publishDesired()
	return Result{Slot: -1, Status: c.status(), Lights: LightResult{LEDIndices: indices}}, nil
}

func invalidLightRequest() *APIError {
	return &APIError{Code: "invalid_request", Message: "request is invalid"}
}

func normalizeIndices(indices []int) ([]int, error) {
	if len(indices) == 0 || len(indices) > LEDCount {
		return nil, errors.New("invalid LED target")
	}
	result := append([]int(nil), indices...)
	seen := make(map[int]struct{}, len(result))
	for _, index := range result {
		if index < 0 || index >= LEDCount {
			return nil, errors.New("invalid LED index")
		}
		if _, ok := seen[index]; ok {
			return nil, errors.New("duplicate LED index")
		}
		seen[index] = struct{}{}
	}
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j] < result[j-1]; j-- {
			result[j], result[j-1] = result[j-1], result[j]
		}
	}
	return result, nil
}

func (c *Core) handleStarted(identity AgentIdentity) (Result, error) {
	if index := c.findIdentity(identity); index >= 0 {
		c.activate(index)
		c.publishDesired()
		return Result{Slot: index, Status: c.status()}, nil
	}
	for i := range c.slots {
		if c.slots[i].state != Free {
			continue
		}
		c.slots[i].identity = identity
		c.slots[i].hasIdentity = true
		c.activate(i)
		c.publishDesired()
		return Result{Slot: i, Status: c.status()}, nil
	}
	return Result{Slot: -1, Status: c.status()}, ErrSlotExhausted
}

func (c *Core) activate(index int) {
	c.stopTimer(index)
	s := &c.slots[index]
	s.generation++
	s.state = Active
	s.leaseEnd = c.clock.Now().Add(LeaseDuration)
	s.timerKind = leaseTimer
	s.timer = c.clock.NewTimer(LeaseDuration)
}

func (c *Core) freeSlot(index int) {
	c.stopTimer(index)
	s := &c.slots[index]
	s.generation++
	s.state = Free
	s.identity = AgentIdentity{}
	s.hasIdentity = false
	s.leaseEnd = time.Time{}
	s.timerKind = noTimer
}

func (c *Core) arm(index int, kind timerKind, duration time.Duration) {
	c.stopTimer(index)
	s := &c.slots[index]
	s.generation++
	s.leaseEnd = c.clock.Now().Add(duration)
	s.timerKind = kind
	s.timer = c.clock.NewTimer(duration)
}

func (c *Core) stopTimer(index int) {
	s := &c.slots[index]
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.timerKind = noTimer
}

func (c *Core) replaceTTL(index int, color Color, expiresAt time.Time, duration time.Duration) {
	c.stopTTL(index)
	ttl := &c.ttl[index]
	ttl.active = true
	ttl.color = color
	ttl.expiresAt = expiresAt
	ttl.generation++
	ttl.timer = c.clock.NewTimer(duration)
}

func (c *Core) stopTTL(index int) {
	ttl := &c.ttl[index]
	if ttl.timer != nil {
		ttl.timer.Stop()
		ttl.timer = nil
	}
	ttl.active = false
	ttl.expiresAt = time.Time{}
	ttl.generation++
}

func (c *Core) startLifecycle(failed bool, now time.Time) {
	if c.lifecycle.timer != nil {
		c.lifecycle.timer.Stop()
	}
	c.lifecycle.active = true
	if failed {
		c.lifecycle.color = Color{R: 64}
	} else {
		c.lifecycle.color = Color{G: 64}
	}
	c.lifecycle.expiresAt = now.Add(TerminalDuration)
	c.lifecycle.generation++
	c.lifecycle.timer = c.clock.NewTimer(TerminalDuration)
}

func (c *Core) stopLifecycle() {
	if c.lifecycle.timer != nil {
		c.lifecycle.timer.Stop()
	}
	c.lifecycle.active = false
	c.lifecycle.expiresAt = time.Time{}
	c.lifecycle.generation++
	c.lifecycle.timer = nil
}

func (c *Core) clearAll() {
	for i := range c.slots {
		c.freeSlot(i)
	}
	for i := range c.ttl {
		c.persistentSet[i] = false
		c.stopTTL(i)
	}
	c.stopLifecycle()
	c.resetDisplayUnbound()
}

func (c *Core) stopTimers() {
	for i := range c.slots {
		c.stopTimer(i)
	}
	for i := range c.ttl {
		c.stopTTL(i)
	}
	c.stopLifecycle()
	c.stopDisplayTimers()
}

func (c *Core) handleTimer(ref timerRef) {
	now := c.clock.Now()
	switch ref.kind {
	case leaseTimer, terminalTimer:
		if ref.index < 0 || ref.index >= SlotCount {
			return
		}
		s := &c.slots[ref.index]
		if s.generation != ref.generation || s.timerKind != ref.kind {
			return
		}
		if now.Before(ref.expiresAt) {
			s.timer = c.clock.NewTimer(ref.expiresAt.Sub(now))
			return
		}
		s.timer = nil
		s.timerKind = noTimer
		c.freeSlot(ref.index)
		c.publishDesired()
	case ttlTimer:
		if ref.index < 0 || ref.index >= LEDCount {
			return
		}
		ttl := &c.ttl[ref.index]
		if ttl.generation != ref.generation || ttl.timer == nil {
			return
		}
		if now.Before(ref.expiresAt) {
			ttl.timer = c.clock.NewTimer(ref.expiresAt.Sub(now))
			return
		}
		c.stopTTL(ref.index)
		c.publishDesired()
	case lifecycleTimer:
		if c.lifecycle.generation != ref.generation || c.lifecycle.timer == nil {
			return
		}
		if now.Before(ref.expiresAt) {
			c.lifecycle.timer = c.clock.NewTimer(ref.expiresAt.Sub(now))
			return
		}
		c.stopLifecycle()
		c.publishDesired()
	case displayLeaseTimer, displayMainTerminalTimer, displaySubagentTerminalTimer:
		if c.handleDisplayTimer(ref) {
			c.publishDesired()
		}
	}
}

func (c *Core) expireDue(now time.Time) bool {
	changed := false
	for i := range c.slots {
		s := &c.slots[i]
		if s.timer != nil && !now.Before(s.leaseEnd) {
			c.freeSlot(i)
			changed = true
		}
	}
	for i := range c.ttl {
		if c.ttl[i].active && !now.Before(c.ttl[i].expiresAt) {
			c.stopTTL(i)
			changed = true
		}
	}
	if c.lifecycle.active && !now.Before(c.lifecycle.expiresAt) {
		c.stopLifecycle()
		changed = true
	}
	if c.expireDisplayDue(now) {
		changed = true
	}
	return changed
}

func (c *Core) findIdentity(identity AgentIdentity) int {
	for i, s := range c.slots {
		if s.hasIdentity && s.identity == identity {
			return i
		}
	}
	return -1
}

func (c *Core) status() StatusView {
	status := StatusView{Device: c.device}
	for i, s := range c.slots {
		status.Slots[i] = SlotView{
			Number:      i,
			LEDIndex:    PhysicalLEDIndexes[i],
			State:       s.state,
			Color:       c.slotColor(i, s.state),
			Identity:    s.identity,
			HasIdentity: s.hasIdentity,
			LeaseEnd:    s.leaseEnd,
		}
	}
	for index := 0; index < LEDCount; index++ {
		color, layer, expiresAt, hasExpiry := c.resolve(index)
		status.Lights[index] = LightView{
			Index:     index,
			Zone:      zoneForIndex(index),
			Color:     color,
			Layer:     layer,
			ExpiresAt: expiresAt,
			HasExpiry: hasExpiry,
		}
	}
	return status
}

func (c *Core) slotColor(index int, state SlotState) Color {
	switch state {
	case Active:
		return ActiveColors[index]
	case Finished:
		return Color{G: 255}
	case Failed:
		return Color{R: 255}
	default:
		return OffColor
	}
}

func (c *Core) resolve(index int) (Color, Layer, time.Time, bool) {
	if c.display.ownsReservedLED(index) {
		return c.display.resolve(index, c.keyBrightness, c.underglowBrightness)
	}
	if slotIndex, ok := slotForLED(index); ok {
		switch c.slots[slotIndex].state {
		case Active:
			return ActiveColors[slotIndex], LayerAgentSlot, time.Time{}, false
		case Finished:
			return Color{G: 255}, LayerAgentSlot, time.Time{}, false
		case Failed:
			return Color{R: 255}, LayerAgentSlot, time.Time{}, false
		}
	}
	if index >= 21 && index <= 26 && c.lifecycle.active {
		return c.lifecycle.color, LayerLifecycle, c.lifecycle.expiresAt, true
	}
	if c.ttl[index].active {
		return c.ttl[index].color, LayerManualTTL, c.ttl[index].expiresAt, true
	}
	if c.persistentSet[index] {
		return c.persistent[index], LayerManualPersistent, time.Time{}, false
	}
	return OffColor, LayerOff, time.Time{}, false
}

func slotForLED(index int) (int, bool) {
	if index < 12 || index > 15 {
		return -1, false
	}
	return index - 12, true
}

func zoneForIndex(index int) Zone {
	switch {
	case index <= 15:
		return ZoneKeys
	case index <= 20:
		return ZoneMMD
	default:
		return ZoneBackglow
	}
}

func (c *Core) publishDesired() {
	var desired DesiredState
	for index := range desired.LEDs {
		color, _, _, _ := c.resolve(index)
		desired.LEDs[index] = LED{Index: index, Color: color}
	}
	for {
		select {
		case c.snapshots <- desired:
			return
		default:
		}
		select {
		case <-c.snapshots:
		default:
		}
	}
}

// SetDeviceStatus submits a device status event to the core.
func (c *Core) SetDeviceStatus(ctx context.Context, status DeviceStatus) error {
	_, err := c.Submit(ctx, Event{Type: EventDeviceStatus, DeviceStatus: status})
	return err
}

// IsAPIError reports whether err is one of the stable core API errors.
func IsAPIError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr)
}
