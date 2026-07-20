// Command seed-agent writes the deterministic stock-abc watcher directly into
// the agents store, bypassing the LLM authoring leg. It reproduces the exact
// agent the real-LLM run authors (verified against a manual full-stack run), so
// the deterministic CI gate can drive tick -> fire -> act without a model.
//
// It creates one agent:
//   - poll trigger: mcp server "testdb", tool "testdb__get_item",
//     arg barcode=ABC-123, watching current_stock
//   - on a downward crossing < 10, runs a workflow that calls
//     testdb__create_ticket for 50 units
//
// Env:
//
//	AGENTS_DB       sqlite path (default ./agents.db)
//	AGENT_INTERVAL  poll interval, Go duration (default 1m; CI uses a short one)
//	AGENT_BARCODE   watched barcode (default ABC-123)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/opentalon/opentalon-agents/internal/agent"
	"github.com/opentalon/opentalon-agents/internal/store"
)

func main() {
	dsn := getenv("AGENTS_DB", "./agents.db")
	interval := getenv("AGENT_INTERVAL", "1m")
	barcode := getenv("AGENT_BARCODE", "ABC-123")

	db, err := store.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("seed-agent: open store %s: %v", dsn, err)
	}
	defer func() { _ = db.Close() }()
	mgr := agent.NewManager(db)

	poll := agent.PollConfig{
		Server:    "testdb",
		Tool:      "testdb__get_item",
		Args:      map[string]string{"barcode": barcode},
		Interval:  interval,
		ValuePath: "current_stock",
		IDField:   "barcode",
		Attribute: "current_stock",
	}
	pc, err := json.Marshal(poll)
	if err != nil {
		log.Fatalf("seed-agent: encode poll config: %v", err)
	}

	talon := fmt.Sprintf(`on change attr "current_stock" {
  when prev_value >= 10 and new_value < 10
  workflow "Refill stock for %s"
}

workflow "Refill stock for %s" {
  step "create_ticket" {
    mcp "testdb" "testdb__create_ticket" {
      barcode "%s"
      qty 50
    }
  }
}`, barcode, barcode, barcode)

	a, err := mgr.Create(context.Background(), agent.Agent{
		Name:        "stock-abc",
		Description: fmt.Sprintf("Watch inventory item barcode %s and open a refill ticket for 50 units when its stock drops below 10", barcode),
		GroupID:     "default",
		EntityID:    "console:user",
		TalonSource: talon,
		Triggers:    []agent.Trigger{{Type: agent.TriggerPoll, Config: json.RawMessage(pc)}},
		Enabled:     true,
	})
	if err != nil {
		log.Fatalf("seed-agent: create agent: %v", err)
	}
	log.Printf("seed-agent: created agent %s (%s) interval=%s barcode=%s", a.ID, a.Name, interval, barcode)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
