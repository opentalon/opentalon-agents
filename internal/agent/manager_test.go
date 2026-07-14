package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/opentalon/opentalon-agents/internal/store"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewManager(db)
}

func TestManager_CRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)

	created, err := m.Create(ctx, Agent{
		Name:        "restock",
		GroupID:     "g1",
		EntityID:    "u1",
		TalonSource: `workflow "x" {}`,
		Triggers:    []Trigger{{Type: TriggerSchedule, Cron: "0 9 * * *"}},
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected generated id")
	}

	got, err := m.Get(ctx, "g1", "restock")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if got.ID != created.ID || got.TalonSource != `workflow "x" {}` {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if len(got.Triggers) != 1 || got.Triggers[0].Cron != "0 9 * * *" {
		t.Errorf("triggers not round-tripped: %+v", got.Triggers)
	}

	// Group isolation: another group can't see it.
	if _, err := m.Get(ctx, "other", "restock"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound across groups, got %v", err)
	}

	// Update source.
	if _, err := m.Update(ctx, "g1", created.ID, `workflow "y" {}`, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = m.Get(ctx, "g1", created.ID)
	if got.TalonSource != `workflow "y" {}` {
		t.Errorf("update not persisted: %q", got.TalonSource)
	}
	if len(got.Triggers) != 1 {
		t.Errorf("nil triggers on update should preserve existing, got %+v", got.Triggers)
	}

	// Enable/disable.
	if a, _ := m.SetEnabled(ctx, "g1", created.ID, false); a.Enabled {
		t.Error("expected disabled")
	}

	// Delete.
	if err := m.Delete(ctx, "g1", created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := m.Get(ctx, "g1", created.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestManager_Runs(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	a, err := m.Create(ctx, Agent{Name: "a", GroupID: "g1", TalonSource: "workflow \"x\" {}"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	run, err := m.CreateRun(ctx, Run{AgentID: a.ID, TriggerType: "llm", Status: StatusRunning})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run.Status = StatusCompleted
	run.Result = []byte(`{"blocks":{}}`)
	if err := m.FinishRun(ctx, run); err != nil {
		t.Fatalf("finish run: %v", err)
	}

	runs, err := m.ListRuns(ctx, a.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != StatusCompleted {
		t.Errorf("unexpected runs: %+v", runs)
	}
}
