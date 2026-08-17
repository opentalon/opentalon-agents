package plugin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

func eventAgent(t *testing.T, mgr *agent.Manager, event string) agent.Agent {
	t.Helper()
	ec := `{"event":"` + event + `"}`
	a, err := mgr.Create(context.Background(), agent.Agent{
		Name: "defective-watch", GroupID: "g1", EntityID: "u1", Enabled: true,
		TlnSource: `on change attr "status" { when new_value == "defective" workflow "Open ticket" }`,
		Triggers:  []agent.Trigger{{Type: agent.TriggerEvent, Config: json.RawMessage(ec)}},
	})
	if err != nil {
		t.Fatalf("create event agent: %v", err)
	}
	return a
}

func TestEngine_DrainsNamedEventAndFires(t *testing.T) {
	ctx := context.Background()
	e, mgr := engineFixture(t)
	a := eventAgent(t, mgr, "item.status_changed")

	// Prior state: item known as "ok", registry maps its id to entity 1.
	if err := mgr.SaveState(ctx, agent.AgentState{
		AgentID: a.ID, FactsSnapshot: json.RawMessage(`{"1":{"status":"ok"}}`),
		EntityMap: map[string]int{"ABC-123": 1},
	}); err != nil {
		t.Fatalf("prime state: %v", err)
	}

	// A named event delivered status="defective" for that item, mapped by the
	// taxonomy (id_field=item_id, value_path=status, attribute=status).
	if _, err := mgr.EnqueueEvent(ctx, agent.PendingEvent{
		AgentID: a.ID, Event: "item.status_changed",
		Payload: json.RawMessage(`{"item_id":"ABC-123","status":"defective"}`),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	host := &statusHost{}
	if _, err := e.tickAt(ctx, host, time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if host.tickets != 1 {
		t.Errorf("expected one ticket from the status event, got %d", host.tickets)
	}
	runs, _ := mgr.ListRuns(ctx, a.ID, 10)
	if len(runs) != 1 || runs[0].TriggerType != agent.TriggerEvent {
		t.Errorf("expected one event-triggered run, got %+v", runs)
	}
	if pend, _ := mgr.ListPendingEvents(ctx); len(pend) != 0 {
		t.Errorf("event should be drained, %d left", len(pend))
	}
	st, _ := mgr.GetState(ctx, a.ID)
	if string(st.FactsSnapshot) != `{"1":{"status":"defective"}}` {
		t.Errorf("snapshot should advance to defective: %s", st.FactsSnapshot)
	}
}

// statusHost simulates tln-plugin.evaluate for a string status attribute: it
// fires "Open ticket" when the asserted status changes to "defective".
type statusHost struct{ tickets int }

func (h *statusHost) RunAction(_ context.Context, _, action string, args map[string]string) (pkg.CallResult, error) {
	if action != "evaluate" {
		return pkg.CallResult{}, nil
	}
	var facts []struct {
		RecordID  string `json:"record_id"`
		Attribute string `json:"attribute"`
		Value     any    `json:"value"`
	}
	_ = json.Unmarshal([]byte(args["facts"]), &facts)

	snap := map[string]map[string]any{}
	if s := args["snapshot"]; s != "" {
		_ = json.Unmarshal([]byte(s), &snap)
	}

	type firing struct {
		OnBlock string `json:"on_block"`
		Ref     string `json:"ref"`
		RefKind string `json:"ref_kind"`
	}
	var firings []firing
	for _, f := range facts {
		prev, _ := snap[f.RecordID]["status"].(string)
		if nv, _ := f.Value.(string); nv == "defective" && prev != "defective" {
			firings = append(firings, firing{OnBlock: `on change attr "status"`, Ref: "Open ticket", RefKind: "workflow"})
			h.tickets++
		}
		if snap[f.RecordID] == nil {
			snap[f.RecordID] = map[string]any{}
		}
		snap[f.RecordID]["status"] = f.Value
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "firings": firings, "snapshot": snap})
	return pkg.CallResult{StructuredContent: string(b)}, nil
}
