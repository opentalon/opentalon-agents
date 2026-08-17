package plugin

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

// saveNotify is a small helper to opt an agent into notifications with a spec.
func saveNotify(t *testing.T, mgr *agent.Manager, agentID string, target agent.DeliveryTarget, spec agent.NotifySpec) {
	t.Helper()
	if err := mgr.SaveNotification(context.Background(), agentID, target, spec); err != nil {
		t.Fatalf("save notification: %v", err)
	}
}

func TestEngine_NotifiesResponsiblePerItemOverChannels(t *testing.T) {
	e, mgr := escEngineFixture(t)
	a := escWatcher(t, mgr) // watches ABC-123 (id_field item.barcode)
	saveNotify(t, mgr, a.ID, agent.DeliveryTarget{}, agent.NotifySpec{
		Enabled:    true,
		Recipients: []agent.Recipient{{Kind: agent.RecipientResponsible}},
		Channels:   []string{agent.ChannelInApp, agent.ChannelEmail},
	})
	host := crossingNotifyHost()
	fireCrossingNotify(t, e, host)

	// One item × two channels → two sends, both addressed by kind (host resolves
	// the responsible person), carrying the item id and no creator address.
	if len(host.sends) != 2 {
		t.Fatalf("expected two responsible deliveries (one per channel), got %d", len(host.sends))
	}
	gotChannels := map[string]bool{}
	for _, s := range host.sends {
		if s["recipient_kind"] != agent.RecipientResponsible {
			t.Errorf("recipient_kind = %q, want responsible", s["recipient_kind"])
		}
		if s["item_id"] != "ABC-123" {
			t.Errorf("item_id = %q, want ABC-123", s["item_id"])
		}
		if s["session_id"] != "" || s["conversation_id"] != "" {
			t.Errorf("responsible send must not carry a creator address: %q/%q", s["session_id"], s["conversation_id"])
		}
		gotChannels[s["delivery_channel"]] = true
	}
	if !gotChannels[agent.ChannelInApp] || !gotChannels[agent.ChannelEmail] {
		t.Errorf("both channels should be delivered, got %v", gotChannels)
	}
}

func TestEngine_NotifiesRole(t *testing.T) {
	e, mgr := escEngineFixture(t)
	a := escWatcher(t, mgr)
	saveNotify(t, mgr, a.ID, agent.DeliveryTarget{}, agent.NotifySpec{
		Enabled:    true,
		Recipients: []agent.Recipient{{Kind: agent.RecipientRole, Role: "procurement"}},
	})
	host := crossingNotifyHost()
	fireCrossingNotify(t, e, host)

	if len(host.sends) != 1 {
		t.Fatalf("expected one role delivery, got %d", len(host.sends))
	}
	s := host.sends[0]
	if s["recipient_kind"] != agent.RecipientRole || s["role"] != "procurement" {
		t.Errorf("role send = kind:%q role:%q", s["recipient_kind"], s["role"])
	}
	if s["delivery_channel"] != agent.ChannelInApp {
		t.Errorf("default channel = %q, want in_app", s["delivery_channel"])
	}
}

func TestEngine_NotifiesCreatorAndResponsibleTogether(t *testing.T) {
	e, mgr := escEngineFixture(t)
	a := escWatcher(t, mgr)
	target := agent.DeliveryTarget{SessionID: "ent-1:telegram:42", ChannelID: "telegram", ConversationID: "42"}
	saveNotify(t, mgr, a.ID, target, agent.NotifySpec{
		Enabled:    true,
		Recipients: []agent.Recipient{{Kind: agent.RecipientCreator}, {Kind: agent.RecipientResponsible}},
	})
	host := crossingNotifyHost()
	fireCrossingNotify(t, e, host)

	if len(host.sends) != 2 {
		t.Fatalf("expected creator + responsible = two deliveries, got %d", len(host.sends))
	}
	var sawCreator, sawResponsible bool
	for _, s := range host.sends {
		switch s["recipient_kind"] {
		case agent.RecipientCreator:
			sawCreator = true
			if s["session_id"] != target.SessionID {
				t.Errorf("creator send missing stored target: %q", s["session_id"])
			}
		case agent.RecipientResponsible:
			sawResponsible = true
			if s["item_id"] != "ABC-123" {
				t.Errorf("responsible send item_id = %q", s["item_id"])
			}
		}
	}
	if !sawCreator || !sawResponsible {
		t.Errorf("both recipients should deliver: creator=%v responsible=%v", sawCreator, sawResponsible)
	}
}

func TestEngine_ResponsibleWithoutItemIsSkipped(t *testing.T) {
	// A watcher with no id_field has only the implicit "self" entity, which has
	// no external id to resolve a responsible person against — so a responsible
	// recipient delivers nothing rather than sending to a bogus target.
	e, mgr := escEngineFixture(t)
	pc := `{"server":"inventory","tool":"get-item","args":{"barcode":"ABC-123"},"interval":"5m","value_path":"item.current_stock","attribute":"current_stock"}`
	a, err := mgr.Create(context.Background(), agent.Agent{
		Name: "restock", GroupID: "g1", EntityID: "ent-1", Enabled: true,
		TlnSource: `on change attr "current_stock" { when prev_value >= 10 and new_value < 10 workflow "Refill stock" }`,
		Triggers:  []agent.Trigger{{Type: agent.TriggerPoll, Config: []byte(pc)}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	saveNotify(t, mgr, a.ID, agent.DeliveryTarget{}, agent.NotifySpec{
		Enabled:    true,
		Recipients: []agent.Recipient{{Kind: agent.RecipientResponsible}},
	})
	host := crossingNotifyHost()
	fireCrossingNotify(t, e, host)
	if len(host.sends) != 0 {
		t.Fatalf("expected no delivery when there is no item to resolve, got %d", len(host.sends))
	}
}
