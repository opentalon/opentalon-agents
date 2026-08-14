-- agent_notifications: per-agent opt-in "push a message to the creator when
-- this agent fires" config, plus the delivery target captured at create time.
-- One row per agent (only agents that opt in get a row). Kept in a side table
-- for the same reason as agent_escalations: the core `agents` SELECT paths stay
-- untouched and the engine loads it lazily, only when an agent actually fires.
--
-- The delivery target is captured from the host-injected context args of the
-- create/update call, NOT from the Tln source — the LLM never sees or bakes
-- an address. session_id alone is usually enough (the host's packed session key
-- carries channel+conversation), but channel_id/conversation_id are stored too
-- when the host injects them, so a fire-time delivery never has to resolve the
-- originating session, which may be long gone.
--
--   - enabled          master opt-in switch for this agent.
--   - template         optional override for the synthesized message text.
--   - session_id       creator's packed session key.
--   - channel_id       channel that delivers (e.g. "telegram", "slack").
--   - conversation_id  chat/room id on that channel.
--   - sender_id        raw sender id of the creator (provenance only).
CREATE TABLE IF NOT EXISTS agent_notifications (
  agent_id        TEXT PRIMARY KEY,
  enabled         INTEGER NOT NULL DEFAULT 0,
  template        TEXT NOT NULL DEFAULT '',
  session_id      TEXT NOT NULL DEFAULT '',
  channel_id      TEXT NOT NULL DEFAULT '',
  conversation_id TEXT NOT NULL DEFAULT '',
  sender_id       TEXT NOT NULL DEFAULT ''
);
