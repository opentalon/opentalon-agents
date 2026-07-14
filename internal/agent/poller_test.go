package agent

import (
	"context"
	"errors"
	"testing"

	pkg "github.com/opentalon/opentalon/pkg/plugin"
)

// pollHost is a fake pkg.HostCaller for poller tests.
type pollHost struct {
	lastServer, lastTool string
	lastArgs             map[string]string
	res                  pkg.CallResult
	err                  error
}

func (h *pollHost) RunAction(_ context.Context, server, tool string, args map[string]string) (pkg.CallResult, error) {
	h.lastServer, h.lastTool, h.lastArgs = server, tool, args
	return h.res, h.err
}

func inventoryPoll() PollConfig {
	return PollConfig{Server: "inventory", Tool: "get-item", Args: map[string]string{"barcode": "ABC-123"}}
}

func TestPoll_DecodesStructuredContentAndForwardsCall(t *testing.T) {
	host := &pollHost{res: pkg.CallResult{StructuredContent: `{"item":{"barcode":"ABC-123","current_stock":8}}`}}
	got, err := Poll(context.Background(), host, inventoryPoll())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// Call routed to the right server/tool with the trigger args.
	if host.lastServer != "inventory" || host.lastTool != "get-item" || host.lastArgs["barcode"] != "ABC-123" {
		t.Errorf("call not forwarded: %s.%s %v", host.lastServer, host.lastTool, host.lastArgs)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected object, got %T", got)
	}
	item := m["item"].(map[string]any)
	if item["current_stock"] != float64(8) {
		t.Errorf("decoded stock: %v", item["current_stock"])
	}
}

func TestPoll_FallsBackToContent(t *testing.T) {
	// No structured content — decode Content JSON.
	host := &pollHost{res: pkg.CallResult{Content: `{"current_stock":3}`}}
	got, err := Poll(context.Background(), host, inventoryPoll())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.(map[string]any)["current_stock"] != float64(3) {
		t.Errorf("content decode: %+v", got)
	}
}

func TestPoll_NonJSONContentWrappedAsText(t *testing.T) {
	host := &pollHost{res: pkg.CallResult{Content: "not json"}}
	got, err := Poll(context.Background(), host, inventoryPoll())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if got.(map[string]any)["text"] != "not json" {
		t.Errorf("expected text wrapper, got %+v", got)
	}
}

func TestPoll_EmptyResponse(t *testing.T) {
	host := &pollHost{res: pkg.CallResult{}}
	got, err := Poll(context.Background(), host, inventoryPoll())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if m, ok := got.(map[string]any); !ok || len(m) != 0 {
		t.Errorf("empty response should decode to empty object, got %+v", got)
	}
}

func TestPoll_HostError(t *testing.T) {
	host := &pollHost{err: errors.New("mcp down")}
	if _, err := Poll(context.Background(), host, inventoryPoll()); err == nil {
		t.Error("expected error when the MCP call fails")
	}
}
