package plugin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

// simHost is a higher-fidelity fake than engineHost: its `evaluate`
// actually simulates Talon's crossing semantics (fire when prev >= 10 and
// new < 10, based on the snapshot it is given), and it records a "ticket"
// each time the fired workflow would run. This lets the test exercise the
// engine's real per-tick state round-trip end to end. `stock` is what the
// next poll returns; the test moves it between ticks.
type simHost struct {
	stock   float64
	tickets int
}

func (h *simHost) RunAction(_ context.Context, _, action string, args map[string]string) (pkg.CallResult, error) {
	switch action {
	case "get-item":
		b, _ := json.Marshal(map[string]any{"item": map[string]any{"barcode": "ABC-123", "current_stock": h.stock}})
		return pkg.CallResult{StructuredContent: string(b)}, nil

	case "evaluate":
		var facts []struct {
			RecordID string  `json:"record_id"`
			Value    float64 `json:"value"`
		}
		_ = json.Unmarshal([]byte(args["facts"]), &facts)

		snap := map[string]map[string]float64{}
		if s := args["snapshot"]; s != "" {
			_ = json.Unmarshal([]byte(s), &snap)
		}

		type firing struct {
			OnBlock string `json:"on_block"`
			Ref     string `json:"ref"`
			RefKind string `json:"ref_kind"`
		}
		var firings []firing
		for _, f := range facts {
			prev, had := snap[f.RecordID]["current_stock"]
			if had && prev >= 10 && f.Value < 10 { // downward crossing → fire once
				firings = append(firings, firing{OnBlock: `on change attr "current_stock"`, Ref: "Refill stock", RefKind: "workflow"})
				h.tickets++ // the fired workflow's mcp "tickets" "create" step
			}
			if snap[f.RecordID] == nil {
				snap[f.RecordID] = map[string]float64{}
			}
			snap[f.RecordID]["current_stock"] = f.Value
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "firings": firings, "snapshot": snap})
		return pkg.CallResult{StructuredContent: string(b)}, nil
	}
	return pkg.CallResult{}, nil
}

// TestE2E_StockWatcherAcceptance runs the full acceptance sequence from
// issue #1 against the real engine + a simulating host: only a genuine
// downward crossing opens a ticket, it fires once, survives a restart, and
// re-fires on a fresh crossing.
func TestE2E_StockWatcherAcceptance(t *testing.T) {
	e, mgr := engineFixture(t)
	a := watcherAgent(t, mgr) // enabled, poll trigger, interval 5m
	host := &simHost{}
	at := func(min int) time.Time {
		return time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
	}

	// 1) stock 15 (first observation) → establishes, no fire, no run, no ticket.
	host.stock = 15
	mustTick(t, e, host, at(0))
	assertCounts(t, mgr, a.ID, host, 0, 0)

	// 2) stock 8 → downward crossing → one run + one ticket.
	host.stock = 8
	mustTick(t, e, host, at(6))
	assertCounts(t, mgr, a.ID, host, 1, 1)

	// 3) stock 8 again (unchanged) → no new run, no new ticket.
	mustTick(t, e, host, at(12))
	assertCounts(t, mgr, a.ID, host, 1, 1)

	// 4) restart (fresh engine, same DB) → replay of the known value → no re-fire.
	e2 := NewEngine(e.cfg, mgr)
	mustTick(t, e2, host, at(18))
	assertCounts(t, mgr, a.ID, host, 1, 1)

	// 5) recover above threshold, then cross again → fires a second time.
	host.stock = 15
	mustTick(t, e2, host, at(24)) // 8→15, upward, no fire
	assertCounts(t, mgr, a.ID, host, 1, 1)
	host.stock = 7
	mustTick(t, e2, host, at(30)) // 15→7, downward crossing → fire
	assertCounts(t, mgr, a.ID, host, 2, 2)
}

func mustTick(t *testing.T, e *Engine, host pkg.HostCaller, now time.Time) {
	t.Helper()
	if _, err := e.tickAt(context.Background(), host, now); err != nil {
		t.Fatalf("tick @ %s: %v", now, err)
	}
}

func assertCounts(t *testing.T, mgr *agent.Manager, agentID string, host *simHost, wantRuns, wantTickets int) {
	t.Helper()
	if got := runCount(t, mgr, agentID); got != wantRuns {
		t.Errorf("runs: got %d, want %d", got, wantRuns)
	}
	if host.tickets != wantTickets {
		t.Errorf("tickets created: got %d, want %d", host.tickets, wantTickets)
	}
}
