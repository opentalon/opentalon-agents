package plugin

import (
	"context"
	"strings"
	"testing"

	pkg "github.com/opentalon/opentalon/pkg/plugin"
)

func TestTickAction_DispatchesToEngineUnscoped(t *testing.T) {
	ctx := context.Background()
	h := testHandler(t)
	watcherAgent(t, h.mgr) // one enabled, poll-triggered, due agent

	host := &engineHost{
		stock:    []float64{15},
		evalResp: []string{`{"ok":true,"firings":[],"snapshot":{"1":{"current_stock":15}}}`},
	}
	// No group_id in args — tick is a system-wide, unscoped action and must
	// still run (the group gate applies only to LLM-facing actions).
	resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "t1", Action: "tick"}, host)
	if resp.Error != "" {
		t.Fatalf("tick should not error: %q", resp.Error)
	}
	if host.pollN == 0 {
		t.Error("tick should have polled the due agent")
	}
	if !strings.Contains(resp.StructuredContent, `"agents":1`) {
		t.Errorf("tick summary should report the swept agent: %q", resp.StructuredContent)
	}
}

func TestTickAction_HiddenFromLLM(t *testing.T) {
	h := testHandler(t)
	for _, a := range h.Capabilities().Actions {
		if a.Name == "tick" {
			if !a.UserOnly {
				t.Error("tick must be UserOnly so it is hidden from the LLM")
			}
			return
		}
	}
	t.Error("tick action should be registered (so the scheduler can dispatch it)")
}
