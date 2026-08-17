-- pending_events.event: the named domain event a queued delivery carries
-- (e.g. "item.status_changed"), set when it arrived via the /v1/events/<event>
-- emitter endpoint. Empty for a generic /v1/hooks/<agent> webhook delivery,
-- whose fact mapping comes from the agent's webhook trigger. The drain step
-- uses this to pick the agent's matching `event` trigger config (with taxonomy
-- defaults) instead of its webhook config.
ALTER TABLE pending_events ADD COLUMN event TEXT NOT NULL DEFAULT '';
