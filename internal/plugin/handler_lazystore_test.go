package plugin

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/config"
	"github.com/opentalon/opentalon-agents/internal/store"
)

// The host does not set OPENTALON_CONFIG; it delivers config through Init,
// which lands in Configure. A handler that starts without a store must open
// the one the host names, rather than the process having had to guess a
// default at startup.
func TestConfigureOpensStoreWhenStartedWithout(t *testing.T) {
	cfg, _ := config.Parse("")
	h := NewHandler(cfg, nil)

	dsn := filepath.Join(t.TempDir(), "agents.db")
	var gotDriver, gotDSN string
	h.SetStoreOpener(func(driver, d string) (*agent.Manager, error) {
		gotDriver, gotDSN = driver, d
		db, err := store.Open(driver, d)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = db.Close() })
		return agent.NewManager(db), nil
	})

	payload, _ := json.Marshal(map[string]any{
		"db":              map[string]string{"driver": "sqlite", "dsn": dsn},
		"tln_plugin_name": "tln",
	})
	if err := h.Configure(string(payload)); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if gotDriver != "sqlite" || gotDSN != dsn {
		t.Fatalf("opener got %q/%q, want sqlite/%s", gotDriver, gotDSN, dsn)
	}
	if h.mgr == nil {
		t.Fatal("manager still nil after Configure")
	}
	if h.currentEngine() == nil {
		t.Fatal("engine not rebuilt around the new manager")
	}
}

// Without a store and without a way to open one, Configure has to say so.
// Failing here is recoverable — the host retries; failing at startup is not,
// because the process is gone before the handshake.
func TestConfigureFailsWithoutStoreOrOpener(t *testing.T) {
	cfg, _ := config.Parse("")
	h := NewHandler(cfg, nil)
	if err := h.Configure(`{"tln_plugin_name":"tln"}`); err == nil {
		t.Fatal("want an error when there is no store and no opener")
	}
}

// A surfaced open failure must carry the driver, so a bad DSN in the host's
// config is diagnosable from the host's own log line.
func TestConfigureReportsOpenFailure(t *testing.T) {
	cfg, _ := config.Parse("")
	h := NewHandler(cfg, nil)
	h.SetStoreOpener(func(driver, dsn string) (*agent.Manager, error) {
		return nil, fmt.Errorf("boom")
	})
	err := h.Configure(`{"db":{"driver":"postgres","dsn":"whatever"},"tln_plugin_name":"tln"}`)
	if err == nil {
		t.Fatal("want an error")
	}
	if got := err.Error(); !strings.Contains(got, "postgres") || !strings.Contains(got, "boom") {
		t.Fatalf("error %q should name the driver and the cause", got)
	}
}

// An existing store is not replaced: a live handle cannot be swapped under
// calls in flight, so a divergent DSN warns and the handle stays.
func TestConfigureKeepsExistingStore(t *testing.T) {
	cfg, _ := config.Parse("")
	db, err := store.Open("sqlite", filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mgr := agent.NewManager(db)
	h := NewHandler(cfg, mgr)
	h.SetStoreOpener(func(driver, dsn string) (*agent.Manager, error) {
		t.Fatal("opener must not run when a store is already open")
		return nil, nil
	})
	if err := h.Configure(`{"db":{"driver":"postgres","dsn":"elsewhere"},"tln_plugin_name":"tln"}`); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if h.mgr != mgr {
		t.Fatal("live manager was replaced")
	}
}
