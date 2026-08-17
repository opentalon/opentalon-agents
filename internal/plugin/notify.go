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

// sendRequest is one delivery: a rendered message plus WHO to deliver to and
// HOW. The recipient is named by kind, never by address — for creator the
// stored Target carries the (host-injected, LLM-invisible) session/conversation;
// for responsible/role the host resolves the address at fire time from ItemID /
// Role. DeliveryChannel selects in-app vs email. entity/group attribute the
// send; source/agentID/trigger stamp its provenance.
type sendRequest struct {
	Target          agent.DeliveryTarget
	RecipientKind   string
	Role            string
	ItemID          string
	DeliveryChannel string
	Text            string
	EntityID        string
	GroupID         string
	AgentID         string
	Trigger         string
}

// Send pushes one message via the host's notify entrypoint. Everything the host
// needs to resolve and address the recipient is passed as args; the plugin
// itself never holds a chat id or email for responsible/role recipients.
func (n notifier) Send(ctx context.Context, host pkg.HostCaller, req sendRequest) (notifyOutcome, error) {
	res, err := host.RunAction(ctx, n.pluginName, notifySendAction, map[string]string{
		"session_id":       req.Target.SessionID,
		"channel_id":       req.Target.ChannelID,
		"conversation_id":  req.Target.ConversationID,
		"recipient_kind":   req.RecipientKind,
		"role":             req.Role,
		"item_id":          req.ItemID,
		"delivery_channel": req.DeliveryChannel,
		"text":             req.Text,
		"entity_id":        req.EntityID,
		"group_id":         req.GroupID,
		"source":           "agent",
		"agent_id":         req.AgentID,
		"trigger":          req.Trigger,
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
// a failed run) comes from a scheduled (cron) run. Entities are the external ids
// of the item(s) in the firing, used to resolve a "responsible person"
// recipient per item.
type notifyEvent struct {
	Trigger  string
	Facts    json.RawMessage
	Firings  []Firing
	Result   json.RawMessage
	Error    string
	Entities []string
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

	text := renderNotification(a, n.Template, ev, now)
	spec := n.Spec()
	base := sendRequest{
		Text: text, EntityID: a.EntityID, GroupID: a.GroupID, AgentID: a.ID, Trigger: ev.Trigger,
	}

	var delivered int
	for _, rcp := range spec.EffectiveRecipients() {
		for _, ch := range spec.EffectiveChannels() {
			reqs := e.resolveRecipient(a, rcp, ev, n.Target, base, ch)
			for _, req := range reqs {
				if e.sendOne(ctx, host, a, rcp, req) {
					delivered++
				}
			}
		}
	}
	if delivered > 0 {
		slog.Info("opentalon-agents: notified", "agent", a.ID, "trigger", ev.Trigger, "deliveries", delivered)
	}
}

// resolveRecipient turns one (recipient, channel) into the concrete send
// request(s). A responsible recipient fans out to one send per fired item; a
// role recipient is a single send the host expands to the role's members; a
// creator recipient uses the stored delivery target. It returns no requests
// (and logs) when a recipient can't be delivered — a creator with no stored
// target, or a responsible recipient with no item to resolve against.
func (e *Engine) resolveRecipient(a agent.Agent, rcp agent.Recipient, ev notifyEvent, target agent.DeliveryTarget, base sendRequest, channel string) []sendRequest {
	base.DeliveryChannel = channel
	switch {
	case rcp.IsCreator():
		if !target.Addressable() {
			slog.Warn("opentalon-agents: notify creator recipient but no delivery target; skipping", "agent", a.ID)
			return nil
		}
		req := base
		req.RecipientKind = agent.RecipientCreator
		req.Target = target
		return []sendRequest{req}
	case rcp.Kind == agent.RecipientRole:
		req := base
		req.RecipientKind = agent.RecipientRole
		req.Role = rcp.Role
		return []sendRequest{req}
	case rcp.Kind == agent.RecipientResponsible:
		if len(ev.Entities) == 0 {
			slog.Info("opentalon-agents: notify responsible recipient but no item to resolve (needs an id_field); skipping", "agent", a.ID)
			return nil
		}
		reqs := make([]sendRequest, 0, len(ev.Entities))
		for _, item := range ev.Entities {
			req := base
			req.RecipientKind = agent.RecipientResponsible
			req.ItemID = item
			reqs = append(reqs, req)
		}
		return reqs
	default:
		return nil
	}
}

// sendOne performs one delivery, reporting whether it landed. A failure or a
// host refusal is logged, never retried and never fatal — the run is already
// recorded, and one recipient failing must not stop the rest of the fan-out.
func (e *Engine) sendOne(ctx context.Context, host pkg.HostCaller, a agent.Agent, rcp agent.Recipient, req sendRequest) bool {
	outcome, err := e.notify.Send(ctx, host, req)
	if err != nil {
		slog.Warn("opentalon-agents: notification send failed", "agent", a.ID, "recipient", rcp.Kind, "channel", req.DeliveryChannel, "error", err)
		return false
	}
	if !outcome.Delivered {
		slog.Info("opentalon-agents: notification not delivered", "agent", a.ID, "recipient", rcp.Kind, "channel", req.DeliveryChannel, "reason", outcome.Reason)
		return false
	}
	return true
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
