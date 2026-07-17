package plugin

import pkg "github.com/opentalon/opentalon/pkg/plugin"

// injected are the context args the host fills in per call (never passed
// by the LLM). group_id scopes every operation; entity_id records the
// author.
var injected = []string{"group_id", "entity_id"}

// actions returns the LLM-visible actions for the agents plugin.
func actions() []pkg.ActionMsg {
	source := pkg.ParameterMsg{
		Name:        "talon_source",
		Description: "The agent's program, as Talon source. Validated (talon-plugin.check) before it is stored; invalid source is rejected with diagnostics.",
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

	return []pkg.ActionMsg{
		{
			Name:              "create",
			Description:       "Create a persistent agent from a Talon program. The source is validated before storing.",
			InjectContextArgs: injected,
			AlwaysInclude:     true,
			Parameters: []pkg.ParameterMsg{
				{Name: "name", Description: "Short unique name for the agent (within your group).", Type: "string", Required: true},
				{Name: "description", Description: "The user's request in their own words — what they asked this agent to do. Store the original ask verbatim (lightly cleaned up), not your paraphrase of the Talon.", Type: "string", Required: true},
				source, triggers,
			},
		},
		{
			Name:              "list",
			Description:       "List all agents in your group.",
			InjectContextArgs: injected,
			ReadOnly:          true,
			AlwaysInclude:     true,
		},
		{
			Name:              "show",
			Description:       "Show one agent, including its Talon source and triggers.",
			InjectContextArgs: injected,
			ReadOnly:          true,
			AlwaysInclude:     true,
			Parameters:        []pkg.ParameterMsg{idParam},
		},
		{
			Name:              "run",
			Description:       "Run an agent's program now (inline), returning the result. Records a run.",
			InjectContextArgs: injected,
			AlwaysInclude:     true,
			Parameters:        []pkg.ParameterMsg{idParam},
		},
		{
			Name:              "update",
			Description:       "Replace an agent's Talon source (and optionally its triggers). The new source is validated before storing.",
			InjectContextArgs: injected,
			Parameters:        []pkg.ParameterMsg{idParam, source, triggers},
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
