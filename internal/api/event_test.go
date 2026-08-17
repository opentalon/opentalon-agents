package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
	"github.com/opentalon/opentalon-agents/internal/store"
)

// eventFixture wires a server with two agents of user u1 that both subscribe to
// item.status_changed, one agent of u1 on a different event, and one agent of a
// different user — so a fan-out can be checked against the scoping.
func eventFixture(t *testing.T, secret string) (http.Handler, *agent.Manager) {
	t.Helper()
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mgr := agent.NewManager(db)
	mk := func(name, owner, event string, enabled bool) {
		_, err := mgr.Create(context.Background(), agent.Agent{
			Name: name, GroupID: "g1", EntityID: owner, Enabled: enabled,
			TlnSource: `on change attr "status" { when new_value == "defective" workflow "x" }`,
			Triggers:  []agent.Trigger{{Type: agent.TriggerEvent, Config: json.RawMessage(`{"event":"` + event + `"}`)}},
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	mk("a1", "u1", "item.status_changed", true)
	mk("a2", "u1", "item.status_changed", true)
	mk("a3", "u1", "item.synced", true)          // different event — not matched
	mk("a4", "u2", "item.status_changed", true)  // different owner — not matched
	mk("a5", "u1", "item.status_changed", false) // disabled — not matched
	return NewServer(&config.Config{WebhookSecret: secret}, mgr), mgr
}

func TestEvent_FansOutToSubscribers(t *testing.T) {
	h, mgr := eventFixture(t, "s3cr3t")
	w := post(h, "/v1/events/item.status_changed?user_id=u1", "s3cr3t", `{"item_id":"ABC-123","status":"defective"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Matched int      `json:"matched"`
		Event   string   `json:"event"`
		IDs     []string `json:"agent_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Matched != 2 || resp.Event != "item.status_changed" || len(resp.IDs) != 2 {
		t.Errorf("expected 2 matches for u1, got %+v", resp)
	}
	// Each matched subscriber got exactly one queued delivery, tagged.
	pend, _ := mgr.ListPendingEvents(context.Background())
	if len(pend) != 2 {
		t.Fatalf("expected two queued deliveries, got %d", len(pend))
	}
	for _, ev := range pend {
		if ev.Event != "item.status_changed" {
			t.Errorf("queued delivery not tagged with event: %+v", ev)
		}
	}
}

func TestEvent_UnknownEventRejected(t *testing.T) {
	h, _ := eventFixture(t, "s3cr3t")
	w := post(h, "/v1/events/item.exploded?user_id=u1", "s3cr3t", `{"item_id":"ABC-123"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown event, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		KnownEvents []string `json:"known_events"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.KnownEvents) == 0 {
		t.Error("400 should list known events")
	}
}

func TestEvent_NoSubscribersStillAccepted(t *testing.T) {
	h, mgr := eventFixture(t, "s3cr3t")
	// A known event nobody subscribes to: accepted, matched 0, nothing queued.
	w := post(h, "/v1/events/checkout.created?user_id=u1", "s3cr3t", `{"item_id":"ABC-123","checkout_id":"c1"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d (%s)", w.Code, w.Body.String())
	}
	if pend, _ := mgr.ListPendingEvents(context.Background()); len(pend) != 0 {
		t.Errorf("nothing should be queued, got %d", len(pend))
	}
}

func TestEvent_AuthAndValidation(t *testing.T) {
	h, _ := eventFixture(t, "s3cr3t")
	cases := []struct {
		name, path, bearer, body string
		want                     int
	}{
		{"no bearer", "/v1/events/item.status_changed?user_id=u1", "", `{}`, http.StatusUnauthorized},
		{"missing user_id", "/v1/events/item.status_changed", "s3cr3t", `{"status":"x"}`, http.StatusBadRequest},
		{"bad json", "/v1/events/item.status_changed?user_id=u1", "s3cr3t", `{not json`, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if w := post(h, c.path, c.bearer, c.body); w.Code != c.want {
				t.Errorf("got %d, want %d (%s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}
