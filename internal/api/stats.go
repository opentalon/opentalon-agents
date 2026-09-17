package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/opentalon/opentalon-agents/internal/agent"
)

// Statistics for a workflow's run history, aggregated SERVER-SIDE so the host
// app (Timly) just renders numbers — it owns none of the run data or the
// derivation. Tiles cover the real-run window (how many ran, how many actions
// they took) plus a snapshot of the most recent dry run (what it would have
// done). Run rows are the raw history the "Run history" table paints.

const (
	defaultStatsWindowDays = 30
	// Cap how many runs we scan per stats request; the history table is paged
	// separately. Plenty for a 30-day window at a daily/hourly cadence.
	statsScanLimit = 500
)

// statsTiles are the summary numbers shown above the tables.
type statsTiles struct {
	WindowDays int `json:"window_days"`
	// Real (non-dry) runs and what they did, within the window.
	RealRuns     int `json:"real_runs"`
	FailedRuns   int `json:"failed_runs"`
	NoMatchRuns  int `json:"no_match_runs"` // completed real runs that matched nothing
	ActionsTaken int `json:"actions_taken"` // writes performed across real runs (tickets opened, etc.)
	// Timestamps (RFC3339), empty when never.
	LastRealRunAt string `json:"last_real_run_at,omitempty"`
	LastDryRunAt  string `json:"last_dry_run_at,omitempty"`
	// Snapshot of the most recent dry run (nil when none).
	LastDryRun *dryRunSnapshot `json:"last_dry_run,omitempty"`
}

// dryRunSnapshot is what the latest dry run found — reads are real, writes are
// what WOULD have happened.
type dryRunSnapshot struct {
	Matched int    `json:"matched"`  // records the reads matched
	WouldDo int    `json:"would_do"` // writes that were skipped (and would run live)
	Status  string `json:"status"`   // completed | failed
	At      string `json:"at"`       // RFC3339
	Error   string `json:"error,omitempty"`
	// Detail for the "what would happen" table: the matched records (raw, as the
	// read tool returned them — the host renders the columns it knows), and the
	// operation each would trigger (e.g. "create_ticket", "notify_user").
	Items  []any  `json:"items,omitempty"`
	Action string `json:"action,omitempty"`
}

// statsRun is one row of the run-history table.
type statsRun struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`   // trigger_type: dry_run | schedule | event | …
	Status  string `json:"status"` // completed | failed | …
	At      string `json:"at"`     // RFC3339 (finished, else queued)
	Matched int    `json:"matched"`
	Actions int    `json:"actions"` // writes performed (live) or would-do (dry run)
	Error   string `json:"error,omitempty"`
}

type statsResponse struct {
	Tiles statsTiles `json:"tiles"`
	Runs  []statsRun `json:"runs"`
}

// handleStats serves GET /v1/agents/{id}/stats?group_id=X&window_days=30 —
// aggregated run statistics for one workflow. Read-only.
func (h *server) handleStats(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	id := r.PathValue("id")
	groupID := r.URL.Query().Get("group_id")
	if groupID == "" {
		writeErr(w, http.StatusBadRequest, "group_id is required")
		return
	}
	a, err := h.mgr.Get(r.Context(), groupID, id)
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "agent not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	windowDays := defaultStatsWindowDays
	if v := r.URL.Query().Get("window_days"); v != "" {
		if n, convErr := strconv.Atoi(v); convErr == nil && n > 0 {
			windowDays = n
		}
	}

	runs, err := h.mgr.ListRuns(r.Context(), a.ID, statsScanLimit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, buildStats(runs, windowDays, time.Now().UTC()))
}

// buildStats is the pure aggregation, split out so it is unit-testable without
// a store. runs are newest-first (ListRuns order).
func buildStats(runs []agent.Run, windowDays int, now time.Time) statsResponse {
	cutoff := now.AddDate(0, 0, -windowDays)
	tiles := statsTiles{WindowDays: windowDays}
	rows := make([]statsRun, 0, len(runs))

	for _, run := range runs {
		at := run.FinishedAt
		if at == nil {
			at = &run.QueuedAt
		}
		matched, actions := deriveRunCounts(run)
		rows = append(rows, statsRun{
			ID: run.ID, Kind: run.TriggerType, Status: run.Status,
			At: rfc3339(at), Matched: matched, Actions: actions, Error: run.Error,
		})

		dry := run.TriggerType == agent.TriggerDryRun
		if dry {
			if tiles.LastDryRun == nil { // runs are newest-first → first seen is latest
				tiles.LastDryRunAt = rfc3339(at)
				items, action := deriveDryRunDetail(run)
				tiles.LastDryRun = &dryRunSnapshot{
					Matched: matched, WouldDo: actions, Status: run.Status,
					At: rfc3339(at), Error: run.Error, Items: items, Action: action,
				}
			}
			continue // dry runs never count toward real-run tiles
		}

		// Real run within the window.
		if at != nil && at.Before(cutoff) {
			continue
		}
		tiles.RealRuns++
		if tiles.LastRealRunAt == "" {
			tiles.LastRealRunAt = rfc3339(at)
		}
		switch run.Status {
		case agent.StatusFailed:
			tiles.FailedRuns++
		case agent.StatusCompleted:
			if matched == 0 {
				tiles.NoMatchRuns++
			}
			tiles.ActionsTaken += actions
		}
	}
	return statsResponse{Tiles: tiles, Runs: rows}
}

func rfc3339(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// deriveRunCounts reads a run's Result (the per-step trace) and returns how many
// records the run MATCHED (the largest list-like step output) and how many
// WRITES it performed or would perform (create/update/delete/notify steps; a
// dry run's skipped writes carry `"dry_run": true`). Best-effort: any parse
// failure yields (0,0) — never an error, so stats never break on an odd trace.
func deriveRunCounts(run agent.Run) (matched, actions int) {
	if len(run.Result) == 0 {
		return 0, 0
	}
	var payload struct {
		Blocks map[string]struct {
			Steps []struct {
				Name   string          `json:"name"`
				Output json.RawMessage `json:"output"`
			} `json:"steps"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(run.Result, &payload); err != nil {
		return 0, 0
	}
	for _, block := range payload.Blocks {
		for _, step := range block.Steps {
			if n, ok := listCount(step.Output); ok && n > matched {
				matched = n
			}
			if isWriteOutput(step.Output) {
				actions++
			}
		}
	}
	return matched, actions
}

// deriveDryRunDetail extracts the "what would happen" detail from a dry run's
// trace: the matched records (the largest list-like step's items — those are
// what the run acted on) and the operation the skipped write would perform
// (e.g. "create_ticket", "notify_user"). Best-effort; empty on any odd trace.
// Items are capped so a huge match set can't bloat the stats payload.
func deriveDryRunDetail(run agent.Run) (items []any, action string) {
	if len(run.Result) == 0 {
		return nil, ""
	}
	var payload struct {
		Blocks map[string]struct {
			Steps []struct {
				Output json.RawMessage `json:"output"`
			} `json:"steps"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(run.Result, &payload); err != nil {
		return nil, ""
	}
	const maxItems = 100
	for _, block := range payload.Blocks {
		for _, step := range block.Steps {
			if rows, ok := listItems(step.Output); ok && len(rows) > len(items) {
				items = rows
				if len(items) > maxItems {
					items = items[:maxItems]
				}
			}
			if action == "" {
				if op := wouldDoOperation(step.Output); op != "" {
					action = op
				}
			}
		}
	}
	return items, action
}

// listItems returns the rows of a list-like tool output.
func listItems(raw json.RawMessage) ([]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	for _, k := range []string{"items", "results", "records", "tickets", "users"} {
		if arr, ok := obj[k].([]any); ok {
			return arr, true
		}
	}
	return nil, false
}

// wouldDoOperation returns the operation a skipped write would have performed,
// from the synthetic dry-run output the executor records.
func wouldDoOperation(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	if dry, _ := obj["dry_run"].(bool); dry {
		if op, ok := obj["operation"].(string); ok {
			return op
		}
	}
	return ""
}

// listCount returns the number of rows in a list-like tool output (an object
// with an "items" array, optionally a pagination total), and whether it looked
// like one.
func listCount(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return 0, false
	}
	// Prefer an explicit total from pagination when present.
	if pag, ok := obj["pagination"].(map[string]any); ok {
		for _, k := range []string{"total_count", "total", "count"} {
			if f, ok := pag[k].(float64); ok {
				return int(f), true
			}
		}
	}
	for _, k := range []string{"items", "results", "records", "tickets", "users"} {
		if arr, ok := obj[k].([]any); ok {
			return len(arr), true
		}
	}
	return 0, false
}

// isWriteOutput reports whether a step output is a write: a dry run's skipped
// write carries `"dry_run": true`; a live write returns the created/updated
// record, which we detect by an "id" without a list payload.
func isWriteOutput(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	if dry, ok := obj["dry_run"].(bool); ok && dry {
		return true
	}
	if _, isList := listCount(raw); isList {
		return false // a list read, not a write
	}
	_, hasID := obj["id"]
	return hasID
}
