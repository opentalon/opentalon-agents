-- agent_escalations: per-agent opt-in escalation config plus its rolling
-- rate-limit state. One row per agent (only agents that opt in get a row).
-- Kept in a side table so the core `agents` SELECT paths are untouched; the
-- engine loads it lazily, only when a watcher actually fires.
--
--   - session_id      target session (the creator's packed session key) the
--                     escalation turn runs against and pushes its reply to.
--   - enabled         master opt-in switch for this agent.
--   - prompt_template optional override for the synthesized seed prompt.
--   - max_per_window  / window_seconds: rate limit (0 = use config default).
--   - fire_count      escalations fired in the current window.
--   - window_start    start of the current rate-limit window (RFC3339), or NULL.
CREATE TABLE IF NOT EXISTS agent_escalations (
  agent_id        TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL DEFAULT '',
  enabled         INTEGER NOT NULL DEFAULT 0,
  prompt_template TEXT NOT NULL DEFAULT '',
  max_per_window  INTEGER NOT NULL DEFAULT 0,
  window_seconds  INTEGER NOT NULL DEFAULT 0,
  fire_count      INTEGER NOT NULL DEFAULT 0,
  window_start    TEXT
);
