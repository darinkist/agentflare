package adapter

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/konstantinrink/agentflare/daemon/source/internal/protocol"
)

func TestCompactIDsKeepMaximumSnapshotWithinSocketLimit(t *testing.T) {
	mapped := newIDMapper()
	long := strings.Repeat("x", 128)
	value, err := mapped.mapID(long)
	if err != nil || len(value) > 27 || value == long {
		t.Fatalf("mapped ID = %q err=%v", value, err)
	}
	runs := make([]Run, 0, MaxOpenSubagents)
	for index := 1; index <= MaxOpenSubagents; index++ {
		runs = append(runs, Run{AgentID: long + "agent-" + strconv.Itoa(index), RunID: long + "run-" + strconv.Itoa(index), Role: "subagent", ParentAgentID: "main", State: "running", StartOrder: uint64(index)})
	}
	registry, err := newRegistry(Snapshot{Subagents: runs})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{ids: mapped, bindingID: "binding", nextSeq: 1}
	request, err := runtime.syncRequest(registry)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > protocol.MaxRequestBytes {
		t.Fatalf("snapshot size = %d, limit = %d", len(payload), protocol.MaxRequestBytes)
	}
}
