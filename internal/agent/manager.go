package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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
