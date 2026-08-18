package plugin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/opentalon/opentalon/pkg/plugin"
)

// tlnProxy is the plugin's ENTIRE coupling to the Tln language. It
// reaches the language exclusively by calling tln-plugin's generic,
// agent-agnostic actions through the host — opentalon-agents links no
// tln-language code of its own.
type tlnProxy struct {
	pluginName string // tln-plugin's capability name (default "tln-plugin")
}

// Identity is the agent-owner scope a background run acts as. A scheduled
// / poll / event run has no profile on the wire, so the workflow's MCP
// steps would otherwise reach Timly with no user (server_context user
// missing → notify-user et al. fail). We carry the owner identity into the
// tln-plugin callback as reserved args; opentalon-core's handleCallback
// pops them off, sets the callback actor, and injects it as the
// X-Timly-User-Id header on the leaf MCP tool call.
type Identity struct {
	EntityID  string // agent owner — the Timly user id (agent.EntityID)
	GroupID   string // agent tenant — the Timly entity id (agent.GroupID)
	SessionID string // optional originating session
}

// Reserved callback arg keys. MUST stay in lockstep with
// pkg/plugin/contextargs.Callback* in the opentalon module; duplicated as
// literals so opentalon-agents needs no local replace directive on the
// core module (the values cross the wire as plain map keys).
const (
	cbEntityIDArg  = "__ot_cb_entity_id"
	cbGroupIDArg   = "__ot_cb_group_id"
	cbSessionIDArg = "__ot_cb_session_id"
)

// apply stamps the non-empty identity fields onto an args map as reserved
// callback keys. Empty fields are omitted so an unscoped run sends nothing
// (the host then falls closed exactly as before).
func (id Identity) apply(args map[string]string) map[string]string {
	if id.EntityID != "" {
		args[cbEntityIDArg] = id.EntityID
	}
	if id.GroupID != "" {
		args[cbGroupIDArg] = id.GroupID
	}
	if id.SessionID != "" {
		args[cbSessionIDArg] = id.SessionID
	}
	return args
}

// Check validates Tln source without executing it, via
// tln-plugin.check. It returns ok=true for valid source; ok=false with
// human-readable diagnostics for invalid source (a normal result, not an
// error). A non-nil error means the check action itself failed to run
// (e.g. tln-plugin not loaded).
func (p tlnProxy) Check(ctx context.Context, host plugin.HostCaller, src string) (ok bool, diagnostics string, err error) {
	res, err := host.RunAction(ctx, p.pluginName, "check", map[string]string{"workflow": src})
	if err != nil {
		return false, "", err
	}
	var parsed struct {
		OK bool `json:"ok"`
	}
	if res.StructuredContent != "" {
		if jerr := json.Unmarshal([]byte(res.StructuredContent), &parsed); jerr == nil && parsed.OK {
			return true, "", nil
		}
	}
	// Invalid source: tln-plugin puts the diagnostics in Content.
	return false, res.Content, nil
}

// Run executes Tln source via tln-plugin.execute_workflow. The MCP
// steps inside the program flow back through the host's orchestrator on
// tln-plugin's own callback stream.
func (p tlnProxy) Run(ctx context.Context, host plugin.HostCaller, src string, id Identity) (plugin.CallResult, error) {
	return host.RunAction(ctx, p.pluginName, "execute_workflow", id.apply(map[string]string{"workflow": src}))
}

// Firing describes one on-block that fired during an Evaluate call.
type Firing struct {
	OnBlock string `json:"on_block"`
	Ref     string `json:"ref,omitempty"`
	RefKind string `json:"ref_kind,omitempty"`
	Error   string `json:"error,omitempty"`
}

// EvalResult is the parsed result of tln-plugin.evaluate: which
// on-blocks fired and the updated fact snapshot to persist.
type EvalResult struct {
	Firings  []Firing        `json:"firings"`
	Snapshot json.RawMessage `json:"snapshot"`
}

// Evaluate reactively evaluates Tln source against facts via
// tln-plugin.evaluate. tln-plugin hydrates a session from the prior
// snapshot, asserts the facts (firing on-blocks, whose workflows run
// their MCP steps back through the host), and returns which blocks fired
// plus the new snapshot. `facts` is a JSON array of
// {record_id,attribute,value}; `snapshot` is the prior snapshot JSON and
// may be empty on the first evaluation.
func (p tlnProxy) Evaluate(ctx context.Context, host plugin.HostCaller, source string, facts, snapshot json.RawMessage, id Identity) (EvalResult, error) {
	args := map[string]string{"source": source, "facts": "[]"}
	if len(facts) > 0 {
		args["facts"] = string(facts)
	}
	if len(snapshot) > 0 {
		args["snapshot"] = string(snapshot)
	}
	id.apply(args)
	res, err := host.RunAction(ctx, p.pluginName, "evaluate", args)
	if err != nil {
		return EvalResult{}, err
	}
	if res.StructuredContent == "" {
		return EvalResult{}, fmt.Errorf("tln-plugin evaluate: empty result")
	}
	var out EvalResult
	if err := json.Unmarshal([]byte(res.StructuredContent), &out); err != nil {
		return EvalResult{}, fmt.Errorf("tln-plugin evaluate: decode result: %w", err)
	}
	return out, nil
}
