package server

import (
	"encoding/json"
	"testing"

	"github.com/konstantinrink/agentflare/daemon/source/internal/core"
	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
)

func TestDisplayV2SocketLifecycle(t *testing.T) {
	running := startTestServer(t)
	bind := request(t, running.server.socketPath, `{"version":2,"type":"display_bind","source":"codex","session_id":"session-1","bind_id":"bind-1"}`)
	if !bind.OK {
		t.Fatalf("bind response = %#v", bind)
	}
	bindData, err := json.Marshal(bind.Result)
	if err != nil {
		t.Fatal(err)
	}
	var bindResult protocol.DisplayBindResult
	if err := json.Unmarshal(bindData, &bindResult); err != nil {
		t.Fatal(err)
	}
	if bindResult.BindingID == "" || bindResult.NextSeq != 1 || bindResult.LeaseMS != core.DisplayLeaseDuration.Milliseconds() {
		t.Fatalf("bind result = %#v", bindResult)
	}

	event := request(t, running.server.socketPath, `{"version":2,"type":"display_event","binding_id":"`+bindResult.BindingID+`","seq":1,"role":"main","agent_id":"main","run_id":"main-1","state":"running"}`)
	if !event.OK {
		t.Fatalf("main event response = %#v", event)
	}
	var eventResult protocol.DisplayWriteResult
	eventData, err := json.Marshal(event.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(eventData, &eventResult); err != nil {
		t.Fatal(err)
	}
	if eventResult.AcceptedSeq != 1 || eventResult.Assignment != "main" {
		t.Fatalf("event result = %#v", eventResult)
	}

	duplicate := request(t, running.server.socketPath, `{"version":2,"type":"display_event","binding_id":"`+bindResult.BindingID+`","seq":1,"role":"main","agent_id":"main","run_id":"main-1","state":"running"}`)
	if !duplicate.OK {
		t.Fatalf("duplicate response = %#v", duplicate)
	}
	conflict := request(t, running.server.socketPath, `{"version":2,"type":"display_event","binding_id":"`+bindResult.BindingID+`","seq":1,"role":"main","agent_id":"main","run_id":"main-1","state":"waiting_user"}`)
	if conflict.OK || conflict.Error == nil || conflict.Error.Code != "sequence_conflict" {
		t.Fatalf("conflict response = %#v", conflict)
	}

	status := request(t, running.server.socketPath, `{"version":2,"type":"status"}`)
	if !status.OK {
		t.Fatalf("status response = %#v", status)
	}
	statusData, err := json.Marshal(status.Result)
	if err != nil {
		t.Fatal(err)
	}
	var statusResult protocol.DisplayStatusResult
	if err := json.Unmarshal(statusData, &statusResult); err != nil {
		t.Fatal(err)
	}
	if statusResult.Health != string(core.DisplayHealthy) || statusResult.MainState != string(core.DisplayRunning) || len(statusResult.Slots) != core.SlotCount || len(statusResult.Lights) != core.LEDCount {
		t.Fatalf("display status = %#v", statusResult)
	}
	if statusResult.Brightness.Keys != core.DefaultKeyBrightness || statusResult.Brightness.Underglow != core.DefaultUnderglowBrightness {
		t.Fatalf("brightness = %#v", statusResult.Brightness)
	}

	v1 := request(t, running.server.socketPath, `{"version":1,"type":"started","source":"legacy","agent_id":"legacy"}`)
	if v1.OK || v1.Error == nil || v1.Error.Code != "display_busy" {
		t.Fatalf("mixed V1 response = %#v", v1)
	}

	takeover := request(t, running.server.socketPath, `{"version":2,"type":"display_bind","source":"codex","session_id":"session-2","bind_id":"bind-2","takeover":true}`)
	if !takeover.OK {
		t.Fatalf("takeover response = %#v", takeover)
	}
	takeoverData, err := json.Marshal(takeover.Result)
	if err != nil {
		t.Fatal(err)
	}
	var takeoverResult protocol.DisplayBindResult
	if err := json.Unmarshal(takeoverData, &takeoverResult); err != nil {
		t.Fatal(err)
	}
	if takeoverResult.BindingID == bindResult.BindingID {
		t.Fatalf("takeover binding ID = %q, want a new binding", takeoverResult.BindingID)
	}

	unbind := request(t, running.server.socketPath, `{"version":2,"type":"display_unbind","binding_id":"`+takeoverResult.BindingID+`","seq":1}`)
	if !unbind.OK {
		t.Fatalf("unbind response = %#v code=%s", unbind, unbind.Error.Code)
	}
	status = request(t, running.server.socketPath, `{"version":2,"type":"status"}`)
	if !status.OK {
		t.Fatalf("unbound status response = %#v", status)
	}
	statusData, err = json.Marshal(status.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(statusData, &statusResult); err != nil {
		t.Fatal(err)
	}
	if statusResult.Health != string(core.DisplayUnbound) {
		t.Fatalf("unbound display status = %#v", statusResult)
	}
}
