//go:build dev

package plugin

import (
	"log/slog"

	"github.com/opentalon/opentalon-agents/internal/config"
)

// devFallbackGroupID is the LOCAL-DEV ONLY fallback, compiled only under
// `-tags dev` (make build-dev). The interactive chat LLM path on the current
// host injects no group_id; prod dispatches (operator / control-plane) always
// stamp one, so a real deployment never reaches this. It fires only in a dev
// binary AND only when default_group_id is explicitly set — and logs loudly
// every time so its use is never silent. The prod build replaces this with a
// stub that always returns "" (devgroup_prod.go).
func devFallbackGroupID(cfg *config.Config, action string) string {
	if cfg.DefaultGroupID == "" {
		return ""
	}
	slog.Warn("opentalon-agents: DEV BUILD group_id fallback in use (never in prod)", "action", action, "default_group_id", cfg.DefaultGroupID)
	return cfg.DefaultGroupID
}
