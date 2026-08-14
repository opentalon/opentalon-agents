// Package plugin implements the opentalon-agents gRPC plugin: it manages
// persistent, LLM-authored Tln agents and runs them by proxying to
// tln-plugin through the host. It links no tln-language code.
package plugin

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
)

//go:embed prompt.txt
var promptText string

// Handler implements pkg/plugin.StreamingHandler and Configurable. It
// advertises SupportsCallbacks=true because every language operation
// (validate, run) is a callback to tln-plugin through the host, which
// requires a live HostCaller — available only on the bidi path.
// Configure can arrive concurrently with an in-flight action (nothing in the
// host contract serializes them), and it replaces tln/engine wholesale, so
// mu guards those two fields: write in Configure, read on every use.
type Handler struct {
	mu     sync.RWMutex
	cfg    *config.Config
	mgr    *agent.Manager
	tln    tlnProxy
	engine *Engine
}

// NewHandler wires the handler.
func NewHandler(cfg *config.Config, mgr *agent.Manager) *Handler {
	return &Handler{
		cfg:    cfg,
		mgr:    mgr,
		tln:    tlnProxy{pluginName: cfg.TlnPluginName},
		engine: NewEngine(cfg, mgr),
	}
}

// Capabilities describes the plugin to the host.
func (h *Handler) Capabilities() pkg.CapabilitiesMsg {
	return pkg.CapabilitiesMsg{
		Name:                 "agents",
		Description:          "Create and manage persistent, LLM-authored automations written in the Tln language. Describe a task; author it as Tln source; the agent is stored and can be run on demand (schedules, polls, and webhooks follow in later phases).",
		Actions:              actions(),
		SystemPromptAddition: promptText,
		SupportsCallbacks:    true,
	}
}

// Execute is the unary path — never used, since SupportsCallbacks=true.
func (h *Handler) Execute(req pkg.Request) pkg.Response {
	return pkg.Response{
		CallID: req.ID,
		Error:  "opentalon-agents requires the host to dispatch over ExecuteBidi (needs a live HostCaller to reach tln-plugin).",
	}
}

// Configure receives the host config block over the Init RPC, before any
// Execute call. This is how the host actually delivers config — it does NOT
// set OPENTALON_CONFIG on the subprocess — so we parse it here and apply it
// into the shared *cfg (the handler and engine hold the same pointer).
//
// The DB handle is already open (main.go opened it from the startup default),
// so a divergent DB config delivered here cannot switch the live handle — we
// warn rather than silently ignore. Every other field (tln_plugin_name,
// default_group_id, timeouts, backoff, webhook_secret) takes effect now.
func (h *Handler) Configure(configJSON string) error {
	parsed, err := config.Parse(configJSON)
	if err != nil {
		return fmt.Errorf("agents: configure: %w", err)
	}
	h.mu.Lock()
	if parsed.DB.DSN != h.cfg.DB.DSN || parsed.DB.Driver != h.cfg.DB.Driver {
		slog.Warn("opentalon-agents: DB config in Configure differs from startup, live DB handle unchanged",
			"startup_driver", h.cfg.DB.Driver, "startup_dsn", h.cfg.DB.DSN,
			"configured_driver", parsed.DB.Driver, "configured_dsn", parsed.DB.DSN)
	}
	*h.cfg = *parsed
	h.tln = tlnProxy{pluginName: h.cfg.TlnPluginName}
	// The engine caches the plugin names it calls out to (tln, escalation,
	// notify) in its proxies, so it has to be rebuilt for a configured name to
	// take effect. It keeps no in-memory state — everything is in the DB.
	//
	// It gets its own copy of the config, not h.cfg: a Tick reads cfg fields
	// (MaxItemsPerPoll, backoff) for as long as it runs, and the next
	// Configure would otherwise overwrite them underneath it.
	engineCfg := *parsed
	h.engine = NewEngine(&engineCfg, h.mgr)
	h.mu.Unlock()
	slog.Info("opentalon-agents: configured", "tln_plugin", parsed.TlnPluginName, "db_driver", parsed.DB.Driver, "default_group_id", parsed.DefaultGroupID)
	return nil
}

// currentEngine returns the engine to use for this call. Configure may swap
// the pointer at any time; a call in flight keeps the one it read.
func (h *Handler) currentEngine() *Engine {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.engine
}

// currentTln returns the tln proxy to use for this call, same rationale.
func (h *Handler) currentTln() tlnProxy {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tln
}

// currentCfg returns a snapshot of the config for this call: Configure
// overwrites *h.cfg in place, so the value has to be copied under the lock.
func (h *Handler) currentCfg() config.Config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return *h.cfg
}

// ExecuteWithCallbacks is the bidi path: it dispatches every action and
// carries the live HostCaller used to reach tln-plugin.
func (h *Handler) ExecuteWithCallbacks(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response {
	// tick is the hidden, system-wide scheduler action — unscoped (no
	// group_id), so it's handled before the group gate below.
	if req.Action == "tick" {
		return h.actionTick(ctx, req, host)
	}

	groupID := req.Args["group_id"]
	if groupID == "" {
		// devFallbackGroupID is "" in the prod build (build tag). Only the
		// dev build (-tags dev) can resolve a non-empty fallback, and only
		// from an explicit config value. Prod therefore always fails closed
		// here: no path can create/act on an agent without an authenticated
		// group_id, regardless of config or env.
		cfg := h.currentCfg()
		groupID = devFallbackGroupID(&cfg, req.Action)
	}
	if groupID == "" {
		return errResp(req.ID, "missing group_id (should be injected by the host)")
	}
	rc := agent.RunContext{
		GroupID:        groupID,
		EntityID:       req.Args["entity_id"],
		SessionID:      req.Args["session_id"],
		ChannelID:      req.Args["channel_id"],
		ConversationID: req.Args["conversation_id"],
		SenderID:       req.Args["sender_id"],
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
// tln-plugin.
func (h *Handler) actionTick(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response {
	res, err := h.currentEngine().Tick(ctx, host)
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	return jsonResp(req.ID,
		fmt.Sprintf("tick: %d agent(s), %d firing(s), %d error(s)", res.Agents, res.Firings, res.Errors),
		res)
}

func (h *Handler) actionCreate(ctx context.Context, req pkg.Request, host pkg.HostCaller, rc agent.RunContext) pkg.Response {
	name := req.Args["name"]
	src := req.Args["tln_source"]
	if name == "" || src == "" {
		return errResp(req.ID, "name and tln_source are required")
	}
	triggers, err := parseTriggers(req.Args["triggers"])
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	if err := agent.ValidateTriggers(triggers); err != nil {
		return errResp(req.ID, err.Error())
	}
	spec, err := agent.ParseEscalationSpec(req.Args["escalate"])
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	nspec, err := agent.ParseNotifySpec(req.Args["notify"])
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	// Reject an opt-in that has nowhere to deliver to BEFORE the agent is
	// committed. On create there is no stored row to fall back on, so the
	// check saveNotification would make later is fully decidable here — and a
	// post-Create rejection would leave a persisted agent behind a failed
	// call (the retry then collides on the unique name).
	if nspec != nil && nspec.Enabled && !rc.Delivery().Addressable() {
		return errResp(req.ID, "notify.enabled needs a conversation to deliver to, but none is available here")
	}
	if spec != nil && spec.Enabled && rc.SessionID == "" {
		return errResp(req.ID, "escalate.enabled needs an interactive session to address the turn to, but none is available here")
	}
	if resp, bad := h.validate(ctx, req.ID, host, src); bad {
		return resp
	}
	a, err := h.mgr.Create(ctx, agent.Agent{
		Name:        name,
		Description: req.Args["description"],
		GroupID:     rc.GroupID,
		EntityID:    rc.EntityID,
		TlnSource:   src,
		Triggers:    triggers,
		Enabled:     true,
	})
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	if resp, bad := h.saveEscalation(ctx, req.ID, a.ID, rc, spec); bad {
		return resp
	}
	if resp, bad := h.saveNotification(ctx, req.ID, a.ID, rc, nspec); bad {
		return resp
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
	view["tln_source"] = a.TlnSource
	view["triggers"] = a.Triggers
	// Neither branch echoes where the agent reaches the user: the escalation
	// session key already encodes channel + conversation, so echoing it hands
	// the LLM the same address the notification target withholds. show reports
	// THAT the agent can reach its creator, never where.
	if esc, found, err := h.mgr.GetEscalation(ctx, a.ID); err == nil && found && esc.Enabled {
		view["escalation"] = map[string]any{
			"enabled":         esc.Enabled,
			"prompt_template": esc.PromptTemplate,
			"max_per_window":  esc.MaxPerWindow,
			"window_seconds":  esc.WindowSeconds,
		}
	}
	if n, found, err := h.mgr.GetNotification(ctx, a.ID); err == nil && found && n.Enabled {
		view["notification"] = map[string]any{
			"enabled":  n.Enabled,
			"template": n.Template,
		}
	}
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

	result, runErr := h.currentTln().Run(ctx, host, a.TlnSource)
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
	src := req.Args["tln_source"]
	if src == "" {
		return errResp(req.ID, "tln_source is required")
	}
	triggers, err := parseTriggers(req.Args["triggers"])
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	if err := agent.ValidateTriggers(triggers); err != nil {
		return errResp(req.ID, err.Error())
	}
	spec, err := agent.ParseEscalationSpec(req.Args["escalate"])
	if err != nil {
		return errResp(req.ID, err.Error())
	}
	nspec, err := agent.ParseNotifySpec(req.Args["notify"])
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
	if resp, bad := h.saveEscalation(ctx, req.ID, a.ID, rc, spec); bad {
		return resp
	}
	if resp, bad := h.saveNotification(ctx, req.ID, a.ID, rc, nspec); bad {
		return resp
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

// saveEscalation persists the agent's escalation config when the author
// supplied one (spec != nil); a nil spec leaves any existing config untouched.
// Opting in requires a target session to address the reply to: reject enable
// with no session unless one was already stored (an update from a
// non-interactive path can keep the session captured at create time).
func (h *Handler) saveEscalation(ctx context.Context, callID, agentID string, rc agent.RunContext, spec *agent.EscalationSpec) (pkg.Response, bool) {
	if spec == nil {
		return pkg.Response{}, false
	}
	if spec.Enabled && rc.SessionID == "" {
		existing, found, err := h.mgr.GetEscalation(ctx, agentID)
		if err != nil {
			return errResp(callID, err.Error()), true
		}
		if !found || existing.SessionID == "" {
			return errResp(callID, "escalate.enabled needs an interactive session to address the turn to, but none is available here"), true
		}
	}
	if err := h.mgr.SaveEscalation(ctx, agentID, rc.SessionID, *spec); err != nil {
		return errResp(callID, err.Error()), true
	}
	return pkg.Response{}, false
}

// saveNotification persists the agent's notification config and the delivery
// target from the call context when the author supplied one (spec != nil); a
// nil spec leaves any existing config untouched. Opting in requires somewhere
// to deliver to: reject enable when neither this call nor a previously stored
// row carries an addressable target.
func (h *Handler) saveNotification(ctx context.Context, callID, agentID string, rc agent.RunContext, spec *agent.NotifySpec) (pkg.Response, bool) {
	if spec == nil {
		return pkg.Response{}, false
	}
	target := rc.Delivery()
	if spec.Enabled && !target.Addressable() {
		existing, found, err := h.mgr.GetNotification(ctx, agentID)
		if err != nil {
			return errResp(callID, err.Error()), true
		}
		if !found || !existing.Target.Addressable() {
			return errResp(callID, "notify.enabled needs a conversation to deliver to, but none is available here"), true
		}
	}
	if err := h.mgr.SaveNotification(ctx, agentID, target, *spec); err != nil {
		return errResp(callID, err.Error()), true
	}
	return pkg.Response{}, false
}

// validate runs tln-plugin.check and, on invalid source, returns a
// populated error response and bad=true. On a valid source it returns
// bad=false.
func (h *Handler) validate(ctx context.Context, callID string, host pkg.HostCaller, src string) (pkg.Response, bool) {
	tln := h.currentTln()
	ok, diagnostics, err := tln.Check(ctx, host, src)
	if err != nil {
		return errResp(callID, fmt.Sprintf("could not validate Tln source (is %q loaded?): %v", tln.pluginName, err)), true
	}
	if !ok {
		return errResp(callID, "invalid Tln source; fix and retry:\n"+diagnostics), true
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
