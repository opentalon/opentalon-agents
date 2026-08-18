package plugin

import pkg "github.com/opentalon/opentalon/pkg/plugin"

// injected are the context args the host fills in per call (never passed
// by the LLM). group_id scopes every operation; entity_id records the
// author.
var injected = []string{"group_id", "entity_id"}

// injectedWithSession additionally captures the caller's DELIVERY CONTEXT so
// create/update can record where to reach the creator later: session_id (the
// packed session key an escalation turn runs in), plus the explicit
// channel_id / conversation_id / sender_id a notification is addressed to.
// All are best-effort — the host injects only the ones it can resolve, and
// skips empty values — so the plugin must not assume any single one is set.
// Other actions don't need them.
var injectedWithSession = []string{"group_id", "entity_id", "session_id", "channel_id", "conversation_id", "sender_id"}

// actions returns the LLM-visible actions for the agents plugin.
func actions() []pkg.ActionMsg {
	source := pkg.ParameterMsg{
		Name:        "tln_source",
		Description: "The agent's program, as Tln source. Validated (tln-plugin.check) before it is stored; invalid source is rejected with diagnostics.",
		Type:        "string",
		Required:    true,
	}
	triggers := pkg.ParameterMsg{
		Name:        "triggers",
		Description: `Optional JSON array of triggers, e.g. [{"type":"schedule","cron":"0 9 * * *"}]. Types: manual|schedule|poll|webhook. Stored now; acted on from Phase 2.`,
		Type:        "string",
		Required:    false,
	}
	idParam := pkg.ParameterMsg{Name: "id", Description: "Agent id or name.", Type: "string", Required: true}
	escalate := pkg.ParameterMsg{
		Name:        "escalate",
		Description: `Optional. Opt this agent into ESCALATION: when its watcher fires, instead of only running its fixed Tln action, start an assistant reasoning turn in the user's session that can investigate and ASK THE USER what to do. Detection stays deterministic; only the reaction is model-driven, and it costs tokens — so it is opt-in and rate-limited. JSON object: {"enabled":true,"prompt_template":"...","max_per_window":5,"window_seconds":3600} (or the shorthand "true"). prompt_template optionally overrides the seed prompt (placeholders: {{agent_name}} {{description}} {{firings}} {{facts}}); max_per_window/window_seconds cap the rate (0 = server default). Use for "watch X and when Y, figure out what's going on and check with me" — NOT for a fully-automated fixed action.`,
		Type:        "string",
		Required:    false,
	}

	notify := pkg.ParameterMsg{
		Name:        "notify",
		Description: `Optional. Opt this agent into NOTIFYING the user: when it fires (or, for a schedule agent, after each run) the system PUSHES a message to the conversation the agent was created in. Use this for "tell me / let me know / ping me when X" — do NOT hardcode a chat id or author an mcp send step for it; the destination is captured automatically and the LLM never sees it. Costs no tokens and starts no assistant turn (that is ` + "`escalate`" + `). JSON object: {"enabled":true,"template":"..."} (or the shorthand "true"). template optionally overrides the message text (placeholders: {{agent_name}} {{description}} {{firings}} {{facts}} {{result}} {{error}} {{trigger}}).`,
		Type:        "string",
		Required:    false,
	}

	return []pkg.ActionMsg{
		{
			Name:              "create",
			Description:       "Create a persistent agent from a Tln program. The source is validated before storing.",
			InjectContextArgs: injectedWithSession,
			Parameters: []pkg.ParameterMsg{
				{Name: "name", Description: "Short unique name for the agent (within your group).", Type: "string", Required: true},
				{Name: "description", Description: "The user's request in their own words — what they asked this agent to do. Store the original ask verbatim (lightly cleaned up), not your paraphrase of the Tln.", Type: "string", Required: true},
				source, triggers, escalate, notify,
			},
		},
		{
			Name:              "list",
			Description:       "List all agents in your group.",
			InjectContextArgs: injected,
			ReadOnly:          true,
		},
		{
			Name:              "show",
			Description:       "Show one agent, including its Tln source and triggers.",
			InjectContextArgs: injected,
			ReadOnly:          true,
			Parameters:        []pkg.ParameterMsg{idParam},
		},
		{
			Name:              "runs",
			Description:       "List an agent's run history (newest first): trigger, status, event, result, error, timestamps.",
			InjectContextArgs: injected,
			ReadOnly:          true,
			Parameters: []pkg.ParameterMsg{
				idParam,
				{Name: "limit", Description: "Max runs to return (default 20).", Type: "string", Required: false},
			},
		},
		{
			Name:              "run",
			Description:       "Run an agent's program now (inline), returning the result. Records a run.",
			InjectContextArgs: injected,
			Parameters:        []pkg.ParameterMsg{idParam},
		},
		{
			Name:              "update",
			Description:       "Replace an agent's Tln source (and optionally its triggers, its escalation setting, and its notification setting). The new source is validated before storing.",
			InjectContextArgs: injectedWithSession,
			Parameters:        []pkg.ParameterMsg{idParam, source, triggers, escalate, notify},
		},
		{
			Name:              "enable",
			Description:       "Enable an agent so its triggers may fire.",
			InjectContextArgs: injected,
			Parameters:        []pkg.ParameterMsg{idParam},
		},
		{
			Name:              "disable",
			Description:       "Disable an agent so its triggers do not fire.",
			InjectContextArgs: injected,
			Parameters:        []pkg.ParameterMsg{idParam},
		},
		{
			Name:              "delete",
			Description:       "Delete an agent permanently.",
			InjectContextArgs: injected,
			Parameters:        []pkg.ParameterMsg{idParam},
		},
		{
			// Hidden from the LLM (UserOnly). Fired by the host scheduler
			// (a `scheduler.jobs` entry with `action: agents.tick`) to run
			// one system-wide watcher sweep. Unscoped: no group_id.
			Name:        "tick",
			Description: "Internal: run one poll/watch sweep across all agents. Fired by the host scheduler, not by users.",
			UserOnly:    true,
		},
	}
}
