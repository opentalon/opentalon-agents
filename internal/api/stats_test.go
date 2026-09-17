package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

// result builds a run Result trace with the given step outputs (name → raw JSON).
func result(t *testing.T, steps map[string]string) json.RawMessage {
	t.Helper()
	type step struct {
		Name   string          `json:"name"`
		Output json.RawMessage `json:"output"`
	}
	var ss []step
	for name, out := range steps {
		ss = append(ss, step{Name: name, Output: json.RawMessage(out)})
	}
	b, err := json.Marshal(map[string]any{"blocks": map[string]any{"wf": map[string]any{"steps": ss}}})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return b
}

func TestBuildStats(t *testing.T) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	ago := func(d int) *time.Time { tt := now.AddDate(0, 0, -d); return &tt }

	runs := []agent.Run{
		// newest first
		{ID: "dry1", TriggerType: agent.TriggerDryRun, Status: agent.StatusCompleted, FinishedAt: ago(0),
			Result: result(t, map[string]string{
				"search": `{"items":[{"id":1},{"id":2}]}`,
				"notify": `{"dry_run":true,"operation":"notify_user"}`,
			})},
		{ID: "live1", TriggerType: agent.TriggerSchedule, Status: agent.StatusCompleted, FinishedAt: ago(1),
			Result: result(t, map[string]string{
				"search": `{"pagination":{"total_count":3},"items":[{"id":1}]}`,
				"ticket": `{"id":99,"name":"Reorder"}`,
			})},
		{ID: "live_nomatch", TriggerType: agent.TriggerEvent, Status: agent.StatusCompleted, FinishedAt: ago(2),
			Result: result(t, map[string]string{"search": `{"items":[]}`})},
		{ID: "live_fail", TriggerType: agent.TriggerSchedule, Status: agent.StatusFailed, FinishedAt: ago(3), Error: "boom"},
		{ID: "old", TriggerType: agent.TriggerSchedule, Status: agent.StatusCompleted, FinishedAt: ago(40),
			Result: result(t, map[string]string{"search": `{"items":[{"id":1}]}`, "ticket": `{"id":7}`})},
	}

	got := buildStats(runs, 30, now)

	if got.Tiles.RealRuns != 3 { // live1, live_nomatch, live_fail (old is outside window)
		t.Errorf("RealRuns = %d, want 3", got.Tiles.RealRuns)
	}
	if got.Tiles.FailedRuns != 1 {
		t.Errorf("FailedRuns = %d, want 1", got.Tiles.FailedRuns)
	}
	if got.Tiles.NoMatchRuns != 1 {
		t.Errorf("NoMatchRuns = %d, want 1", got.Tiles.NoMatchRuns)
	}
	if got.Tiles.ActionsTaken != 1 { // only live1 opened a ticket
		t.Errorf("ActionsTaken = %d, want 1", got.Tiles.ActionsTaken)
	}
	if got.Tiles.LastDryRun == nil || got.Tiles.LastDryRun.Matched != 2 || got.Tiles.LastDryRun.WouldDo != 1 {
		t.Errorf("LastDryRun = %+v, want matched=2 would_do=1", got.Tiles.LastDryRun)
	}
	if len(got.Runs) != 5 {
		t.Fatalf("rows = %d, want 5 (all runs listed)", len(got.Runs))
	}
	// live1 row: matched from pagination total (3), one write action.
	if got.Runs[1].Matched != 3 || got.Runs[1].Actions != 1 {
		t.Errorf("live1 row = matched %d actions %d, want 3/1", got.Runs[1].Matched, got.Runs[1].Actions)
	}
}
