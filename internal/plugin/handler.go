// Package plugin implements the opentalon-agents gRPC plugin: it manages
// persistent, LLM-authored Talon agents and runs them by proxying to
// talon-plugin through the host. It links no talon-language code.
package plugin

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
)

//go:embed prompt.txt
var promptText string

// Handler implements pkg/plugin.StreamingHandler and Configurable. It
// advertises SupportsCallbacks=true because every language operation
// (validate, run) is a callback to talon-plugin through the host, which
// requires a live HostCaller — available only on the bidi path.
type Handler struct {
	cfg    *config.Config
	mgr    *agent.Manager
	talon  talonProxy
	engine *Engine
}

// NewHandler wires the handler.
func NewHandler(cfg *config.Config, mgr *agent.Manager) *Handler {
	return &Handler{
		cfg:    cfg,
		mgr:    mgr,
		talon:  talonProxy{pluginName: cfg.TalonPluginName},
		engine: NewEngine(cfg, mgr),
	}
}

// Capabilities describes the plugin to the host.
func (h *Handler) Capabilities() pkg.CapabilitiesMsg {
	return pkg.CapabilitiesMsg{
		Name:                 "agents",
		Description:          "Create and manage persistent, LLM-authored automations written in the Talon language. Describe a task; author it as Talon source; the agent is stored and can be run on demand (schedules, polls, and webhooks follow in later phases).",
		Actions:              actions(),
		SystemPromptAddition: promptText,
		SupportsCallbacks:    true,
	}
}

// Execute is the unary path — never used, since SupportsCallbacks=true.
func (h *Handler) Execute(req pkg.Request) pkg.Response {
	return pkg.Response{
		CallID: req.ID,
		Error:  "opentalon-agents requires the host to dispatch over ExecuteBidi (needs a live HostCaller to reach talon-plugin).",
	}
}

// Configure receives the host config block. The authoritative config is
// read from OPENTALON_CONFIG at startup (main.go), so this only logs.
func (h *Handler) Configure(string) error {
	slog.Info("opentalon-agents: configured", "talon_plugin", h.cfg.TalonPluginName, "db_driver", h.cfg.DB.Driver)
	return nil
}

// ExecuteWithCallbacks is the bidi path: it dispatches every action and
// carries the live HostCaller used to reach talon-plugin.
func (h *Handler) ExecuteWithCallbacks(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response {
	// tick is the hidden, system-wide scheduler action — unscoped (no
	// group_id), so it's handled before the group gate below.
	if req.Action == "tick" {
		return h.actionTick(ctx, req, host)
	}

	rc := agent.RunContext{GroupID: req.Args["group_id"], EntityID: req.Args["entity_id"]}
	if rc.GroupID == "" {
		return errResp(req.ID, "missing group_id (should be injected by the host)")
	}

	switch req.Action {
	case "create":
		return h.actionCreate(ctx, req, host, rc)
	case "list":
		return h.actionList(ctx, req, rc)
	case "show":
		return h.actionShow(ctx, req, rc)
	case "run":
		return h.actionRun(ctx, req, host, rc)
	case "update":
		return h.actionUpdate(ctx, req, host, rc)
	case "enable":
		return h.actionSetEnabled(ctx, req, rc, true)
	case "disable":
		return h.actionSetEnabled(ctx, req, rc, false)
	case "delete":
		return h.actionDelete(ctx, req, rc)
	default:
		return errResp(req.ID, "unknown action: "+req.Action)
	}
}

// actionTick runs one system-wide watcher sweep. It is fired by the host
// scheduler (a `scheduler.jobs` entry with `action: agents.tick`), not by
// the LLM, and needs the live HostCaller to poll sources and reach
// talon-plugin.
func (h *Handler) actionTick(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response {
	res, err := h.engine.Tick(ctx, host)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	return jsonResp(req.ID,
		fmt.Sprintf("tick: %d agent(s), %d firing(s), %d error(s)", res.Agents, res.Firings, res.Errors),
		res)
}

func (h *Handler) actionCreate(ctx context.Context, req pkg.Request, host pkg.HostCaller, rc agent.RunContext) pkg.Response {
	name := req.Args["name"]
	src := req.Args["talon_source"]
	if name == "" || src == "" {
		return errResp(req.ID, "name and talon_source are required")
	}
	triggers, err := parseTriggers(req.Args["triggers"])
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	if resp, bad := h.validate(ctx, req.ID, host, src); bad {
		return resp
	}
	a, err := h.mgr.Create(ctx, agent.Agent{
		Name:        name,
		Description: req.Args["description"],
		GroupID:     rc.GroupID,
		EntityID:    rc.EntityID,
		TalonSource: src,
		Triggers:    triggers,
		Enabled:     true,
	})
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	return jsonResp(req.ID, fmt.Sprintf("Created agent %q (id %s).", a.Name, a.ID), summarize(a))
}

func (h *Handler) actionList(ctx context.Context, req pkg.Request, rc agent.RunContext) pkg.Response {
	agents, err := h.mgr.List(ctx, rc.GroupID)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	views := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		views = append(views, summarize(a))
	}
	return jsonResp(req.ID, fmt.Sprintf("%d agent(s).", len(agents)), map[string]any{"agents": views})
}

func (h *Handler) actionShow(ctx context.Context, req pkg.Request, rc agent.RunContext) pkg.Response {
	a, err := h.get(ctx, req, rc)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	view := summarize(a)
	view["talon_source"] = a.TalonSource
	view["triggers"] = a.Triggers
	return jsonResp(req.ID, fmt.Sprintf("Agent %q (id %s).", a.Name, a.ID), view)
}

func (h *Handler) actionRun(ctx context.Context, req pkg.Request, host pkg.HostCaller, rc agent.RunContext) pkg.Response {
	a, err := h.get(ctx, req, rc)
	if err != nil {
		return errResp(req.ID, err.Error())
	}

	run, err := h.mgr.CreateRun(ctx, agent.Run{AgentID: a.ID, TriggerType: "llm", Status: agent.StatusRunning})
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	started := time.Now().UTC()
	run.StartedAt = &started

	result, runErr := h.talon.Run(ctx, host, a.TalonSource)
	finished := time.Now().UTC()
	run.FinishedAt = &finished

	if runErr != nil {
		run.Status = agent.StatusFailed
		run.Error = runErr.Error()
		_ = h.mgr.FinishRun(ctx, run)
		return errResp(req.ID, fmt.Sprintf("agent %q run failed: %v", a.Name, runErr))
	}
	run.Status = agent.StatusCompleted
	run.Result = resultJSON(result)
	if err := h.mgr.FinishRun(ctx, run); err != nil {
		slog.Warn("opentalon-agents: persist run failed", "run_id", run.ID, "error", err)
	}
	return pkg.Response{
		CallID:            req.ID,
		Content:           fmt.Sprintf("Ran agent %q (run %s): %s", a.Name, run.ID, result.Content),
		StructuredContent: result.StructuredContent,
	}
}

func (h *Handler) actionUpdate(ctx context.Context, req pkg.Request, host pkg.HostCaller, rc agent.RunContext) pkg.Response {
	src := req.Args["talon_source"]
	if src == "" {
		return errResp(req.ID, "talon_source is required")
	}
	triggers, err := parseTriggers(req.Args["triggers"])
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	if resp, bad := h.validate(ctx, req.ID, host, src); bad {
		return resp
	}
	a, err := h.mgr.Update(ctx, rc.GroupID, req.Args["id"], src, triggers)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	return jsonResp(req.ID, fmt.Sprintf("Updated agent %q (id %s).", a.Name, a.ID), summarize(a))
}

func (h *Handler) actionSetEnabled(ctx context.Context, req pkg.Request, rc agent.RunContext, enabled bool) pkg.Response {
	a, err := h.mgr.SetEnabled(ctx, rc.GroupID, req.Args["id"], enabled)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	return jsonResp(req.ID, fmt.Sprintf("Agent %q %s.", a.Name, state), summarize(a))
}

func (h *Handler) actionDelete(ctx context.Context, req pkg.Request, rc agent.RunContext) pkg.Response {
	if err := h.mgr.Delete(ctx, rc.GroupID, req.Args["id"]); err != nil {
		return errResp(req.ID, err.Error())
	}
	return pkg.Response{CallID: req.ID, Content: "Deleted."}
}

// validate runs talon-plugin.check and, on invalid source, returns a
// populated error response and bad=true. On a valid source it returns
// bad=false.
func (h *Handler) validate(ctx context.Context, callID string, host pkg.HostCaller, src string) (pkg.Response, bool) {
	ok, diagnostics, err := h.talon.Check(ctx, host, src)
	if err != nil {
		return errResp(callID, fmt.Sprintf("could not validate Talon source (is %q loaded?): %v", h.cfg.TalonPluginName, err)), true
	}
	if !ok {
		return errResp(callID, "invalid Talon source; fix and retry:\n"+diagnostics), true
	}
	return pkg.Response{}, false
}

// get resolves the agent named by req.Args["id"] within the caller's group.
func (h *Handler) get(ctx context.Context, req pkg.Request, rc agent.RunContext) (agent.Agent, error) {
	id := req.Args["id"]
	if id == "" {
		return agent.Agent{}, fmt.Errorf("id is required")
	}
	return h.mgr.Get(ctx, rc.GroupID, id)
}

// --- helpers ---

func summarize(a agent.Agent) map[string]any {
	types := make([]string, 0, len(a.Triggers))
	for _, t := range a.Triggers {
		types = append(types, t.Type)
	}
	return map[string]any{
		"id":            a.ID,
		"name":          a.Name,
		"description":   a.Description,
		"enabled":       a.Enabled,
		"trigger_types": types,
		"updated_at":    a.UpdatedAt,
	}
}

func parseTriggers(s string) ([]agent.Trigger, error) {
	if s == "" {
		return nil, nil
	}
	var triggers []agent.Trigger
	if err := json.Unmarshal([]byte(s), &triggers); err != nil {
		return nil, fmt.Errorf("triggers must be a JSON array of {type,...}: %w", err)
	}
	return triggers, nil
}

func resultJSON(r pkg.CallResult) json.RawMessage {
	if json.Valid([]byte(r.StructuredContent)) && r.StructuredContent != "" {
		return json.RawMessage(r.StructuredContent)
	}
	b, _ := json.Marshal(map[string]string{"content": r.Content})
	return b
}

func errResp(callID, msg string) pkg.Response {
	return pkg.Response{CallID: callID, Error: msg}
}

func jsonResp(callID, summary string, structured any) pkg.Response {
	b, err := json.Marshal(structured)
	if err != nil {
		return pkg.Response{CallID: callID, Content: summary}
	}
	return pkg.Response{CallID: callID, Content: summary, StructuredContent: string(b)}
}
