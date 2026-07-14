package agent

import (
	"context"
	"testing"
)

func TestManager_DuplicateNameRejected(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	base := Agent{Name: "dup", GroupID: "g1", TalonSource: `workflow "x" {}`}
	if _, err := m.Create(ctx, base); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := m.Create(ctx, base); err == nil {
		t.Error("expected duplicate name in same group to be rejected by the unique index")
	}
	// Same name in a different group is fine.
	other := base
	other.GroupID = "g2"
	if _, err := m.Create(ctx, other); err != nil {
		t.Errorf("same name in different group should be allowed: %v", err)
	}
}

func TestManager_MutationsNotFound(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	if _, err := m.Update(ctx, "g1", "nope", "workflow \"y\" {}", nil); err != ErrNotFound {
		t.Errorf("update missing: got %v", err)
	}
	if _, err := m.SetEnabled(ctx, "g1", "nope", false); err != ErrNotFound {
		t.Errorf("set-enabled missing: got %v", err)
	}
	if err := m.Delete(ctx, "g1", "nope"); err != ErrNotFound {
		t.Errorf("delete missing: got %v", err)
	}
}

func TestManager_FailedRunPersistsError(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	a, _ := m.Create(ctx, Agent{Name: "a", GroupID: "g1", TalonSource: `workflow "x" {}`})

	run, err := m.CreateRun(ctx, Run{AgentID: a.ID, TriggerType: "llm", Status: StatusRunning})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	run.Status = StatusFailed
	run.Error = "boom"
	if err := m.FinishRun(ctx, run); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	runs, _ := m.ListRuns(ctx, a.ID, 10)
	if len(runs) != 1 || runs[0].Status != StatusFailed || runs[0].Error != "boom" {
		t.Errorf("failed run not persisted: %+v", runs)
	}
}

func TestManager_ListRunsLimit(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	a, _ := m.Create(ctx, Agent{Name: "a", GroupID: "g1", TalonSource: `workflow "x" {}`})
	for i := 0; i < 5; i++ {
		if _, err := m.CreateRun(ctx, Run{AgentID: a.ID, TriggerType: "llm", Status: StatusCompleted}); err != nil {
			t.Fatalf("create run %d: %v", i, err)
		}
	}
	runs, err := m.ListRuns(ctx, a.ID, 3)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(runs) != 3 {
		t.Errorf("limit not applied: got %d", len(runs))
	}
}
