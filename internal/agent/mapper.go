package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// Fact is one asserted fact, serialized to the shape talon-plugin's
// `evaluate` action expects: {"record_id","attribute","value"}. RecordID
// is the integer entity id (as a string) — Talon snapshots are keyed by
// int, so external ids are mapped through the per-agent registry first.
type Fact struct {
	RecordID  string `json:"record_id"`
	Attribute string `json:"attribute"`
	Value     any    `json:"value"`
}

// Map extracts facts from a decoded poll response for a poll trigger. It
// reads the watched value at ValuePath and the entity's external id at
// IDField (dot-paths), maps the external id to a stable integer via the
// per-agent registry, and returns the fact(s) plus the (possibly grown)
// registry for the caller to persist.
//
// v1 handles a single watched entity per poll — the stock-watcher shape.
// Multi-entity list responses (+ max-items) are a later phase.
// Map extracts facts from a decoded poll response. With no ItemsPath it
// maps a single entity (value_path/id_field relative to the whole
// response). With ItemsPath set it maps EACH element of that list (a
// multi-entity watch), value_path/id_field relative to each item, capped
// at maxItems (0 = uncapped). It returns the facts, the grown registry,
// and the number of items dropped by the cap (so the caller can surface a
// non-silent truncation).
func Map(pc PollConfig, response any, registry map[string]int, maxItems int) ([]Fact, map[string]int, int, error) {
	if registry == nil {
		registry = map[string]int{}
	}
	if pc.ItemsPath == "" {
		facts, reg, err := MapValue(pc.ValuePath, pc.IDField, pc.Attribute, response, registry)
		return facts, reg, 0, err
	}

	node, ok := dotPath(response, pc.ItemsPath)
	if !ok {
		return nil, registry, 0, fmt.Errorf("mapper: items_path %q not found in response", pc.ItemsPath)
	}
	list, ok := node.([]any)
	if !ok {
		return nil, registry, 0, fmt.Errorf("mapper: items_path %q is not a list", pc.ItemsPath)
	}

	truncated := 0
	if maxItems > 0 && len(list) > maxItems {
		truncated = len(list) - maxItems
		list = list[:maxItems]
	}
	facts := make([]Fact, 0, len(list))
	for _, item := range list {
		f, reg, err := MapValue(pc.ValuePath, pc.IDField, pc.Attribute, item, registry)
		if err != nil {
			return nil, registry, 0, err
		}
		registry = reg
		facts = append(facts, f...)
	}
	return facts, registry, truncated, nil
}

// MapValue extracts one fact from a decoded response given a mapping spec
// (value_path / id_field / attribute). It is shared by poll and webhook
// triggers. The external id is mapped to a stable int via the registry.
func MapValue(valuePath, idField, attribute string, response any, registry map[string]int) ([]Fact, map[string]int, error) {
	if registry == nil {
		registry = map[string]int{}
	}

	val, ok := dotPath(response, valuePath)
	if !ok {
		return nil, registry, fmt.Errorf("mapper: value_path %q not found in response", valuePath)
	}

	extID := "self" // single implicit entity when no id_field is given
	if idField != "" {
		idv, ok := dotPath(response, idField)
		if !ok {
			return nil, registry, fmt.Errorf("mapper: id_field %q not found in response", idField)
		}
		extID = fmt.Sprintf("%v", idv)
	}

	if attribute == "" {
		attribute = "value"
	}
	id := assignEntityID(registry, extID)
	return []Fact{{RecordID: strconv.Itoa(id), Attribute: attribute, Value: val}}, registry, nil
}

// assignEntityID returns the stable int id for an external id, assigning a
// fresh one (max+1, starting at 1) on first sight. The registry MUST
// persist so the same external entity keeps the same int across ticks and
// restarts (Talon snapshots are int-keyed).
func assignEntityID(reg map[string]int, ext string) int {
	if id, ok := reg[ext]; ok {
		return id
	}
	max := 0
	for _, v := range reg {
		if v > max {
			max = v
		}
	}
	id := max + 1
	reg[ext] = id
	return id
}

// dotPath resolves a dot-separated path into a decoded JSON value.
// Segments index into objects by key and into arrays by integer position
// (e.g. "items.0.stock"). An empty path returns the value unchanged.
func dotPath(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	cur := v
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			nv, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = nv
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}
