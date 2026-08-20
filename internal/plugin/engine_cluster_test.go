package plugin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
	"github.com/opentalon/opentalon-agents/internal/store"
)

// TestEngine_ClusterNoDoublePollFire simulates two plugin instances sharing one
// database (each with its own *sql.DB pool over the same SQLite file, as two
// processes would) sweeping the same due poll tick concurrently. The atomic
// ClaimPollDue must let exactly one instance process the agent, so a single
// downward crossing records exactly one run — never two. Run with -race.
func TestEngine_ClusterNoDoublePollFire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster.db")
	open := func() (*Engine, *agent.Manager) {
		t.Helper()
		db, err := store.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		cfg, _ := config.Parse("")
		mgr := agent.NewManager(db)
		return NewEngine(cfg, mgr), mgr
	}

	eA, mgrA := open()
	a := watcherAgent(t, mgrA)
	t0 := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	// Establish the baseline (stock 15, first observation, no fire) on one
	// instance so the concurrent tick below is a genuine 15→8 crossing.
	establish := &engineHost{
		stock:    []float64{15},
		evalResp: []string{`{"ok":true,"firings":[],"snapshot":{"1":{"current_stock":15}}}`},
	}
	if _, err := eA.tickAt(context.Background(), establish, t0); err != nil {
		t.Fatalf("establish tick: %v", err)
	}

	// Two instances (separate pools over the same file) sweep the same due tick
	// at once. Each gets its own host so the claim — not a shared counter — is
	// what serialises access; only the winner ever reaches the poll/evaluate.
	eB, _ := open()
	crossResp := `{"ok":true,"firings":[{"on_block":"on change attr \"current_stock\"","ref":"Refill stock","ref_kind":"workflow"}],"snapshot":{"1":{"current_stock":8}}}`
	mkHost := func() *engineHost {
		return &engineHost{stock: []float64{8}, evalResp: []string{crossResp}}
	}
	due := t0.Add(6 * time.Minute)

	var wg sync.WaitGroup
	for _, e := range []*Engine{eA, eB} {
		e := e
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.tickAt(context.Background(), mkHost(), due)
		}()
	}
	wg.Wait()

	if got := runCount(t, mgrA, a.ID); got != 1 {
		t.Fatalf("cluster double-fire: expected exactly 1 run, got %d", got)
	}
}

// TestEngine_ClusterNoDoubleScheduleFire is the cron analogue: two instances
// sweep the same due cron fire concurrently; ClaimScheduleDue must let only one
// run the workflow.
func TestEngine_ClusterNoDoubleScheduleFire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster-cron.db")
	open := func() (*Engine, *agent.Manager) {
		t.Helper()
		db, err := store.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		cfg, _ := config.Parse("")
		mgr := agent.NewManager(db)
		return NewEngine(cfg, mgr), mgr
	}

	eA, mgrA := open()
	a := cronAgent(t, mgrA)
	t0 := time.Date(2026, 7, 14, 12, 1, 0, 0, time.UTC)

	// First-sight initialisation (no run) on one instance.
	if _, err := eA.tickAt(context.Background(), &schedHost{}, t0); err != nil {
		t.Fatalf("init tick: %v", err)
	}
	st, _ := mgrA.GetState(context.Background(), a.ID)
	if st.NextCronAt == nil {
		t.Fatal("first sight should set next_cron_at")
	}
	due := st.NextCronAt.Add(time.Second)

	eB, _ := open()
	var wg sync.WaitGroup
	hosts := []*schedHost{{}, {}}
	for i, e := range []*Engine{eA, eB} {
		e, h := e, hosts[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.tickAt(context.Background(), h, due)
		}()
	}
	wg.Wait()

	total := hosts[0].execCalls + hosts[1].execCalls
	if total != 1 {
		t.Fatalf("cluster double-fire: expected exactly 1 execute_workflow across instances, got %d", total)
	}
	if got := runCount(t, mgrA, a.ID); got != 1 {
		t.Fatalf("expected exactly 1 run, got %d", got)
	}
}

// TestEngine_ClusterNoDoubleEventDrain simulates two instances draining the
// same queued event concurrently; ClaimEvent (delete-to-claim) must let only
// one process it.
func TestEngine_ClusterNoDoubleEventDrain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cluster-event.db")
	open := func() (*Engine, *agent.Manager) {
		t.Helper()
		db, err := store.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		cfg, _ := config.Parse("")
		mgr := agent.NewManager(db)
		return NewEngine(cfg, mgr), mgr
	}

	eA, mgrA := open()
	// An event-triggered (workflow) agent: EventKindRun runs the workflow.
	a, err := mgrA.Create(context.Background(), agent.Agent{
		Name: "onevent", GroupID: "g1", Enabled: true,
		TlnSource: `workflow "w" { step "s" { mcp "x" "y" { } } }`,
		Triggers:  []agent.Trigger{{Type: agent.TriggerEvent, Config: json.RawMessage(`{"event":"thing"}`)}},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := mgrA.EnqueueEvent(context.Background(), agent.PendingEvent{
		AgentID: a.ID, Kind: agent.EventKindRun, Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	eB, _ := open()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for _, e := range []*Engine{eA, eB} {
		e := e
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.tickAt(context.Background(), &schedHost{}, now)
		}()
	}
	wg.Wait()

	if got := runCount(t, mgrA, a.ID); got != 1 {
		t.Fatalf("cluster double-drain: expected exactly 1 run, got %d", got)
	}
}
