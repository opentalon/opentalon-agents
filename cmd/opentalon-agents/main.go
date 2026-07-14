// Command opentalon-agents is the gRPC plugin binary. It opens its store,
// wires the handler, and serves the opentalon host until terminated.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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

	db, err := store.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opentalon-agents: open db: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	mgr := agent.NewManager(db)
	handler := aplugin.NewHandler(cfg, mgr)

	// Webhook ingress: when the host grants an HTTP port (expose_http),
	// serve the webhook endpoint on the private loopback listener it
	// reverse-proxies. It only enqueues; the tick drains it.
	if port := os.Getenv("OPENTALON_HTTP_PORT"); port != "" {
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
	}

	// Exit cleanly on termination so the deferred db.Close runs.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = db.Close()
		os.Exit(0)
	}()

	if err := pkg.Serve(handler); err != nil {
		fmt.Fprintf(os.Stderr, "opentalon-agents: serve: %v\n", err)
		os.Exit(1)
	}
}
