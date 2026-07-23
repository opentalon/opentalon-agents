package store

import (
	"path/filepath"
	"testing"
)

func TestRebind(t *testing.T) {
	if got := sqliteDialect.Rebind("a=? AND b=?"); got != "a=? AND b=?" {
		t.Errorf("sqlite Rebind should be a no-op, got %q", got)
	}
	if got := postgresDialect.Rebind("a=? AND b=?"); got != "a=$1 AND b=$2" {
		t.Errorf("postgres Rebind: got %q", got)
	}
}

func TestMigrationNumber(t *testing.T) {
	cases := map[string]int{"001_init.sql": 1, "042_x.sql": 42}
	for name, want := range cases {
		got, err := migrationNumber(name)
		if err != nil || got != want {
			t.Errorf("migrationNumber(%q) = %d, %v; want %d", name, got, err, want)
		}
	}
	if _, err := migrationNumber("init.sql"); err == nil {
		t.Error("expected error for filename without leading number")
	}
}

func TestOpen_MigratesAndIsIdempotent(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "m.db")

	db, err := Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	v, err := db.currentVersion()
	if err != nil || v != 2 {
		t.Fatalf("version after migrate: %d, %v", v, err)
	}
	// All tables should exist.
	for _, table := range []string{"agents", "runs", "agent_state", "pending_events", "agent_escalations"} {
		if _, err := db.SQL().Exec("SELECT 1 FROM " + table + " WHERE 1=0"); err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}
	_ = db.Close()

	// Re-open the same file: migrations must not re-run or error.
	db2, err := Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer func() { _ = db2.Close() }()
	if v, _ := db2.currentVersion(); v != 2 {
		t.Errorf("version should stay 2 on reopen, got %d", v)
	}
}

func TestOpen_UnsupportedDriver(t *testing.T) {
	if _, err := Open("mysql", "x"); err == nil {
		t.Error("expected error for unsupported driver")
	}
}
