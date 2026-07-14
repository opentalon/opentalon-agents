package agent

import (
	"context"
	"encoding/json"
	"testing"
)

const webhookCfg = `{"value_path":"stock","id_field":"barcode","attribute":"current_stock"}`

func webhookTrigger() Trigger {
	return Trigger{Type: TriggerWebhook, Config: json.RawMessage(webhookCfg)}
}

func TestWebhookConfig_Decode(t *testing.T) {
	a := Agent{Triggers: []Trigger{webhookTrigger()}}
	wc, ok := a.WebhookTrigger()
	if !ok || wc.ValuePath != "stock" || wc.IDField != "barcode" || wc.Attribute != "current_stock" {
		t.Errorf("webhook trigger: ok=%v %+v", ok, wc)
	}
	if _, err := (Trigger{Type: TriggerPoll}).Webhook(); err == nil {
		t.Error("Webhook() on a poll trigger should error")
	}
}

func TestWebhookAgent_ScopedByUser(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	a, err := m.Create(ctx, Agent{Name: "restock", GroupID: "g1", EntityID: "u1", Enabled: true,
		TalonSource: `workflow "x" {}`, Triggers: []Trigger{webhookTrigger()}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := m.WebhookAgent(ctx, "u1", "restock")
	if err != nil || got.ID != a.ID {
		t.Errorf("WebhookAgent by name: %v %s", err, got.ID)
	}
	if _, err := m.WebhookAgent(ctx, "u1", a.ID); err != nil {
		t.Errorf("WebhookAgent by id: %v", err)
	}
	// Wrong user → not found (scoped by entity_id).
	if _, err := m.WebhookAgent(ctx, "other", "restock"); err != ErrNotFound {
		t.Errorf("cross-user lookup: got %v", err)
	}
	// An agent without a webhook trigger is not resolvable here.
	mustCreate(t, m, Agent{Name: "manual", GroupID: "g1", EntityID: "u1", Enabled: true,
		TalonSource: `workflow "x" {}`, Triggers: []Trigger{{Type: TriggerManual}}})
	if _, err := m.WebhookAgent(ctx, "u1", "manual"); err != ErrNotFound {
		t.Errorf("non-webhook agent: got %v", err)
	}
}

func TestPendingEvents_Queue(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)

	ev, err := m.EnqueueEvent(ctx, PendingEvent{AgentID: "a1", Payload: json.RawMessage(`{"stock":8}`)})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if ev.ID == "" || ev.Kind != EventKindFacts {
		t.Errorf("enqueue defaults: %+v", ev)
	}

	list, err := m.ListPendingEvents(ctx)
	if err != nil || len(list) != 1 || list[0].AgentID != "a1" {
		t.Fatalf("list: %v %+v", err, list)
	}
	if string(list[0].Payload) != `{"stock":8}` {
		t.Errorf("payload round-trip: %s", list[0].Payload)
	}

	if err := m.DeleteEvent(ctx, ev.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if list, _ := m.ListPendingEvents(ctx); len(list) != 0 {
		t.Errorf("expected empty queue after delete, got %d", len(list))
	}
}

func TestGetByID(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	a, _ := m.Create(ctx, Agent{Name: "x", GroupID: "g1", TalonSource: `workflow "x" {}`})
	got, err := m.GetByID(ctx, a.ID)
	if err != nil || got.Name != "x" {
		t.Errorf("GetByID: %v %+v", err, got)
	}
	if _, err := m.GetByID(ctx, "nope"); err != ErrNotFound {
		t.Errorf("GetByID missing: %v", err)
	}
}
