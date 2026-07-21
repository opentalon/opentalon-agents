//go:build !dev

package plugin

import "github.com/opentalon/opentalon-agents/internal/config"

// devFallbackGroupID is the production stub: it always returns "", so a call
// with no injected group_id fails closed no matter what the config or env
// says. The dev-only fallback that honors config.DefaultGroupID lives in
// devgroup_dev.go and is compiled ONLY under `-tags dev`. The prod binary
// literally does not contain that code, so the group-scope invariant cannot
// be weakened by misconfiguration.
func devFallbackGroupID(_ *config.Config, _ string) string { return "" }
