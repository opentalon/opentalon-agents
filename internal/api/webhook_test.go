package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
	"github.com/opentalon/opentalon-agents/internal/store"
)

func fixture(t *testing.T, secret string) (http.Handler, *agent.Manager) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mgr := agent.NewManager(db)
	_, err = mgr.Create(context.Background(), agent.Agent{
		Name: "restock", GroupID: "g1", EntityID: "u1", Enabled: true,
		TlnSource: `workflow "x" {}`,
		Triggers:  []agent.Trigger{{Type: agent.TriggerWebhook, Config: json.RawMessage(`{"value_path":"stock","attribute":"current_stock"}`)}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return NewWebhookServer(&config.Config{WebhookSecret: secret}, mgr), mgr
}

func post(h http.Handler, path, bearer, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWebhook_HappyPathEnqueues(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	w := post(h, "/v1/hooks/restock?user_id=u1", "s3cr3t", `{"barcode":"ABC-123","stock":8}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	pend, _ := mgr.ListPendingEvents(context.Background())
	if len(pend) != 1 {
		t.Errorf("expected one queued event, got %d", len(pend))
	}
}

func TestWebhook_UserIDFromBody(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	w := post(h, "/v1/hooks/restock", "s3cr3t", `{"user_id":"u1","stock":8}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	if pend, _ := mgr.ListPendingEvents(context.Background()); len(pend) != 1 {
		t.Errorf("expected one queued event, got %d", len(pend))
	}
}

func TestWebhook_AuthAndValidation(t *testing.T) {
	h, _ := fixture(t, "s3cr3t")
	cases := []struct {
		name, path, bearer, body string
		want                     int
	}{
		{"no bearer", "/v1/hooks/restock?user_id=u1", "", `{"stock":8}`, http.StatusUnauthorized},
		{"wrong bearer", "/v1/hooks/restock?user_id=u1", "nope", `{"stock":8}`, http.StatusUnauthorized},
		{"missing user_id", "/v1/hooks/restock", "s3cr3t", `{"stock":8}`, http.StatusBadRequest},
		{"bad json", "/v1/hooks/restock?user_id=u1", "s3cr3t", `{not json`, http.StatusBadRequest},
		{"unknown user", "/v1/hooks/restock?user_id=ghost", "s3cr3t", `{"stock":8}`, http.StatusNotFound},
		{"unknown agent", "/v1/hooks/nope?user_id=u1", "s3cr3t", `{"stock":8}`, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if w := post(h, c.path, c.bearer, c.body); w.Code != c.want {
				t.Errorf("got %d, want %d (%s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}

func TestWebhook_DisabledWithoutSecret(t *testing.T) {
	h, _ := fixture(t, "") // no secret configured
	if w := post(h, "/v1/hooks/restock?user_id=u1", "anything", `{"stock":8}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when webhook_secret unset, got %d", w.Code)
	}
}

func get(h http.Handler, path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestListAgents_FiltersAndAuth(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t") // fixture already has "restock" (g1, u1, webhook)
	if _, err := mgr.Create(context.Background(), agent.Agent{
		Name: "alerts", GroupID: "g2", EntityID: "u2", Enabled: true, TlnSource: `workflow "x" {}`,
		Triggers: []agent.Trigger{{Type: agent.TriggerManual}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// group filter returns only g1's agent; never the Tln source.
	w := get(h, "/v1/agents?group_id=g1", "s3cr3t")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "restock") || strings.Contains(body, "alerts") {
		t.Errorf("group filter body: %s", body)
	}
	if strings.Contains(body, "tln_source") || strings.Contains(body, "workflow \\\"x\\\"") {
		t.Errorf("list must not leak tln_source: %s", body)
	}

	// name substring filter.
	if w := get(h, "/v1/agents?name=aler", "s3cr3t"); !strings.Contains(w.Body.String(), "alerts") || strings.Contains(w.Body.String(), "restock") {
		t.Errorf("name filter: %s", w.Body.String())
	}

	// auth.
	if w := get(h, "/v1/agents", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no bearer → 401, got %d", w.Code)
	}
	if w := get(h, "/v1/agents", "nope"); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer → 401, got %d", w.Code)
	}
}

func TestListAgents_DisabledWithoutSecret(t *testing.T) {
	h, _ := fixture(t, "")
	if w := get(h, "/v1/agents", "anything"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func put(h http.Handler, path, bearer, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestCreateAgent_HappyPath(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	body := `{"name":"draft-1","group_id":"g1","entity_id":"u1","tln_source":"workflow \"x\" {}","enabled":false}`
	if w := post(h, "/v1/agents", "s3cr3t", body); w.Code != http.StatusCreated {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	agents, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1", NameContains: "draft-1"})
	if len(agents) != 1 || agents[0].Enabled {
		t.Errorf("expected one disabled draft, got %+v", agents)
	}
}

func TestCreateAgent_Validation(t *testing.T) {
	h, _ := fixture(t, "s3cr3t")
	// Missing name and tln_source.
	if w := post(h, "/v1/agents", "s3cr3t", `{"group_id":"g1"}`); w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if w := post(h, "/v1/agents", "nope", `{}`); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong bearer → 401, got %d", w.Code)
	}
}

func TestUpdateAgent_TlnAndEnabled(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	agents, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1", NameContains: "restock"})
	id := agents[0].ID // seeded enabled:true
	body := `{"group_id":"g1","tln_source":"workflow \"y\" {}","enabled":false}`
	if w := put(h, "/v1/agents/"+id, "s3cr3t", body); w.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.Get(context.Background(), "g1", id)
	if got.TlnSource != `workflow "y" {}` || got.Enabled {
		t.Errorf("update not applied: tln=%q enabled=%v", got.TlnSource, got.Enabled)
	}
}

func TestUpdateAgent_NotFound(t *testing.T) {
	h, _ := fixture(t, "s3cr3t")
	if w := put(h, "/v1/agents/nope", "s3cr3t", `{"group_id":"g1","enabled":true}`); w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func del(h http.Handler, path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodDelete, path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUpdateAgent_Meta(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	agents, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1"})
	id := agents[0].ID
	before, _ := mgr.Get(context.Background(), "g1", id)

	body := `{"group_id":"g1","name":"Renamed","description":"new desc"}`
	if w := put(h, "/v1/agents/"+id, "s3cr3t", body); w.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.Get(context.Background(), "g1", id)
	if got.Name != "Renamed" || got.Description != "new desc" {
		t.Errorf("meta not applied: name=%q desc=%q", got.Name, got.Description)
	}
	if got.TlnSource != before.TlnSource {
		t.Errorf("tln should be unchanged: before=%q after=%q", before.TlnSource, got.TlnSource)
	}
}

func TestDeleteAgent(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	agents, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1"})
	id := agents[0].ID

	if w := del(h, "/v1/agents/"+id, "s3cr3t"); w.Code != http.StatusBadRequest {
		t.Errorf("no group_id: expected 400, got %d", w.Code)
	}
	if w := del(h, "/v1/agents/"+id+"?group_id=other", "s3cr3t"); w.Code != http.StatusNotFound {
		t.Errorf("wrong group: expected 404, got %d", w.Code)
	}
	if w := del(h, "/v1/agents/"+id+"?group_id=g1", "s3cr3t"); w.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d body %s", w.Code, w.Body.String())
	}
	if _, err := mgr.Get(context.Background(), "g1", id); err == nil {
		t.Error("agent should be gone after delete")
	}
}

func TestUpdateAgent_TriggersOnly(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	agents, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1", NameContains: "restock"})
	id := agents[0].ID
	before, _ := mgr.Get(context.Background(), "g1", id)

	body := `{"group_id":"g1","triggers":[{"type":"schedule","cron":"* * * * *"}]}`
	if w := put(h, "/v1/agents/"+id, "s3cr3t", body); w.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.Get(context.Background(), "g1", id)
	if got.TlnSource != before.TlnSource {
		t.Errorf("tln should be unchanged: before=%q after=%q", before.TlnSource, got.TlnSource)
	}
	if len(got.Triggers) != 1 || got.Triggers[0].Type != agent.TriggerSchedule || got.Triggers[0].Cron != "* * * * *" {
		t.Errorf("trigger not applied: %+v", got.Triggers)
	}
}

func TestGetAgent_FullIncludesTln(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	agents, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1"})
	id := agents[0].ID

	w := get(h, "/v1/agents/"+id+"?group_id=g1", "s3cr3t")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The full view carries the program, unlike the list summary.
	if src, _ := resp["tln_source"].(string); src == "" {
		t.Errorf("expected tln_source in full agent, got %+v", resp)
	}

	if w := get(h, "/v1/agents/"+id, "s3cr3t"); w.Code != http.StatusBadRequest {
		t.Errorf("no group_id: expected 400, got %d", w.Code)
	}
	if w := get(h, "/v1/agents/"+id+"?group_id=other", "s3cr3t"); w.Code != http.StatusNotFound {
		t.Errorf("wrong group: expected 404, got %d", w.Code)
	}
}

func TestAgentRuns_ListsHistory(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	agents, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1", NameContains: "restock"})
	id := agents[0].ID
	if _, err := mgr.CreateRun(context.Background(), agent.Run{
		AgentID: id, TriggerType: agent.TriggerPoll, Status: agent.StatusCompleted,
		Event:  json.RawMessage(`{"stock":2}`),
		Result: json.RawMessage(`{"firings":["reorder"]}`),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	w := get(h, "/v1/agents/"+id+"/runs?group_id=g1", "s3cr3t")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Runs []agent.Run `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Runs) != 1 || resp.Runs[0].Status != agent.StatusCompleted {
		t.Errorf("unexpected runs: %+v", resp.Runs)
	}
}

func TestLatestRunPerAgent_OnePerAgent(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	agents, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1"})
	id := agents[0].ID
	// Two runs for the same agent — only the newest should come back.
	for _, st := range []string{agent.StatusFailed, agent.StatusCompleted} {
		if _, err := mgr.CreateRun(context.Background(), agent.Run{
			AgentID: id, TriggerType: agent.TriggerSchedule, Status: st,
		}); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}

	w := get(h, "/v1/agents/runs?group_id=g1", "s3cr3t")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Runs []agent.Run `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Exactly one row for the agent despite two runs (the N+1-avoiding dedup).
	if len(resp.Runs) != 1 {
		t.Fatalf("expected one latest run for the one agent, got %d", len(resp.Runs))
	}
	if resp.Runs[0].AgentID != id {
		t.Errorf("expected run for agent %s, got %+v", id, resp.Runs[0])
	}

	if w := get(h, "/v1/agents/runs", "s3cr3t"); w.Code != http.StatusBadRequest {
		t.Errorf("no group_id: expected 400, got %d", w.Code)
	}
}

func TestAgentRuns_ScopeAndValidation(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	agents, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1", NameContains: "restock"})
	id := agents[0].ID

	// Missing group_id is a bad request.
	if w := get(h, "/v1/agents/"+id+"/runs", "s3cr3t"); w.Code != http.StatusBadRequest {
		t.Errorf("no group_id: expected 400, got %d", w.Code)
	}
	// Wrong group can't read another group's agent runs.
	if w := get(h, "/v1/agents/"+id+"/runs?group_id=other", "s3cr3t"); w.Code != http.StatusNotFound {
		t.Errorf("wrong group: expected 404, got %d", w.Code)
	}
	// Unknown agent id.
	if w := get(h, "/v1/agents/nope/runs?group_id=g1", "s3cr3t"); w.Code != http.StatusNotFound {
		t.Errorf("unknown id: expected 404, got %d", w.Code)
	}
	// Unauthorized.
	if w := get(h, "/v1/agents/"+id+"/runs?group_id=g1", "wrong"); w.Code != http.StatusUnauthorized {
		t.Errorf("bad bearer: expected 401, got %d", w.Code)
	}
}

func TestAgentAutonomy_CreateDefaultAndUpdate(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")

	// Create without autonomy → defaults to "ask", and the summary carries it.
	body := `{"name":"auto-default","group_id":"g1","entity_id":"u1","tln_source":"workflow \"x\" {}"}`
	w := post(h, "/v1/agents", "s3cr3t", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: got %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"autonomy":"ask"`) {
		t.Errorf("create summary should default autonomy to ask: %s", w.Body.String())
	}

	// Create WITH an explicit autonomy is stored verbatim.
	body = `{"name":"auto-act","group_id":"g1","entity_id":"u1","tln_source":"workflow \"x\" {}","autonomy":"act"}`
	if w := post(h, "/v1/agents", "s3cr3t", body); w.Code != http.StatusCreated {
		t.Fatalf("create act: got %d, body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1", NameContains: "auto-act"})
	if len(got) != 1 || got[0].Autonomy != "act" {
		t.Fatalf("expected autonomy=act, got %+v", got)
	}

	// Update flips autonomy without touching the program.
	id := got[0].ID
	before := got[0]
	if w := put(h, "/v1/agents/"+id, "s3cr3t", `{"group_id":"g1","autonomy":"notify"}`); w.Code != http.StatusOK {
		t.Fatalf("update autonomy: got %d, body %s", w.Code, w.Body.String())
	}
	after, _ := mgr.Get(context.Background(), "g1", id)
	if after.Autonomy != "notify" {
		t.Errorf("autonomy not updated: %q", after.Autonomy)
	}
	if after.TlnSource != before.TlnSource {
		t.Errorf("tln should be unchanged: before=%q after=%q", before.TlnSource, after.TlnSource)
	}

	// The list summary exposes autonomy.
	if w := get(h, "/v1/agents?group_id=g1&name=auto-act", "s3cr3t"); !strings.Contains(w.Body.String(), `"autonomy":"notify"`) {
		t.Errorf("list summary should carry autonomy: %s", w.Body.String())
	}
}

func TestAgentConfig_CreateStoreAndUpdate(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")

	// Create WITH a config blob stores it verbatim and the full GET echoes it.
	cfg := `{\"template\":\"low_stock_reorder\",\"category\":\"Drills\"}`
	body := `{"name":"cfg-a","group_id":"g1","entity_id":"u1","tln_source":"workflow \"x\" {}","config":"` + cfg + `"}`
	if w := post(h, "/v1/agents", "s3cr3t", body); w.Code != http.StatusCreated {
		t.Fatalf("create with config: got %d, body %s", w.Code, w.Body.String())
	}
	got, _ := mgr.QueryAgents(context.Background(), agent.AgentFilter{GroupID: "g1", NameContains: "cfg-a"})
	if len(got) != 1 || !strings.Contains(got[0].Config, `"template":"low_stock_reorder"`) {
		t.Fatalf("config not stored: %+v", got)
	}
	id := got[0].ID
	if w := get(h, "/v1/agents/"+id+"?group_id=g1", "s3cr3t"); !strings.Contains(w.Body.String(), `low_stock_reorder`) {
		t.Errorf("full GET should echo config: %s", w.Body.String())
	}

	// Update replaces config without touching the program.
	before := got[0]
	if w := put(h, "/v1/agents/"+id, "s3cr3t", `{"group_id":"g1","config":"{\"category\":\"Ladders\"}"}`); w.Code != http.StatusOK {
		t.Fatalf("update config: got %d, body %s", w.Code, w.Body.String())
	}
	after, _ := mgr.Get(context.Background(), "g1", id)
	if !strings.Contains(after.Config, "Ladders") {
		t.Errorf("config not updated: %q", after.Config)
	}
	if after.TlnSource != before.TlnSource {
		t.Errorf("tln should be unchanged: before=%q after=%q", before.TlnSource, after.TlnSource)
	}
}

func TestListAgents_Pagination(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t") // g1 already has one agent ("restock")
	for _, n := range []string{"p-a", "p-b", "p-c", "p-d"} {
		if _, err := mgr.Create(context.Background(), agent.Agent{
			Name: n, GroupID: "gp", EntityID: "u1", Enabled: true, TlnSource: `workflow "x" {}`,
		}); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}

	// Page 1 of 2 with per_page=2 → two rows + a pagination block.
	w := get(h, "/v1/agents?group_id=gp&per_page=2&page=1", "s3cr3t")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Agents     []map[string]any `json:"agents"`
		Pagination struct {
			Page       int `json:"page"`
			PerPage    int `json:"per_page"`
			Total      int `json:"total"`
			TotalPages int `json:"total_pages"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Errorf("expected 2 rows on page 1, got %d", len(resp.Agents))
	}
	if resp.Pagination.Total != 4 || resp.Pagination.TotalPages != 2 || resp.Pagination.Page != 1 || resp.Pagination.PerPage != 2 {
		t.Errorf("bad pagination block: %+v", resp.Pagination)
	}

	// Last page carries the remainder.
	w = get(h, "/v1/agents?group_id=gp&per_page=2&page=2", "s3cr3t")
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode p2: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Errorf("expected 2 rows on page 2, got %d", len(resp.Agents))
	}

	// Without per_page: unpaginated, no pagination block.
	w = get(h, "/v1/agents?group_id=gp", "s3cr3t")
	if strings.Contains(w.Body.String(), "pagination") {
		t.Errorf("no per_page should omit pagination block: %s", w.Body.String())
	}
}

func TestListAgents_SortAndAutonomyFilter(t *testing.T) {
	h, mgr := fixture(t, "s3cr3t")
	seed := []struct{ name, autonomy string }{
		{"gamma", "notify"}, {"alpha", "act"}, {"beta", "ask"},
	}
	for _, s := range seed {
		if _, err := mgr.Create(context.Background(), agent.Agent{
			Name: s.name, GroupID: "gs", EntityID: "u1", TlnSource: `workflow "x" {}`, Autonomy: s.autonomy,
		}); err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}
	}

	names := func(body string) []string {
		var resp struct {
			Agents []struct {
				Name string `json:"name"`
			} `json:"agents"`
		}
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := make([]string, len(resp.Agents))
		for i, a := range resp.Agents {
			out[i] = a.Name
		}
		return out
	}

	// Sort by name ascending.
	got := names(get(h, "/v1/agents?group_id=gs&sort=name&dir=asc", "s3cr3t").Body.String())
	if len(got) != 3 || got[0] != "alpha" || got[1] != "beta" || got[2] != "gamma" {
		t.Errorf("name asc: got %v", got)
	}

	// Sort by name descending.
	got = names(get(h, "/v1/agents?group_id=gs&sort=name&dir=desc", "s3cr3t").Body.String())
	if len(got) != 3 || got[0] != "gamma" {
		t.Errorf("name desc: got %v", got)
	}

	// Autonomy filter narrows to one row.
	got = names(get(h, "/v1/agents?group_id=gs&autonomy=ask", "s3cr3t").Body.String())
	if len(got) != 1 || got[0] != "beta" {
		t.Errorf("autonomy=ask filter: got %v", got)
	}

	// Multi-value autonomy (comma-separated → IN) matches any listed value.
	got = names(get(h, "/v1/agents?group_id=gs&autonomy=ask,act&sort=name&dir=asc", "s3cr3t").Body.String())
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("autonomy=ask,act filter: got %v", got)
	}
}
