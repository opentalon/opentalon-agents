package plugin

import (
	"context"
	"strings"
	"testing"

	pkg "github.com/opentalon/opentalon/pkg/plugin"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

// createWithNotify runs create with the given notify arg plus whatever delivery
// context the caller wants injected.
func createWithNotify(t *testing.T, h *Handler, name, notify string, delivery map[string]string) pkg.Response {
	t.Helper()
	args := ctxArgs(map[string]string{"name": name, "talon_source": `workflow "ok" {}`, "notify": notify})
	for k, v := range delivery {
		args[k] = v
	}
	return h.ExecuteWithCallbacks(context.Background(), pkg.Request{ID: "c1", Action: "create", Args: args}, &fakeHost{})
}

func TestCreate_NotifyCapturesDeliveryContext(t *testing.T) {
	h := testHandler(t)
	resp := createWithNotify(t, h, "restock", `{"enabled":true,"template":"low: {{facts}}"}`, map[string]string{
		"session_id": "u1:telegram:42", "channel_id": "telegram", "conversation_id": "42", "sender_id": "u1",
	})
	if resp.Error != "" {
		t.Fatalf("create should succeed: %q", resp.Error)
	}
	a, err := h.mgr.Get(context.Background(), "g1", "restock")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	n, found, err := h.mgr.GetNotification(context.Background(), a.ID)
	if err != nil || !found {
		t.Fatalf("notification row: found=%v err=%v", found, err)
	}
	if !n.Enabled || n.Template != "low: {{facts}}" {
		t.Errorf("config not stored: %+v", n)
	}
	want := agent.DeliveryTarget{SessionID: "u1:telegram:42", ChannelID: "telegram", ConversationID: "42", SenderID: "u1"}
	if n.Target != want {
		t.Errorf("target = %+v, want %+v", n.Target, want)
	}
}

func TestCreate_NotifyRejectedWithoutDeliveryContext(t *testing.T) {
	h := testHandler(t)
	// No session/channel/conversation injected (e.g. a control-plane call):
	// enabling notify would store an agent that can never reach anyone.
	resp := createWithNotify(t, h, "restock", "true", nil)
	if resp.Error == "" {
		t.Fatal("expected notify.enabled with no delivery context to be rejected")
	}
	if !strings.Contains(resp.Error, "deliver") {
		t.Errorf("error should explain the missing target; got %q", resp.Error)
	}
}

func TestCreate_NotifyDisabledNeedsNoDeliveryContext(t *testing.T) {
	h := testHandler(t)
	if resp := createWithNotify(t, h, "restock", "false", nil); resp.Error != "" {
		t.Fatalf("notify:false must not require a target: %q", resp.Error)
	}
}

func TestCreate_MalformedNotifyRejected(t *testing.T) {
	h := testHandler(t)
	resp := createWithNotify(t, h, "restock", "{oops", map[string]string{"session_id": "s"})
	if resp.Error == "" {
		t.Fatal("expected malformed notify to be rejected")
	}
	if _, err := h.mgr.Get(context.Background(), "g1", "restock"); err == nil {
		t.Error("a malformed notify must reject before the agent is stored")
	}
}

func TestUpdate_EnablingNotifyReusesCapturedTarget(t *testing.T) {
	ctx := context.Background()
	h := testHandler(t)
	if resp := createWithNotify(t, h, "restock", "false", map[string]string{"session_id": "u1:telegram:42"}); resp.Error != "" {
		t.Fatalf("create: %q", resp.Error)
	}
	// Later update carries no delivery context, but the create-time target is
	// still on file, so enabling is allowed and keeps addressing it.
	resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "u1", Action: "update", Args: ctxArgs(map[string]string{
		"id": "restock", "talon_source": `workflow "ok2" {}`, "notify": "true",
	})}, &fakeHost{})
	if resp.Error != "" {
		t.Fatalf("update: %q", resp.Error)
	}
	a, _ := h.mgr.Get(ctx, "g1", "restock")
	n, _, err := h.mgr.GetNotification(ctx, a.ID)
	if err != nil {
		t.Fatalf("get notification: %v", err)
	}
	if !n.Enabled || n.Target.SessionID != "u1:telegram:42" {
		t.Errorf("expected enabled with the captured target; got %+v", n)
	}
}

func TestShow_ReportsNotificationWithoutLeakingTheAddress(t *testing.T) {
	ctx := context.Background()
	h := testHandler(t)
	if resp := createWithNotify(t, h, "restock", "true", map[string]string{
		"session_id": "u1:telegram:42", "channel_id": "telegram", "conversation_id": "42",
	}); resp.Error != "" {
		t.Fatalf("create: %q", resp.Error)
	}
	resp := h.ExecuteWithCallbacks(ctx, pkg.Request{ID: "s1", Action: "show",
		Args: ctxArgs(map[string]string{"id": "restock"})}, &fakeHost{})
	if resp.Error != "" {
		t.Fatalf("show: %q", resp.Error)
	}
	if !strings.Contains(resp.StructuredContent, `"notification"`) {
		t.Errorf("show should report the notification config; got %s", resp.StructuredContent)
	}
	// The LLM must not learn the address — that's the whole point of capturing
	// it host-side.
	for _, leak := range []string{"u1:telegram:42", `"conversation_id"`} {
		if strings.Contains(resp.StructuredContent, leak) {
			t.Errorf("show leaked the delivery address (%s): %s", leak, resp.StructuredContent)
		}
	}
}
