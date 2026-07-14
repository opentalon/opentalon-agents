package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
	"github.com/opentalon/opentalon-agents/internal/store"
)

// engineHost fakes both sides of a tick: the inventory poll (get-item)
// returns the next scripted stock value; talon-plugin.evaluate returns the
// next scripted result and records the args it received (so tests can
// assert the prior snapshot was forwarded).
type engineHost struct {
	stock    []float64
	pollN    int
	pollErr  error
	evalResp []string
	evalN    int
	evalArgs []map[string]string
}

func (h *engineHost) RunAction(_ context.Context, _, action string, args map[string]string) (pkg.CallResult, error) {
	switch action {
	case "get-item":
		if h.pollErr != nil {
			return pkg.CallResult{}, h.pollErr
		}
		v := h.stock[min(h.pollN, len(h.stock)-1)]
		h.pollN++
		return pkg.CallResult{StructuredContent: fmt.Sprintf(`{"item":{"barcode":"ABC-123","current_stock":%v}}`, v)}, nil
	case "evaluate":
		h.evalArgs = append(h.evalArgs, args)
		r := h.evalResp[min(h.evalN, len(h.evalResp)-1)]
		h.evalN++
		return pkg.CallResult{StructuredContent: r}, nil
	}
	return pkg.CallResult{}, nil
}

func engineFixture(t *testing.T) (*Engine, *agent.Manager) {
	t.Helper()
	cfg, _ := config.Parse("")
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mgr := agent.NewManager(db)
	return NewEngine(cfg, mgr), mgr
}

func watcherAgent(t *testing.T, mgr *agent.Manager) agent.Agent {
	t.Helper()
	pc := `{"server":"inventory","tool":"get-item","args":{"barcode":"ABC-123"},"interval":"5m","value_path":"item.current_stock","id_field":"item.barcode","attribute":"current_stock"}`
	a, err := mgr.Create(context.Background(), agent.Agent{
		Name: "restock", GroupID: "g1", Enabled: true,
		TalonSource: `on change attr "current_stock" { when prev_value >= 10 and new_value < 10 workflow "Refill stock" }`,
		Triggers:    []agent.Trigger{{Type: agent.TriggerPoll, Config: json.RawMessage(pc)}},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return a
}

func runCount(t *testing.T, mgr *agent.Manager, agentID string) int {
	t.Helper()
	runs, err := mgr.ListRuns(context.Background(), agentID, 100)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	return len(runs)
}

func TestEngine_NoFireThenCrossingRecordsOneRun(t *testing.T) {
	ctx := context.Background()
	e, mgr := engineFixture(t)
	a := watcherAgent(t, mgr)
	host := &engineHost{
		stock: []float64{15, 8},
		evalResp: []string{
			`{"ok":true,"firings":[],"snapshot":{"1":{"current_stock":15}}}`,
			`{"ok":true,"firings":[{"on_block":"on change attr \"current_stock\"","ref":"Refill stock","ref_kind":"workflow"}],"snapshot":{"1":{"current_stock":8}}}`,
		},
	}
	t0 := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	// Tick 1: stock 15 (first observation) → no firing, no run, but state advances.
	r1, err := e.tickAt(ctx, host, t0)
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if r1.Firings != 0 || runCount(t, mgr, a.ID) != 0 {
		t.Fatalf("tick1 should not fire or record a run: %+v, runs=%d", r1, runCount(t, mgr, a.ID))
	}
	st, _ := mgr.GetState(ctx, a.ID)
	if st.NextPollAt == nil {
		t.Fatal("tick1 should schedule next poll")
	}
	if string(st.FactsSnapshot) != `{"1":{"current_stock":15}}` {
		t.Errorf("tick1 snapshot: %s", st.FactsSnapshot)
	}

	// Not due yet (before next_poll_at).
	if r, _ := e.tickAt(ctx, host, t0.Add(time.Minute)); r.Agents != 0 {
		t.Errorf("agent should not be due 1m later, got %+v", r)
	}

	// Tick 2 (after interval): stock 8 → downward crossing → one firing + one run.
	r2, err := e.tickAt(ctx, host, t0.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if r2.Firings != 1 {
		t.Fatalf("tick2 should fire once, got %+v", r2)
	}
	if runCount(t, mgr, a.ID) != 1 {
		t.Errorf("expected exactly one run after the crossing, got %d", runCount(t, mgr, a.ID))
	}
	// The engine forwarded tick 1's snapshot into tick 2's evaluate.
	if got := host.evalArgs[1]["snapshot"]; got != `{"1":{"current_stock":15}}` {
		t.Errorf("tick2 evaluate should receive the prior snapshot, got %q", got)
	}
	st, _ = mgr.GetState(ctx, a.ID)
	if string(st.FactsSnapshot) != `{"1":{"current_stock":8}}` {
		t.Errorf("tick2 snapshot should update to 8: %s", st.FactsSnapshot)
	}
}

func TestEngine_PollFailureBacksOffAndRecordsFailedRun(t *testing.T) {
	ctx := context.Background()
	e, mgr := engineFixture(t)
	a := watcherAgent(t, mgr)
	host := &engineHost{pollErr: errors.New("mcp down")}
	t0 := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	r, err := e.tickAt(ctx, host, t0)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if r.Errors != 1 {
		t.Errorf("expected one errored agent, got %+v", r)
	}
	runs, _ := mgr.ListRuns(ctx, a.ID, 10)
	if len(runs) != 1 || runs[0].Status != agent.StatusFailed || runs[0].Error == "" {
		t.Errorf("expected one failed run with an error, got %+v", runs)
	}
	st, _ := mgr.GetState(ctx, a.ID)
	if st.ConsecutiveFailures != 1 || st.NextPollAt == nil {
		t.Errorf("failure should bump backoff + reschedule: %+v", st)
	}
}

func TestEngine_RestartDoesNotRefire(t *testing.T) {
	ctx := context.Background()
	e, mgr := engineFixture(t)
	a := watcherAgent(t, mgr)

	// Prime state as if a prior run left the item at 8 (already below).
	if err := mgr.SaveState(ctx, agent.AgentState{
		AgentID: a.ID, FactsSnapshot: json.RawMessage(`{"1":{"current_stock":8}}`),
		EntityMap: map[string]int{"ABC-123": 1},
	}); err != nil {
		t.Fatalf("prime state: %v", err)
	}
	// After "restart", polling 8 again is unchanged → evaluate fires nothing.
	host := &engineHost{
		stock:    []float64{8},
		evalResp: []string{`{"ok":true,"firings":[],"snapshot":{"1":{"current_stock":8}}}`},
	}
	r, err := e.tickAt(ctx, host, time.Date(2026, 7, 14, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if r.Firings != 0 || runCount(t, mgr, a.ID) != 0 {
		t.Errorf("replay of the known value must not refire: %+v runs=%d", r, runCount(t, mgr, a.ID))
	}
	// It forwarded the persisted snapshot to evaluate.
	if host.evalArgs[0]["snapshot"] != `{"1":{"current_stock":8}}` {
		t.Errorf("restart should hydrate from persisted snapshot, got %q", host.evalArgs[0]["snapshot"])
	}
}
