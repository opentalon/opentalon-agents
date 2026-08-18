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
	"strconv"
	"strings"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
)

const maxBodyBytes = 1 << 20 // 1 MiB

// NewServer builds the plugin's inbound HTTP handler: the webhook ingress
// plus a read-only agents query API. Every request must carry
// `Authorization: Bearer <webhook_secret>` (shared secret gating the whole
// surface). Retained alias NewWebhookServer for callers.
func NewServer(cfg *config.Config, mgr *agent.Manager) http.Handler {
	h := &server{cfg: cfg, mgr: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/hooks/{agent}", h.handleHook)
	mux.HandleFunc("GET /v1/agents", h.handleList)
	mux.HandleFunc("POST /v1/agents", h.handleCreate)
	mux.HandleFunc("PUT /v1/agents/{id}", h.handleUpdate)
	mux.HandleFunc("GET /v1/agents/{id}/runs", h.handleRuns)
	return mux
}

// NewWebhookServer is kept for backwards compatibility.
func NewWebhookServer(cfg *config.Config, mgr *agent.Manager) http.Handler {
	return NewServer(cfg, mgr)
}

type server struct {
	cfg *config.Config
	mgr *agent.Manager
}

// guard enforces the shared bearer on every endpoint. Returns false (and
// writes the response) when the endpoint is disabled or unauthorized.
func (h *server) guard(w http.ResponseWriter, r *http.Request) bool {
	if h.cfg.WebhookSecret == "" {
		writeErr(w, http.StatusServiceUnavailable, "http endpoint disabled (set webhook_secret)")
		return false
	}
	if !h.authorized(r) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	return true
}

// handleList serves GET /v1/agents with optional group_id / entity_id /
// name (substring) / enabled filters, returning agent summaries.
func (h *server) handleList(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	q := r.URL.Query()
	f := agent.AgentFilter{
		GroupID:      q.Get("group_id"),
		EntityID:     q.Get("entity_id"),
		NameContains: q.Get("name"),
	}
	if e := q.Get("enabled"); e != "" {
		b := e == "true" || e == "1"
		f.Enabled = &b
	}
	agents, err := h.mgr.QueryAgents(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	summaries := make([]agent.AgentSummary, 0, len(agents))
	for _, a := range agents {
		summaries = append(summaries, a.Summary())
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": summaries})
}

// createRequest is the body of POST /v1/agents. It lets a host app (Timly)
// presave a draft workflow directly into the agents store — the write
// counterpart to handleList, gated by the same shared bearer. Drafts are
// typically created with enabled=false and activated later.
type createRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	GroupID     string          `json:"group_id"`
	EntityID    string          `json:"entity_id"`
	TlnSource   string          `json:"tln_source"`
	Enabled     bool            `json:"enabled"`
	Triggers    []agent.Trigger `json:"triggers"`
}

// handleCreate serves POST /v1/agents — persist a (draft) agent. The Tln is
// stored as-is (no tln-plugin.check here: the HTTP request has no HostCaller;
// validation happens on activation).
func (h *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	if req.Name == "" || req.GroupID == "" || req.TlnSource == "" {
		writeErr(w, http.StatusBadRequest, "name, group_id and tln_source are required")
		return
	}
	a, err := h.mgr.Create(r.Context(), agent.Agent{
		Name:        req.Name,
		Description: req.Description,
		GroupID:     req.GroupID,
		EntityID:    req.EntityID,
		TlnSource:   req.TlnSource,
		Enabled:     req.Enabled,
		Triggers:    req.Triggers,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a.Summary())
}

// updateRequest is the body of PUT /v1/agents/{id}. Fields are optional:
// send tln_source to re-save the program (e.g. after editing slots), enabled
// to activate/pause. group_id scopes the lookup.
type updateRequest struct {
	GroupID   string          `json:"group_id"`
	TlnSource *string         `json:"tln_source"`
	Enabled   *bool           `json:"enabled"`
	Triggers  []agent.Trigger `json:"triggers"`
}

// handleUpdate serves PUT /v1/agents/{id} — re-save a draft's Tln and/or flip
// its enabled flag. The write counterpart used by the wizard when a draft is
// edited or activated.
func (h *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	id := r.PathValue("id")
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body")
		return
	}
	var req updateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON")
		return
	}
	if req.GroupID == "" {
		writeErr(w, http.StatusBadRequest, "group_id is required")
		return
	}

	var a agent.Agent
	switch {
	case req.TlnSource != nil:
		a, err = h.mgr.Update(r.Context(), req.GroupID, id, *req.TlnSource, req.Triggers)
	case req.Triggers != nil:
		// Triggers-only update (e.g. the wizard's "When" step re-saving the
		// schedule/event without touching the program): keep the stored Tln.
		var cur agent.Agent
		if cur, err = h.mgr.Get(r.Context(), req.GroupID, id); err == nil {
			a, err = h.mgr.Update(r.Context(), req.GroupID, id, cur.TlnSource, req.Triggers)
		}
	default:
		a, err = h.mgr.Get(r.Context(), req.GroupID, id)
	}
	if err == nil && req.Enabled != nil {
		a, err = h.mgr.SetEnabled(r.Context(), req.GroupID, id, *req.Enabled)
	}
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "agent not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.Summary())
}

// handleRuns serves GET /v1/agents/{id}/runs — the agent's action history,
// newest first. This is what the runs table already records on every tick /
// webhook / schedule fire (trigger, status, the event that fired it, and what
// it did). group_id scopes the lookup so one group can't read another's runs;
// optional limit caps the count (default 50). It backs both the roster's
// activity view and the "chat with your workflow" grounding.
func (h *server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	id := r.PathValue("id")
	groupID := r.URL.Query().Get("group_id")
	if groupID == "" {
		writeErr(w, http.StatusBadRequest, "group_id is required")
		return
	}
	// Verify the agent exists in this group before exposing its runs.
	a, err := h.mgr.Get(r.Context(), groupID, id)
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "agent not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	limit := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, convErr := strconv.Atoi(l); convErr == nil {
			limit = n
		}
	}
	runs, err := h.mgr.ListRuns(r.Context(), a.ID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runs == nil {
		runs = []agent.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (h *server) handleHook(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
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
func (h *server) authorized(r *http.Request) bool {
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
