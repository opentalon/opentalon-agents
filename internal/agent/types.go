// Package agent holds the domain model for persistent, LLM-authored Tln
// workflow agents and the CRUD manager over the store.
package agent

import (
	"encoding/json"
	"fmt"
	"time"
)

// Agent is one persistent automation: a stored Tln program plus the
// triggers that fire it. In Phase 1 only manual/llm `run` is wired;
// schedule/poll/webhook triggers are stored but not yet acted on.
type Agent struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	GroupID     string    `json:"group_id"`
	EntityID    string    `json:"entity_id,omitempty"`
	TlnSource   string    `json:"tln_source"`
	Triggers    []Trigger `json:"triggers,omitempty"`
	Enabled     bool      `json:"enabled"`
	// Autonomy is how much the workflow may do on its own:
	// "notify" | "ask" | "act". Set by the host wizard; defaults to "ask".
	Autonomy string `json:"autonomy,omitempty"`
	// Config is an opaque JSON blob of the host wizard's structured selections
	// (template key + slot values), stored verbatim and echoed back so the host
	// can rehydrate its editor. The plugin never interprets or queries it; the
	// executable artifact is TlnSource.
	Config    string    `json:"config,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Trigger describes when an agent should fire. Type is one of
// "manual" | "schedule" | "poll" | "webhook". Cron carries the schedule;
// Config carries the type-specific payload (poll/webhook) as raw JSON,
// decoded via the typed accessors below.
type Trigger struct {
	Type   string          `json:"type"`
	Cron   string          `json:"cron,omitempty"`   // type == schedule
	Config json.RawMessage `json:"config,omitempty"` // type-specific payload (e.g. PollConfig)
}

// Autonomy values — how much a workflow may do on its own.
const (
	AutonomyNotify  = "notify" // report findings only, never act
	AutonomyAsk     = "ask"    // propose the action, wait for the user's OK
	AutonomyAct     = "act"    // act on its own, then report
	AutonomyDefault = AutonomyAsk
)

// TriggerType values.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
	TriggerPoll     = "poll"
	TriggerWebhook  = "webhook"
	TriggerEvent    = "event"
)

// PollConfig is the `config` payload of a poll trigger: which MCP tool to
// call, how often, and how to turn its response into a fact. The engine
// (Phase 2) reads it each tick; in Phase 1 it is only stored/validated.
type PollConfig struct {
	Server    string            `json:"server"`               // MCP server name
	Tool      string            `json:"tool"`                 // MCP tool name
	Args      map[string]string `json:"args,omitempty"`       // static tool args (e.g. {"barcode":"ABC-123"})
	Interval  string            `json:"interval"`             // Go duration, e.g. "5m"
	ItemsPath string            `json:"items_path,omitempty"` // dot-path to a list; set for multi-entity watches (value_path/id_field are then per-item)
	ValuePath string            `json:"value_path"`           // dot-path to the watched value (in the response, or in each item)
	IDField   string            `json:"id_field,omitempty"`   // dot-path to the entity's external id
	Attribute string            `json:"attribute"`            // fact attribute name, e.g. "current_stock"
}

// Poll decodes the trigger's Config as a PollConfig. It errors if the
// trigger is not a poll trigger or the payload is malformed.
func (t Trigger) Poll() (*PollConfig, error) {
	if t.Type != TriggerPoll {
		return nil, fmt.Errorf("trigger is %q, not a poll trigger", t.Type)
	}
	var c PollConfig
	if err := json.Unmarshal(t.Config, &c); err != nil {
		return nil, fmt.Errorf("decode poll config: %w", err)
	}
	return &c, nil
}

// IntervalDuration parses the poll interval.
func (p PollConfig) IntervalDuration() (time.Duration, error) {
	return time.ParseDuration(p.Interval)
}

// PollTrigger returns the agent's first poll trigger config, if any.
func (a Agent) PollTrigger() (*PollConfig, bool) {
	for _, t := range a.Triggers {
		if t.Type == TriggerPoll {
			if c, err := t.Poll(); err == nil {
				return c, true
			}
		}
	}
	return nil, false
}

// WebhookConfig is the `config` payload of a webhook trigger. An external
// system POSTs a JSON body to /v1/hooks/<agent> (authenticated by the
// caller's bearer token); the body is mapped to a fact
// (value_path/id_field/attribute, same as poll) and evaluated on the next
// tick.
type WebhookConfig struct {
	ValuePath string `json:"value_path"`         // dot-path to the watched value in the POST body
	IDField   string `json:"id_field,omitempty"` // dot-path to the entity id
	Attribute string `json:"attribute"`          // fact attribute name
}

// Webhook decodes the trigger's Config as a WebhookConfig.
func (t Trigger) Webhook() (*WebhookConfig, error) {
	if t.Type != TriggerWebhook {
		return nil, fmt.Errorf("trigger is %q, not a webhook trigger", t.Type)
	}
	var c WebhookConfig
	if err := json.Unmarshal(t.Config, &c); err != nil {
		return nil, fmt.Errorf("decode webhook config: %w", err)
	}
	return &c, nil
}

// WebhookTrigger returns the agent's first webhook trigger config, if any.
func (a Agent) WebhookTrigger() (*WebhookConfig, bool) {
	for _, t := range a.Triggers {
		if t.Type == TriggerWebhook {
			if c, err := t.Webhook(); err == nil {
				return c, true
			}
		}
	}
	return nil, false
}

// EventConfig is the `config` payload of an event trigger. Unlike a webhook
// trigger (which maps a POST body to a fact and evaluates detect rules), an
// event trigger names a Timly domain event (e.g. "item_created"); when that
// event fires, the agent's workflow is RUN with the event payload. The
// name-to-agent match happens in the /v1/events fan-out.
type EventConfig struct {
	Event string `json:"event"` // domain event name, e.g. "item_created"
}

// Event decodes the trigger's Config as an EventConfig.
func (t Trigger) Event() (*EventConfig, error) {
	if t.Type != TriggerEvent {
		return nil, fmt.Errorf("trigger is %q, not an event trigger", t.Type)
	}
	var c EventConfig
	if err := json.Unmarshal(t.Config, &c); err != nil {
		return nil, fmt.Errorf("decode event config: %w", err)
	}
	return &c, nil
}

// EventTrigger returns the agent's first event trigger config, if any.
func (a Agent) EventTrigger() (*EventConfig, bool) {
	for _, t := range a.Triggers {
		if t.Type == TriggerEvent {
			if c, err := t.Event(); err == nil {
				return c, true
			}
		}
	}
	return nil, false
}

// PendingEvent is a queued webhook delivery awaiting the next tick, stored
// in the pending_events table. The HTTP handler that receives a webhook
// has no HostCaller, so it can only enqueue; the tick drains and evaluates.
type PendingEvent struct {
	ID         string          `json:"id"`
	AgentID    string          `json:"agent_id"`
	Kind       string          `json:"kind"` // "facts"
	Payload    json.RawMessage `json:"payload"`
	ReceivedAt time.Time       `json:"received_at"`
	// IdempotencyKey, when set, dedupes duplicate deliveries: a second enqueue
	// with the same key is silently dropped. Empty means no dedup (stored NULL).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// PendingEvent kinds.
const (
	// EventKindFacts: map the payload to a fact and evaluate detect rules
	// (webhook/poll reactive path).
	EventKindFacts = "facts"
	// EventKindRun: run the agent's workflow with the payload (event triggers).
	EventKindRun = "run"
)

// Run is one execution of an agent.
type Run struct {
	ID          string          `json:"id"`
	AgentID     string          `json:"agent_id"`
	TriggerType string          `json:"trigger_type"` // manual|llm|schedule|poll|webhook
	Status      string          `json:"status"`       // queued|running|completed|failed
	Event       json.RawMessage `json:"event,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	QueuedAt    time.Time       `json:"queued_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
}

// Run status values.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// RunContext carries the caller identity the host injects into each
// action call. SessionID is the caller's packed session key, captured on
// create/update so an escalation turn can be addressed back to it.
// ChannelID/ConversationID/SenderID are the creator's delivery context,
// captured on the same calls so a notification can be pushed at fire time
// without querying the originating session.
type RunContext struct {
	GroupID        string
	EntityID       string
	SessionID      string
	ChannelID      string
	ConversationID string
	SenderID       string
}

// Delivery returns the creator's delivery target from the run context.
func (rc RunContext) Delivery() DeliveryTarget {
	return DeliveryTarget{
		SessionID:      rc.SessionID,
		ChannelID:      rc.ChannelID,
		ConversationID: rc.ConversationID,
		SenderID:       rc.SenderID,
	}
}

// AgentSummary is the list-view of an agent — everything but the full
// Tln source. Returned by the query API.
type AgentSummary struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	GroupID      string    `json:"group_id"`
	EntityID     string    `json:"entity_id,omitempty"`
	Enabled      bool      `json:"enabled"`
	Autonomy     string    `json:"autonomy,omitempty"`
	TriggerTypes []string  `json:"trigger_types"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Summary returns the agent's list-view (omits the Tln source).
func (a Agent) Summary() AgentSummary {
	types := make([]string, 0, len(a.Triggers))
	for _, t := range a.Triggers {
		types = append(types, t.Type)
	}
	return AgentSummary{
		ID: a.ID, Name: a.Name, Description: a.Description,
		GroupID: a.GroupID, EntityID: a.EntityID, Enabled: a.Enabled,
		Autonomy: a.Autonomy, TriggerTypes: types, UpdatedAt: a.UpdatedAt,
	}
}

// AgentFilter selects agents for QueryAgents. Empty fields are ignored;
// set fields are AND-combined.
type AgentFilter struct {
	GroupID      string
	EntityID     string // comma-separated → matches any (IN); single value → exact
	NameContains string // case-insensitive substring match on name
	Enabled      *bool
	Autonomy     string // comma-separated → matches any of "notify" | "ask" | "act" (IN)
	// SortBy/SortDir order the result via orderClause (whitelisted column +
	// asc/desc); empty = newest-first (created_at DESC).
	SortBy  string
	SortDir string
	// Limit/Offset paginate the result. Limit <= 0 means "no pagination"
	// (return every match); Offset is ignored then. CountAgents applies the
	// same WHERE while ignoring Limit/Offset, for the total.
	Limit  int
	Offset int
}

// AgentState is the restart-safe watcher state for one agent (Phase 2),
// stored one row per agent in the agent_state table.
//
//   - FactsSnapshot is the Tln Session snapshot ({"<int>":{attr:val}}),
//     carried between ticks so an unchanged value fires nothing and a
//     restart replays without re-firing.
//   - EntityMap maps external ids (e.g. a barcode) to the small integer
//     entity ids Tln snapshots are keyed by. It MUST persist so the
//     same external entity keeps the same int across ticks/restarts.
//   - NextPollAt / NextCronAt are the due-times the engine schedules.
//   - ConsecutiveFailures drives poll backoff.
type AgentState struct {
	AgentID             string          `json:"agent_id"`
	FactsSnapshot       json.RawMessage `json:"facts_snapshot,omitempty"`
	EntityMap           map[string]int  `json:"entity_map,omitempty"`
	NextPollAt          *time.Time      `json:"next_poll_at,omitempty"`
	NextCronAt          *time.Time      `json:"next_cron_at,omitempty"`
	ConsecutiveFailures int             `json:"consecutive_failures"`
}
