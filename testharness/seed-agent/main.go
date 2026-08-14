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
// It also opts the agent into NOTIFICATION (#28) with a sentinel template, so
// the deterministic gate can assert the firing pushed a message to the
// creator's conversation. Authoring runs get their delivery target injected by
// the host; a pre-seeded agent has no such context, so the console channel's
// (channel, conversation) pair is written in directly.
//
// Env:
//
//	AGENTS_DB        sqlite path (default ./agents.db)
//	AGENT_INTERVAL   poll interval, Go duration (default 1m; CI uses a short one)
//	AGENT_BARCODE    watched barcode (default ABC-123)
//	NOTIFY_CHANNEL   channel that delivers the notification (default console)
//	NOTIFY_CONVERSATION  conversation id on that channel (default console)
//	NOTIFY_MARKER    sentinel prefix the E2E greps for (default E2E-NOTIFY)
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

	tln := fmt.Sprintf(`on change attr "current_stock" {
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
		TlnSource:   tln,
		Triggers:    []agent.Trigger{{Type: agent.TriggerPoll, Config: json.RawMessage(pc)}},
		Enabled:     true,
	})
	if err != nil {
		log.Fatalf("seed-agent: create agent: %v", err)
	}

	// Notification opt-in. The template is a single greppable line carrying the
	// observed facts, so the E2E asserts the message was rendered and delivered
	// — not merely that a send was attempted.
	target := agent.DeliveryTarget{
		ChannelID:      getenv("NOTIFY_CHANNEL", "console"),
		ConversationID: getenv("NOTIFY_CONVERSATION", "console"),
		SenderID:       "user",
	}
	marker := getenv("NOTIFY_MARKER", "E2E-NOTIFY")
	spec := agent.NotifySpec{
		Enabled:  true,
		Template: marker + " {{agent_name}} {{trigger}} {{facts}}",
	}
	if err := mgr.SaveNotification(context.Background(), a.ID, target, spec); err != nil {
		log.Fatalf("seed-agent: save notification: %v", err)
	}

	log.Printf("seed-agent: created agent %s (%s) interval=%s barcode=%s notify=%s/%s",
		a.ID, a.Name, interval, barcode, target.ChannelID, target.ConversationID)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
