package plugin

import (
	"context"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

// schedHost records execute_workflow calls (what a schedule agent runs).
type schedHost struct{ execCalls int }

func (h *schedHost) RunAction(_ context.Context, _, action string, _ map[string]string) (pkg.CallResult, error) {
	if action == "execute_workflow" {
		h.execCalls++
		return pkg.CallResult{Content: "Workflow completed.", StructuredContent: `{"blocks":{}}`}, nil
	}
	return pkg.CallResult{}, nil
}

func cronAgent(t *testing.T, mgr *agent.Manager) agent.Agent {
	t.Helper()
	a, err := mgr.Create(context.Background(), agent.Agent{
		Name: "report", GroupID: "g1", Enabled: true,
		TalonSource: `workflow "daily" { step "s" { mcp "reports" "run" { } } }`,
		Triggers:    []agent.Trigger{{Type: agent.TriggerSchedule, Cron: "*/5 * * * *"}},
	})
	if err != nil {
		t.Fatalf("create cron agent: %v", err)
	}
	return a
}

func TestEngine_Schedule_InitThenRun(t *testing.T) {
	ctx := context.Background()
	e, mgr := engineFixture(t)
	a := cronAgent(t, mgr)
	host := &schedHost{}
	t0 := time.Date(2026, 7, 14, 12, 1, 0, 0, time.UTC)

	// First tick: initialize next_cron_at, do NOT run (cron = "at those times", not now).
	if _, err := e.tickAt(ctx, host, t0); err != nil {
		t.Fatalf("init tick: %v", err)
	}
	if host.execCalls != 0 || runCount(t, mgr, a.ID) != 0 {
		t.Fatalf("first sight should not run: execCalls=%d runs=%d", host.execCalls, runCount(t, mgr, a.ID))
	}
	st, _ := mgr.GetState(ctx, a.ID)
	if st.NextCronAt == nil {
		t.Fatal("first sight should set next_cron_at")
	}
	firstNext := *st.NextCronAt

	// Tick at the scheduled time: runs the workflow once and reschedules.
	if _, err := e.tickAt(ctx, host, firstNext.Add(time.Second)); err != nil {
		t.Fatalf("due tick: %v", err)
	}
	if host.execCalls != 1 {
		t.Errorf("expected one execute_workflow, got %d", host.execCalls)
	}
	runs, _ := mgr.ListRuns(ctx, a.ID, 10)
	if len(runs) != 1 || runs[0].TriggerType != agent.TriggerSchedule || runs[0].Status != agent.StatusCompleted {
		t.Errorf("expected one completed schedule run, got %+v", runs)
	}
	st2, _ := mgr.GetState(ctx, a.ID)
	if st2.NextCronAt == nil || !st2.NextCronAt.After(firstNext) {
		t.Errorf("next_cron_at should advance past %v, got %v", firstNext, st2.NextCronAt)
	}
}
