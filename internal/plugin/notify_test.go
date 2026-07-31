package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

// notifyHost scripts a poll, talon-plugin.evaluate, and the host's _notify.send
// entrypoint, capturing every send so tests can assert the delivery args.
type notifyHost struct {
	stock    []float64
	pollN    int
	evalResp []string
	evalN    int
	sends    []map[string]string
	outcome  string // structured reply for "send"; default {"delivered":true}
	sendErr  error  // when set, the send action itself fails
}

func (h *notifyHost) RunAction(_ context.Context, _, action string, args map[string]string) (pkg.CallResult, error) {
	switch action {
	case "get-item":
		v := h.stock[min(h.pollN, len(h.stock)-1)]
		h.pollN++
		return pkg.CallResult{StructuredContent: fmt.Sprintf(`{"item":{"barcode":"ABC-123","current_stock":%v}}`, v)}, nil
	case "evaluate":
		r := h.evalResp[min(h.evalN, len(h.evalResp)-1)]
		h.evalN++
		return pkg.CallResult{StructuredContent: r}, nil
	case notifySendAction:
		h.sends = append(h.sends, args)
		if h.sendErr != nil {
			return pkg.CallResult{}, h.sendErr
		}
		return pkg.CallResult{StructuredContent: h.outcome}, nil
	}
	return pkg.CallResult{}, nil
}

// crossingNotifyHost observes 15 then 8 — one downward crossing, one firing.
func crossingNotifyHost() *notifyHost {
	return &notifyHost{
		stock: []float64{15, 8},
		evalResp: []string{
			`{"ok":true,"firings":[],"snapshot":{"1":{"current_stock":15}}}`,
			`{"ok":true,"firings":[{"on_block":"on change attr \"current_stock\"","ref":"Refill stock","ref_kind":"workflow"}],"snapshot":{"1":{"current_stock":8}}}`,
		},
	}
}

func fireCrossingNotify(t *testing.T, e *Engine, host *notifyHost) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	if _, err := e.tickAt(ctx, host, t0); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if _, err := e.tickAt(ctx, host, t0.Add(6*time.Minute)); err != nil {
		t.Fatalf("tick2: %v", err)
	}
}

func TestEngine_NotifiesOnFireWithStoredTarget(t *testing.T) {
	ctx := context.Background()
	e, mgr := escEngineFixture(t)
	a := escWatcher(t, mgr)
	target := agent.DeliveryTarget{SessionID: "ent-1:telegram:42", ChannelID: "telegram", ConversationID: "42", SenderID: "u1"}
	if err := mgr.SaveNotification(ctx, a.ID, target, agent.NotifySpec{Enabled: true}); err != nil {
		t.Fatalf("save notification: %v", err)
	}
	host := crossingNotifyHost()
	fireCrossingNotify(t, e, host)

	if len(host.sends) != 1 {
		t.Fatalf("expected exactly one notification, got %d", len(host.sends))
	}
	got := host.sends[0]
	if got["session_id"] != target.SessionID || got["channel_id"] != "telegram" || got["conversation_id"] != "42" {
		t.Errorf("delivery target = %q/%q/%q", got["session_id"], got["channel_id"], got["conversation_id"])
	}
	if got["entity_id"] != "ent-1" || got["group_id"] != "g1" {
		t.Errorf("entity/group = %q/%q, want ent-1/g1", got["entity_id"], got["group_id"])
	}
	if got["source"] != "agent" || got["agent_id"] != a.ID || got["trigger"] != agent.TriggerPoll {
		t.Errorf("provenance = source:%q agent_id:%q trigger:%q", got["source"], got["agent_id"], got["trigger"])
	}
	if !strings.Contains(got["text"], a.Name) || !strings.Contains(got["text"], "current_stock") {
		t.Errorf("message should name the agent and carry the observed facts; got:\n%s", got["text"])
	}
}

func TestEngine_NoNotificationWhenNotOptedIn(t *testing.T) {
	e, mgr := escEngineFixture(t)
	_ = escWatcher(t, mgr) // no SaveNotification → notifications off
	host := crossingNotifyHost()
	fireCrossingNotify(t, e, host)
	if len(host.sends) != 0 {
		t.Fatalf("expected no notification for a non-opted-in agent, got %d", len(host.sends))
	}
}

func TestEngine_NoNotificationWithoutDeliveryTarget(t *testing.T) {
	ctx := context.Background()
	e, mgr := escEngineFixture(t)
	a := escWatcher(t, mgr)
	// Opted in, but nothing to address: the engine must skip, not panic or
	// send to an empty conversation.
	if err := mgr.SaveNotification(ctx, a.ID, agent.DeliveryTarget{SenderID: "u1"}, agent.NotifySpec{Enabled: true}); err != nil {
		t.Fatalf("save notification: %v", err)
	}
	host := crossingNotifyHost()
	fireCrossingNotify(t, e, host)
	if len(host.sends) != 0 {
		t.Fatalf("expected no send without an addressable target, got %d", len(host.sends))
	}
}

func TestEngine_NotificationFailureDoesNotFailTheTick(t *testing.T) {
	ctx := context.Background()
	e, mgr := escEngineFixture(t)
	a := escWatcher(t, mgr)
	if err := mgr.SaveNotification(ctx, a.ID, agent.DeliveryTarget{SessionID: "s"}, agent.NotifySpec{Enabled: true}); err != nil {
		t.Fatalf("save notification: %v", err)
	}
	host := crossingNotifyHost()
	host.sendErr = fmt.Errorf("no such plugin _notify")
	fireCrossingNotify(t, e, host) // fatals on a tick error

	// The firing itself must still be recorded even though delivery failed.
	runs, err := mgr.ListRuns(ctx, a.ID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Status != agent.StatusCompleted {
		t.Fatalf("expected one completed run despite the failed send, got %+v", runs)
	}
}

func TestEngine_NotificationRefusedByHost(t *testing.T) {
	ctx := context.Background()
	e, mgr := escEngineFixture(t)
	a := escWatcher(t, mgr)
	if err := mgr.SaveNotification(ctx, a.ID, agent.DeliveryTarget{SessionID: "s"}, agent.NotifySpec{Enabled: true}); err != nil {
		t.Fatalf("save notification: %v", err)
	}
	host := crossingNotifyHost()
	host.outcome = `{"delivered":false,"reason":"channel gone"}`
	fireCrossingNotify(t, e, host)
	if len(host.sends) != 1 {
		t.Fatalf("expected the send to be attempted once, got %d", len(host.sends))
	}
}

func TestNotifier_EmptyReplyCountsAsDelivered(t *testing.T) {
	// A host that accepts the send without returning a structured body did
	// deliver; treating that as a failure would spam the log on every fire.
	host := &notifyHost{}
	out, err := notifier{pluginName: "_notify"}.Send(context.Background(), host,
		agent.DeliveryTarget{SessionID: "s"}, "hi", "e", "g", "a", agent.TriggerPoll)
	if err != nil || !out.Delivered {
		t.Fatalf("empty reply: out=%+v err=%v", out, err)
	}
}

func TestNotifier_UndecodableReplyIsAnError(t *testing.T) {
	host := &notifyHost{outcome: "not json"}
	if _, err := (notifier{pluginName: "_notify"}).Send(context.Background(), host,
		agent.DeliveryTarget{SessionID: "s"}, "hi", "e", "g", "a", agent.TriggerPoll); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestRenderNotification(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	a := agent.Agent{Name: "restock", Description: "tell me when stock is low"}
	facts := json.RawMessage(`[{"attribute":"current_stock","value":8}]`)
	firings := []Firing{{OnBlock: `on change attr "current_stock"`, Ref: "ABC-123", RefKind: "entity"}}

	got := renderNotification(a, "", notifyEvent{Trigger: agent.TriggerPoll, Facts: facts, Firings: firings}, now)
	for _, want := range []string{"restock", "ABC-123", "current_stock"} {
		if !strings.Contains(got, want) {
			t.Errorf("default watcher text missing %q:\n%s", want, got)
		}
	}

	got = renderNotification(a, "", notifyEvent{Trigger: agent.TriggerSchedule, Result: json.RawMessage(`{"content":"ok"}`)}, now)
	if !strings.Contains(got, "ran on schedule") || !strings.Contains(got, `{"content":"ok"}`) {
		t.Errorf("schedule text:\n%s", got)
	}

	got = renderNotification(a, "", notifyEvent{Trigger: agent.TriggerSchedule, Error: "mcp tickets unreachable"}, now)
	if !strings.Contains(got, "failed to run") || !strings.Contains(got, "mcp tickets unreachable") {
		t.Errorf("failure text should carry the error:\n%s", got)
	}

	// An empty event still renders something sendable rather than a message
	// with holes in it.
	got = renderNotification(a, "", notifyEvent{Trigger: agent.TriggerPoll}, now)
	if !strings.Contains(got, "(none captured)") || !strings.Contains(got, "a watched condition") {
		t.Errorf("empty event text:\n%s", got)
	}

	got = renderNotification(a, "ASK={{description}} FACTS={{facts}} T={{trigger}}",
		notifyEvent{Trigger: agent.TriggerPoll, Facts: facts}, now)
	want := `ASK=tell me when stock is low FACTS=[{"attribute":"current_stock","value":8}] T=poll`
	if got != want {
		t.Errorf("template render = %q, want %q", got, want)
	}
}
