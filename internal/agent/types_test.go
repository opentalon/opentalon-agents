package agent

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPollTrigger_RoundTripAndDecode(t *testing.T) {
	// Shape the LLM would author (matches the README example).
	raw := `[{"type":"poll","config":{
		"server":"inventory","tool":"get-item",
		"args":{"barcode":"ABC-123"},
		"interval":"5m",
		"value_path":"item.current_stock",
		"id_field":"item.barcode",
		"attribute":"current_stock"
	}}]`

	var triggers []Trigger
	if err := json.Unmarshal([]byte(raw), &triggers); err != nil {
		t.Fatalf("unmarshal triggers: %v", err)
	}
	if len(triggers) != 1 || triggers[0].Type != TriggerPoll {
		t.Fatalf("unexpected triggers: %+v", triggers)
	}

	pc, err := triggers[0].Poll()
	if err != nil {
		t.Fatalf("Poll(): %v", err)
	}
	if pc.Server != "inventory" || pc.Tool != "get-item" || pc.Args["barcode"] != "ABC-123" {
		t.Errorf("poll config: %+v", pc)
	}
	if pc.ValuePath != "item.current_stock" || pc.IDField != "item.barcode" || pc.Attribute != "current_stock" {
		t.Errorf("poll mapping fields: %+v", pc)
	}
	d, err := pc.IntervalDuration()
	if err != nil || d != 5*time.Minute {
		t.Errorf("interval: got %v, %v; want 5m", d, err)
	}

	// Re-marshal round-trips.
	out, _ := json.Marshal(triggers)
	var back []Trigger
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if _, err := back[0].Poll(); err != nil {
		t.Errorf("round-trip Poll(): %v", err)
	}
}

func TestPoll_RejectsNonPollTrigger(t *testing.T) {
	tr := Trigger{Type: TriggerSchedule, Cron: "0 9 * * *"}
	if _, err := tr.Poll(); err == nil {
		t.Error("Poll() on a schedule trigger should error")
	}
}

func TestPollTrigger_Helper(t *testing.T) {
	a := Agent{Triggers: []Trigger{
		{Type: TriggerSchedule, Cron: "0 9 * * *"},
		{Type: TriggerPoll, Config: json.RawMessage(`{"server":"inv","tool":"get","interval":"1m","value_path":"v","attribute":"current_stock"}`)},
	}}
	pc, ok := a.PollTrigger()
	if !ok || pc.Server != "inv" || pc.Attribute != "current_stock" {
		t.Errorf("PollTrigger: ok=%v pc=%+v", ok, pc)
	}

	none := Agent{Triggers: []Trigger{{Type: TriggerManual}}}
	if _, ok := none.PollTrigger(); ok {
		t.Error("agent without a poll trigger should return ok=false")
	}
}

func TestAgentState_RoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	st := AgentState{
		AgentID:             "a1",
		FactsSnapshot:       json.RawMessage(`{"1":{"current_stock":8}}`),
		EntityMap:           map[string]int{"ABC-123": 1},
		NextPollAt:          &now,
		ConsecutiveFailures: 2,
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back AgentState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.EntityMap["ABC-123"] != 1 || back.ConsecutiveFailures != 2 {
		t.Errorf("state round-trip: %+v", back)
	}
	if back.NextPollAt == nil || !back.NextPollAt.Equal(now) {
		t.Errorf("next_poll_at round-trip: %v", back.NextPollAt)
	}
	if string(back.FactsSnapshot) != `{"1":{"current_stock":8}}` {
		t.Errorf("snapshot round-trip: %s", back.FactsSnapshot)
	}
}
