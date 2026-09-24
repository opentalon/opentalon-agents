package config

import "testing"

// db_access: true makes the host inject its own state-store credentials as
// __db_driver / __db_dsn, already expanded. Using them is what lets the plugin
// share the host's database without the DSN being written down a second time.
func TestParseUsesHostInjectedDB(t *testing.T) {
	cfg, err := Parse(`{"__db_driver":"postgres","__db_dsn":"postgres://u:p@h:5432/d"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.DB.Driver != "postgres" {
		t.Errorf("driver = %q, want postgres", cfg.DB.Driver)
	}
	if cfg.DB.DSN != "postgres://u:p@h:5432/d" {
		t.Errorf("dsn = %q", cfg.DB.DSN)
	}
}

// An explicit db: block is a deliberate choice and outranks the injection.
func TestParseExplicitDBWinsOverInjection(t *testing.T) {
	cfg, err := Parse(`{"db":{"driver":"sqlite","dsn":"/tmp/mine.db"},"__db_driver":"postgres","__db_dsn":"postgres://u:p@h:5432/d"}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.DB.Driver != "sqlite" || cfg.DB.DSN != "/tmp/mine.db" {
		t.Errorf("explicit db was overridden: %q %q", cfg.DB.Driver, cfg.DB.DSN)
	}
}

// With neither, the sqlite default still applies.
func TestParseFallsBackToSqlite(t *testing.T) {
	cfg, err := Parse("")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.DB.Driver != "sqlite" || cfg.DB.DSN != "./agents.db" {
		t.Errorf("default = %q %q", cfg.DB.Driver, cfg.DB.DSN)
	}
}
