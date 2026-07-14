package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	pkg "github.com/opentalon/opentalon/pkg/plugin"
)

// scriptHost is a configurable pkg.HostCaller for exercising failure
// paths: check validity/error and execute_workflow error are scripted.
type scriptHost struct {
	checkOK   bool
	checkErr  error
	execErr   error
	execCalls int
}

func (s *scriptHost) RunAction(_ context.Context, _, action string, _ map[string]string) (pkg.CallResult, error) {
	switch action {
	case "check":
		if s.checkErr != nil {
			return pkg.CallResult{}, s.checkErr
		}
		if s.checkOK {
			return pkg.CallResult{StructuredContent: `{"ok":true}`}, nil
		}
		return pkg.CallResult{Content: "talon: parse: bad", StructuredContent: `{"ok":false,"stage":"parse"}`}, nil
	case "execute_workflow":
		s.execCalls++
		if s.execErr != nil {
			return pkg.CallResult{}, s.execErr
		}
		return pkg.CallResult{Content: "done", StructuredContent: `{"blocks":{}}`}, nil
	}
	return pkg.CallResult{}, nil
}

func create(t *testing.T, h *Handler, host pkg.HostCaller, name, src string) pkg.Response {
	t.Helper()
	return h.ExecuteWithCallbacks(context.Background(), pkg.Request{
		ID: "c", Action: "create",
		Args: ctxArgs(map[string]string{"name": name, "talon_source": src}),
	}, host)
}

func TestCapabilities(t *testing.T) {
	h := testHandler(t)
	caps := h.Capabilities()
	if caps.Name != "agents" {
		t.Errorf("name: %q", caps.Name)
	}
	if !caps.SupportsCallbacks {
		t.Error("SupportsCallbacks must be true (needs live HostCaller to reach talon-plugin)")
	}
	if caps.SystemPromptAddition == "" {
		t.Error("expected a system prompt addition")
	}
	want := map[string]bool{"create": true, "list": true, "show": true, "run": true, "update": true, "enable": true, "disable": true, "delete": true}
	for _, a := range caps.Actions {
		delete(want, a.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing actions: %v", want)
	}
}

func TestUpdate_ValidReplacesSource_InvalidRejected(t *testing.T) {
	ctx := context.Background()
	h := testHandler(t)
	host := &scriptHost{checkOK: true}
	if resp := create(t, h, host, "a", `workflow "v1" {}`); resp.Error != "" {
		t.Fatalf("create: %q", resp.Error)
	}

	// Valid update replaces the source.
	if resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "u1", Action: "update",
		Args: ctxArgs(map[string]string{"id": "a", "talon_source": `workflow "v2" {}`})}, host); resp.Error != "" {
		t.Fatalf("valid update: %q", resp.Error)
	}
	got, _ := h.mgr.Get(ctx, "g1", "a")
	if got.TalonSource != `workflow "v2" {}` {
		t.Errorf("source not updated: %q", got.TalonSource)
	}

	// Invalid update is rejected and leaves the source unchanged.
	host.checkOK = false
	if resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "u2", Action: "update",
		Args: ctxArgs(map[string]string{"id": "a", "talon_source": `bad`})}, host); resp.Error == "" {
		t.Fatal("invalid update should be rejected")
	}
	got, _ = h.mgr.Get(ctx, "g1", "a")
	if got.TalonSource != `workflow "v2" {}` {
		t.Errorf("rejected update must not change source: %q", got.TalonSource)
	}
}

func TestLifecycle_EnableDisableDelete(t *testing.T) {
	ctx := context.Background()
	h := testHandler(t)
	host := &scriptHost{checkOK: true}
	create(t, h, host, "a", `workflow "x" {}`)

	if resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "d1", Action: "disable", Args: ctxArgs(map[string]string{"id": "a"})}, host); resp.Error != "" {
		t.Fatalf("disable: %q", resp.Error)
	}
	if a, _ := h.mgr.Get(ctx, "g1", "a"); a.Enabled {
		t.Error("expected disabled")
	}
	if resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "e1", Action: "enable", Args: ctxArgs(map[string]string{"id": "a"})}, host); resp.Error != "" {
		t.Fatalf("enable: %q", resp.Error)
	}
	if a, _ := h.mgr.Get(ctx, "g1", "a"); !a.Enabled {
		t.Error("expected enabled")
	}
	if resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "x1", Action: "delete", Args: ctxArgs(map[string]string{"id": "a"})}, host); resp.Error != "" {
		t.Fatalf("delete: %q", resp.Error)
	}
	if _, err := h.mgr.Get(ctx, "g1", "a"); err == nil {
		t.Error("expected agent gone after delete")
	}
}

func TestCreate_StoresUserRequestAsDescription(t *testing.T) {
	ctx := context.Background()
	h := testHandler(t)
	host := &scriptHost{checkOK: true}
	want := "watch stock ABC-123 and open a refill ticket when it drops below 10"
	if resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "c", Action: "create",
		Args: ctxArgs(map[string]string{"name": "restock", "description": want, "talon_source": `workflow "x" {}`})}, host); resp.Error != "" {
		t.Fatalf("create: %q", resp.Error)
	}
	a, _ := h.mgr.Get(ctx, "g1", "restock")
	if a.Description != want {
		t.Errorf("description not stored: %q", a.Description)
	}
	// show surfaces it back.
	show := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "s", Action: "show", Args: ctxArgs(map[string]string{"id": "restock"})}, host)
	if !strings.Contains(show.StructuredContent, "drops below 10") {
		t.Errorf("show should include the user's request: %s", show.StructuredContent)
	}
}

func TestShow_IncludesSource(t *testing.T) {
	h := testHandler(t)
	host := &scriptHost{checkOK: true}
	create(t, h, host, "a", `workflow "secret" {}`)
	resp := h.ExecuteWithCallbacks(context.Background(), pkg.Request{ID: "s1", Action: "show", Args: ctxArgs(map[string]string{"id": "a"})}, host)
	if resp.Error != "" {
		t.Fatalf("show: %q", resp.Error)
	}
	if !strings.Contains(resp.StructuredContent, "secret") {
		t.Errorf("show should include the source: %q", resp.StructuredContent)
	}
}

func TestRun_ExecFailureRecordsFailedRun(t *testing.T) {
	ctx := context.Background()
	h := testHandler(t)
	host := &scriptHost{checkOK: true, execErr: errors.New("mcp exploded")}
	create(t, h, host, "a", `workflow "x" {}`)

	resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "r1", Action: "run", Args: ctxArgs(map[string]string{"id": "a"})}, host)
	if resp.Error == "" {
		t.Fatal("run should surface the execution error")
	}
	a, _ := h.mgr.Get(ctx, "g1", "a")
	runs, _ := h.mgr.ListRuns(ctx, a.ID, 10)
	if len(runs) != 1 || runs[0].Status != "failed" || runs[0].Error == "" {
		t.Errorf("expected one failed run with an error: %+v", runs)
	}
}

func TestCreate_CheckUnavailableSurfacesError(t *testing.T) {
	h := testHandler(t)
	host := &scriptHost{checkErr: errors.New("talon-plugin not loaded")}
	resp := create(t, h, host, "a", `workflow "x" {}`)
	if resp.Error == "" || !strings.Contains(resp.Error, "validate") {
		t.Errorf("expected a validation-unavailable error, got %q", resp.Error)
	}
}

func TestUnknownActionAndMissingGroup(t *testing.T) {
	h := testHandler(t)
	host := &scriptHost{checkOK: true}
	if resp := h.ExecuteWithCallbacks(context.Background(), pkg.Request{ID: "u", Action: "frobnicate", Args: ctxArgs(nil)}, host); resp.Error == "" {
		t.Error("unknown action should error")
	}
	if resp := h.ExecuteWithCallbacks(context.Background(), pkg.Request{ID: "g", Action: "list", Args: map[string]string{}}, host); resp.Error == "" || !strings.Contains(resp.Error, "group_id") {
		t.Errorf("missing group_id should error, got %q", resp.Error)
	}
}
