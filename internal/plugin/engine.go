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

// Engine drives the autonomous watchers. On each tick it sweeps the agents
// whose poll trigger is due and, for each, polls the source, maps the
// result to facts, evaluates the agent's Tln reactively (via
// tln-plugin), and persists the resulting snapshot / schedule / run.
// It lives here (not in package agent) because it needs the live
// HostCaller and the tln proxy alongside the Manager.
type Engine struct {
	cfg    *config.Config
	mgr    *agent.Manager
	tln    tlnProxy
	esc    escalator
	notify notifier
}

// NewEngine builds the tick engine.
func NewEngine(cfg *config.Config, mgr *agent.Manager) *Engine {
	return &Engine{
		cfg:    cfg,
		mgr:    mgr,
		tln:    tlnProxy{pluginName: cfg.TlnPluginName},
		esc:    escalator{pluginName: cfg.EscalationPluginName},
		notify: notifier{pluginName: cfg.NotifyPluginName},
	}
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
// schedule agent runs its Tln as a one-shot workflow (execute_workflow)
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

	// Due: claim this fire atomically before running, so in a cluster only one
	// instance advances next_cron_at and runs the workflow. A loser skips.
	next := sched.Next(now)
	won, err := e.mgr.ClaimScheduleDue(ctx, a.ID, now, next)
	if err != nil {
		return fmt.Errorf("claim schedule: %w", err)
	}
	if !won {
		return nil // another instance took this cron fire
	}

	// Run the workflow (next_cron_at is already advanced by the claim).
	// Carry the agent owner so the workflow's MCP steps act as that user.
	result, runErr := e.tln.Run(ctx, host, a.TlnSource, Identity{EntityID: a.EntityID, GroupID: a.GroupID})

	started := now
	run := agent.Run{AgentID: a.ID, TriggerType: agent.TriggerSchedule, StartedAt: &started, FinishedAt: &started}
	if runErr != nil {
		run.Status = agent.StatusFailed
		run.Error = runErr.Error()
		_, _ = e.mgr.CreateRun(ctx, run)
		e.maybeNotify(ctx, host, a, notifyEvent{Trigger: agent.TriggerSchedule, Error: runErr.Error()}, now)
		return runErr
	}
	run.Status = agent.StatusCompleted
	run.Result = resultJSON(result)
	_, _ = e.mgr.CreateRun(ctx, run)
	e.maybeNotify(ctx, host, a, notifyEvent{Trigger: agent.TriggerSchedule, Result: run.Result}, now)
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
		// Cluster-safe claim: whoever deletes the row owns it. A loser skips
		// (another instance is processing it), preventing double execution.
		// The delete also fulfils the at-most-once contract (events were
		// already removed regardless of outcome, to avoid poison-message loops).
		won, err := e.mgr.ClaimEvent(ctx, ev.ID)
		if err != nil {
			slog.Warn("opentalon-agents: claim pending event", "event", ev.ID, "error", err)
			continue
		}
		if !won {
			continue // another instance claimed it
		}
		fired, err := e.applyEvent(ctx, host, ev, now)
		res.Firings += fired
		if err != nil {
			res.Errors++
			slog.Warn("opentalon-agents: pending event failed", "event", ev.ID, "agent", ev.AgentID, "error", err)
		}
	}
}

// runEventWorkflow runs an event-triggered agent's workflow (imperative steps)
// as the agent owner, records the run, and notifies. The event payload is kept
// as the run's Event for history; passing it into the program as event.* is a
// follow-up (tln has no context-injection option yet).
func (e *Engine) runEventWorkflow(ctx context.Context, host pkg.HostCaller, a agent.Agent, ev agent.PendingEvent, now time.Time) (int, error) {
	result, runErr := e.tln.Run(ctx, host, a.TlnSource, Identity{EntityID: a.EntityID, GroupID: a.GroupID})
	started := now
	run := agent.Run{AgentID: a.ID, TriggerType: agent.TriggerEvent, Event: ev.Payload, StartedAt: &started, FinishedAt: &started}
	if runErr != nil {
		run.Status = agent.StatusFailed
		run.Error = runErr.Error()
		_, _ = e.mgr.CreateRun(ctx, run)
		e.maybeNotify(ctx, host, a, notifyEvent{Trigger: agent.TriggerEvent, Error: runErr.Error()}, now)
		return 0, runErr
	}
	run.Status = agent.StatusCompleted
	run.Result = resultJSON(result)
	_, _ = e.mgr.CreateRun(ctx, run)
	e.maybeNotify(ctx, host, a, notifyEvent{Trigger: agent.TriggerEvent, Result: run.Result}, now)
	return 1, nil
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
	// EventKindRun still runs the imperative workflow directly (legacy path).
	// Domain events now arrive as EventKindFacts carrying a pre-mapped facts
	// array, so their detect/on rules evaluate with the record bound.
	if ev.Kind == agent.EventKindRun {
		return e.runEventWorkflow(ctx, host, a, ev, now)
	}
	wc, ok := a.WebhookTrigger()
	if !ok {
		// No webhook trigger: this is a Timly domain event. Route by the shape
		// of the agent's TLN. A reactive program (on/detect) asserts the
		// payload's facts and evaluates, so the record binds in the rule and its
		// notify text. A workflow-only program has no rule to fire, so it runs
		// imperatively (the pre-reactive path). On any doubt, run.
		reactive, rerr := e.tln.Reactive(ctx, host, a.TlnSource)
		if rerr == nil && reactive {
			return e.applyDomainFacts(ctx, host, a, ev, now)
		}
		return e.runEventWorkflow(ctx, host, a, ev, now)
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
	evalRes, err := e.tln.Evaluate(ctx, host, a.TlnSource, factsJSON, state.FactsSnapshot,
		Identity{EntityID: a.EntityID, GroupID: a.GroupID})
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
		e.maybeEscalate(ctx, host, a, agent.TriggerWebhook, factsJSON, evalRes, now)
		e.maybeNotify(ctx, host, a, notifyEvent{
			Trigger: agent.TriggerWebhook, Facts: factsJSON, Firings: evalRes.Firings,
		}, now)
	}
	return len(evalRes.Firings), nil
}

// domainFactsPayload is the body Timly sends to POST /v1/events for a domain
// event: an EAV facts array already mapped to the record's attributes, ready to
// assert. RecordID is the Timly record id (Tln snapshots key on int ids), so no
// per-agent registry mapping is needed.
type domainFactsPayload struct {
	Facts json.RawMessage `json:"facts"`
}

// applyDomainFacts asserts a Timly domain event's pre-mapped facts and
// evaluates the agent's detect/on rules reactively, so notify text can bind the
// record ({item.name}, {category}, {attr.*}). It advances the facts snapshot and
// records a run / notifies only when a rule fires.
func (e *Engine) applyDomainFacts(ctx context.Context, host pkg.HostCaller, a agent.Agent, ev agent.PendingEvent, now time.Time) (int, error) {
	var p domainFactsPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return 0, fmt.Errorf("decode domain event payload: %w", err)
	}
	if len(p.Facts) == 0 {
		return 0, nil // nothing to assert (e.g. a record with no mappable fields)
	}
	state, err := e.mgr.GetState(ctx, a.ID)
	if err != nil {
		return 0, fmt.Errorf("get state: %w", err)
	}
	evalRes, err := e.tln.Evaluate(ctx, host, a.TlnSource, p.Facts, state.FactsSnapshot,
		Identity{EntityID: a.EntityID, GroupID: a.GroupID})
	if err != nil {
		return 0, err
	}
	state.FactsSnapshot = evalRes.Snapshot
	if err := e.mgr.SaveState(ctx, state); err != nil {
		return 0, fmt.Errorf("save state: %w", err)
	}
	if len(evalRes.Firings) > 0 {
		e.recordRun(ctx, a, agent.TriggerEvent, p.Facts, evalRes, now)
		e.maybeEscalate(ctx, host, a, agent.TriggerEvent, p.Facts, evalRes, now)
		e.maybeNotify(ctx, host, a, notifyEvent{
			Trigger: agent.TriggerEvent, Facts: p.Facts, Firings: evalRes.Firings,
		}, now)
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

	// Cluster-safe claim: advance next_poll_at up front so a second instance
	// sweeping the same tick skips this agent. Losing the claim means another
	// instance owns this poll — do nothing.
	won, err := e.mgr.ClaimPollDue(ctx, a.ID, now, now.Add(interval))
	if err != nil {
		return 0, fmt.Errorf("claim poll: %w", err)
	}
	if !won {
		return 0, nil
	}

	resp, err := agent.Poll(ctx, host, *pc)
	if err != nil {
		return 0, e.failAgent(ctx, a, state, interval, now, err)
	}
	facts, registry, truncated, err := agent.Map(*pc, resp, state.EntityMap, e.cfg.MaxItemsPerPoll)
	if err != nil {
		return 0, e.failAgent(ctx, a, state, interval, now, err)
	}
	if truncated > 0 {
		slog.Warn("opentalon-agents: poll result truncated", "agent", a.ID, "dropped", truncated, "max_items", e.cfg.MaxItemsPerPoll)
	}
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		return 0, e.failAgent(ctx, a, state, interval, now, err)
	}
	evalRes, err := e.tln.Evaluate(ctx, host, a.TlnSource, factsJSON, state.FactsSnapshot,
		Identity{EntityID: a.EntityID, GroupID: a.GroupID})
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
		e.maybeEscalate(ctx, host, a, agent.TriggerPoll, factsJSON, evalRes, now)
		e.maybeNotify(ctx, host, a, notifyEvent{
			Trigger: agent.TriggerPoll, Facts: factsJSON, Firings: evalRes.Firings,
		}, now)
	}
	return len(evalRes.Firings), nil
}

// failAgent records the failure: bumps the backoff, pushes next_poll_at
// out, preserves the snapshot, and records a failed run.
func (e *Engine) failAgent(ctx context.Context, a agent.Agent, state agent.AgentState, interval time.Duration, now time.Time, cause error) error {
	state.ConsecutiveFailures++
	cap := time.Duration(e.cfg.MaxBackoffSeconds) * time.Second
	next := now.Add(backoff(interval, state.ConsecutiveFailures, cap))
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

// backoff is interval * 2^(failures-1), capped at cap.
func backoff(interval time.Duration, failures int, cap time.Duration) time.Duration {
	d := interval
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= cap {
			return cap
		}
	}
	if d > cap {
		return cap
	}
	return d
}
