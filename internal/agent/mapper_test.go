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

	facts, reg, trunc, err := Map(pc, resp, nil, 0)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(facts) != 1 || trunc != 0 {
		t.Fatalf("expected 1 fact, 0 truncated; got %d, %d", len(facts), trunc)
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

	_, reg, _, _ = Map(pc, decode(t, `{"id":"ABC-123","stock":8}`), reg, 0)
	f2, reg, _, _ := Map(pc, decode(t, `{"id":"ABC-123","stock":5}`), reg, 0)
	if f2[0].RecordID != "1" {
		t.Errorf("same entity should keep id 1, got %s", f2[0].RecordID)
	}
	f3, reg, _, _ := Map(pc, decode(t, `{"id":"XYZ-9","stock":2}`), reg, 0)
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
	facts, _, _, err := Map(pc, resp, nil, 0)
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
	_, reg, _, err := Map(pc, decode(t, `{"current_stock":8}`), reg, 0)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if reg["self"] != 1 {
		t.Errorf("no id_field should map the implicit 'self' entity, got %+v", reg)
	}
}

func TestMap_ValuePathMissing(t *testing.T) {
	pc := PollConfig{ValuePath: "item.nope", Attribute: "x"}
	if _, _, _, err := Map(pc, decode(t, `{"item":{"current_stock":8}}`), nil, 0); err == nil {
		t.Error("missing value_path should error")
	}
}

func TestMap_MultiEntity(t *testing.T) {
	// items_path → map each element; value_path/id_field are per-item.
	pc := PollConfig{ItemsPath: "items", ValuePath: "stock", IDField: "barcode", Attribute: "current_stock"}
	resp := decode(t, `{"items":[{"barcode":"A","stock":8},{"barcode":"B","stock":3},{"barcode":"C","stock":20}]}`)

	facts, reg, trunc, err := Map(pc, resp, nil, 0)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(facts) != 3 || trunc != 0 {
		t.Fatalf("expected 3 facts, 0 truncated; got %d, %d", len(facts), trunc)
	}
	if reg["A"] == 0 || reg["B"] == 0 || reg["C"] == 0 || reg["A"] == reg["B"] {
		t.Errorf("registry should assign distinct ids: %+v", reg)
	}
}

func TestMap_MultiEntityCapTruncates(t *testing.T) {
	pc := PollConfig{ItemsPath: "items", ValuePath: "stock", IDField: "barcode", Attribute: "current_stock"}
	resp := decode(t, `{"items":[{"barcode":"A","stock":1},{"barcode":"B","stock":2},{"barcode":"C","stock":3}]}`)

	facts, _, trunc, err := Map(pc, resp, nil, 2) // cap at 2
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(facts) != 2 || trunc != 1 {
		t.Errorf("cap should keep 2 and report 1 dropped; got %d facts, %d truncated", len(facts), trunc)
	}
}

func TestMap_ItemsPathNotAList(t *testing.T) {
	pc := PollConfig{ItemsPath: "items", ValuePath: "stock", Attribute: "current_stock"}
	if _, _, _, err := Map(pc, decode(t, `{"items":{"not":"a list"}}`), nil, 0); err == nil {
		t.Error("items_path pointing at a non-list should error")
	}
}
