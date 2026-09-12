package protocol

import (
	"bytes"
	"encoding/json"
	"unicode"
	"unicode/utf8"
)

const (
	DisplayVersion = 2
	VersionV2      = DisplayVersion
)

// DisplayRequest is the strict V2 controller request. V1 fields remain on
// Request so existing clients keep their tolerant parsing rules.
type DisplayRequest struct {
	Type          EventType
	Source        string
	SessionID     string
	BindID        string
	Takeover      bool
	BindingID     string
	Seq           uint64
	HasSeq        bool
	Role          string
	AgentID       string
	RunID         string
	ParentAgentID string
	State         string
	Reason        string
	Main          *DisplaySnapshot
	HasMain       bool
	Subagents     []DisplaySnapshot
	Fingerprint   []byte
}

// DisplaySnapshot is one entry in a full V2 sync snapshot.
type DisplaySnapshot struct {
	AgentID       string `json:"agent_id"`
	RunID         string `json:"run_id"`
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	State         string `json:"state"`
	StartOrder    uint64 `json:"start_order,omitempty"`
}

// MarshalJSON makes protocol.Request useful for V2 client tests and small
// local integrations while ParseRequest remains the canonical validator.
func (r Request) MarshalJSON() ([]byte, error) {
	if r.Version != DisplayVersion || r.V2 == nil {
		type requestAlias Request
		return json.Marshal(requestAlias(r))
	}
	display := r.V2
	displayType := display.Type
	if displayType == "" {
		displayType = r.Type
	}
	payload := map[string]any{
		"version": DisplayVersion,
		"type":    displayType,
	}
	switch displayType {
	case DisplayBind:
		payload["source"] = display.Source
		payload["session_id"] = display.SessionID
		payload["bind_id"] = display.BindID
		if display.Takeover {
			payload["takeover"] = true
		}
	case DisplayEvent:
		payload["binding_id"] = display.BindingID
		payload["seq"] = display.Seq
		payload["role"] = display.Role
		payload["agent_id"] = display.AgentID
		payload["run_id"] = display.RunID
		payload["state"] = display.State
		if display.ParentAgentID != "" {
			payload["parent_agent_id"] = display.ParentAgentID
		}
		if display.Reason != "" {
			payload["reason"] = display.Reason
		}
	case DisplaySync:
		payload["binding_id"] = display.BindingID
		payload["seq"] = display.Seq
		if display.HasMain {
			payload["main"] = display.Main
		}
		subagents := display.Subagents
		if subagents == nil {
			subagents = []DisplaySnapshot{}
		}
		payload["subagents"] = subagents
	case DisplayHeartbeat, DisplayUnbind:
		payload["binding_id"] = display.BindingID
		payload["seq"] = display.Seq
	}
	return json.Marshal(payload)
}

type displayFingerprint struct {
	Type          EventType
	Source        string
	SessionID     string
	BindID        string
	Takeover      bool
	BindingID     string
	Seq           uint64
	HasSeq        bool
	Role          string
	AgentID       string
	RunID         string
	ParentAgentID string
	State         string
	Reason        string
	Main          *DisplaySnapshot
	HasMain       bool
	Subagents     []DisplaySnapshot
}

func parseDisplayRequest(fields map[string]json.RawMessage) (Request, *APIError) {
	typeValue, ok := parseString(fields, "type")
	if !ok || typeValue == "" {
		return Request{}, invalidRequest()
	}
	display := DisplayRequest{Type: EventType(typeValue)}
	switch display.Type {
	case DisplayBind:
		if !onlyFields(fields, "version", "type", "source", "session_id", "bind_id", "takeover") {
			return Request{}, invalidRequest()
		}
		var valid bool
		if display.Source, valid = requiredIdentifier(fields, "source"); !valid {
			return Request{}, invalidRequest()
		}
		if display.SessionID, valid = requiredIdentifier(fields, "session_id"); !valid {
			return Request{}, invalidRequest()
		}
		if display.BindID, valid = requiredIdentifier(fields, "bind_id"); !valid {
			return Request{}, invalidRequest()
		}
		if raw, present := fields["takeover"]; present {
			if err := json.Unmarshal(raw, &display.Takeover); err != nil {
				return Request{}, invalidRequest()
			}
		}
	case DisplayEvent:
		if !onlyFields(fields, "version", "type", "binding_id", "seq", "role", "agent_id", "run_id", "parent_agent_id", "state", "reason") {
			return Request{}, invalidRequest()
		}
		if !parseDisplayWriteFields(fields, &display, false) {
			return Request{}, invalidRequest()
		}
	case DisplaySync:
		if !onlyFields(fields, "version", "type", "binding_id", "seq", "main", "subagents") {
			return Request{}, invalidRequest()
		}
		if !parseDisplayWriteFields(fields, &display, true) {
			return Request{}, invalidRequest()
		}
		if apiErr := parseSyncFields(fields, &display); apiErr != nil {
			return Request{}, apiErr
		}
	case DisplayHeartbeat, DisplayUnbind:
		if !onlyFields(fields, "version", "type", "binding_id", "seq") {
			return Request{}, invalidRequest()
		}
		if !parseDisplayBindingFields(fields, &display) {
			return Request{}, invalidRequest()
		}
	case Status:
		if !onlyFields(fields, "version", "type") {
			return Request{}, invalidRequest()
		}
	default:
		return Request{}, &APIError{Code: "unknown_event_type", Message: "unknown event type"}
	}
	fingerprint, err := json.Marshal(displayFingerprint{
		Type:          display.Type,
		Source:        display.Source,
		SessionID:     display.SessionID,
		BindID:        display.BindID,
		Takeover:      display.Takeover,
		BindingID:     display.BindingID,
		Seq:           display.Seq,
		HasSeq:        display.HasSeq,
		Role:          display.Role,
		AgentID:       display.AgentID,
		RunID:         display.RunID,
		ParentAgentID: display.ParentAgentID,
		State:         display.State,
		Reason:        display.Reason,
		Main:          display.Main,
		HasMain:       display.HasMain,
		Subagents:     display.Subagents,
	})
	if err != nil {
		return Request{}, invalidRequest()
	}
	display.Fingerprint = fingerprint
	return Request{Version: DisplayVersion, Type: display.Type, V2: &display}, nil
}

func parseDisplayWriteFields(fields map[string]json.RawMessage, display *DisplayRequest, sync bool) bool {
	if !parseDisplayBindingFields(fields, display) {
		return false
	}
	if sync {
		return true
	}
	var valid bool
	if display.Role, valid = requiredEnum(fields, "role", "main", "subagent"); !valid {
		return false
	}
	if display.AgentID, valid = requiredIdentifier(fields, "agent_id"); !valid {
		return false
	}
	if display.RunID, valid = requiredIdentifier(fields, "run_id"); !valid {
		return false
	}
	if display.State, valid = requiredEnum(fields, "state", "running", "waiting_user", "succeeded", "failed", "cancelled"); !valid {
		return false
	}
	if raw, present := fields["parent_agent_id"]; present {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
		if display.ParentAgentID, valid = parseIdentifier(raw); !valid {
			return false
		}
	} else if display.Role == "subagent" {
		return false
	}
	if display.Role == "main" && display.ParentAgentID != "" {
		return false
	}
	if raw, present := fields["reason"]; present {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return false
		}
		if display.Reason, valid = parseStringValue(raw); !valid || !validReason(display.Reason) {
			return false
		}
	}
	return true
}

func parseDisplayBindingFields(fields map[string]json.RawMessage, display *DisplayRequest) bool {
	var valid bool
	if display.BindingID, valid = requiredIdentifier(fields, "binding_id"); !valid {
		return false
	}
	if display.Seq, valid = requiredPositiveNumber(fields, "seq"); !valid {
		return false
	}
	display.HasSeq = true
	return true
}

func parseSyncFields(fields map[string]json.RawMessage, display *DisplayRequest) *APIError {
	mainRaw, hasMain := fields["main"]
	subagentsRaw, hasSubagents := fields["subagents"]
	if !hasMain || !hasSubagents || bytes.Equal(bytes.TrimSpace(subagentsRaw), []byte("null")) {
		return invalidRequest()
	}
	display.HasMain = true
	if bytes.Equal(bytes.TrimSpace(mainRaw), []byte("null")) {
		display.Main = nil
	} else {
		snapshot, ok := parseDisplaySnapshot(mainRaw, true)
		if !ok {
			return invalidRequest()
		}
		display.Main = &snapshot
	}
	var rawSubagents []json.RawMessage
	if err := json.Unmarshal(subagentsRaw, &rawSubagents); err != nil {
		return invalidRequest()
	}
	if len(rawSubagents) > 64 {
		return &APIError{Code: "capacity_exceeded", Message: "subagent capacity is exhausted"}
	}
	display.Subagents = make([]DisplaySnapshot, 0, len(rawSubagents))
	seenRuns := make(map[string]struct{}, len(rawSubagents))
	seenOrders := make(map[uint64]struct{}, len(rawSubagents))
	for _, raw := range rawSubagents {
		snapshot, ok := parseDisplaySnapshot(raw, false)
		if !ok {
			return invalidRequest()
		}
		key := snapshot.AgentID + "\x00" + snapshot.RunID
		if _, exists := seenRuns[key]; exists {
			return invalidRequest()
		}
		if _, exists := seenOrders[snapshot.StartOrder]; exists {
			return invalidRequest()
		}
		seenRuns[key] = struct{}{}
		seenOrders[snapshot.StartOrder] = struct{}{}
		display.Subagents = append(display.Subagents, snapshot)
	}
	return nil
}

func parseDisplaySnapshot(raw json.RawMessage, main bool) (DisplaySnapshot, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return DisplaySnapshot{}, false
	}
	if main {
		if !onlyFields(fields, "agent_id", "run_id", "state") {
			return DisplaySnapshot{}, false
		}
	} else if !onlyFields(fields, "agent_id", "run_id", "parent_agent_id", "state", "start_order") {
		return DisplaySnapshot{}, false
	}
	snapshot := DisplaySnapshot{}
	var valid bool
	if snapshot.AgentID, valid = requiredIdentifier(fields, "agent_id"); !valid {
		return DisplaySnapshot{}, false
	}
	if snapshot.RunID, valid = requiredIdentifier(fields, "run_id"); !valid {
		return DisplaySnapshot{}, false
	}
	if snapshot.State, valid = requiredEnum(fields, "state", "running", "waiting_user"); !valid {
		return DisplaySnapshot{}, false
	}
	if main {
		return snapshot, true
	}
	if snapshot.ParentAgentID, valid = requiredIdentifier(fields, "parent_agent_id"); !valid {
		return DisplaySnapshot{}, false
	}
	if snapshot.StartOrder, valid = requiredPositiveNumber(fields, "start_order"); !valid {
		return DisplaySnapshot{}, false
	}
	return snapshot, true
}

func onlyFields(fields map[string]json.RawMessage, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		set[name] = struct{}{}
	}
	for name := range fields {
		if _, ok := set[name]; !ok {
			return false
		}
	}
	return true
}

func requiredIdentifier(fields map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	value, ok := parseIdentifier(raw)
	return value, ok
}

func parseIdentifier(raw json.RawMessage) (string, bool) {
	value, ok := parseStringValue(raw)
	return value, ok && validIdentifier(value)
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func requiredEnum(fields map[string]json.RawMessage, name string, allowed ...string) (string, bool) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	value, ok := parseStringValue(raw)
	if !ok {
		return "", false
	}
	for _, candidate := range allowed {
		if value == candidate {
			return value, true
		}
	}
	return "", false
}

func parseStringValue(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func requiredPositiveNumber(fields map[string]json.RawMessage, name string) (uint64, bool) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.ContainsAny(trimmed, ".eE") {
		return 0, false
	}
	var value uint64
	if err := json.Unmarshal(trimmed, &value); err != nil || value == 0 || value > 9007199254740991 {
		return 0, false
	}
	return value, true
}

func validReason(value string) bool {
	switch value {
	case "approval", "decision", "usage_limit", "execution_error", "user_cancelled", "other":
		return true
	default:
		return false
	}
}

// DisplayBindResult is the stable bind response.
type DisplayBindResult struct {
	BindingID string `json:"binding_id"`
	NextSeq   uint64 `json:"next_seq"`
	LeaseMS   int64  `json:"lease_ms"`
}

// DisplayWriteResult is returned for a sequenced V2 write.
type DisplayWriteResult struct {
	AcceptedSeq      uint64 `json:"accepted_seq"`
	Assignment       string `json:"assignment,omitempty"`
	Slot             *int   `json:"slot,omitempty"`
	QueueLength      int    `json:"queue_length,omitempty"`
	TerminalDeadline string `json:"terminal_deadline,omitempty"`
	LeaseEnd         string `json:"lease_end,omitempty"`
	Unbound          bool   `json:"unbound,omitempty"`
}

// DisplayRunStatus describes a main, visible, or queued run.
type DisplayRunStatus struct {
	Role             string `json:"role"`
	Source           string `json:"source,omitempty"`
	SessionID        string `json:"session_id,omitempty"`
	AgentID          string `json:"agent_id"`
	RunID            string `json:"run_id"`
	ParentAgentID    string `json:"parent_agent_id,omitempty"`
	State            string `json:"state"`
	Slot             *int   `json:"slot,omitempty"`
	StartOrder       uint64 `json:"start_order,omitempty"`
	TerminalDeadline string `json:"terminal_deadline,omitempty"`
}

// DisplaySlotStatus describes a physical subagent slot.
type DisplaySlotStatus struct {
	Slot             int               `json:"slot"`
	LEDIndex         int               `json:"led_index"`
	State            string            `json:"state"`
	Role             string            `json:"role,omitempty"`
	Source           string            `json:"source,omitempty"`
	SessionID        string            `json:"session_id,omitempty"`
	AgentID          string            `json:"agent_id,omitempty"`
	RunID            string            `json:"run_id,omitempty"`
	ParentAgentID    string            `json:"parent_agent_id,omitempty"`
	TerminalDeadline string            `json:"terminal_deadline,omitempty"`
	Run              *DisplayRunStatus `json:"run,omitempty"`
}

// DisplayBrightnessStatus contains the configured semantic brightness.
type DisplayBrightnessStatus struct {
	Keys      int `json:"keys"`
	Underglow int `json:"underglow"`
}

// DisplayStatusResult is the V2 status response.
type DisplayStatusResult struct {
	Health      string                  `json:"health"`
	BindingID   string                  `json:"binding_id,omitempty"`
	Source      string                  `json:"source,omitempty"`
	SessionID   string                  `json:"session_id,omitempty"`
	LeaseEnd    string                  `json:"lease_end,omitempty"`
	MainState   string                  `json:"main_state"`
	Main        *DisplayRunStatus       `json:"main"`
	Slots       []DisplaySlotStatus     `json:"slots"`
	Queue       []DisplayRunStatus      `json:"queue"`
	QueueLength int                     `json:"queue_length"`
	Brightness  DisplayBrightnessStatus `json:"brightness"`
	Device      DeviceStatus            `json:"device"`
	Lights      []LightStatus           `json:"lights"`
}
