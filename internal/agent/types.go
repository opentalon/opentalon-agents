// Package agent holds the domain model for persistent, LLM-authored Talon
// workflow agents and the CRUD manager over the store.
package agent

import (
	"encoding/json"
	"fmt"
	"time"
)

// Agent is one persistent automation: a stored Talon program plus the
// triggers that fire it. In Phase 1 only manual/llm `run` is wired;
// schedule/poll/webhook triggers are stored but not yet acted on.
type Agent struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	GroupID     string    `json:"group_id"`
	EntityID    string    `json:"entity_id,omitempty"`
	TalonSource string    `json:"talon_source"`
	Triggers    []Trigger `json:"triggers,omitempty"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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

// TriggerType values.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
	TriggerPoll     = "poll"
	TriggerWebhook  = "webhook"
)

// PollConfig is the `config` payload of a poll trigger: which MCP tool to
// call, how often, and how to turn its response into a fact. The engine
// (Phase 2) reads it each tick; in Phase 1 it is only stored/validated.
type PollConfig struct {
	Server    string            `json:"server"`             // MCP server name
	Tool      string            `json:"tool"`               // MCP tool name
	Args      map[string]string `json:"args,omitempty"`     // static tool args (e.g. {"barcode":"ABC-123"})
	Interval  string            `json:"interval"`           // Go duration, e.g. "5m"
	ValuePath string            `json:"value_path"`         // dot-path to the watched value in the response
	IDField   string            `json:"id_field,omitempty"` // dot-path to the entity's external id
	Attribute string            `json:"attribute"`          // fact attribute name, e.g. "current_stock"
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
// action call.
type RunContext struct {
	GroupID  string
	EntityID string
}

// AgentState is the restart-safe watcher state for one agent (Phase 2),
// stored one row per agent in the agent_state table.
//
//   - FactsSnapshot is the Talon Session snapshot ({"<int>":{attr:val}}),
//     carried between ticks so an unchanged value fires nothing and a
//     restart replays without re-firing.
//   - EntityMap maps external ids (e.g. a barcode) to the small integer
//     entity ids Talon snapshots are keyed by. It MUST persist so the
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
