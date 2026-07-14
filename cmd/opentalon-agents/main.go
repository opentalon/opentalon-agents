// Command opentalon-agents is the gRPC plugin binary. It opens its store,
// wires the handler, and serves the opentalon host until terminated.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
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

	handler := aplugin.NewHandler(cfg, agent.NewManager(db))

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
