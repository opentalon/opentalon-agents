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
		TalonSource: `workflow "x" {}`,
		Triggers:    []agent.Trigger{{Type: agent.TriggerWebhook, Config: json.RawMessage(`{"value_path":"stock","attribute":"current_stock"}`)}},
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
		Name: "alerts", GroupID: "g2", EntityID: "u2", Enabled: true, TalonSource: `workflow "x" {}`,
		Triggers: []agent.Trigger{{Type: agent.TriggerManual}},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// group filter returns only g1's agent; never the Talon source.
	w := get(h, "/v1/agents?group_id=g1", "s3cr3t")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "restock") || strings.Contains(body, "alerts") {
		t.Errorf("group filter body: %s", body)
	}
	if strings.Contains(body, "talon_source") || strings.Contains(body, "workflow \\\"x\\\"") {
		t.Errorf("list must not leak talon_source: %s", body)
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
