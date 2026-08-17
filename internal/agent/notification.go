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
	// Recipients selects WHO is notified. Empty means the creator (the
	// historical, and default, behavior). See Recipient.
	Recipients []Recipient `json:"recipients,omitempty"`
	// Channels selects HOW it is delivered — any of "in_app" / "email".
	// Empty means in-app only.
	Channels []string `json:"channels,omitempty"`
}

// Recipient names a delivery audience by KIND, never by address — the address
// is resolved host-side at fire time, so no chat id / email ever appears in the
// Tln source or the LLM context.
//
//   - "creator" (alias "me"): the conversation the agent was authored in (the
//     stored DeliveryTarget). This is the default when no recipients are given.
//   - "responsible": the person responsible for each fired item, resolved per
//     item at fire time from the item's identity. Requires the trigger to carry
//     an entity id (id_field), so there is an item to resolve against.
//   - "role": a named role or team (e.g. "procurement"); the host resolves its
//     members. Requires Role.
type Recipient struct {
	Kind string `json:"kind"`
	Role string `json:"role,omitempty"`
}

// Recipient kinds.
const (
	RecipientCreator     = "creator"
	RecipientMe          = "me" // alias of creator
	RecipientResponsible = "responsible"
	RecipientRole        = "role"
)

// Delivery channels.
const (
	ChannelInApp = "in_app"
	ChannelEmail = "email"
)

// IsCreator reports whether the recipient targets the creator's conversation
// ("creator" or its alias "me").
func (r Recipient) IsCreator() bool {
	return r.Kind == RecipientCreator || r.Kind == RecipientMe
}

// EffectiveRecipients returns the recipients to deliver to, defaulting to the
// creator when none are configured (the historical behavior).
func (s NotifySpec) EffectiveRecipients() []Recipient {
	if len(s.Recipients) == 0 {
		return []Recipient{{Kind: RecipientCreator}}
	}
	return s.Recipients
}

// EffectiveChannels returns the delivery channels, defaulting to in-app.
func (s NotifySpec) EffectiveChannels() []string {
	if len(s.Channels) == 0 {
		return []string{ChannelInApp}
	}
	return s.Channels
}

// NeedsCreatorTarget reports whether delivering this spec requires a stored
// creator conversation — true when any effective recipient is the creator. A
// spec that targets only responsible-person/role needs no creator address, so
// it can be enabled from a non-interactive context.
func (s NotifySpec) NeedsCreatorTarget() bool {
	for _, r := range s.EffectiveRecipients() {
		if r.IsCreator() {
			return true
		}
	}
	return false
}

// Validate checks the recipient kinds, role presence, and channel names so an
// authoring mistake is rejected up front rather than silently dropping a
// recipient at fire time.
func (s NotifySpec) Validate() error {
	for _, r := range s.Recipients {
		switch r.Kind {
		case RecipientCreator, RecipientMe, RecipientResponsible:
			if r.Role != "" {
				return fmt.Errorf("notify recipient %q must not set a role", r.Kind)
			}
		case RecipientRole:
			if strings.TrimSpace(r.Role) == "" {
				return fmt.Errorf("notify recipient of kind \"role\" requires a role name")
			}
		default:
			return fmt.Errorf("unknown notify recipient kind %q (want creator|me|responsible|role)", r.Kind)
		}
	}
	for _, c := range s.Channels {
		if c != ChannelInApp && c != ChannelEmail {
			return fmt.Errorf("unknown notify channel %q (want in_app|email)", c)
		}
	}
	return nil
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
	AgentID    string
	Enabled    bool
	Template   string
	Target     DeliveryTarget
	Recipients []Recipient
	Channels   []string
}

// Spec reconstructs the NotifySpec captured in this row (for the fan-out
// defaults on EffectiveRecipients/EffectiveChannels).
func (n Notification) Spec() NotifySpec {
	return NotifySpec{Enabled: n.Enabled, Template: n.Template, Recipients: n.Recipients, Channels: n.Channels}
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
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// SaveNotification upserts an agent's notification config and delivery target.
// Each target field is only overwritten when the caller supplies a non-empty
// value, so an update from a non-interactive path (no injected context) keeps
// the target captured at create time.
func (m *Manager) SaveNotification(ctx context.Context, agentID string, target DeliveryTarget, spec NotifySpec) error {
	recipients, err := marshalRecipients(spec.Recipients)
	if err != nil {
		return err
	}
	channels, err := marshalChannels(spec.Channels)
	if err != nil {
		return err
	}
	q := m.db.Dialect.Rebind(`INSERT INTO agent_notifications
		(agent_id, enabled, template, session_id, channel_id, conversation_id, sender_id, recipients_json, channels_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			enabled = excluded.enabled,
			template = excluded.template,
			session_id = CASE WHEN excluded.session_id = '' THEN agent_notifications.session_id ELSE excluded.session_id END,
			channel_id = CASE WHEN excluded.channel_id = '' THEN agent_notifications.channel_id ELSE excluded.channel_id END,
			conversation_id = CASE WHEN excluded.conversation_id = '' THEN agent_notifications.conversation_id ELSE excluded.conversation_id END,
			sender_id = CASE WHEN excluded.sender_id = '' THEN agent_notifications.sender_id ELSE excluded.sender_id END,
			recipients_json = CASE WHEN excluded.recipients_json = '' THEN agent_notifications.recipients_json ELSE excluded.recipients_json END,
			channels_json = CASE WHEN excluded.channels_json = '' THEN agent_notifications.channels_json ELSE excluded.channels_json END`)
	_, err = m.db.SQL().ExecContext(ctx, q,
		agentID, boolToInt(spec.Enabled), spec.Template,
		target.SessionID, target.ChannelID, target.ConversationID, target.SenderID,
		recipients, channels)
	if err != nil {
		return fmt.Errorf("notification save: %w", err)
	}
	return nil
}

// marshalRecipients serializes the recipient list for storage. An empty list
// stores "" (not "[]") so the SaveNotification upsert's CASE-WHEN-empty
// preserves any previously stored recipients on a bare re-enable.
func marshalRecipients(rs []Recipient) (string, error) {
	if len(rs) == 0 {
		return "", nil
	}
	b, err := json.Marshal(rs)
	if err != nil {
		return "", fmt.Errorf("marshal recipients: %w", err)
	}
	return string(b), nil
}

func marshalChannels(cs []string) (string, error) {
	if len(cs) == 0 {
		return "", nil
	}
	b, err := json.Marshal(cs)
	if err != nil {
		return "", fmt.Errorf("marshal channels: %w", err)
	}
	return string(b), nil
}

// GetNotification returns an agent's notification row. found is false when the
// agent never opted in (no row) — callers treat that as "notifications off".
func (m *Manager) GetNotification(ctx context.Context, agentID string) (Notification, bool, error) {
	q := m.db.Dialect.Rebind(`SELECT enabled, template, session_id, channel_id, conversation_id, sender_id, recipients_json, channels_json
		FROM agent_notifications WHERE agent_id = ?`)
	var (
		n          = Notification{AgentID: agentID}
		enabled    int
		recipients string
		channels   string
	)
	err := m.db.SQL().QueryRowContext(ctx, q, agentID).Scan(
		&enabled, &n.Template, &n.Target.SessionID, &n.Target.ChannelID,
		&n.Target.ConversationID, &n.Target.SenderID, &recipients, &channels)
	if errors.Is(err, sql.ErrNoRows) {
		return Notification{}, false, nil
	}
	if err != nil {
		return Notification{}, false, fmt.Errorf("notification get: %w", err)
	}
	n.Enabled = enabled != 0
	if recipients != "" {
		if err := json.Unmarshal([]byte(recipients), &n.Recipients); err != nil {
			return Notification{}, false, fmt.Errorf("notification get: decode recipients: %w", err)
		}
	}
	if channels != "" {
		if err := json.Unmarshal([]byte(channels), &n.Channels); err != nil {
			return Notification{}, false, fmt.Errorf("notification get: decode channels: %w", err)
		}
	}
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
