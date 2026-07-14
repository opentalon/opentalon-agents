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
	return cfg, nil
}
