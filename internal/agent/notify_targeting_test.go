package agent

import (
	"context"
	"reflect"
	"testing"
)

func TestNotifySpec_Defaults(t *testing.T) {
	// No recipients/channels → creator over in-app; and it needs a creator target.
	var s NotifySpec
	if got := s.EffectiveRecipients(); len(got) != 1 || got[0].Kind != RecipientCreator {
		t.Errorf("default recipient = %+v, want creator", got)
	}
	if got := s.EffectiveChannels(); len(got) != 1 || got[0] != ChannelInApp {
		t.Errorf("default channel = %+v, want in_app", got)
	}
	if !s.NeedsCreatorTarget() {
		t.Error("default (creator) spec should need a creator target")
	}
}

func TestNotifySpec_NeedsCreatorTarget(t *testing.T) {
	cases := []struct {
		name string
		rs   []Recipient
		want bool
	}{
		{"only responsible", []Recipient{{Kind: RecipientResponsible}}, false},
		{"only role", []Recipient{{Kind: RecipientRole, Role: "procurement"}}, false},
		{"me alias", []Recipient{{Kind: RecipientMe}}, true},
		{"responsible + creator", []Recipient{{Kind: RecipientResponsible}, {Kind: RecipientCreator}}, true},
	}
	for _, c := range cases {
		if got := (NotifySpec{Recipients: c.rs}).NeedsCreatorTarget(); got != c.want {
			t.Errorf("%s: NeedsCreatorTarget = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNotifySpec_Validate(t *testing.T) {
	ok := []NotifySpec{
		{Recipients: []Recipient{{Kind: RecipientCreator}}},
		{Recipients: []Recipient{{Kind: RecipientResponsible}}, Channels: []string{ChannelEmail, ChannelInApp}},
		{Recipients: []Recipient{{Kind: RecipientRole, Role: "procurement"}}},
	}
	for i, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("valid spec %d rejected: %v", i, err)
		}
	}
	bad := []NotifySpec{
		{Recipients: []Recipient{{Kind: "boss"}}},                      // unknown kind
		{Recipients: []Recipient{{Kind: RecipientRole}}},               // role without a name
		{Recipients: []Recipient{{Kind: RecipientCreator, Role: "x"}}}, // creator must not carry a role
		{Channels: []string{"carrier-pigeon"}},                         // unknown channel
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("invalid spec %d accepted", i)
		}
	}
}

func TestParseNotifySpec_TargetingRoundTrip(t *testing.T) {
	spec, err := ParseNotifySpec(`{"enabled":true,"recipients":[{"kind":"responsible"},{"kind":"role","role":"procurement"}],"channels":["in_app","email"]}`)
	if err != nil || spec == nil {
		t.Fatalf("parse: %v", err)
	}
	want := []Recipient{{Kind: "responsible"}, {Kind: "role", Role: "procurement"}}
	if !reflect.DeepEqual(spec.Recipients, want) {
		t.Errorf("recipients = %+v, want %+v", spec.Recipients, want)
	}
	if !reflect.DeepEqual(spec.Channels, []string{"in_app", "email"}) {
		t.Errorf("channels = %+v", spec.Channels)
	}
	// A malformed recipient is rejected at parse.
	if _, err := ParseNotifySpec(`{"enabled":true,"recipients":[{"kind":"role"}]}`); err == nil {
		t.Error("role recipient without a name should be rejected at parse")
	}
}

func TestNotification_RecipientsChannelsRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	a, err := m.Create(ctx, Agent{Name: "w", GroupID: "g1", EntityID: "e1", TlnSource: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	spec := NotifySpec{
		Enabled:    true,
		Recipients: []Recipient{{Kind: RecipientResponsible}, {Kind: RecipientRole, Role: "procurement"}},
		Channels:   []string{ChannelInApp, ChannelEmail},
	}
	if err := m.SaveNotification(ctx, a.ID, DeliveryTarget{}, spec); err != nil {
		t.Fatalf("save: %v", err)
	}
	n, found, err := m.GetNotification(ctx, a.ID)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(n.Recipients, spec.Recipients) {
		t.Errorf("recipients = %+v, want %+v", n.Recipients, spec.Recipients)
	}
	if !reflect.DeepEqual(n.Channels, spec.Channels) {
		t.Errorf("channels = %+v, want %+v", n.Channels, spec.Channels)
	}

	// A bare re-enable (no recipients/channels) preserves the stored targeting,
	// mirroring the delivery-target preservation.
	if err := m.SaveNotification(ctx, a.ID, DeliveryTarget{}, NotifySpec{Enabled: true}); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	n, _, _ = m.GetNotification(ctx, a.ID)
	if !reflect.DeepEqual(n.Recipients, spec.Recipients) || !reflect.DeepEqual(n.Channels, spec.Channels) {
		t.Errorf("bare re-enable wiped targeting: recipients=%+v channels=%+v", n.Recipients, n.Channels)
	}
}

func TestEntitiesForFacts(t *testing.T) {
	registry := map[string]int{"ABC-123": 1, "XYZ-9": 2, "self": 3}
	facts := []Fact{
		{RecordID: "1", Attribute: "status", Value: "defective"},
		{RecordID: "2", Attribute: "status", Value: "ok"},
		{RecordID: "1", Attribute: "status", Value: "defective"}, // dup id → deduped
		{RecordID: "3", Attribute: "x", Value: 1},                // maps to "self" → skipped
	}
	got := EntitiesForFacts(facts, registry)
	want := []string{"ABC-123", "XYZ-9"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EntitiesForFacts = %+v, want %+v", got, want)
	}
}
