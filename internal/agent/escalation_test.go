package agent

import (
	"context"
	"testing"
	"time"
)

func TestParseEscalationSpec(t *testing.T) {
	cases := []struct {
		in      string
		wantNil bool
		want    EscalationSpec
		wantErr bool
	}{
		{in: "", wantNil: true},
		{in: "   ", wantNil: true},
		{in: "true", want: EscalationSpec{Enabled: true}},
		{in: "false", want: EscalationSpec{Enabled: false}},
		{in: `{"enabled":true}`, want: EscalationSpec{Enabled: true}},
		{in: `{"enabled":true,"prompt_template":"hi","max_per_window":3,"window_seconds":60}`,
			want: EscalationSpec{Enabled: true, PromptTemplate: "hi", MaxPerWindow: 3, WindowSeconds: 60}},
		{in: `{"enabled":true,"max_per_window":-1}`, wantErr: true},
		{in: `not json`, wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseEscalationSpec(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseEscalationSpec(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseEscalationSpec(%q): %v", c.in, err)
			continue
		}
		if c.wantNil {
			if got != nil {
				t.Errorf("ParseEscalationSpec(%q) = %+v, want nil", c.in, got)
			}
			continue
		}
		if got == nil || *got != c.want {
			t.Errorf("ParseEscalationSpec(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestManager_EscalationRoundTrip(t *testing.T) {
	ctx := context.Background()
	m := testManager(t)
	a, err := m.Create(ctx, Agent{Name: "w", GroupID: "g1", EntityID: "e1", TalonSource: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// No row yet → not found.
	if _, found, err := m.GetEscalation(ctx, a.ID); err != nil || found {
		t.Fatalf("GetEscalation before save: found=%v err=%v", found, err)
	}

	// Save config + session.
	spec := EscalationSpec{Enabled: true, PromptTemplate: "t", MaxPerWindow: 4, WindowSeconds: 120}
	if err := m.SaveEscalation(ctx, a.ID, "sess-1", spec); err != nil {
		t.Fatalf("SaveEscalation: %v", err)
	}
	esc, found, err := m.GetEscalation(ctx, a.ID)
	if err != nil || !found {
		t.Fatalf("GetEscalation: found=%v err=%v", found, err)
	}
	if !esc.Enabled || esc.SessionID != "sess-1" || esc.PromptTemplate != "t" || esc.MaxPerWindow != 4 || esc.WindowSeconds != 120 {
		t.Errorf("round-trip mismatch: %+v", esc)
	}

	// Advance rate-limit state.
	now := time.Now().UTC().Truncate(time.Second)
	if err := m.SaveEscalationState(ctx, a.ID, 2, &now); err != nil {
		t.Fatalf("SaveEscalationState: %v", err)
	}
	esc, _, _ = m.GetEscalation(ctx, a.ID)
	if esc.FireCount != 2 || esc.WindowStart == nil || !esc.WindowStart.Equal(now) {
		t.Errorf("state not persisted: count=%d window=%v", esc.FireCount, esc.WindowStart)
	}

	// Re-saving config must PRESERVE the session (blank) and the rate state.
	if err := m.SaveEscalation(ctx, a.ID, "", EscalationSpec{Enabled: false}); err != nil {
		t.Fatalf("SaveEscalation (update): %v", err)
	}
	esc, _, _ = m.GetEscalation(ctx, a.ID)
	if esc.Enabled {
		t.Error("expected enabled=false after update")
	}
	if esc.SessionID != "sess-1" {
		t.Errorf("blank session on update must preserve prior; got %q", esc.SessionID)
	}
	if esc.FireCount != 2 {
		t.Errorf("config update must not reset rate state; count=%d", esc.FireCount)
	}

	// Deleting the agent removes the escalation row.
	if err := m.Delete(ctx, "g1", a.ID); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	if _, found, _ := m.GetEscalation(ctx, a.ID); found {
		t.Error("escalation row should be gone after agent delete")
	}
}
