package protocol

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseRequestValidation(t *testing.T) {
	tests := []struct {
		name string
		data string
		code string
	}{
		{name: "missing version", data: `{"type":"status"}`, code: "invalid_request"},
		{name: "missing type", data: `{"version":1}`, code: "invalid_request"},
		{name: "unsupported version", data: `{"version":3,"type":"status"}`, code: "unsupported_version"},
		{name: "unknown type", data: `{"version":1,"type":"paused"}`, code: "unknown_event_type"},
		{name: "missing identity", data: `{"version":1,"type":"started","source":"demo"}`, code: "invalid_request"},
		{name: "malformed json", data: `{"version":1`, code: "invalid_request"},
		{name: "not object", data: `[]`, code: "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseRequest([]byte(test.data))
			if err == nil || err.Code != test.code {
				t.Fatalf("error = %#v, want code %q", err, test.code)
			}
		})
	}
}

func TestParseRequestToleratesAdditionalFields(t *testing.T) {
	request, err := ParseRequest([]byte(`{"version":1,"type":"started","source":"demo","agent_id":"alpha","extra":{"ignored":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.Type != Started || request.Source != "demo" || request.AgentID != "alpha" {
		t.Fatalf("request = %#v", request)
	}
}

func TestRequestSizeLimit(t *testing.T) {
	data := bytes.Repeat([]byte{'x'}, MaxRequestBytes+1)
	if _, err := ParseRequest(data); err == nil || err.Code != "invalid_request" {
		t.Fatalf("oversized request error = %#v", err)
	}
}

func TestResponseEnvelope(t *testing.T) {
	data, err := EncodeResponse(Success(map[string]int{"slot": 0}))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"ok":true,"result":{"slot":0}}`+"\n" {
		t.Fatalf("response = %q", data)
	}
	failure, err := EncodeResponse(Failure(APIError{Code: "slot_exhausted", Message: "all four agent slots are in use"}))
	if err != nil {
		t.Fatal(err)
	}
	var decoded Envelope
	if err := json.Unmarshal(bytes.TrimSpace(failure), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.OK || decoded.Error == nil || decoded.Error.Code != "slot_exhausted" {
		t.Fatalf("decoded failure = %#v", decoded)
	}
}

func TestResponseSizeLimit(t *testing.T) {
	_, err := EncodeResponse(Success(strings.Repeat("x", MaxResponseBytes)))
	if err == nil {
		t.Fatal("expected oversized response to be rejected")
	}
}

func TestLightsRequestValidationAndNormalization(t *testing.T) {
	request, err := ParseRequest([]byte(`{"version":1,"type":"lights_set","target":{"led_indices":[7,0,3]},"color":{"r":255,"g":0,"b":1},"ttl_ms":2000}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.Target == nil || request.Color == nil || request.TTLMS == nil {
		t.Fatalf("request = %#v", request)
	}
	if got, want := request.Target.LEDIndices, []int{0, 3, 7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("indices = %v, want %v", got, want)
	}
	if *request.TTLMS != 2000 || *request.Color != (Color{R: 255, B: 1}) {
		t.Fatalf("request fields = %#v", request)
	}
}

func TestLightsRequestRejectsPresenceAndRangeErrors(t *testing.T) {
	tests := []string{
		`{"version":1,"type":"lights_set","target":{"zone":"keys"},"color":{"r":0,"g":0}}`,
		`{"version":1,"type":"lights_set","target":{"zone":"keys"},"color":{"r":0,"g":0,"b":null}}`,
		`{"version":1,"type":"lights_set","target":{"zone":"keys"},"color":{"r":0,"g":0,"b":0},"ttl_ms":null}`,
		`{"version":1,"type":"lights_set","target":{"zone":"keys","led_indices":[0]},"color":{"r":0,"g":0,"b":0}}`,
		`{"version":1,"type":"lights_set","target":{"led_indices":[1,1]},"color":{"r":0,"g":0,"b":0}}`,
		`{"version":1,"type":"lights_set","target":{"led_indices":[27]},"color":{"r":0,"g":0,"b":0}}`,
		`{"version":1,"type":"lights_set","target":{"zone":"keys"},"color":{"r":0,"g":0,"b":0},"ttl_ms":86400001}`,
		`{"version":1,"type":"lights_clear","target":{"zone":"mmd"},"color":null}`,
		`{"version":1,"type":"lights_clear","target":{"zone":"mmd"},"ttl_ms":null}`,
	}
	for _, data := range tests {
		t.Run(data, func(t *testing.T) {
			if _, err := ParseRequest([]byte(data)); err == nil || err.Code != "invalid_request" {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestLightsClearAndZones(t *testing.T) {
	request, err := ParseRequest([]byte(`{"version":1,"type":"lights_clear","target":{"zone":"backglow"},"ignored":true}`))
	if err != nil {
		t.Fatal(err)
	}
	indices, ok := request.Target.Indices()
	if !ok || !reflect.DeepEqual(indices, []int{21, 22, 23, 24, 25, 26}) {
		t.Fatalf("indices = %v, ok=%t", indices, ok)
	}
}

func TestParseDisplayRequestsStrictly(t *testing.T) {
	request, err := ParseRequest([]byte(`{"version":2,"type":"display_event","binding_id":"binding-1","seq":1,"role":"subagent","agent_id":"worker-a","run_id":"run-a-1","parent_agent_id":"main","state":"running"}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.Version != DisplayVersion || request.V2 == nil || request.V2.Seq != 1 || request.V2.Role != "subagent" {
		t.Fatalf("display request = %#v", request)
	}
	for _, data := range []string{
		`{"version":2,"type":"display_event","binding_id":"binding-1","seq":1,"role":"subagent","agent_id":"worker-a","run_id":"run-a-1","parent_agent_id":"main","state":"running","extra":true}`,
		`{"version":2,"type":"display_event","binding_id":"binding-1","seq":1.0,"role":"subagent","agent_id":"worker-a","run_id":"run-a-1","parent_agent_id":"main","state":"running"}`,
		`{"version":2,"type":"display_event","binding_id":"binding-1","seq":1,"role":"subagent","agent_id":"worker-a","run_id":"run-a-1","parent_agent_id":"main","state":"waiting_user","reason":null}`,
		`{"version":2,"type":"display_sync","binding_id":"binding-1","seq":1,"main":null,"subagents":[{"agent_id":"worker-a","run_id":"run-a-1","parent_agent_id":"main","state":"running","start_order":0}]}`,
	} {
		if _, err := ParseRequest([]byte(data)); err == nil || err.Code != "invalid_request" {
			t.Fatalf("request %s error = %#v", data, err)
		}
	}
}

func TestParseDisplaySyncAllowsExplicitIdleMain(t *testing.T) {
	request, err := ParseRequest([]byte(`{"version":2,"type":"display_sync","binding_id":"binding-1","seq":1,"main":null,"subagents":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if request.V2 == nil || !request.V2.HasMain || request.V2.Main != nil || request.V2.Subagents == nil {
		t.Fatalf("sync request = %#v", request.V2)
	}
}

func TestDisplayRequestMarshalsForLocalClients(t *testing.T) {
	requestData, err := json.Marshal(Request{
		Version: DisplayVersion,
		Type:    DisplayEvent,
		V2: &DisplayRequest{
			Type:          DisplayEvent,
			BindingID:     "binding-1",
			Seq:           1,
			Role:          "subagent",
			AgentID:       "worker-a",
			RunID:         "run-a-1",
			ParentAgentID: "main",
			State:         "running",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, parseErr := ParseRequest(requestData)
	if parseErr != nil {
		t.Fatalf("marshaled V2 request %s: %v", requestData, parseErr)
	}
	if parsed.V2 == nil || parsed.V2.AgentID != "worker-a" {
		t.Fatalf("parsed marshaled request = %#v", parsed)
	}
}
