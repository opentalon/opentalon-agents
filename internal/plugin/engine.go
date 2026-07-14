package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
)

const maxBackoff = 30 * time.Minute

// Engine drives the autonomous watchers. On each tick it sweeps the agents
// whose poll trigger is due and, for each, polls the source, maps the
// result to facts, evaluates the agent's Talon reactively (via
// talon-plugin), and persists the resulting snapshot / schedule / run.
// It lives here (not in package agent) because it needs the live
// HostCaller and the talon proxy alongside the Manager.
type Engine struct {
	cfg   *config.Config
	mgr   *agent.Manager
	talon talonProxy
}

// NewEngine builds the tick engine.
func NewEngine(cfg *config.Config, mgr *agent.Manager) *Engine {
	return &Engine{cfg: cfg, mgr: mgr, talon: talonProxy{pluginName: cfg.TalonPluginName}}
}

// TickResult is a per-tick summary for logging/observability.
type TickResult struct {
	Agents  int `json:"agents"`  // agents processed this tick
	Firings int `json:"firings"` // on-blocks fired across all agents
	Errors  int `json:"errors"`  // agents whose tick failed
}

// Tick runs one system-wide sweep at the current time.
func (e *Engine) Tick(ctx context.Context, host pkg.HostCaller) (TickResult, error) {
	return e.tickAt(ctx, host, time.Now().UTC())
}

// tickAt is Tick with an injectable clock (for tests).
func (e *Engine) tickAt(ctx context.Context, host pkg.HostCaller, now time.Time) (TickResult, error) {
	due, err := e.mgr.ListEnabledPollDue(ctx, now)
	if err != nil {
		return TickResult{}, fmt.Errorf("tick: list due agents: %w", err)
	}
	res := TickResult{Agents: len(due)}
	for _, a := range due {
		fired, terr := e.tickAgent(ctx, host, a, now)
		res.Firings += fired
		if terr != nil {
			res.Errors++
			slog.Warn("opentalon-agents: agent tick failed", "agent", a.ID, "name", a.Name, "error", terr)
		}
	}
	return res, nil
}

// tickAgent polls, maps, evaluates, and persists a single agent. A poll
// with no firing still updates the snapshot/schedule but records no run —
// a run is recorded only when the agent actually fires (or fails).
func (e *Engine) tickAgent(ctx context.Context, host pkg.HostCaller, a agent.Agent, now time.Time) (int, error) {
	pc, ok := a.PollTrigger()
	if !ok {
		return 0, nil // no poll trigger — nothing to do (shouldn't reach here)
	}
	state, err := e.mgr.GetState(ctx, a.ID)
	if err != nil {
		return 0, fmt.Errorf("get state: %w", err)
	}
	interval := e.pollInterval(pc)

	resp, err := agent.Poll(ctx, host, *pc)
	if err != nil {
		return 0, e.failAgent(ctx, a, state, interval, now, err)
	}
	facts, registry, err := agent.Map(*pc, resp, state.EntityMap)
	if err != nil {
		return 0, e.failAgent(ctx, a, state, interval, now, err)
	}
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return 0, e.failAgent(ctx, a, state, interval, now, err)
	}
	evalRes, err := e.talon.Evaluate(ctx, host, a.TalonSource, factsJSON, state.FactsSnapshot)
	if err != nil {
		return 0, e.failAgent(ctx, a, state, interval, now, err)
	}

	// Success: advance state (snapshot + registry + next poll), reset backoff.
	next := now.Add(interval)
	state.FactsSnapshot = evalRes.Snapshot
	state.EntityMap = registry
	state.ConsecutiveFailures = 0
	state.NextPollAt = &next
	if err := e.mgr.SaveState(ctx, state); err != nil {
		return 0, fmt.Errorf("save state: %w", err)
	}

	if len(evalRes.Firings) > 0 {
		e.recordRun(ctx, a, factsJSON, evalRes, now)
	}
	return len(evalRes.Firings), nil
}

// failAgent records the failure: bumps the backoff, pushes next_poll_at
// out, preserves the snapshot, and records a failed run.
func (e *Engine) failAgent(ctx context.Context, a agent.Agent, state agent.AgentState, interval time.Duration, now time.Time, cause error) error {
	state.ConsecutiveFailures++
	next := now.Add(backoff(interval, state.ConsecutiveFailures))
	state.NextPollAt = &next
	if err := e.mgr.SaveState(ctx, state); err != nil {
		slog.Warn("opentalon-agents: save state after failure", "agent", a.ID, "error", err)
	}
	started := now
	if _, err := e.mgr.CreateRun(ctx, agent.Run{
		AgentID: a.ID, TriggerType: agent.TriggerPoll, Status: agent.StatusFailed,
		Error: cause.Error(), StartedAt: &started, FinishedAt: &started,
	}); err != nil {
		slog.Warn("opentalon-agents: record failed run", "agent", a.ID, "error", err)
	}
	return cause
}

// recordRun stores a completed run capturing the asserted facts and the
// firings.
func (e *Engine) recordRun(ctx context.Context, a agent.Agent, factsJSON json.RawMessage, evalRes EvalResult, now time.Time) {
	result, _ := json.Marshal(map[string]any{"firings": evalRes.Firings})
	started := now
	if _, err := e.mgr.CreateRun(ctx, agent.Run{
		AgentID: a.ID, TriggerType: agent.TriggerPoll, Status: agent.StatusCompleted,
		Event: factsJSON, Result: result, StartedAt: &started, FinishedAt: &started,
	}); err != nil {
		slog.Warn("opentalon-agents: record run", "agent", a.ID, "error", err)
	}
}

// pollInterval parses the trigger interval, clamped up to the configured
// floor; a missing/invalid interval falls back to the floor.
func (e *Engine) pollInterval(pc *agent.PollConfig) time.Duration {
	floor := time.Duration(e.cfg.PollFloorSeconds) * time.Second
	d, err := pc.IntervalDuration()
	if err != nil || d < floor {
		return floor
	}
	return d
}

// backoff is interval * 2^(failures-1), capped at maxBackoff.
func backoff(interval time.Duration, failures int) time.Duration {
	d := interval
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}
