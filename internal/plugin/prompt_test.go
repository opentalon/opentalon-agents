package plugin

import (
	"strings"
	"testing"
)

// TestPromptCoversWatcherAuthoring guards the embedded authoring prompt
// against regressions: it must explain the poll-trigger schema and the
// on-block watcher pattern the LLM needs to assemble an agent.
func TestPromptCoversWatcherAuthoring(t *testing.T) {
	must := []string{
		"create(",          // actions
		"poll",             // poll trigger
		"value_path",       // mapping
		"attribute",        // fact attribute
		"on change attr",   // reactive pattern
		"prev_value >= 10", // fire-once crossing idiom
		"MUST match",       // attribute<->on-block linkage rule
		"6-field cron is invalid", // steer away from seconds-cron toward poll interval
	}
	for _, s := range must {
		if !strings.Contains(promptText, s) {
			t.Errorf("prompt.txt should mention %q", s)
		}
	}
}
