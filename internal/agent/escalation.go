package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EscalationSpec is the author-supplied escalation config for an agent — the
// opt-in decision plus its bounds. It is parsed from the `escalate` argument of
// the create/update actions and stored in the agent_escalations side table.
//
// Escalation is the hybrid mode: detection stays deterministic (the watcher
// fires exactly as before), but when it fires an LLM reasoning turn is started
// in the creator's session so the assistant can investigate, decide, and ask
// the user what to do. It costs tokens and is nondeterministic, so it is
// opt-in per agent and rate-limited.
type EscalationSpec struct {
	Enabled bool `json:"enabled"`
	// PromptTemplate optionally overrides the synthesized seed prompt. It may
	// use {{placeholders}}: {{agent_name}}, {{description}}, {{firings}},
	// {{facts}}. Empty uses the built-in template.
	PromptTemplate string `json:"prompt_template,omitempty"`
	// MaxPerWindow / WindowSeconds bound how often this agent may escalate. 0
	// means "use the plugin config default". The bound is belt-and-suspenders on
	// top of the edge-triggered firing (an `on change` block already fires once
	// per crossing, not every tick).
	MaxPerWindow  int `json:"max_per_window,omitempty"`
	WindowSeconds int `json:"window_seconds,omitempty"`
}

// Escalation is one agent_escalations row: the config plus the target session
// and the rolling rate-limit state.
type Escalation struct {
	AgentID        string
	SessionID      string
	Enabled        bool
	PromptTemplate string
	MaxPerWindow   int
	WindowSeconds  int
	FireCount      int
	WindowStart    *time.Time
}

// ParseEscalationSpec decodes the `escalate` action argument. It accepts a JSON
// object ({"enabled":true,...}) or the bare shorthands "true"/"false". An empty
// string means "not provided" and returns (nil, nil) so callers can leave any
// existing config untouched.
func ParseEscalationSpec(s string) (*EscalationSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	switch strings.ToLower(s) {
	case "true":
		return &EscalationSpec{Enabled: true}, nil
	case "false":
		return &EscalationSpec{Enabled: false}, nil
	}
	var spec EscalationSpec
	if err := json.Unmarshal([]byte(s), &spec); err != nil {
		return nil, fmt.Errorf("escalate must be a JSON object like {\"enabled\":true} or \"true\"/\"false\": %w", err)
	}
	if spec.MaxPerWindow < 0 || spec.WindowSeconds < 0 {
		return nil, fmt.Errorf("escalate max_per_window and window_seconds must be non-negative")
	}
	return &spec, nil
}

// SaveEscalation upserts an agent's escalation config and target session,
// preserving the rolling rate-limit state (fire_count/window_start) on an
// existing row — re-saving config (e.g. on update) must not reset the window.
// A blank sessionID leaves any previously stored session untouched.
func (m *Manager) SaveEscalation(ctx context.Context, agentID, sessionID string, spec EscalationSpec) error {
	// COALESCE keeps the prior session_id when the caller passes "" (e.g. a
	// control-plane update with no session in context).
	q := m.db.Dialect.Rebind(`INSERT INTO agent_escalations
		(agent_id, session_id, enabled, prompt_template, max_per_window, window_seconds, fire_count, window_start)
		VALUES (?, ?, ?, ?, ?, ?, 0, NULL)
		ON CONFLICT(agent_id) DO UPDATE SET
			session_id = CASE WHEN excluded.session_id = '' THEN agent_escalations.session_id ELSE excluded.session_id END,
			enabled = excluded.enabled,
			prompt_template = excluded.prompt_template,
			max_per_window = excluded.max_per_window,
			window_seconds = excluded.window_seconds`)
	_, err := m.db.SQL().ExecContext(ctx, q,
		agentID, sessionID, boolToInt(spec.Enabled), spec.PromptTemplate, spec.MaxPerWindow, spec.WindowSeconds)
	if err != nil {
		return fmt.Errorf("escalation save: %w", err)
	}
	return nil
}

// GetEscalation returns an agent's escalation row. found is false when the
// agent never opted in (no row) — callers treat that as "escalation off".
func (m *Manager) GetEscalation(ctx context.Context, agentID string) (Escalation, bool, error) {
	q := m.db.Dialect.Rebind(`SELECT session_id, enabled, prompt_template, max_per_window,
		window_seconds, fire_count, window_start FROM agent_escalations WHERE agent_id = ?`)
	var (
		e       = Escalation{AgentID: agentID}
		enabled int
		ws      sql.NullString
	)
	err := m.db.SQL().QueryRowContext(ctx, q, agentID).Scan(
		&e.SessionID, &enabled, &e.PromptTemplate, &e.MaxPerWindow, &e.WindowSeconds, &e.FireCount, &ws)
	if errors.Is(err, sql.ErrNoRows) {
		return Escalation{}, false, nil
	}
	if err != nil {
		return Escalation{}, false, fmt.Errorf("escalation get: %w", err)
	}
	e.Enabled = enabled != 0
	e.WindowStart = parseNullTime(ws)
	return e, true, nil
}

// SaveEscalationState persists just the rolling rate-limit counters.
func (m *Manager) SaveEscalationState(ctx context.Context, agentID string, fireCount int, windowStart *time.Time) error {
	q := m.db.Dialect.Rebind(`UPDATE agent_escalations SET fire_count = ?, window_start = ? WHERE agent_id = ?`)
	if _, err := m.db.SQL().ExecContext(ctx, q, fireCount, nullTime(windowStart), agentID); err != nil {
		return fmt.Errorf("escalation state save: %w", err)
	}
	return nil
}

// DeleteEscalation removes an agent's escalation row (called when the agent is
// deleted).
func (m *Manager) DeleteEscalation(ctx context.Context, agentID string) error {
	q := m.db.Dialect.Rebind(`DELETE FROM agent_escalations WHERE agent_id = ?`)
	if _, err := m.db.SQL().ExecContext(ctx, q, agentID); err != nil {
		return fmt.Errorf("escalation delete: %w", err)
	}
	return nil
}
