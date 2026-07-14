// Package api hosts the plugin's inbound HTTP surface. Today that is the
// webhook ingress, reverse-proxied by the host at /<config-key>/* to the
// plugin's private localhost listener.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// NewWebhookServer builds the webhook HTTP handler. Every request must
// carry `Authorization: Bearer <webhook_secret>`; the target agent is
// scoped by the `user_id` request param (query or body) and named by the
// `{agent}` path segment. The handler only ENQUEUES — it has no
// HostCaller, so evaluation happens on the next tick.
func NewWebhookServer(cfg *config.Config, mgr *agent.Manager) http.Handler {
	h := &webhook{cfg: cfg, mgr: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/hooks/{agent}", h.handle)
	return mux
}

type webhook struct {
	cfg *config.Config
	mgr *agent.Manager
}

func (h *webhook) handle(w http.ResponseWriter, r *http.Request) {
	if h.cfg.WebhookSecret == "" {
		writeErr(w, http.StatusServiceUnavailable, "webhook endpoint disabled (set webhook_secret)")
		return
	}
	if !h.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	if len(body) > 0 && !json.Valid(body) {
		writeErr(w, http.StatusBadRequest, "body must be JSON")
		return
	}

	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = userIDFromBody(body)
	}
	if userID == "" {
		writeErr(w, http.StatusBadRequest, "user_id is required (query param or body field)")
		return
	}

	a, err := h.mgr.WebhookAgent(r.Context(), userID, r.PathValue("agent"))
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "no webhook-triggered agent for that user_id")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := h.mgr.EnqueueEvent(r.Context(), agent.PendingEvent{AgentID: a.ID, Kind: agent.EventKindFacts, Payload: body}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "agent_id": a.ID})
}

// authorized compares the bearer token to the configured secret in
// constant time.
func (h *webhook) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	hdr := r.Header.Get("Authorization")
	if !strings.HasPrefix(hdr, prefix) {
		return false
	}
	got := []byte(strings.TrimPrefix(hdr, prefix))
	want := []byte(h.cfg.WebhookSecret)
	return len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1
}

// userIDFromBody extracts a top-level "user_id" string from a JSON body.
func userIDFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		if v, ok := m["user_id"].(string); ok {
			return v
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
