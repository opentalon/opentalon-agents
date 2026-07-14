-- agents: one persistent, LLM-authored Talon workflow agent per row.
CREATE TABLE IF NOT EXISTS agents (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL,
  description   TEXT NOT NULL DEFAULT '',
  group_id      TEXT NOT NULL,
  entity_id     TEXT NOT NULL DEFAULT '',
  talon_source  TEXT NOT NULL,
  triggers_json TEXT NOT NULL DEFAULT '[]',
  enabled       INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agents_group_id ON agents(group_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_agents_group_name ON agents(group_id, name);

-- runs: one execution of an agent (manual/llm now; schedule/poll/webhook in Phase 2).
CREATE TABLE IF NOT EXISTS runs (
  id           TEXT PRIMARY KEY,
  agent_id     TEXT NOT NULL,
  trigger_type TEXT NOT NULL,
  status       TEXT NOT NULL,
  event_json   TEXT NOT NULL DEFAULT '',
  result_json  TEXT NOT NULL DEFAULT '',
  error        TEXT NOT NULL DEFAULT '',
  queued_at    TEXT NOT NULL,
  started_at   TEXT,
  finished_at  TEXT
);
CREATE INDEX IF NOT EXISTS idx_runs_agent_id ON runs(agent_id);
CREATE INDEX IF NOT EXISTS idx_runs_queued_at ON runs(queued_at);

-- agent_state: restart-safe watcher state, one row per agent (Phase 2).
CREATE TABLE IF NOT EXISTS agent_state (
  agent_id             TEXT PRIMARY KEY,
  facts_snapshot_json  TEXT NOT NULL DEFAULT '{}',
  entity_map_json      TEXT NOT NULL DEFAULT '{}',
  next_poll_at         TEXT,
  next_cron_at         TEXT,
  consecutive_failures INTEGER NOT NULL DEFAULT 0
);

-- pending_events: webhook/queued events awaiting the next tick (Phase 2/3).
CREATE TABLE IF NOT EXISTS pending_events (
  id           TEXT PRIMARY KEY,
  agent_id     TEXT NOT NULL,
  kind         TEXT NOT NULL,
  payload_json TEXT NOT NULL DEFAULT '{}',
  received_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pending_events_agent_id ON pending_events(agent_id);
