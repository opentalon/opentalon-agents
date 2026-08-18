package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	pkg "github.com/opentalon/opentalon/pkg/plugin"
)

// evalHost scripts tln-plugin.evaluate and records the args it received.
type evalHost struct {
	lastArgs map[string]string
	result   string
	err      error
}

func (h *evalHost) RunAction(_ context.Context, _, action string, args map[string]string) (pkg.CallResult, error) {
	if action == "evaluate" {
		h.lastArgs = args
		if h.err != nil {
			return pkg.CallResult{}, h.err
		}
		return pkg.CallResult{StructuredContent: h.result}, nil
	}
	return pkg.CallResult{}, nil
}

func TestEvaluate_ParsesFiringsAndSnapshotAndForwardsArgs(t *testing.T) {
	host := &evalHost{result: `{"ok":true,"firings":[{"on_block":"on change attr \"current_stock\"","ref":"Refill stock","ref_kind":"workflow"}],"snapshot":{"1":{"current_stock":8}}}`}
	p := tlnProxy{pluginName: "tln-plugin"}

	facts := json.RawMessage(`[{"record_id":"1","attribute":"current_stock","value":8}]`)
	snap := json.RawMessage(`{"1":{"current_stock":15}}`)
	res, err := p.Evaluate(context.Background(), host, "SRC", facts, snap, Identity{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(res.Firings) != 1 || res.Firings[0].Ref != "Refill stock" || res.Firings[0].RefKind != "workflow" {
		t.Errorf("firings: %+v", res.Firings)
	}
	if string(res.Snapshot) != `{"1":{"current_stock":8}}` {
		t.Errorf("snapshot: %s", res.Snapshot)
	}
	// Args forwarded verbatim.
	if host.lastArgs["source"] != "SRC" || host.lastArgs["facts"] != string(facts) || host.lastArgs["snapshot"] != string(snap) {
		t.Errorf("args not forwarded: %+v", host.lastArgs)
	}
}

func TestEvaluate_DefaultsEmptyFacts(t *testing.T) {
	host := &evalHost{result: `{"ok":true,"firings":[],"snapshot":{}}`}
	p := tlnProxy{pluginName: "tln-plugin"}
	if _, err := p.Evaluate(context.Background(), host, "SRC", nil, nil, Identity{}); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if host.lastArgs["facts"] != "[]" {
		t.Errorf("empty facts should default to []: %q", host.lastArgs["facts"])
	}
	if _, ok := host.lastArgs["snapshot"]; ok {
		t.Errorf("empty snapshot should be omitted, got %q", host.lastArgs["snapshot"])
	}
}

func TestEvaluate_HostError(t *testing.T) {
	host := &evalHost{err: errors.New("tln-plugin not loaded")}
	p := tlnProxy{pluginName: "tln-plugin"}
	if _, err := p.Evaluate(context.Background(), host, "SRC", nil, nil, Identity{}); err == nil {
		t.Error("expected error when the evaluate action fails")
	}
}
