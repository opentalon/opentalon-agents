// Command opentalon-agents is the gRPC plugin binary. It opens its store,
// wires the handler, and serves the opentalon host until terminated.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/api"
	"github.com/opentalon/opentalon-agents/internal/config"
	aplugin "github.com/opentalon/opentalon-agents/internal/plugin"
	"github.com/opentalon/opentalon-agents/internal/store"
)

func main() {
	cfg, err := config.Parse(os.Getenv("OPENTALON_CONFIG"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "opentalon-agents: parse config: %v\n", err)
		os.Exit(1)
	}

	// The host does NOT set OPENTALON_CONFIG on this subprocess — it delivers
	// configuration through Init, which reaches Handler.Configure. Opening the
	// store here would therefore always use the startup defaults (sqlite at
	// ./agents.db), and where the working directory is not writable — a
	// container with a read-only root filesystem, for instance — that fails
	// outright and the process exits before the handshake. The host then sees
	// "plugin closed stdout before handshake" and retries forever.
	//
	// So open eagerly only when this process was actually handed a config of its
	// own (standalone runs), and otherwise let Configure open the store the host
	// names. The webhook server starts with it, since it needs the manager.
	var (
		dbMu sync.Mutex
		db   *store.DB
	)
	closeDB := func() {
		dbMu.Lock()
		defer dbMu.Unlock()
		if db != nil {
			_ = db.Close()
			db = nil
		}
	}
	defer closeDB()

	// Webhook ingress: when the host grants an HTTP port (expose_http),
	// serve the webhook endpoint on the private loopback listener it
	// reverse-proxies. It only enqueues; the tick drains it.
	var webhookOnce sync.Once
	startWebhook := func(mgr *agent.Manager) {
		webhookOnce.Do(func() {
			port := os.Getenv("OPENTALON_HTTP_PORT")
			if port == "" {
				return
			}
			srv := &http.Server{
				Addr:              "127.0.0.1:" + port,
				Handler:           api.NewWebhookServer(cfg, mgr),
				ReadHeaderTimeout: 5 * time.Second,
			}
			go func() {
				slog.Info("opentalon-agents: webhook server listening", "addr", srv.Addr, "enabled", cfg.WebhookSecret != "")
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					slog.Error("opentalon-agents: webhook server", "error", err)
				}
			}()
		})
	}

	openStore := func(driver, dsn string) (*agent.Manager, error) {
		d, err := store.Open(driver, dsn)
		if err != nil {
			return nil, err
		}
		dbMu.Lock()
		db = d
		dbMu.Unlock()
		return agent.NewManager(d), nil
	}

	var curMgr *agent.Manager

	var mgr *agent.Manager
	if os.Getenv("OPENTALON_CONFIG") != "" {
		mgr, err = openStore(cfg.DB.Driver, cfg.DB.DSN)
		if err != nil {
			fmt.Fprintf(os.Stderr, "opentalon-agents: open db: %v\n", err)
			os.Exit(1)
		}
	}

	handler := aplugin.NewHandler(cfg, mgr)
	handler.SetStoreOpener(func(driver, dsn string) (*agent.Manager, error) {
		m, err := openStore(driver, dsn)
		if err != nil {
			return nil, err
		}
		curMgr = m
		return m, nil
	})
	// The webhook server reads cfg at construction — webhook_secret decides
	// whether its endpoints answer at all. Start it only after a Configure has
	// applied, so it sees what the host sent rather than the startup defaults.
	// cfg is the same struct Configure writes into, so no copy is needed here.
	handler.SetConfiguredHook(func() { startWebhook(curMgr) })
	if mgr != nil {
		// Standalone: this process had its own configuration, so there is
		// nothing to wait for.
		curMgr = mgr
		startWebhook(mgr)
	}

	// Exit cleanly on termination so the deferred db.Close runs.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		closeDB()
		os.Exit(0)
	}()

	if err := pkg.Serve(handler); err != nil {
		fmt.Fprintf(os.Stderr, "opentalon-agents: serve: %v\n", err)
		os.Exit(1)
	}
}
