package agent

import (
	"context"
	"testing"
)

func TestQueryAgents_Filters(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	src := `workflow "x" {}`
	mustCreate(t, m, Agent{Name: "restock-a", GroupID: "g1", EntityID: "u1", TalonSource: src, Enabled: true})
	mustCreate(t, m, Agent{Name: "restock-b", GroupID: "g1", EntityID: "u2", TalonSource: src, Enabled: true})
	mustCreate(t, m, Agent{Name: "alerts", GroupID: "g2", EntityID: "u1", TalonSource: src, Enabled: false})

	names := func(f AgentFilter) map[string]bool {
		got, err := m.QueryAgents(ctx, f)
		if err != nil {
			t.Fatalf("query %+v: %v", f, err)
		}
		out := map[string]bool{}
		for _, a := range got {
			out[a.Name] = true
		}
		return out
	}

	if n := names(AgentFilter{}); len(n) != 3 {
		t.Errorf("no filter → all, got %v", n)
	}
	if n := names(AgentFilter{GroupID: "g1"}); !n["restock-a"] || !n["restock-b"] || n["alerts"] {
		t.Errorf("by group: %v", n)
	}
	if n := names(AgentFilter{EntityID: "u1"}); !n["restock-a"] || !n["alerts"] || n["restock-b"] {
		t.Errorf("by entity: %v", n)
	}
	if n := names(AgentFilter{NameContains: "REST"}); !n["restock-a"] || !n["restock-b"] || n["alerts"] {
		t.Errorf("name substring (case-insensitive): %v", n)
	}
	if n := names(AgentFilter{EntityID: "u1", NameContains: "alert"}); len(n) != 1 || !n["alerts"] {
		t.Errorf("combined entity+name: %v", n)
	}
	disabled := false
	if n := names(AgentFilter{Enabled: &disabled}); len(n) != 1 || !n["alerts"] {
		t.Errorf("by enabled=false: %v", n)
	}
}
