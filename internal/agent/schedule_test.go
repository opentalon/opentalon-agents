package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func schedTrigger(cron string) Trigger { return Trigger{Type: TriggerSchedule, Cron: cron} }

func TestValidateTriggers(t *testing.T) {
	good := [][]Trigger{
		{schedTrigger("*/5 * * * *")},
		{{Type: TriggerPoll, Config: json.RawMessage(pollCfg)}},
		{{Type: TriggerWebhook, Config: json.RawMessage(webhookCfg)}},
		{{Type: TriggerManual}},
	}
	for i, ts := range good {
		if err := ValidateTriggers(ts); err != nil {
			t.Errorf("good[%d] should validate: %v", i, err)
		}
	}

	bad := map[string][]Trigger{
		"bad cron":          {schedTrigger("not a cron")},
		"empty cron":        {{Type: TriggerSchedule}},
		"poll no value":     {{Type: TriggerPoll, Config: json.RawMessage(`{"server":"s","tool":"t","interval":"5m","attribute":"a"}`)}},
		"poll bad interval": {{Type: TriggerPoll, Config: json.RawMessage(`{"server":"s","tool":"t","interval":"soon","value_path":"v","attribute":"a"}`)}},
		"webhook no attr":   {{Type: TriggerWebhook, Config: json.RawMessage(`{"value_path":"v"}`)}},
		"unknown type":      {{Type: "sometime"}},
	}
	for name, ts := range bad {
		if err := ValidateTriggers(ts); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

func TestScheduleTrigger_Helper(t *testing.T) {
	a := Agent{Triggers: []Trigger{{Type: TriggerManual}, schedTrigger("0 9 * * *")}}
	if c, ok := a.ScheduleTrigger(); !ok || c != "0 9 * * *" {
		t.Errorf("ScheduleTrigger: %q %v", c, ok)
	}
	if _, ok := (Agent{Triggers: []Trigger{{Type: TriggerPoll}}}).ScheduleTrigger(); ok {
		t.Error("no schedule trigger should return false")
	}
}

func TestListEnabledScheduleDue(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	now := time.Now().UTC()

	due := mustCreate(t, m, Agent{Name: "cron", GroupID: "g1", TalonSource: `workflow "x" {}`, Enabled: true, Triggers: []Trigger{schedTrigger("*/5 * * * *")}})
	notDue := mustCreate(t, m, Agent{Name: "cron2", GroupID: "g1", TalonSource: `workflow "x" {}`, Enabled: true, Triggers: []Trigger{schedTrigger("*/5 * * * *")}})
	future := now.Add(time.Hour)
	if err := m.SaveState(ctx, AgentState{AgentID: notDue.ID, NextCronAt: &future}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// A poll-only agent must not appear in the schedule sweep.
	mustCreate(t, m, Agent{Name: "poll", GroupID: "g1", TalonSource: `workflow "x" {}`, Enabled: true, Triggers: []Trigger{{Type: TriggerPoll, Config: json.RawMessage(pollCfg)}}})

	got, err := m.ListEnabledScheduleDue(ctx, now)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	ids := map[string]bool{}
	for _, a := range got {
		ids[a.ID] = true
	}
	if !ids[due.ID] || ids[notDue.ID] || len(got) != 1 {
		t.Errorf("schedule-due selection wrong: %v", ids)
	}
}
