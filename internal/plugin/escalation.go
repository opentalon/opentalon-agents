package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

// escalateTurnAction is the action on the host's built-in escalation plugin
// (see opentalon's _escalate). The plugin name is configurable
// (Config.EscalationPluginName) but the action is fixed.
const escalateTurnAction = "turn"

// escalator reaches the host's background-turn entrypoint. It is the escalation
// analogue of talonProxy: the plugin's whole coupling to the escalation
// capability is this one RunAction call.
type escalator struct {
	pluginName string
}

// escalateOutcome is the host's synchronous reply: escalated=true means the
// turn was accepted and spawned (it runs asynchronously), not that it finished.
type escalateOutcome struct {
	Escalated bool   `json:"escalated"`
	Reason    string `json:"reason,omitempty"`
}

// Escalate starts a background assistant turn for the target session. entity/
// group name the identity the turn runs and bills as (the tick context carries
// no profile); source/agentID/trigger are stamped onto the pushed reply's
// metadata so clients can distinguish an agent-initiated message.
func (e escalator) Escalate(ctx context.Context, host pkg.HostCaller, sessionID, prompt, entityID, groupID, agentID, trigger string) (escalateOutcome, error) {
	res, err := host.RunAction(ctx, e.pluginName, escalateTurnAction, map[string]string{
		"session_id": sessionID,
		"prompt":     prompt,
		"entity_id":  entityID,
		"group_id":   groupID,
		"source":     "agent",
		"agent_id":   agentID,
		"trigger":    trigger,
	})
	if err != nil {
		return escalateOutcome{}, err
	}
	if res.StructuredContent == "" {
		return escalateOutcome{}, nil
	}
	var out escalateOutcome
	if jerr := json.Unmarshal([]byte(res.StructuredContent), &out); jerr != nil {
		return escalateOutcome{}, fmt.Errorf("escalate: decode result: %w", jerr)
	}
	return out, nil
}

// maybeEscalate is called after a watcher fires. When the agent has opted into
// escalation and is within its rate limit, it synthesizes a seed prompt from
// the firing and starts a background LLM turn in the creator's session.
//
// Detection stays deterministic — this runs only on an actual firing (an edge,
// since watchers use fire-once crossing guards) and is additionally bounded by
// a per-agent, per-window rate limit. A refusal from the host (escalation
// disabled, budget exhausted, one already in flight) is logged, not retried.
func (e *Engine) maybeEscalate(ctx context.Context, host pkg.HostCaller, a agent.Agent, triggerType string, factsJSON json.RawMessage, evalRes EvalResult, now time.Time) {
	esc, found, err := e.mgr.GetEscalation(ctx, a.ID)
	if err != nil {
		slog.Warn("opentalon-agents: load escalation config", "agent", a.ID, "error", err)
		return
	}
	if !found || !esc.Enabled {
		return
	}
	if esc.SessionID == "" {
		slog.Warn("opentalon-agents: escalation enabled but no target session; skipping", "agent", a.ID)
		return
	}

	maxPer, window := e.escalationBounds(esc)
	count, windowStart, allowed := rateLimit(esc.FireCount, esc.WindowStart, maxPer, window, now)
	if !allowed {
		slog.Info("opentalon-agents: escalation rate-limited; skipping",
			"agent", a.ID, "max_per_window", maxPer, "window", window)
		// Persist a window reset if one happened, so the counter doesn't drift.
		if err := e.mgr.SaveEscalationState(ctx, a.ID, count, windowStart); err != nil {
			slog.Warn("opentalon-agents: save escalation state", "agent", a.ID, "error", err)
		}
		return
	}

	prompt := synthesizeEscalationPrompt(a, esc, factsJSON, evalRes.Firings)
	outcome, err := e.esc.Escalate(ctx, host, esc.SessionID, prompt, a.EntityID, a.GroupID, a.ID, triggerType)
	if err != nil {
		slog.Warn("opentalon-agents: escalation call failed", "agent", a.ID, "error", err)
		return
	}
	if !outcome.Escalated {
		slog.Info("opentalon-agents: escalation not started", "agent", a.ID, "reason", outcome.Reason)
		// Not counted against the limit — the turn never ran. Still persist any
		// window reset so the window boundary advances.
		if err := e.mgr.SaveEscalationState(ctx, a.ID, esc.FireCount, windowStart); err != nil {
			slog.Warn("opentalon-agents: save escalation state", "agent", a.ID, "error", err)
		}
		return
	}
	slog.Info("opentalon-agents: escalated", "agent", a.ID, "session", esc.SessionID, "trigger", triggerType)
	if err := e.mgr.SaveEscalationState(ctx, a.ID, count+1, windowStart); err != nil {
		slog.Warn("opentalon-agents: save escalation state", "agent", a.ID, "error", err)
	}
}

// escalationBounds resolves the effective rate limit: the agent's own override
// when set, else the plugin config default.
func (e *Engine) escalationBounds(esc agent.Escalation) (maxPer int, window time.Duration) {
	maxPer = esc.MaxPerWindow
	if maxPer <= 0 {
		maxPer = e.cfg.EscalationMaxPerWindow
	}
	secs := esc.WindowSeconds
	if secs <= 0 {
		secs = e.cfg.EscalationWindowSeconds
	}
	return maxPer, time.Duration(secs) * time.Second
}

// rateLimit applies a fixed-window limiter. It returns the count and window
// start to persist and whether a new escalation is allowed. When the window has
// elapsed it resets (count 0, window starts now); within the window it allows
// only while count < maxPer.
func rateLimit(count int, windowStart *time.Time, maxPer int, window time.Duration, now time.Time) (int, *time.Time, bool) {
	if windowStart == nil || now.Sub(*windowStart) >= window {
		start := now
		return 0, &start, true
	}
	if count >= maxPer {
		return count, windowStart, false
	}
	return count, windowStart, true
}

// synthesizeEscalationPrompt builds the seed message for the escalation turn.
// A per-agent PromptTemplate (with {{placeholders}}) overrides the built-in
// text; both weave in the user's original ask, what tripped, and the observed
// values so the assistant can investigate and come back to the user.
func synthesizeEscalationPrompt(a agent.Agent, esc agent.Escalation, factsJSON json.RawMessage, firings []Firing) string {
	facts := strings.TrimSpace(string(factsJSON))
	if facts == "" {
		facts = "(none captured)"
	}
	firingsText := renderFirings(firings)

	if tmpl := strings.TrimSpace(esc.PromptTemplate); tmpl != "" {
		r := strings.NewReplacer(
			"{{agent_name}}", a.Name,
			"{{description}}", a.Description,
			"{{firings}}", firingsText,
			"{{facts}}", facts,
		)
		return r.Replace(tmpl)
	}

	desc := a.Description
	if desc == "" {
		desc = "(no original request was recorded)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your background agent %q just fired and escalated to you.\n\n", a.Name)
	fmt.Fprintf(&b, "What the user originally asked for:\n%s\n\n", desc)
	fmt.Fprintf(&b, "What tripped the watcher:\n%s\n\n", firingsText)
	fmt.Fprintf(&b, "Latest observed values (facts):\n%s\n\n", facts)
	b.WriteString("Investigate what is going on — you may fan out focused sub-agent checks to look into each affected entity — then decide what, if anything, should be done. Come back to the user with a short summary and ask how they would like to proceed. Do not take irreversible action without confirming with the user first.")
	return b.String()
}

// renderFirings turns the fired on-blocks into a short bulleted list for the
// prompt. Each line names the on-block and, when present, the entity ref.
func renderFirings(firings []Firing) string {
	if len(firings) == 0 {
		return "- (a watched condition was met)"
	}
	var b strings.Builder
	for _, f := range firings {
		b.WriteString("- ")
		if f.OnBlock != "" {
			fmt.Fprintf(&b, "on-block %q", f.OnBlock)
		} else {
			b.WriteString("a watched condition")
		}
		if f.Ref != "" {
			fmt.Fprintf(&b, " for %s", f.Ref)
			if f.RefKind != "" {
				fmt.Fprintf(&b, " (%s)", f.RefKind)
			}
		}
		if f.Error != "" {
			fmt.Fprintf(&b, " — error: %s", f.Error)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
