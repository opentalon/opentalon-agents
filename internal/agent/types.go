// Package agent holds the domain model for persistent, LLM-authored Talon
// workflow agents and the CRUD manager over the store.
package agent

import (
	"encoding/json"
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
// "manual" | "schedule" | "poll" | "webhook". Cron/Config carry the
// type-specific parameters; they are round-tripped verbatim in Phase 1.
type Trigger struct {
	Type   string         `json:"type"`
	Cron   string         `json:"cron,omitempty"`
	Config map[string]any `json:"config,omitempty"`
}

// TriggerType values.
const (
	TriggerManual   = "manual"
	TriggerSchedule = "schedule"
	TriggerPoll     = "poll"
	TriggerWebhook  = "webhook"
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
// action call.
type RunContext struct {
	GroupID  string
	EntityID string
}
