package agent

import (
	"fmt"
	"sort"
	"strings"
)

// This file defines the NAMED domain-event taxonomy. An agent can subscribe to
// a semantic event (e.g. "item.status_changed") with an `event` trigger; an
// emitter POSTs the named event to /v1/events/<event>, and the plugin maps its
// payload to a fact using the event's default mapping (overridable per agent)
// and evaluates it on the next tick, exactly like a poll or webhook.
//
// The taxonomy is the contract between the emitter (e.g. Timly) and the agents:
// each entry documents the payload the emitter must send and the fact it maps
// to. Keeping it in one place lets both the validator (author time) and the
// mapper (fire time) agree on names, and lets `explain`/docs enumerate it.

// EventDef is one entry in the named-event taxonomy: the semantic event name,
// the payload the emitter is expected to POST, and the DEFAULT fact mapping
// applied when an agent's `event` trigger omits value_path/id_field/attribute.
type EventDef struct {
	// Name is the wire name of the event, e.g. "item.status_changed".
	Name string
	// Summary is a one-line human description (for docs and `explain`).
	Summary string
	// ValuePath / IDField / Attribute are the default mapping: the dot-paths
	// into the emitted payload for the watched value and the entity id, and the
	// fact attribute the value is asserted under. An agent may override any of
	// them in its trigger config, but the defaults make "subscribe by name
	// alone" work.
	ValuePath string
	IDField   string
	Attribute string
	// PayloadKeys are the top-level fields the emitter is expected to include.
	// Documented so the contract is explicit; not enforced (an emitter may send
	// extra fields, and a custom mapping may read nested paths).
	PayloadKeys []string
}

// eventTaxonomy is the registry of named domain events. It mirrors the semantic
// events the host app (Timly) emits — item lifecycle and checkout activity —
// each mapped to a fact an `on change attr` block can watch. Discrete events
// (a checkout happening) are modelled as a change of a monotonic value (the
// checkout id) so the same edge-triggered crossing machinery applies.
var eventTaxonomy = map[string]EventDef{
	"item.status_changed": {
		Name:        "item.status_changed",
		Summary:     "An item's status changed (e.g. to \"defective\" or \"in_repair\").",
		ValuePath:   "status",
		IDField:     "item_id",
		Attribute:   "status",
		PayloadKeys: []string{"item_id", "status"},
	},
	"item.stock_changed": {
		Name:        "item.stock_changed",
		Summary:     "An item's on-hand stock quantity changed.",
		ValuePath:   "stock",
		IDField:     "item_id",
		Attribute:   "current_stock",
		PayloadKeys: []string{"item_id", "stock"},
	},
	"checkout.created": {
		Name:        "checkout.created",
		Summary:     "An item was checked out. The checkout id is the watched value.",
		ValuePath:   "checkout_id",
		IDField:     "item_id",
		Attribute:   "active_checkout",
		PayloadKeys: []string{"item_id", "checkout_id"},
	},
	"checkout.returned": {
		Name:        "checkout.returned",
		Summary:     "A checked-out item was returned. The checkout id is the watched value.",
		ValuePath:   "checkout_id",
		IDField:     "item_id",
		Attribute:   "last_return",
		PayloadKeys: []string{"item_id", "checkout_id"},
	},
	"item.synced": {
		Name:        "item.synced",
		Summary:     "An item was (re)synced from an external source (e.g. Lansweeper).",
		ValuePath:   "synced_at",
		IDField:     "item_id",
		Attribute:   "last_synced",
		PayloadKeys: []string{"item_id", "synced_at"},
	},
}

// LookupEvent returns the taxonomy entry for a named event, if it is known.
func LookupEvent(name string) (EventDef, bool) {
	d, ok := eventTaxonomy[name]
	return d, ok
}

// KnownEvents returns the event names in the taxonomy, sorted, for error
// messages, docs, and `explain`.
func KnownEvents() []string {
	names := make([]string, 0, len(eventTaxonomy))
	for n := range eventTaxonomy {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// EventConfig is the `config` payload of an event trigger: the named domain
// event to subscribe to, plus the same value_path/id_field/attribute mapping as
// poll/webhook. When a mapping field is empty it falls back to the taxonomy
// default for Event, so a template can subscribe by name alone:
//
//	{"type":"event","config":{"event":"item.status_changed"}}
type EventConfig struct {
	Event     string `json:"event"`
	ValuePath string `json:"value_path,omitempty"`
	IDField   string `json:"id_field,omitempty"`
	Attribute string `json:"attribute,omitempty"`
}

// Resolved returns the config with taxonomy defaults filled in for any mapping
// field the author left empty. It errors if Event is not in the taxonomy.
func (c EventConfig) Resolved() (EventConfig, error) {
	def, ok := LookupEvent(c.Event)
	if !ok {
		return EventConfig{}, fmt.Errorf("unknown event %q; known events: %s", c.Event, strings.Join(KnownEvents(), ", "))
	}
	out := c
	if out.ValuePath == "" {
		out.ValuePath = def.ValuePath
	}
	if out.IDField == "" {
		out.IDField = def.IDField
	}
	if out.Attribute == "" {
		out.Attribute = def.Attribute
	}
	return out, nil
}
