package agent

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestMap_SingleEntity(t *testing.T) {
	pc := PollConfig{ValuePath: "item.current_stock", IDField: "item.barcode", Attribute: "current_stock"}
	resp := decode(t, `{"item":{"barcode":"ABC-123","current_stock":8}}`)

	facts, reg, err := Map(pc, resp, nil)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("expected 1 fact, got %d", len(facts))
	}
	f := facts[0]
	if f.RecordID != "1" || f.Attribute != "current_stock" || f.Value != float64(8) {
		t.Errorf("fact: %+v", f)
	}
	if reg["ABC-123"] != 1 {
		t.Errorf("registry: %+v", reg)
	}
}

func TestMap_StableAndGrowingIDs(t *testing.T) {
	pc := PollConfig{ValuePath: "stock", IDField: "id", Attribute: "current_stock"}
	reg := map[string]int{}

	_, reg, _ = Map(pc, decode(t, `{"id":"ABC-123","stock":8}`), reg)
	// Same entity again keeps id 1.
	f2, reg, _ := Map(pc, decode(t, `{"id":"ABC-123","stock":5}`), reg)
	if f2[0].RecordID != "1" {
		t.Errorf("same entity should keep id 1, got %s", f2[0].RecordID)
	}
	// A different entity gets the next id.
	f3, reg, _ := Map(pc, decode(t, `{"id":"XYZ-9","stock":2}`), reg)
	if f3[0].RecordID != "2" {
		t.Errorf("new entity should get id 2, got %s", f3[0].RecordID)
	}
	if reg["ABC-123"] != 1 || reg["XYZ-9"] != 2 {
		t.Errorf("registry: %+v", reg)
	}
}

func TestMap_ArrayIndexPath(t *testing.T) {
	pc := PollConfig{ValuePath: "items.0.stock", IDField: "items.0.id", Attribute: "current_stock"}
	resp := decode(t, `{"items":[{"id":"A","stock":4}]}`)
	facts, _, err := Map(pc, resp, nil)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if facts[0].Value != float64(4) {
		t.Errorf("array-index extraction: %+v", facts[0])
	}
}

func TestMap_NoIDFieldUsesSelf(t *testing.T) {
	pc := PollConfig{ValuePath: "current_stock", Attribute: "current_stock"}
	reg := map[string]int{}
	_, reg, err := Map(pc, decode(t, `{"current_stock":8}`), reg)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if reg["self"] != 1 {
		t.Errorf("no id_field should map the implicit 'self' entity, got %+v", reg)
	}
}

func TestMap_ValuePathMissing(t *testing.T) {
	pc := PollConfig{ValuePath: "item.nope", Attribute: "x"}
	if _, _, err := Map(pc, decode(t, `{"item":{"current_stock":8}}`), nil); err == nil {
		t.Error("missing value_path should error")
	}
}
