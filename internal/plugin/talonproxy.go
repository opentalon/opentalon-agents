package plugin

import (
	"context"
	"encoding/json"

	"github.com/opentalon/opentalon/pkg/plugin"
)

// talonProxy is the plugin's ENTIRE coupling to the Talon language. It
// reaches the language exclusively by calling talon-plugin's generic,
// agent-agnostic actions through the host — opentalon-agents links no
// talon-language code of its own.
type talonProxy struct {
	pluginName string // talon-plugin's capability name (default "talon-plugin")
}

// Check validates Talon source without executing it, via
// talon-plugin.check. It returns ok=true for valid source; ok=false with
// human-readable diagnostics for invalid source (a normal result, not an
// error). A non-nil error means the check action itself failed to run
// (e.g. talon-plugin not loaded).
func (p talonProxy) Check(ctx context.Context, host plugin.HostCaller, src string) (ok bool, diagnostics string, err error) {
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
	// Invalid source: talon-plugin puts the diagnostics in Content.
	return false, res.Content, nil
}

// Run executes Talon source via talon-plugin.execute_workflow. The MCP
// steps inside the program flow back through the host's orchestrator on
// talon-plugin's own callback stream.
func (p talonProxy) Run(ctx context.Context, host plugin.HostCaller, src string) (plugin.CallResult, error) {
	return host.RunAction(ctx, p.pluginName, "execute_workflow", map[string]string{"workflow": src})
}
