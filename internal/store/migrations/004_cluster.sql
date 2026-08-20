-- Cluster-safe execution.
--
-- idempotency_key lets a caller collapse duplicate event deliveries: a
-- repeated POST carrying the same key is dropped at enqueue time. The unique
-- index treats NULLs as distinct on both SQLite and Postgres, so events without
-- a key are never deduped (fully backward compatible with existing callers).
ALTER TABLE pending_events ADD COLUMN idempotency_key TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_pending_events_idem ON pending_events(idempotency_key);
