package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// NotifySpec is the author-supplied notification config for an agent: the
// opt-in decision plus an optional message override. It is parsed from the
// `notify` argument of the create/update actions and stored in the
// agent_notifications side table.
//
// Notification is the cheap, model-free counterpart to escalation: when the
// agent fires, the plugin PUSHES a message to the creator's conversation. No
// LLM turn runs, and the destination is never baked into the Tln source —
// it comes from the delivery context the host injected when the agent was
// authored.
type NotifySpec struct {
	Enabled bool `json:"enabled"`
	// Template optionally overrides the synthesized message. It may use
	// {{placeholders}}: {{agent_name}}, {{description}}, {{firings}},
	// {{facts}}, {{result}}, {{trigger}}. Empty uses the built-in text.
	Template string `json:"template,omitempty"`
}

// DeliveryTarget addresses the creator's conversation. SessionID is the host's
// packed session key (it already encodes channel + conversation); ChannelID /
// ConversationID are the explicit pair when the host injects them. SenderID is
// provenance only — it is never used to route.
type DeliveryTarget struct {
	SessionID      string
	ChannelID      string
	ConversationID string
	SenderID       string
}

// Addressable reports whether the target names somewhere to deliver to: either
// a packed session key or an explicit channel+conversation pair.
func (t DeliveryTarget) Addressable() bool {
	return t.SessionID != "" || (t.ChannelID != "" && t.ConversationID != "")
}

// Notification is one agent_notifications row: the config plus the stored
// delivery target.
type Notification struct {
	AgentID  string
	Enabled  bool
	Template string
	Target   DeliveryTarget
}

// ParseNotifySpec decodes the `notify` action argument. It accepts a JSON
// object ({"enabled":true,"template":"..."}) or the bare shorthands
// "true"/"false". An empty string means "not provided" and returns (nil, nil)
// so callers leave any existing config untouched.
func ParseNotifySpec(s string) (*NotifySpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	switch strings.ToLower(s) {
	case "true":
		return &NotifySpec{Enabled: true}, nil
	case "false":
		return &NotifySpec{Enabled: false}, nil
	}
	var spec NotifySpec
	if err := json.Unmarshal([]byte(s), &spec); err != nil {
		return nil, fmt.Errorf("notify must be a JSON object like {\"enabled\":true} or \"true\"/\"false\": %w", err)
	}
	return &spec, nil
}

// SaveNotification upserts an agent's notification config and delivery target.
// Each target field is only overwritten when the caller supplies a non-empty
// value, so an update from a non-interactive path (no injected context) keeps
// the target captured at create time.
func (m *Manager) SaveNotification(ctx context.Context, agentID string, target DeliveryTarget, spec NotifySpec) error {
	q := m.db.Dialect.Rebind(`INSERT INTO agent_notifications
		(agent_id, enabled, template, session_id, channel_id, conversation_id, sender_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			enabled = excluded.enabled,
			template = excluded.template,
			session_id = CASE WHEN excluded.session_id = '' THEN agent_notifications.session_id ELSE excluded.session_id END,
			channel_id = CASE WHEN excluded.channel_id = '' THEN agent_notifications.channel_id ELSE excluded.channel_id END,
			conversation_id = CASE WHEN excluded.conversation_id = '' THEN agent_notifications.conversation_id ELSE excluded.conversation_id END,
			sender_id = CASE WHEN excluded.sender_id = '' THEN agent_notifications.sender_id ELSE excluded.sender_id END`)
	_, err := m.db.SQL().ExecContext(ctx, q,
		agentID, boolToInt(spec.Enabled), spec.Template,
		target.SessionID, target.ChannelID, target.ConversationID, target.SenderID)
	if err != nil {
		return fmt.Errorf("notification save: %w", err)
	}
	return nil
}

// GetNotification returns an agent's notification row. found is false when the
// agent never opted in (no row) — callers treat that as "notifications off".
func (m *Manager) GetNotification(ctx context.Context, agentID string) (Notification, bool, error) {
	q := m.db.Dialect.Rebind(`SELECT enabled, template, session_id, channel_id, conversation_id, sender_id
		FROM agent_notifications WHERE agent_id = ?`)
	var (
		n       = Notification{AgentID: agentID}
		enabled int
	)
	err := m.db.SQL().QueryRowContext(ctx, q, agentID).Scan(
		&enabled, &n.Template, &n.Target.SessionID, &n.Target.ChannelID,
		&n.Target.ConversationID, &n.Target.SenderID)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, false, nil
	}
	if err != nil {
		return Notification{}, false, fmt.Errorf("notification get: %w", err)
	}
	n.Enabled = enabled != 0
	return n, true, nil
}

// DeleteNotification removes an agent's notification row (called when the agent
// is deleted).
func (m *Manager) DeleteNotification(ctx context.Context, agentID string) error {
	q := m.db.Dialect.Rebind(`DELETE FROM agent_notifications WHERE agent_id = ?`)
	if _, err := m.db.SQL().ExecContext(ctx, q, agentID); err != nil {
		return fmt.Errorf("notification delete: %w", err)
	}
	return nil
}
