package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventConfig_ResolvedFillsTaxonomyDefaults(t *testing.T) {
	// Subscribe by name alone → the taxonomy supplies the mapping.
	got, err := EventConfig{Event: "item.status_changed"}.Resolved()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.ValuePath != "status" || got.IDField != "item_id" || got.Attribute != "status" {
		t.Errorf("defaults not applied: %+v", got)
	}
}

func TestEventConfig_ResolvedKeepsOverrides(t *testing.T) {
	got, err := EventConfig{Event: "item.stock_changed", ValuePath: "payload.qty", Attribute: "qty"}.Resolved()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.ValuePath != "payload.qty" || got.Attribute != "qty" {
		t.Errorf("override not kept: %+v", got)
	}
	if got.IDField != "item_id" { // untouched field still defaulted
		t.Errorf("id_field default not applied: %+v", got)
	}
}

func TestEventConfig_ResolvedRejectsUnknown(t *testing.T) {
	_, err := EventConfig{Event: "item.exploded"}.Resolved()
	if err == nil {
		t.Fatal("expected an error for an unknown event")
	}
	if !strings.Contains(err.Error(), "known events:") {
		t.Errorf("error should list known events, got: %v", err)
	}
}

func TestValidateTriggers_Event(t *testing.T) {
	ok := []Trigger{{Type: TriggerEvent, Config: json.RawMessage(`{"event":"checkout.created"}`)}}
	if err := ValidateTriggers(ok); err != nil {
		t.Errorf("valid event trigger rejected: %v", err)
	}

	noName := []Trigger{{Type: TriggerEvent, Config: json.RawMessage(`{}`)}}
	if err := ValidateTriggers(noName); err == nil {
		t.Error("event trigger without a name should be rejected")
	}

	unknown := []Trigger{{Type: TriggerEvent, Config: json.RawMessage(`{"event":"nope"}`)}}
	if err := ValidateTriggers(unknown); err == nil {
		t.Error("event trigger for an unknown event should be rejected")
	}
}

func TestAgent_EventTriggerAndSubscribesTo(t *testing.T) {
	a := Agent{Triggers: []Trigger{
		{Type: TriggerEvent, Config: json.RawMessage(`{"event":"item.status_changed"}`)},
		{Type: TriggerEvent, Config: json.RawMessage(`{"event":"checkout.returned","attribute":"custom"}`)},
	}}

	if got := a.EventTriggers(); len(got) != 2 {
		t.Fatalf("expected two event triggers, got %d", len(got))
	}
	if !a.SubscribesTo("item.status_changed") || !a.SubscribesTo("checkout.returned") {
		t.Error("SubscribesTo missed a declared event")
	}
	if a.SubscribesTo("item.synced") {
		t.Error("SubscribesTo matched an undeclared event")
	}

	// Resolved config for the second: override kept, defaults filled.
	ec, ok := a.EventTrigger("checkout.returned")
	if !ok {
		t.Fatal("EventTrigger did not find checkout.returned")
	}
	if ec.Attribute != "custom" || ec.ValuePath != "checkout_id" || ec.IDField != "item_id" {
		t.Errorf("resolved event trigger = %+v", ec)
	}
}

func TestKnownEventsSorted(t *testing.T) {
	names := KnownEvents()
	if len(names) == 0 {
		t.Fatal("taxonomy is empty")
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("KnownEvents not sorted: %v", names)
			break
		}
	}
}
