package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"strings"

	"github.com/google/uuid"
	"github.com/opentalon/opentalon-agents/internal/store"
)

// ErrNotFound is returned when no agent matches the given id/name in the
// caller's group.
var ErrNotFound = errors.New("agent not found")

// Manager provides group-scoped CRUD for agents and their runs.
type Manager struct {
	db *store.DB
}

// NewManager returns a manager backed by the given store.
func NewManager(db *store.DB) *Manager { return &Manager{db: db} }

const timeFmt = time.RFC3339

// Create stores a new agent, generating its id and timestamps. The
// caller is responsible for having validated TalonSource first.
func (m *Manager) Create(ctx context.Context, a Agent) (Agent, error) {
	a.ID = uuid.NewString()
	now := time.Now().UTC()
	a.CreatedAt, a.UpdatedAt = now, now
	if a.Triggers == nil {
		a.Triggers = []Trigger{}
	}
	triggers, err := json.Marshal(a.Triggers)
	if err != nil {
		return Agent{}, fmt.Errorf("agent create: encode triggers: %w", err)
	}
	q := m.db.Dialect.Rebind(`INSERT INTO agents
		(id, name, description, group_id, entity_id, talon_source, triggers_json, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	_, err = m.db.SQL().ExecContext(ctx, q,
		a.ID, a.Name, a.Description, a.GroupID, a.EntityID, a.TalonSource,
		string(triggers), boolToInt(a.Enabled), now.Format(timeFmt), now.Format(timeFmt))
	if err != nil {
		return Agent{}, fmt.Errorf("agent create: %w", err)
	}
	return a, nil
}

// List returns all agents in the group, newest first.
func (m *Manager) List(ctx context.Context, groupID string) ([]Agent, error) {
	q := m.db.Dialect.Rebind(`SELECT id, name, description, group_id, entity_id, talon_source,
		triggers_json, enabled, created_at, updated_at FROM agents
		WHERE group_id = ? ORDER BY created_at DESC`)
	rows, err := m.db.SQL().QueryContext(ctx, q, groupID)
	if err != nil {
		return nil, fmt.Errorf("agent list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get resolves an agent by id or name within the group.
func (m *Manager) Get(ctx context.Context, groupID, idOrName string) (Agent, error) {
	q := m.db.Dialect.Rebind(`SELECT id, name, description, group_id, entity_id, talon_source,
		triggers_json, enabled, created_at, updated_at FROM agents
		WHERE group_id = ? AND (id = ? OR name = ?) LIMIT 1`)
	row := m.db.SQL().QueryRowContext(ctx, q, groupID, idOrName, idOrName)
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// Update overwrites an agent's Talon source and triggers. The caller must
// have validated newSource first. Returns the updated agent.
func (m *Manager) Update(ctx context.Context, groupID, idOrName, newSource string, triggers []Trigger) (Agent, error) {
	a, err := m.Get(ctx, groupID, idOrName)
	if err != nil {
		return Agent{}, err
	}
	if triggers == nil {
		triggers = a.Triggers
	}
	tj, err := json.Marshal(triggers)
	if err != nil {
		return Agent{}, fmt.Errorf("agent update: encode triggers: %w", err)
	}
	now := time.Now().UTC()
	q := m.db.Dialect.Rebind(`UPDATE agents SET talon_source = ?, triggers_json = ?, updated_at = ?
		WHERE id = ?`)
	if _, err := m.db.SQL().ExecContext(ctx, q, newSource, string(tj), now.Format(timeFmt), a.ID); err != nil {
		return Agent{}, fmt.Errorf("agent update: %w", err)
	}
	a.TalonSource = newSource
	a.Triggers = triggers
	a.UpdatedAt = now
	return a, nil
}

// SetEnabled flips an agent's enabled flag.
func (m *Manager) SetEnabled(ctx context.Context, groupID, idOrName string, enabled bool) (Agent, error) {
	a, err := m.Get(ctx, groupID, idOrName)
	if err != nil {
		return Agent{}, err
	}
	now := time.Now().UTC()
	q := m.db.Dialect.Rebind(`UPDATE agents SET enabled = ?, updated_at = ? WHERE id = ?`)
	if _, err := m.db.SQL().ExecContext(ctx, q, boolToInt(enabled), now.Format(timeFmt), a.ID); err != nil {
		return Agent{}, fmt.Errorf("agent set-enabled: %w", err)
	}
	a.Enabled = enabled
	a.UpdatedAt = now
	return a, nil
}

// Delete removes an agent by id or name within the group.
func (m *Manager) Delete(ctx context.Context, groupID, idOrName string) error {
	a, err := m.Get(ctx, groupID, idOrName)
	if err != nil {
		return err
	}
	q := m.db.Dialect.Rebind(`DELETE FROM agents WHERE id = ?`)
	if _, err := m.db.SQL().ExecContext(ctx, q, a.ID); err != nil {
		return fmt.Errorf("agent delete: %w", err)
	}
	return nil
}

// CreateRun inserts a new run row, generating its id and queued_at.
func (m *Manager) CreateRun(ctx context.Context, r Run) (Run, error) {
	r.ID = uuid.NewString()
	r.QueuedAt = time.Now().UTC()
	if r.Status == "" {
		r.Status = StatusQueued
	}
	q := m.db.Dialect.Rebind(`INSERT INTO runs
		(id, agent_id, trigger_type, status, event_json, result_json, error, queued_at, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	_, err := m.db.SQL().ExecContext(ctx, q,
		r.ID, r.AgentID, r.TriggerType, r.Status, string(r.Event), string(r.Result), r.Error,
		r.QueuedAt.Format(timeFmt), nullTime(r.StartedAt), nullTime(r.FinishedAt))
	if err != nil {
		return Run{}, fmt.Errorf("run create: %w", err)
	}
	return r, nil
}

// FinishRun updates a run's terminal status, result, error, and
// started/finished timestamps.
func (m *Manager) FinishRun(ctx context.Context, r Run) error {
	q := m.db.Dialect.Rebind(`UPDATE runs SET status = ?, result_json = ?, error = ?,
		started_at = ?, finished_at = ? WHERE id = ?`)
	_, err := m.db.SQL().ExecContext(ctx, q,
		r.Status, string(r.Result), r.Error, nullTime(r.StartedAt), nullTime(r.FinishedAt), r.ID)
	if err != nil {
		return fmt.Errorf("run finish: %w", err)
	}
	return nil
}

// ListRuns returns an agent's runs, newest first, capped at limit.
func (m *Manager) ListRuns(ctx context.Context, agentID string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 50
	}
	q := m.db.Dialect.Rebind(`SELECT id, agent_id, trigger_type, status, event_json, result_json,
		error, queued_at, started_at, finished_at FROM runs
		WHERE agent_id = ? ORDER BY queued_at DESC LIMIT ?`)
	rows, err := m.db.SQL().QueryContext(ctx, q, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("run list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetState returns the watcher state for an agent. When no row exists yet
// (the agent has never ticked), it returns a zero state (with AgentID and
// an empty EntityMap) and a nil error — callers treat "no state" as the
// initial state.
func (m *Manager) GetState(ctx context.Context, agentID string) (AgentState, error) {
	q := m.db.Dialect.Rebind(`SELECT facts_snapshot_json, entity_map_json, next_poll_at, next_cron_at,
		consecutive_failures FROM agent_state WHERE agent_id = ?`)
	var (
		snap, em string
		np, nc   sql.NullString
		failures int
	)
	err := m.db.SQL().QueryRowContext(ctx, q, agentID).Scan(&snap, &em, &np, &nc, &failures)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentState{AgentID: agentID, EntityMap: map[string]int{}}, nil
	}
	if err != nil {
		return AgentState{}, fmt.Errorf("agent state get: %w", err)
	}
	st := AgentState{
		AgentID:             agentID,
		ConsecutiveFailures: failures,
		NextPollAt:          parseNullTime(np),
		NextCronAt:          parseNullTime(nc),
		EntityMap:           map[string]int{},
	}
	if snap != "" {
		st.FactsSnapshot = json.RawMessage(snap)
	}
	if em != "" {
		if err := json.Unmarshal([]byte(em), &st.EntityMap); err != nil {
			return AgentState{}, fmt.Errorf("agent state get: decode entity_map: %w", err)
		}
	}
	return st, nil
}

// SaveState upserts an agent's watcher state (one row per agent).
func (m *Manager) SaveState(ctx context.Context, s AgentState) error {
	snap := string(s.FactsSnapshot)
	if snap == "" {
		snap = "{}"
	}
	em, err := json.Marshal(s.EntityMap)
	if err != nil {
		return fmt.Errorf("agent state save: encode entity_map: %w", err)
	}
	if s.EntityMap == nil {
		em = []byte("{}")
	}
	q := m.db.Dialect.Rebind(`INSERT INTO agent_state
		(agent_id, facts_snapshot_json, entity_map_json, next_poll_at, next_cron_at, consecutive_failures)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			facts_snapshot_json = excluded.facts_snapshot_json,
			entity_map_json = excluded.entity_map_json,
			next_poll_at = excluded.next_poll_at,
			next_cron_at = excluded.next_cron_at,
			consecutive_failures = excluded.consecutive_failures`)
	_, err = m.db.SQL().ExecContext(ctx, q,
		s.AgentID, snap, string(em), nullTime(s.NextPollAt), nullTime(s.NextCronAt), s.ConsecutiveFailures)
	if err != nil {
		return fmt.Errorf("agent state save: %w", err)
	}
	return nil
}

// ListEnabledPollDue returns enabled agents that have a poll trigger which
// is due (no state yet, or next_poll_at <= now), across ALL groups — the
// tick is a system-wide, unscoped sweep. Whether a trigger is a poll and
// its due-time are checked in Go against the per-agent state join.
func (m *Manager) ListEnabledPollDue(ctx context.Context, now time.Time) ([]Agent, error) {
	q := m.db.Dialect.Rebind(`SELECT a.id, a.name, a.description, a.group_id, a.entity_id, a.talon_source,
		a.triggers_json, a.enabled, a.created_at, a.updated_at, s.next_poll_at
		FROM agents a LEFT JOIN agent_state s ON s.agent_id = a.id
		WHERE a.enabled = 1`)
	rows, err := m.db.SQL().QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("agent list poll-due: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Agent
	for rows.Next() {
		a, nextPoll, err := scanAgentJoinedTime(rows)
		if err != nil {
			return nil, err
		}
		if _, ok := a.PollTrigger(); !ok {
			continue // no poll trigger — not our concern here
		}
		due := nextPoll == nil || !nextPoll.After(now)
		if due {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// ListEnabledScheduleDue returns enabled agents with a schedule (cron)
// trigger that is due (no state yet, or next_cron_at <= now), across all
// groups. Like the poll sweep, but keyed on next_cron_at.
func (m *Manager) ListEnabledScheduleDue(ctx context.Context, now time.Time) ([]Agent, error) {
	q := m.db.Dialect.Rebind(`SELECT a.id, a.name, a.description, a.group_id, a.entity_id, a.talon_source,
		a.triggers_json, a.enabled, a.created_at, a.updated_at, s.next_cron_at
		FROM agents a LEFT JOIN agent_state s ON s.agent_id = a.id
		WHERE a.enabled = 1`)
	rows, err := m.db.SQL().QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("agent list schedule-due: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Agent
	for rows.Next() {
		a, nextCron, err := scanAgentJoinedTime(rows)
		if err != nil {
			return nil, err
		}
		if _, ok := a.ScheduleTrigger(); !ok {
			continue
		}
		if nextCron == nil || !nextCron.After(now) {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// scanAgentJoinedTime scans the agent columns plus one joined, nullable
// agent_state timestamp (next_poll_at or next_cron_at, per the query).
func scanAgentJoinedTime(s scanner) (Agent, *time.Time, error) {
	var (
		a        Agent
		triggers string
		enabled  int
		created  string
		updated  string
		nextPoll sql.NullString
	)
	if err := s.Scan(&a.ID, &a.Name, &a.Description, &a.GroupID, &a.EntityID, &a.TalonSource,
		&triggers, &enabled, &created, &updated, &nextPoll); err != nil {
		return Agent{}, nil, err
	}
	if triggers != "" {
		if err := json.Unmarshal([]byte(triggers), &a.Triggers); err != nil {
			return Agent{}, nil, fmt.Errorf("agent scan: decode triggers: %w", err)
		}
	}
	a.Enabled = enabled != 0
	a.CreatedAt, _ = time.Parse(timeFmt, created)
	a.UpdatedAt, _ = time.Parse(timeFmt, updated)
	return a, parseNullTime(nextPoll), nil
}

// QueryAgents lists agents matching the filter (all fields AND-combined),
// newest first. Used by the read-only query API.
func (m *Manager) QueryAgents(ctx context.Context, f AgentFilter) ([]Agent, error) {
	q := `SELECT id, name, description, group_id, entity_id, talon_source,
		triggers_json, enabled, created_at, updated_at FROM agents WHERE 1=1`
	var args []any
	if f.GroupID != "" {
		q += " AND group_id = ?"
		args = append(args, f.GroupID)
	}
	if f.EntityID != "" {
		q += " AND entity_id = ?"
		args = append(args, f.EntityID)
	}
	if f.NameContains != "" {
		q += " AND LOWER(name) LIKE ?"
		args = append(args, "%"+strings.ToLower(f.NameContains)+"%")
	}
	if f.Enabled != nil {
		q += " AND enabled = ?"
		args = append(args, boolToInt(*f.Enabled))
	}
	q += " ORDER BY created_at DESC"

	rows, err := m.db.SQL().QueryContext(ctx, m.db.Dialect.Rebind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("agent query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetByID resolves an agent by id across all groups (used by the tick
// engine, which is unscoped).
func (m *Manager) GetByID(ctx context.Context, id string) (Agent, error) {
	q := m.db.Dialect.Rebind(`SELECT id, name, description, group_id, entity_id, talon_source,
		triggers_json, enabled, created_at, updated_at FROM agents WHERE id = ? LIMIT 1`)
	a, err := scanAgent(m.db.SQL().QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// WebhookAgent resolves the enabled, webhook-triggered agent named by
// idOrName and owned by userID (its entity_id). Used by the webhook
// endpoint, which authenticates via a shared bearer secret and scopes the
// lookup by the request's user_id param.
func (m *Manager) WebhookAgent(ctx context.Context, userID, idOrName string) (Agent, error) {
	if userID == "" || idOrName == "" {
		return Agent{}, ErrNotFound
	}
	q := m.db.Dialect.Rebind(`SELECT id, name, description, group_id, entity_id, talon_source,
		triggers_json, enabled, created_at, updated_at FROM agents
		WHERE enabled = 1 AND entity_id = ? AND (id = ? OR name = ?) LIMIT 1`)
	a, err := scanAgent(m.db.SQL().QueryRowContext(ctx, q, userID, idOrName, idOrName))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, err
	}
	if _, ok := a.WebhookTrigger(); !ok {
		return Agent{}, ErrNotFound // exists but isn't webhook-triggered
	}
	return a, nil
}

// EnqueueEvent stores a pending webhook delivery for later draining.
func (m *Manager) EnqueueEvent(ctx context.Context, ev PendingEvent) (PendingEvent, error) {
	ev.ID = uuid.NewString()
	ev.ReceivedAt = time.Now().UTC()
	if ev.Kind == "" {
		ev.Kind = EventKindFacts
	}
	payload := string(ev.Payload)
	if payload == "" {
		payload = "{}"
	}
	q := m.db.Dialect.Rebind(`INSERT INTO pending_events (id, agent_id, kind, payload_json, received_at)
		VALUES (?, ?, ?, ?, ?)`)
	if _, err := m.db.SQL().ExecContext(ctx, q, ev.ID, ev.AgentID, ev.Kind, payload, ev.ReceivedAt.Format(timeFmt)); err != nil {
		return PendingEvent{}, fmt.Errorf("enqueue event: %w", err)
	}
	return ev, nil
}

// ListPendingEvents returns all queued events, oldest first.
func (m *Manager) ListPendingEvents(ctx context.Context) ([]PendingEvent, error) {
	q := m.db.Dialect.Rebind(`SELECT id, agent_id, kind, payload_json, received_at
		FROM pending_events ORDER BY received_at ASC`)
	rows, err := m.db.SQL().QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list pending events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingEvent
	for rows.Next() {
		var (
			ev       PendingEvent
			payload  string
			received string
		)
		if err := rows.Scan(&ev.ID, &ev.AgentID, &ev.Kind, &payload, &received); err != nil {
			return nil, err
		}
		ev.Payload = json.RawMessage(payload)
		ev.ReceivedAt, _ = time.Parse(timeFmt, received)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// DeleteEvent removes a pending event after it has been processed.
func (m *Manager) DeleteEvent(ctx context.Context, id string) error {
	q := m.db.Dialect.Rebind(`DELETE FROM pending_events WHERE id = ?`)
	if _, err := m.db.SQL().ExecContext(ctx, q, id); err != nil {
		return fmt.Errorf("delete event: %w", err)
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanAgent(s scanner) (Agent, error) {
	var (
		a        Agent
		triggers string
		enabled  int
		created  string
		updated  string
	)
	if err := s.Scan(&a.ID, &a.Name, &a.Description, &a.GroupID, &a.EntityID, &a.TalonSource,
		&triggers, &enabled, &created, &updated); err != nil {
		return Agent{}, err
	}
	if triggers != "" {
		if err := json.Unmarshal([]byte(triggers), &a.Triggers); err != nil {
			return Agent{}, fmt.Errorf("agent scan: decode triggers: %w", err)
		}
	}
	a.Enabled = enabled != 0
	a.CreatedAt, _ = time.Parse(timeFmt, created)
	a.UpdatedAt, _ = time.Parse(timeFmt, updated)
	return a, nil
}

func scanRun(s scanner) (Run, error) {
	var (
		r        Run
		event    string
		result   string
		queued   string
		started  sql.NullString
		finished sql.NullString
	)
	if err := s.Scan(&r.ID, &r.AgentID, &r.TriggerType, &r.Status, &event, &result,
		&r.Error, &queued, &started, &finished); err != nil {
		return Run{}, err
	}
	if event != "" {
		r.Event = json.RawMessage(event)
	}
	if result != "" {
		r.Result = json.RawMessage(result)
	}
	r.QueuedAt, _ = time.Parse(timeFmt, queued)
	r.StartedAt = parseNullTime(started)
	r.FinishedAt = parseNullTime(finished)
	return r, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(timeFmt)
}

func parseNullTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(timeFmt, s.String)
	if err != nil {
		return nil
	}
	return &t
}
