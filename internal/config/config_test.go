package config

import "testing"

func TestParse_Defaults(t *testing.T) {
	cfg, err := Parse("")
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if cfg.DB.Driver != "sqlite" {
		t.Errorf("default driver: %q", cfg.DB.Driver)
	}
	if cfg.DB.DSN != "./agents.db" {
		t.Errorf("default sqlite dsn: %q", cfg.DB.DSN)
	}
	if cfg.TlnPluginName != "tln-plugin" {
		t.Errorf("default tln plugin name: %q", cfg.TlnPluginName)
	}
	if cfg.PollFloorSeconds != 15 || cfg.MaxItemsPerPoll != 500 {
		t.Errorf("default poll bounds: %d / %d", cfg.PollFloorSeconds, cfg.MaxItemsPerPoll)
	}
	if cfg.NotifyPluginName != "_notify" {
		t.Errorf("default notify plugin name: %q", cfg.NotifyPluginName)
	}
}

func TestParse_NotifyPluginOverride(t *testing.T) {
	cfg, err := Parse(`{"notify_plugin_name":"messages"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.NotifyPluginName != "messages" {
		t.Errorf("notify plugin override: %q", cfg.NotifyPluginName)
	}
}

func TestParse_Overrides(t *testing.T) {
	cfg, err := Parse(`{"db":{"driver":"postgres","dsn":"postgres://x"},"tln_plugin_name":"tln","poll_floor_seconds":30}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.DB.Driver != "postgres" || cfg.DB.DSN != "postgres://x" {
		t.Errorf("db override not applied: %+v", cfg.DB)
	}
	if cfg.TlnPluginName != "tln" {
		t.Errorf("tln plugin override: %q", cfg.TlnPluginName)
	}
	if cfg.PollFloorSeconds != 30 {
		t.Errorf("poll floor override: %d", cfg.PollFloorSeconds)
	}
	// postgres with a dsn must not get the sqlite default.
	if cfg.MaxItemsPerPoll != 500 {
		t.Errorf("unspecified max items should default: %d", cfg.MaxItemsPerPoll)
	}
}

func TestParse_Invalid(t *testing.T) {
	if _, err := Parse(`{not json`); err == nil {
		t.Error("expected error on malformed JSON")
	}
}
