package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

const pollCfg = `{"server":"inventory","tool":"get-item","interval":"5m","value_path":"item.current_stock","attribute":"current_stock"}`

func pollTrigger() Trigger {
	return Trigger{Type: TriggerPoll, Config: json.RawMessage(pollCfg)}
}

func mustCreate(t *testing.T, m *Manager, a Agent) Agent {
	t.Helper()
	created, err := m.Create(context.Background(), a)
	if err != nil {
		t.Fatalf("create %q: %v", a.Name, err)
	}
	return created
}

func TestState_RoundTripAndUpsert(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	next := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)

	if err := m.SaveState(ctx, AgentState{
		AgentID:             "a1",
		FactsSnapshot:       json.RawMessage(`{"1":{"current_stock":8}}`),
		EntityMap:           map[string]int{"ABC-123": 1},
		NextPollAt:          &next,
		ConsecutiveFailures: 1,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := m.GetState(ctx, "a1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.EntityMap["ABC-123"] != 1 || got.ConsecutiveFailures != 1 {
		t.Errorf("round-trip: %+v", got)
	}
	if got.NextPollAt == nil || !got.NextPollAt.Equal(next) {
		t.Errorf("next_poll_at: %v want %v", got.NextPollAt, next)
	}
	if string(got.FactsSnapshot) != `{"1":{"current_stock":8}}` {
		t.Errorf("snapshot: %s", got.FactsSnapshot)
	}

	// Upsert: saving again updates in place.
	if err := m.SaveState(ctx, AgentState{AgentID: "a1", EntityMap: map[string]int{"ABC-123": 1}, ConsecutiveFailures: 3}); err != nil {
		t.Fatalf("resave: %v", err)
	}
	got, _ = m.GetState(ctx, "a1")
	if got.ConsecutiveFailures != 3 {
		t.Errorf("upsert failures: got %d want 3", got.ConsecutiveFailures)
	}
	if got.NextPollAt != nil {
		t.Errorf("upsert should have cleared next_poll_at, got %v", got.NextPollAt)
	}
}

func TestGetState_MissingReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	st, err := m.GetState(ctx, "nope")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if st.AgentID != "nope" || st.EntityMap == nil || len(st.EntityMap) != 0 {
		t.Errorf("missing state should be empty-but-usable: %+v", st)
	}
	if st.NextPollAt != nil || st.ConsecutiveFailures != 0 {
		t.Errorf("missing state should be zero: %+v", st)
	}
}

func TestListEnabledPollDue(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	now := time.Now().UTC()

	// Due: enabled, poll trigger, no state yet.
	due, _ := m.Create(ctx, Agent{Name: "due", GroupID: "g1", TalonSource: `workflow "x" {}`, Triggers: []Trigger{pollTrigger()}, Enabled: true})
	// Not due: enabled, poll trigger, next_poll_at in the future.
	notDue, _ := m.Create(ctx, Agent{Name: "notdue", GroupID: "g1", TalonSource: `workflow "x" {}`, Triggers: []Trigger{pollTrigger()}, Enabled: true})
	future := now.Add(time.Hour)
	if err := m.SaveState(ctx, AgentState{AgentID: notDue.ID, NextPollAt: &future}); err != nil {
		t.Fatalf("save notdue state: %v", err)
	}
	// Excluded: no poll trigger.
	mustCreate(t, m, Agent{Name: "manual", GroupID: "g1", TalonSource: `workflow "x" {}`, Triggers: []Trigger{{Type: TriggerManual}}, Enabled: true})
	// Excluded: disabled (even though it has a poll trigger, in another group).
	mustCreate(t, m, Agent{Name: "off", GroupID: "g2", TalonSource: `workflow "x" {}`, Triggers: []Trigger{pollTrigger()}, Enabled: false})
	// Due in another group — the sweep is system-wide.
	due2, _ := m.Create(ctx, Agent{Name: "due2", GroupID: "g2", TalonSource: `workflow "x" {}`, Triggers: []Trigger{pollTrigger()}, Enabled: true})

	got, err := m.ListEnabledPollDue(ctx, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, a := range got {
		ids[a.ID] = true
	}
	if !ids[due.ID] || !ids[due2.ID] {
		t.Errorf("expected due agents (incl. cross-group), got ids=%v", ids)
	}
	if ids[notDue.ID] {
		t.Error("future next_poll_at agent must not be due")
	}
	if len(got) != 2 {
		t.Errorf("expected exactly 2 due agents, got %d", len(got))
	}
}
