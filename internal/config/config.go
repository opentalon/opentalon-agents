// Package config parses the JSON blob the opentalon host delivers to the
// plugin (via the OPENTALON_CONFIG env var / the Configure RPC).
package config

import (
	"encoding/json"
	"fmt"
)

// Config is the plugin's configuration. All fields are optional; sane
// defaults are applied by Parse.
type Config struct {
	DB DBConfig `json:"db"`

	// TalonPluginName is the capability name of the talon-plugin the
	// host loads. opentalon-agents reaches the Talon language purely by
	// calling this plugin's actions (check / execute_workflow) through
	// the host — it never links talon-language itself. Defaults to
	// "talon-plugin".
	TalonPluginName string `json:"talon_plugin_name"`

	// RunTimeoutSeconds bounds a single inline workflow run. 0 means no
	// explicit timeout (the host's own action timeout still applies).
	RunTimeoutSeconds int `json:"run_timeout_seconds"`

	// PollFloorSeconds and MaxItemsPerPoll bound watcher polls. Unused in
	// Phase 1 (no tick engine yet); parsed now so the schema is stable.
	PollFloorSeconds int `json:"poll_floor_seconds"`
	MaxItemsPerPoll  int `json:"max_items_per_poll"`

	// MaxBackoffSeconds caps the poll-failure backoff. Default 1800 (30m).
	MaxBackoffSeconds int `json:"max_backoff_seconds"`

	// WebhookSecret is the shared bearer token gating the webhook HTTP
	// endpoint (Authorization: Bearer <secret>). Empty disables the
	// endpoint entirely — we refuse to serve an unauthenticated ingress.
	WebhookSecret string `json:"webhook_secret"`

	// EscalationPluginName is the host's built-in escalation entrypoint the
	// engine calls when an escalation-enabled watcher fires. Defaults to
	// "_escalate" (the reserved built-in in opentalon). The host must have
	// escalation enabled (orchestrator.escalation.enabled) for the call to do
	// anything; otherwise it returns {escalated:false} and the engine logs and
	// moves on.
	EscalationPluginName string `json:"escalation_plugin_name"`

	// EscalationMaxPerWindow / EscalationWindowSeconds are the default rate
	// limit applied to an escalating agent that doesn't set its own. Defaults:
	// 5 escalations per 3600s (1h). A per-agent override (EscalationSpec)
	// takes precedence.
	EscalationMaxPerWindow  int `json:"escalation_max_per_window"`
	EscalationWindowSeconds int `json:"escalation_window_seconds"`

	// DefaultGroupID is a local-development fallback for the group scope
	// when the host injects no group_id. Prod dispatches (operator /
	// control-plane) always stamp a real group_id in the call args, so
	// this fallback never fires there. The interactive chat LLM path on
	// the current host injects no group_id, so set this (e.g. "default")
	// to author agents from a local console/telegram/slack chat. Unset =
	// fail-closed: a call with no group_id is rejected, exactly as before.
	DefaultGroupID string `json:"default_group_id"`
}

// DBConfig selects the storage backend.
type DBConfig struct {
	Driver string `json:"driver"` // "sqlite" (default) or "postgres"
	DSN    string `json:"dsn"`    // sqlite: file path; postgres: connection URL
}

// Parse reads the JSON config and applies defaults. An empty string (or
// "{}") is valid and yields an all-defaults config.
func Parse(jsonStr string) (*Config, error) {
	cfg := &Config{}
	if jsonStr != "" && jsonStr != "{}" {
		if err := json.Unmarshal([]byte(jsonStr), cfg); err != nil {
			return nil, fmt.Errorf("agents: parse config: %w", err)
		}
	}
	if cfg.DB.Driver == "" {
		cfg.DB.Driver = "sqlite"
	}
	if cfg.DB.DSN == "" && cfg.DB.Driver == "sqlite" {
		cfg.DB.DSN = "./agents.db"
	}
	if cfg.TalonPluginName == "" {
		cfg.TalonPluginName = "talon-plugin"
	}
	if cfg.PollFloorSeconds == 0 {
		cfg.PollFloorSeconds = 15
	}
	if cfg.MaxItemsPerPoll == 0 {
		cfg.MaxItemsPerPoll = 500
	}
	if cfg.MaxBackoffSeconds == 0 {
		cfg.MaxBackoffSeconds = 1800
	}
	if cfg.EscalationPluginName == "" {
		cfg.EscalationPluginName = "_escalate"
	}
	if cfg.EscalationMaxPerWindow == 0 {
		cfg.EscalationMaxPerWindow = 5
	}
	if cfg.EscalationWindowSeconds == 0 {
		cfg.EscalationWindowSeconds = 3600
	}
	return cfg, nil
}
