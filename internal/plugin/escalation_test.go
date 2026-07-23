package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
	"github.com/opentalon/opentalon-agents/internal/store"
)

// escHost scripts a poll (get-item), talon-plugin.evaluate, and the host's
// _escalate.turn entrypoint, capturing every turn call so tests can assert the
// escalation args.
type escHost struct {
	stock       []float64
	pollN       int
	evalResp    []string
	evalN       int
	turns       []map[string]string
	turnOutcome string // structured reply for "turn"; default {"escalated":true}
}

func (h *escHost) RunAction(_ context.Context, _, action string, args map[string]string) (pkg.CallResult, error) {
	switch action {
	case "get-item":
		v := h.stock[min(h.pollN, len(h.stock)-1)]
		h.pollN++
		return pkg.CallResult{StructuredContent: fmt.Sprintf(`{"item":{"barcode":"ABC-123","current_stock":%v}}`, v)}, nil
	case "evaluate":
		r := h.evalResp[min(h.evalN, len(h.evalResp)-1)]
		h.evalN++
		return pkg.CallResult{StructuredContent: r}, nil
	case escalateTurnAction:
		h.turns = append(h.turns, args)
		out := h.turnOutcome
		if out == "" {
			out = `{"escalated":true}`
		}
		return pkg.CallResult{StructuredContent: out}, nil
	}
	return pkg.CallResult{}, nil
}

func escEngineFixture(t *testing.T) (*Engine, *agent.Manager) {
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

// escWatcher creates a stock watcher owned by ent-1/g1.
func escWatcher(t *testing.T, mgr *agent.Manager) agent.Agent {
	t.Helper()
	pc := `{"server":"inventory","tool":"get-item","args":{"barcode":"ABC-123"},"interval":"5m","value_path":"item.current_stock","id_field":"item.barcode","attribute":"current_stock"}`
	a, err := mgr.Create(context.Background(), agent.Agent{
		Name: "restock", GroupID: "g1", EntityID: "ent-1", Enabled: true,
		Description: "watch ABC-123 stock and tell me when it's low",
		TalonSource: `on change attr "current_stock" { when prev_value >= 10 and new_value < 10 workflow "Refill stock" }`,
		Triggers:    []agent.Trigger{{Type: agent.TriggerPoll, Config: json.RawMessage(pc)}},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return a
}

// crossingHost returns a host that observes 15 then 8 (a downward crossing on
// the second tick), firing the on-block once.
func crossingHost(turnOutcome string) *escHost {
	return &escHost{
		stock: []float64{15, 8},
		evalResp: []string{
			`{"ok":true,"firings":[],"snapshot":{"1":{"current_stock":15}}}`,
			`{"ok":true,"firings":[{"on_block":"on change attr \"current_stock\"","ref":"Refill stock","ref_kind":"workflow"}],"snapshot":{"1":{"current_stock":8}}}`,
		},
		turnOutcome: turnOutcome,
	}
}

func fireCrossing(t *testing.T, e *Engine, host *escHost) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	if _, err := e.tickAt(ctx, host, t0); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if _, err := e.tickAt(ctx, host, t0.Add(6*time.Minute)); err != nil {
		t.Fatalf("tick2: %v", err)
	}
}

func TestEngine_EscalatesOnFireWithProvenance(t *testing.T) {
	ctx := context.Background()
	e, mgr := escEngineFixture(t)
	a := escWatcher(t, mgr)
	if err := mgr.SaveEscalation(ctx, a.ID, "sess-1", agent.EscalationSpec{Enabled: true}); err != nil {
		t.Fatalf("save escalation: %v", err)
	}
	host := crossingHost("")
	fireCrossing(t, e, host)

	if len(host.turns) != 1 {
		t.Fatalf("expected exactly one escalation turn, got %d", len(host.turns))
	}
	got := host.turns[0]
	if got["session_id"] != "sess-1" {
		t.Errorf("session_id = %q, want sess-1", got["session_id"])
	}
	if got["entity_id"] != "ent-1" || got["group_id"] != "g1" {
		t.Errorf("entity/group = %q/%q, want ent-1/g1", got["entity_id"], got["group_id"])
	}
	if got["source"] != "agent" || got["agent_id"] != a.ID || got["trigger"] != agent.TriggerPoll {
		t.Errorf("provenance = source:%q agent_id:%q trigger:%q", got["source"], got["agent_id"], got["trigger"])
	}
	if !strings.Contains(got["prompt"], a.Description) {
		t.Errorf("prompt should weave in the user's original ask; got:\n%s", got["prompt"])
	}
	if !strings.Contains(got["prompt"], "current_stock") {
		t.Errorf("prompt should include the observed facts; got:\n%s", got["prompt"])
	}

	esc, found, _ := mgr.GetEscalation(ctx, a.ID)
	if !found || esc.FireCount != 1 {
		t.Errorf("fire_count = %d (found=%v), want 1", esc.FireCount, found)
	}
}

func TestEngine_NoEscalationWhenNotOptedIn(t *testing.T) {
	e, mgr := escEngineFixture(t)
	_ = escWatcher(t, mgr) // no SaveEscalation → escalation off
	host := crossingHost("")
	fireCrossing(t, e, host)
	if len(host.turns) != 0 {
		t.Fatalf("expected no escalation for a non-opted-in agent, got %d", len(host.turns))
	}
}

func TestEngine_EscalationRefusedDoesNotCount(t *testing.T) {
	ctx := context.Background()
	e, mgr := escEngineFixture(t)
	a := escWatcher(t, mgr)
	if err := mgr.SaveEscalation(ctx, a.ID, "sess-1", agent.EscalationSpec{Enabled: true}); err != nil {
		t.Fatalf("save escalation: %v", err)
	}
	// Host reports the turn was refused (e.g. host escalation disabled).
	host := crossingHost(`{"escalated":false,"reason":"disabled"}`)
	fireCrossing(t, e, host)

	if len(host.turns) != 1 {
		t.Fatalf("expected the turn to be attempted once, got %d", len(host.turns))
	}
	esc, _, _ := mgr.GetEscalation(ctx, a.ID)
	if esc.FireCount != 0 {
		t.Errorf("a refused escalation must not count against the limit; fire_count = %d", esc.FireCount)
	}
}

func TestRateLimit(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	window := time.Hour

	// First ever: no window → allowed, window starts now, count reset to 0.
	count, ws, ok := rateLimit(0, nil, 2, window, now)
	if !ok || count != 0 || ws == nil || !ws.Equal(now) {
		t.Fatalf("first call: count=%d ws=%v ok=%v", count, ws, ok)
	}

	// Within window, under the cap → allowed, window unchanged.
	start := now
	count, ws, ok = rateLimit(1, &start, 2, window, now.Add(10*time.Minute))
	if !ok || count != 1 || !ws.Equal(start) {
		t.Fatalf("under cap: count=%d ok=%v", count, ok)
	}

	// Within window, at the cap → refused.
	if _, _, ok := rateLimit(2, &start, 2, window, now.Add(20*time.Minute)); ok {
		t.Fatal("at cap: expected refusal")
	}

	// Window elapsed → resets and allows.
	count, ws, ok = rateLimit(2, &start, 2, window, now.Add(2*time.Hour))
	if !ok || count != 0 || !ws.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("after window: count=%d ok=%v", count, ok)
	}
}

func TestSynthesizeEscalationPrompt_TemplateOverride(t *testing.T) {
	a := agent.Agent{Name: "restock", Description: "watch stock"}
	esc := agent.Escalation{PromptTemplate: "AGENT={{agent_name}} ASK={{description}} FACTS={{facts}}"}
	got := synthesizeEscalationPrompt(a, esc, json.RawMessage(`[{"attribute":"current_stock"}]`), nil)
	want := `AGENT=restock ASK=watch stock FACTS=[{"attribute":"current_stock"}]`
	if got != want {
		t.Errorf("template render = %q, want %q", got, want)
	}
}
