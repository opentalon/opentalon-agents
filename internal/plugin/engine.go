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
	res := TickResult{}

	// Drain queued webhook deliveries first (they carry a HostCaller only
	// now, at tick time), then sweep the due poll watchers.
	e.drainPending(ctx, host, now, &res)

	due, err := e.mgr.ListEnabledPollDue(ctx, now)
	if err != nil {
		return res, fmt.Errorf("tick: list due agents: %w", err)
	}
	res.Agents += len(due)
	for _, a := range due {
		fired, terr := e.tickAgent(ctx, host, a, now)
		res.Firings += fired
		if terr != nil {
			res.Errors++
			slog.Warn("opentalon-agents: agent tick failed", "agent", a.ID, "name", a.Name, "error", terr)
		}
	}

	e.sweepSchedules(ctx, host, now, &res)
	return res, nil
}

// sweepSchedules runs every enabled schedule (cron) agent that is due. A
// schedule agent runs its Talon as a one-shot workflow (execute_workflow)
// on its cron cadence — no facts/snapshot involved.
func (e *Engine) sweepSchedules(ctx context.Context, host pkg.HostCaller, now time.Time, res *TickResult) {
	due, err := e.mgr.ListEnabledScheduleDue(ctx, now)
	if err != nil {
		slog.Warn("opentalon-agents: list schedule-due", "error", err)
		return
	}
	res.Agents += len(due)
	for _, a := range due {
		if err := e.scheduleAgent(ctx, host, a, now); err != nil {
			res.Errors++
			slog.Warn("opentalon-agents: scheduled run failed", "agent", a.ID, "name", a.Name, "error", err)
		}
	}
}

// scheduleAgent advances one cron agent. On first sight it just computes
// the next fire time (cron means "at these times", not "now"); when due it
// runs the workflow and reschedules.
func (e *Engine) scheduleAgent(ctx context.Context, host pkg.HostCaller, a agent.Agent, now time.Time) error {
	spec, ok := a.ScheduleTrigger()
	if !ok {
		return nil
	}
	sched, err := agent.ParseCron(spec)
	if err != nil {
		return err // rejected at create time, but guard anyway
	}
	state, err := e.mgr.GetState(ctx, a.ID)
	if err != nil {
		return fmt.Errorf("get state: %w", err)
	}

	// First sight: initialize the next fire time, don't run.
	if state.NextCronAt == nil {
		next := sched.Next(now)
		state.NextCronAt = &next
		return e.mgr.SaveState(ctx, state)
	}
	if state.NextCronAt.After(now) {
		return nil // not due yet
	}

	// Due: run the workflow, then reschedule (regardless of run outcome).
	result, runErr := e.talon.Run(ctx, host, a.TalonSource)
	next := sched.Next(now)
	state.NextCronAt = &next
	if err := e.mgr.SaveState(ctx, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	started := now
	run := agent.Run{AgentID: a.ID, TriggerType: agent.TriggerSchedule, StartedAt: &started, FinishedAt: &started}
	if runErr != nil {
		run.Status = agent.StatusFailed
		run.Error = runErr.Error()
		_, _ = e.mgr.CreateRun(ctx, run)
		return runErr
	}
	run.Status = agent.StatusCompleted
	run.Result = resultJSON(result)
	_, _ = e.mgr.CreateRun(ctx, run)
	return nil
}

// drainPending processes every queued webhook event: map its payload to a
// fact, evaluate the agent reactively, persist, and record a run if it
// fired. Events are deleted after processing (success or failure) to avoid
// a poison-message loop.
func (e *Engine) drainPending(ctx context.Context, host pkg.HostCaller, now time.Time, res *TickResult) {
	events, err := e.mgr.ListPendingEvents(ctx)
	if err != nil {
		slog.Warn("opentalon-agents: list pending events", "error", err)
		return
	}
	for _, ev := range events {
		fired, err := e.applyEvent(ctx, host, ev, now)
		res.Firings += fired
		if err != nil {
			res.Errors++
			slog.Warn("opentalon-agents: pending event failed", "event", ev.ID, "agent", ev.AgentID, "error", err)
		}
		if derr := e.mgr.DeleteEvent(ctx, ev.ID); derr != nil {
			slog.Warn("opentalon-agents: delete pending event", "event", ev.ID, "error", derr)
		}
	}
}

// applyEvent maps a webhook payload to a fact and evaluates the agent.
func (e *Engine) applyEvent(ctx context.Context, host pkg.HostCaller, ev agent.PendingEvent, now time.Time) (int, error) {
	a, err := e.mgr.GetByID(ctx, ev.AgentID)
	if err != nil {
		return 0, fmt.Errorf("load agent: %w", err)
	}
	if !a.Enabled {
		return 0, nil // dropped: agent disabled since the event arrived
	}
	wc, ok := a.WebhookTrigger()
	if !ok {
		return 0, fmt.Errorf("agent %s has no webhook trigger", a.ID)
	}
	var body any
	if err := json.Unmarshal(ev.Payload, &body); err != nil {
		return 0, fmt.Errorf("decode payload: %w", err)
	}
	state, err := e.mgr.GetState(ctx, a.ID)
	if err != nil {
		return 0, fmt.Errorf("get state: %w", err)
	}
	facts, registry, err := agent.MapValue(wc.ValuePath, wc.IDField, wc.Attribute, body, state.EntityMap)
	if err != nil {
		return 0, err
	}
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return 0, err
	}
	evalRes, err := e.talon.Evaluate(ctx, host, a.TalonSource, factsJSON, state.FactsSnapshot)
	if err != nil {
		return 0, err
	}
	state.FactsSnapshot = evalRes.Snapshot
	state.EntityMap = registry
	if err := e.mgr.SaveState(ctx, state); err != nil {
		return 0, fmt.Errorf("save state: %w", err)
	}
	if len(evalRes.Firings) > 0 {
		e.recordRun(ctx, a, agent.TriggerWebhook, factsJSON, evalRes, now)
	}
	return len(evalRes.Firings), nil
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
		e.recordRun(ctx, a, agent.TriggerPoll, factsJSON, evalRes, now)
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
// firings, tagged with the trigger that produced it.
func (e *Engine) recordRun(ctx context.Context, a agent.Agent, triggerType string, factsJSON json.RawMessage, evalRes EvalResult, now time.Time) {
	result, _ := json.Marshal(map[string]any{"firings": evalRes.Firings})
	started := now
	if _, err := e.mgr.CreateRun(ctx, agent.Run{
		AgentID: a.ID, TriggerType: triggerType, Status: agent.StatusCompleted,
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
