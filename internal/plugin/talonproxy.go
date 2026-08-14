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
func (p tlnProxy) Run(ctx context.Context, host plugin.HostCaller, src string) (plugin.CallResult, error) {
	return host.RunAction(ctx, p.pluginName, "execute_workflow", map[string]string{"workflow": src})
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
func (p tlnProxy) Evaluate(ctx context.Context, host plugin.HostCaller, source string, facts, snapshot json.RawMessage) (EvalResult, error) {
	args := map[string]string{"source": source, "facts": "[]"}
	if len(facts) > 0 {
		args["facts"] = string(facts)
	}
	if len(snapshot) > 0 {
		args["snapshot"] = string(snapshot)
	}
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
