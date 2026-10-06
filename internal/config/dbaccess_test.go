package config

import "testing"

// The host passes plugin config blocks verbatim, so the plugin expands ${VAR}
// in the DSN itself (e.g. "${HOME}/.opentalon-local-dev/agents.db").
func TestParseExpandsEnvInDSN(t *testing.T) {
	t.Setenv("AGENTS_TEST_HOME", "/Users/test")
	cfg, err := Parse(`{"db":{"driver":"sqlite","dsn":"${AGENTS_TEST_HOME}/.opentalon-local-dev/agents.db"}}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := "/Users/test/.opentalon-local-dev/agents.db"; cfg.DB.DSN != want {
		t.Errorf("dsn = %q, want %q", cfg.DB.DSN, want)
	}
}

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
