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

// notifySendAction is the action on the host's built-in notify plugin. The
// plugin name is configurable (Config.NotifyPluginName) but the action is
// fixed.
const notifySendAction = "send"

// notifier reaches the host's "push a message to a conversation" entrypoint.
// It is the notification analogue of tlnProxy and escalator: the plugin's
// whole coupling to message delivery is this one RunAction call — it never
// links a channel client and never learns a channel's wire format.
type notifier struct {
	pluginName string
}

// notifyOutcome is the host's reply. An empty structured reply is treated as
// success: a host that accepted the send without reporting on it did deliver.
type notifyOutcome struct {
	Delivered bool   `json:"delivered"`
	Reason    string `json:"reason,omitempty"`
}

// Send pushes text to the stored delivery target. entity/group name the
// identity the send is attributed to (the tick context carries no profile);
// source/agentID/trigger are stamped so clients can tell an agent-initiated
// message from an assistant reply.
func (n notifier) Send(ctx context.Context, host pkg.HostCaller, target agent.DeliveryTarget, text, entityID, groupID, agentID, trigger string) (notifyOutcome, error) {
	res, err := host.RunAction(ctx, n.pluginName, notifySendAction, map[string]string{
		"session_id":      target.SessionID,
		"channel_id":      target.ChannelID,
		"conversation_id": target.ConversationID,
		"text":            text,
		"entity_id":       entityID,
		"group_id":        groupID,
		"source":          "agent",
		"agent_id":        agentID,
		"trigger":         trigger,
	})
	if err != nil {
		return notifyOutcome{}, err
	}
	if res.StructuredContent == "" {
		return notifyOutcome{Delivered: true}, nil
	}
	var out notifyOutcome
	if jerr := json.Unmarshal([]byte(res.StructuredContent), &out); jerr != nil {
		return notifyOutcome{}, fmt.Errorf("notify: decode result: %w", jerr)
	}
	return out, nil
}

// notifyEvent is what the agent just did, as far as a notification is
// concerned. Firings/Facts come from a watcher evaluation; Result (or Error, on
// a failed run) comes from a scheduled (cron) run.
type notifyEvent struct {
	Trigger string
	Facts   json.RawMessage
	Firings []Firing
	Result  json.RawMessage
	Error   string
}

// maybeNotify pushes a message to the creator's conversation after the agent
// fired (watcher) or ran (schedule), when the agent opted in.
//
// Unlike escalation this costs no tokens and starts no turn — it is a plain
// message. It is not rate-limited: firings are edge-triggered (a fire-once
// crossing guard) and schedule runs fire at the author's own cadence, so the
// firing itself is the bound. A delivery failure is logged, not retried — the
// run is already recorded, and retrying a stale alert is worse than dropping
// it.
func (e *Engine) maybeNotify(ctx context.Context, host pkg.HostCaller, a agent.Agent, ev notifyEvent, now time.Time) {
	n, found, err := e.mgr.GetNotification(ctx, a.ID)
	if err != nil {
		slog.Warn("opentalon-agents: load notification config", "agent", a.ID, "error", err)
		return
	}
	if !found || !n.Enabled {
		return
	}
	if !n.Target.Addressable() {
		slog.Warn("opentalon-agents: notify enabled but no delivery target; skipping", "agent", a.ID)
		return
	}

	text := renderNotification(a, n.Template, ev, now)
	outcome, err := e.notify.Send(ctx, host, n.Target, text, a.EntityID, a.GroupID, a.ID, ev.Trigger)
	if err != nil {
		slog.Warn("opentalon-agents: notification send failed", "agent", a.ID, "error", err)
		return
	}
	if !outcome.Delivered {
		slog.Info("opentalon-agents: notification not delivered", "agent", a.ID, "reason", outcome.Reason)
		return
	}
	slog.Info("opentalon-agents: notified", "agent", a.ID, "trigger", ev.Trigger)
}

// renderNotification builds the message text. A per-agent template (with
// {{placeholders}}) overrides the built-in wording; both stay factual — this
// text is written by no model, so it must not editorialize about what the
// values mean.
func renderNotification(a agent.Agent, template string, ev notifyEvent, now time.Time) string {
	facts := blankAs(string(ev.Facts), "(none captured)")
	result := blankAs(string(ev.Result), "(no result)")
	firings := renderFirings(ev.Firings)

	if tmpl := strings.TrimSpace(template); tmpl != "" {
		r := strings.NewReplacer(
			"{{agent_name}}", a.Name,
			"{{description}}", a.Description,
			"{{firings}}", firings,
			"{{facts}}", facts,
			"{{result}}", result,
			"{{error}}", ev.Error,
			"{{trigger}}", ev.Trigger,
		)
		return r.Replace(tmpl)
	}

	var b strings.Builder
	if ev.Error != "" {
		fmt.Fprintf(&b, "Your agent %q failed to run (%s).\n\n", a.Name, now.Format(time.RFC3339))
		fmt.Fprintf(&b, "Error: %s", ev.Error)
		return b.String()
	}
	if ev.Trigger == agent.TriggerSchedule {
		fmt.Fprintf(&b, "Your agent %q ran on schedule (%s).\n\n", a.Name, now.Format(time.RFC3339))
		fmt.Fprintf(&b, "Result:\n%s", result)
		return b.String()
	}
	fmt.Fprintf(&b, "Your agent %q fired (%s).\n\n", a.Name, now.Format(time.RFC3339))
	fmt.Fprintf(&b, "What tripped it:\n%s\n\n", firings)
	fmt.Fprintf(&b, "Latest observed values:\n%s", facts)
	return b.String()
}

func blankAs(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
