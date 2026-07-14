package plugin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

func webhookAgent(t *testing.T, mgr *agent.Manager) agent.Agent {
	t.Helper()
	wc := `{"value_path":"stock","id_field":"barcode","attribute":"current_stock"}`
	a, err := mgr.Create(context.Background(), agent.Agent{
		Name: "restock", GroupID: "g1", EntityID: "u1", Enabled: true,
		TalonSource: `on change attr "current_stock" { when prev_value >= 10 and new_value < 10 workflow "Refill stock" }`,
		Triggers:    []agent.Trigger{{Type: agent.TriggerWebhook, Config: json.RawMessage(wc)}},
	})
	if err != nil {
		t.Fatalf("create webhook agent: %v", err)
	}
	return a
}

func TestEngine_DrainsWebhookEventAndFires(t *testing.T) {
	ctx := context.Background()
	e, mgr := engineFixture(t)
	a := webhookAgent(t, mgr)

	// Prior state: known at 15, registry maps the barcode to entity 1.
	if err := mgr.SaveState(ctx, agent.AgentState{
		AgentID: a.ID, FactsSnapshot: json.RawMessage(`{"1":{"current_stock":15}}`),
		EntityMap: map[string]int{"ABC-123": 1},
	}); err != nil {
		t.Fatalf("prime state: %v", err)
	}

	// A webhook delivered stock=8 (a downward crossing).
	if _, err := mgr.EnqueueEvent(ctx, agent.PendingEvent{
		AgentID: a.ID, Payload: json.RawMessage(`{"barcode":"ABC-123","stock":8}`),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	host := &simHost{} // simulates crossing from snapshot+facts
	if _, err := e.tickAt(ctx, host, time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Fired once, run recorded (trigger webhook), ticket created, queue drained.
	if host.tickets != 1 {
		t.Errorf("expected one ticket from the webhook crossing, got %d", host.tickets)
	}
	runs, _ := mgr.ListRuns(ctx, a.ID, 10)
	if len(runs) != 1 || runs[0].TriggerType != agent.TriggerWebhook {
		t.Errorf("expected one webhook run, got %+v", runs)
	}
	if pend, _ := mgr.ListPendingEvents(ctx); len(pend) != 0 {
		t.Errorf("event should be drained, %d left", len(pend))
	}
	st, _ := mgr.GetState(ctx, a.ID)
	if string(st.FactsSnapshot) != `{"1":{"current_stock":8}}` {
		t.Errorf("snapshot should advance to 8: %s", st.FactsSnapshot)
	}
}
