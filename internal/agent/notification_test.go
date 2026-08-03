package agent

import (
	"context"
	"testing"
)

func TestParseNotifySpec(t *testing.T) {
	if spec, err := ParseNotifySpec("  "); spec != nil || err != nil {
		t.Errorf("blank should mean 'not provided'; got %v, %v", spec, err)
	}
	if spec, err := ParseNotifySpec("TRUE"); err != nil || spec == nil || !spec.Enabled {
		t.Errorf("shorthand true: %v, %v", spec, err)
	}
	if spec, err := ParseNotifySpec("false"); err != nil || spec == nil || spec.Enabled {
		t.Errorf("shorthand false: %v, %v", spec, err)
	}
	spec, err := ParseNotifySpec(`{"enabled":true,"template":"stock: {{facts}}"}`)
	if err != nil || spec == nil || !spec.Enabled || spec.Template != "stock: {{facts}}" {
		t.Errorf("object form: %v, %v", spec, err)
	}
	if _, err := ParseNotifySpec("yes please"); err == nil {
		t.Error("garbage should be rejected, not silently treated as off")
	}
	if _, err := ParseNotifySpec(`{"enabled":`); err == nil {
		t.Error("truncated JSON should be rejected")
	}
}

func TestDeliveryTargetAddressable(t *testing.T) {
	cases := []struct {
		name string
		tgt  DeliveryTarget
		want bool
	}{
		{"empty", DeliveryTarget{}, false},
		{"session only", DeliveryTarget{SessionID: "s"}, true},
		{"channel without conversation", DeliveryTarget{ChannelID: "telegram"}, false},
		{"conversation without channel", DeliveryTarget{ConversationID: "42"}, false},
		{"sender alone is not routable", DeliveryTarget{SenderID: "u1"}, false},
		{"explicit pair", DeliveryTarget{ChannelID: "telegram", ConversationID: "42"}, true},
	}
	for _, c := range cases {
		if got := c.tgt.Addressable(); got != c.want {
			t.Errorf("%s: Addressable() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNotificationRoundTripAndTargetPreservation(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	a, err := m.Create(ctx, Agent{Name: "w", GroupID: "g1", EntityID: "e1", TalonSource: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, found, err := m.GetNotification(ctx, a.ID); err != nil || found {
		t.Fatalf("no row expected before opt-in: found=%v err=%v", found, err)
	}

	target := DeliveryTarget{SessionID: "e1:telegram:42", ChannelID: "telegram", ConversationID: "42", SenderID: "u1"}
	if err := m.SaveNotification(ctx, a.ID, target, NotifySpec{Enabled: true, Template: "t1"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	n, found, err := m.GetNotification(ctx, a.ID)
	if err != nil || !found || !n.Enabled || n.Template != "t1" || n.Target != target {
		t.Fatalf("round trip: %+v found=%v err=%v", n, found, err)
	}

	// A later update from a context with no delivery info (e.g. control plane)
	// must not wipe the target captured at create time.
	if err := m.SaveNotification(ctx, a.ID, DeliveryTarget{}, NotifySpec{Enabled: false}); err != nil {
		t.Fatalf("save without target: %v", err)
	}
	n, _, err = m.GetNotification(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n.Target != target {
		t.Errorf("target must survive a blank-context save; got %+v", n.Target)
	}
	if n.Enabled || n.Template != "" {
		t.Errorf("config should have been overwritten; got enabled=%v template=%q", n.Enabled, n.Template)
	}
}

func TestDeleteAgentRemovesNotification(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	a, err := m.Create(ctx, Agent{Name: "w", GroupID: "g1", TalonSource: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.SaveNotification(ctx, a.ID, DeliveryTarget{SessionID: "s"}, NotifySpec{Enabled: true}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := m.Delete(ctx, "g1", a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, err := m.GetNotification(ctx, a.ID); err != nil || found {
		t.Errorf("notification row should be gone: found=%v err=%v", found, err)
	}
}
