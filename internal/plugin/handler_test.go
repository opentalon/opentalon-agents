package plugin

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
	"github.com/opentalon/opentalon-agents/internal/store"
)

// fakeHost implements pkg.HostCaller with scripted talon-plugin replies.
// check(): invalid when the source contains "INVALID"; valid otherwise.
// execute_workflow(): records the call and returns a canned result.
type fakeHost struct {
	execCalls []map[string]string
}

func (f *fakeHost) RunAction(_ context.Context, plugin, action string, args map[string]string) (pkg.CallResult, error) {
	switch action {
	case "check":
		if strings.Contains(args["workflow"], "INVALID") {
			return pkg.CallResult{
				Content:           "talon: parse: 1 diagnostic(s)\n  unexpected token",
				StructuredContent: `{"ok":false,"stage":"parse"}`,
			}, nil
		}
		return pkg.CallResult{StructuredContent: `{"ok":true}`}, nil
	case "execute_workflow":
		f.execCalls = append(f.execCalls, args)
		return pkg.CallResult{Content: "Workflow completed: 1 block(s), 1 step(s).", StructuredContent: `{"blocks":{}}`}, nil
	default:
		return pkg.CallResult{}, nil
	}
}

func testHandler(t *testing.T) *Handler {
	t.Helper()
	cfg, _ := config.Parse("")
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewHandler(cfg, agent.NewManager(db))
}

func ctxArgs(m map[string]string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m["group_id"] = "g1"
	m["entity_id"] = "u1"
	return m
}

func TestCreate_ValidSourcePersists(t *testing.T) {
	h := testHandler(t)
	host := &fakeHost{}
	resp := h.ExecuteWithCallbacks(context.Background(), pkg.Request{
		ID: "c1", Action: "create",
		Args: ctxArgs(map[string]string{"name": "restock", "talon_source": `workflow "ok" {}`}),
	}, host)
	if resp.Error != "" {
		t.Fatalf("create should succeed: %q", resp.Error)
	}
	list := h.ExecuteWithCallbacks(context.Background(), pkg.Request{ID: "l1", Action: "list", Args: ctxArgs(nil)}, host)
	if !strings.Contains(list.StructuredContent, "restock") {
		t.Errorf("agent not persisted; list=%q", list.StructuredContent)
	}
}

func TestCreate_InvalidSourceRejectedAndNotPersisted(t *testing.T) {
	h := testHandler(t)
	host := &fakeHost{}
	resp := h.ExecuteWithCallbacks(context.Background(), pkg.Request{
		ID: "c1", Action: "create",
		Args: ctxArgs(map[string]string{"name": "bad", "talon_source": "INVALID {"}),
	}, host)
	if resp.Error == "" {
		t.Fatal("invalid source should be rejected")
	}
	if !strings.Contains(resp.Error, "unexpected token") {
		t.Errorf("error should relay diagnostics: %q", resp.Error)
	}
	list := h.ExecuteWithCallbacks(context.Background(), pkg.Request{ID: "l1", Action: "list", Args: ctxArgs(nil)}, host)
	if strings.Contains(list.StructuredContent, "\"bad\"") {
		t.Errorf("invalid agent must not persist; list=%q", list.StructuredContent)
	}
}

func TestRun_IssuesOneExecuteWorkflowWithStoredSource(t *testing.T) {
	h := testHandler(t)
	host := &fakeHost{}
	src := `workflow "restock" { step "s" { mcp "inv" "refill" { id "1" } } }`
	if resp := h.ExecuteWithCallbacks(context.Background(), pkg.Request{
		ID: "c1", Action: "create",
		Args: ctxArgs(map[string]string{"name": "restock", "talon_source": src}),
	}, host); resp.Error != "" {
		t.Fatalf("create: %q", resp.Error)
	}

	resp := h.ExecuteWithCallbacks(context.Background(), pkg.Request{
		ID: "r1", Action: "run", Args: ctxArgs(map[string]string{"id": "restock"}),
	}, host)
	if resp.Error != "" {
		t.Fatalf("run should succeed: %q", resp.Error)
	}
	if len(host.execCalls) != 1 {
		t.Fatalf("expected exactly one execute_workflow call, got %d", len(host.execCalls))
	}
	if host.execCalls[0]["workflow"] != src {
		t.Errorf("execute_workflow received wrong source: %q", host.execCalls[0]["workflow"])
	}
}

func TestExecute_RequiresCallbacks(t *testing.T) {
	h := testHandler(t)
	if resp := h.Execute(pkg.Request{ID: "x"}); resp.Error == "" {
		t.Error("unary Execute should return an error")
	}
}
