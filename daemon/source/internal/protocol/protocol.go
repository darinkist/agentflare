// Package protocol defines the versioned local JSON API.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
)

const (
	Version         = 1
	MaxRequestBytes = 16 * 1024
	// MaxResponseBytes bounds one newline-delimited response independently of
	// the request limit. Status snapshots can legitimately be larger than a
	// small lifecycle request, but never need an unbounded allocation.
	MaxResponseBytes = 64 * 1024
)

type EventType string

const (
	Started          EventType = "started"
	Heartbeat        EventType = "heartbeat"
	Finished         EventType = "finished"
	Failed           EventType = "failed"
	Cancelled        EventType = "cancelled"
	Clear            EventType = "clear"
	Status           EventType = "status"
	LightsSet        EventType = "lights_set"
	LightsClear      EventType = "lights_clear"
	DisplayBind      EventType = "display_bind"
	DisplayEvent     EventType = "display_event"
	DisplaySync      EventType = "display_sync"
	DisplayHeartbeat EventType = "display_heartbeat"
	DisplayUnbind    EventType = "display_unbind"
)

// Request is the public newline-delimited API request.
type Request struct {
	Version int             `json:"version"`
	Type    EventType       `json:"type"`
	Source  string          `json:"source,omitempty"`
	AgentID string          `json:"agent_id,omitempty"`
	Target  *Target         `json:"target,omitempty"`
	Color   *Color          `json:"color,omitempty"`
	TTLMS   *int64          `json:"ttl_ms,omitempty"`
	V2      *DisplayRequest `json:"-"`
}

// Target selects exactly one zone or an explicit list of physical LED indices.
type Target struct {
	Zone       string `json:"zone,omitempty"`
	LEDIndices []int  `json:"led_indices,omitempty"`
}

// Indices returns a normalized ascending copy of the target indices.
func (t Target) Indices() ([]int, bool) {
	if t.Zone != "" && len(t.LEDIndices) != 0 {
		return nil, false
	}
	if t.Zone != "" {
		indices, ok := core.LEDIndicesForZone(core.Zone(t.Zone))
		return indices, ok
	}
	if len(t.LEDIndices) == 0 {
		return nil, false
	}
	indices := append([]int(nil), t.LEDIndices...)
	sort.Ints(indices)
	for i, index := range indices {
		if index < 0 || index >= core.LEDCount || (i > 0 && indices[i-1] == index) {
			return nil, false
		}
	}
	return indices, true
}

// APIError is the public stable error envelope.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Envelope is the sole response shape used by the socket API.
type Envelope struct {
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *APIError `json:"error,omitempty"`
}

type Color struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
}

type DeviceStatus struct {
	Status string `json:"status"`
}

type SlotStatus struct {
	Slot     int    `json:"slot"`
	LEDIndex int    `json:"led_index"`
	State    string `json:"state"`
	Color    Color  `json:"color"`
	Source   string `json:"source,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
	LeaseEnd string `json:"lease_end,omitempty"`
}

type LightStatus struct {
	LEDIndex  int    `json:"led_index"`
	Zone      string `json:"zone"`
	Color     Color  `json:"color"`
	Layer     string `json:"layer"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type StatusResult struct {
	Device DeviceStatus  `json:"device"`
	Slots  []SlotStatus  `json:"slots"`
	Lights []LightStatus `json:"lights"`
}

type LightResult struct {
	LEDIndices []int  `json:"led_indices"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

// ParseRequest validates the schema and returns a normalized request.
func ParseRequest(data []byte) (Request, *APIError) {
	if len(data) > MaxRequestBytes || !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return Request{}, invalidRequest()
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return Request{}, invalidRequest()
	}
	version, ok := parseInt(fields, "version")
	if !ok {
		return Request{}, invalidRequest()
	}
	if version == DisplayVersion {
		return parseDisplayRequest(fields)
	}
	if version != Version {
		return Request{}, &APIError{Code: "unsupported_version", Message: "unsupported request version"}
	}
	typeValue, ok := parseString(fields, "type")
	if !ok || typeValue == "" {
		return Request{}, invalidRequest()
	}
	request := Request{Version: version, Type: EventType(typeValue)}
	if source, present := fields["source"]; present {
		if err := json.Unmarshal(source, &request.Source); err != nil {
			return Request{}, invalidRequest()
		}
	}
	if agentID, present := fields["agent_id"]; present {
		if err := json.Unmarshal(agentID, &request.AgentID); err != nil {
			return Request{}, invalidRequest()
		}
	}

	switch request.Type {
	case Started, Heartbeat, Finished, Failed, Cancelled:
		if request.Source == "" || request.AgentID == "" {
			return Request{}, invalidRequest()
		}
		return request, nil
	case Clear, Status:
		return request, nil
	case LightsSet:
		target, err := parseTarget(fields["target"])
		if err != nil {
			return Request{}, invalidRequest()
		}
		color, err := parseColor(fields["color"])
		if err != nil {
			return Request{}, invalidRequest()
		}
		request.Target = &target
		request.Color = &color
		if raw, present := fields["ttl_ms"]; present {
			value, ok := parseInt64(raw)
			if !ok || value < 1 || value > int64((24*time.Hour)/time.Millisecond) {
				return Request{}, invalidRequest()
			}
			request.TTLMS = &value
		}
		return request, nil
	case LightsClear:
		if _, present := fields["color"]; present {
			return Request{}, invalidRequest()
		}
		if _, present := fields["ttl_ms"]; present {
			return Request{}, invalidRequest()
		}
		target, err := parseTarget(fields["target"])
		if err != nil {
			return Request{}, invalidRequest()
		}
		request.Target = &target
		return request, nil
	default:
		return Request{}, &APIError{Code: "unknown_event_type", Message: "unknown event type"}
	}
}

func parseTarget(raw json.RawMessage) (Target, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Target{}, fmt.Errorf("target is required")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Target{}, fmt.Errorf("target is invalid")
	}
	zoneRaw, hasZone := fields["zone"]
	indicesRaw, hasIndices := fields["led_indices"]
	if hasZone == hasIndices {
		return Target{}, fmt.Errorf("target must have one selector")
	}
	if hasZone {
		var zone string
		if err := json.Unmarshal(zoneRaw, &zone); err != nil || zone == "" {
			return Target{}, fmt.Errorf("zone is invalid")
		}
		if _, ok := core.LEDIndicesForZone(core.Zone(zone)); !ok {
			return Target{}, fmt.Errorf("zone is invalid")
		}
		return Target{Zone: zone}, nil
	}
	var rawIndices []json.RawMessage
	if err := json.Unmarshal(indicesRaw, &rawIndices); err != nil || len(rawIndices) == 0 || len(rawIndices) > core.LEDCount {
		return Target{}, fmt.Errorf("LED indices are invalid")
	}
	indices := make([]int, len(rawIndices))
	for i, rawIndex := range rawIndices {
		value, ok := parseInt64(rawIndex)
		if !ok || value < 0 || value >= core.LEDCount {
			return Target{}, fmt.Errorf("LED index is invalid")
		}
		indices[i] = int(value)
	}
	sort.Ints(indices)
	for i := 1; i < len(indices); i++ {
		if indices[i] == indices[i-1] {
			return Target{}, fmt.Errorf("LED indices contain duplicates")
		}
	}
	return Target{LEDIndices: indices}, nil
}

func parseColor(raw json.RawMessage) (Color, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Color{}, fmt.Errorf("color is required")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Color{}, fmt.Errorf("color is invalid")
	}
	values := [3]uint8{}
	for index, channel := range []string{"r", "g", "b"} {
		rawValue, present := fields[channel]
		if !present {
			return Color{}, fmt.Errorf("color channel is required")
		}
		value, ok := parseInt64(rawValue)
		if !ok || value < 0 || value > 255 {
			return Color{}, fmt.Errorf("color channel is invalid")
		}
		values[index] = uint8(value)
	}
	return Color{R: values[0], G: values[1], B: values[2]}, nil
}

func parseInt(fields map[string]json.RawMessage, name string) (int, bool) {
	raw, ok := fields[name]
	if !ok {
		return 0, false
	}
	value, ok := parseInt64(raw)
	return int(value), ok && int64(int(value)) == value
}

func parseInt64(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

func parseString(fields map[string]json.RawMessage, name string) (string, bool) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

func invalidRequest() *APIError {
	return &APIError{Code: "invalid_request", Message: "request is invalid"}
}

func Success(result any) Envelope   { return Envelope{OK: true, Result: result} }
func Failure(err APIError) Envelope { return Envelope{OK: false, Error: &err} }

// EncodeResponse serializes one response envelope followed by one newline.
func EncodeResponse(response Envelope) ([]byte, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode response: %w", err)
	}
	data = append(data, '\n')
	if len(data) > MaxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", MaxResponseBytes)
	}
	return data, nil
}
