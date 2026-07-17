-- Deterministic test data for the opentalon-agents stock watcher.
--
--   createdb opentalon_test        # once (or: CREATE DATABASE opentalon_test)
--   psql -d opentalon_test -f testharness/seed.sql
--
-- Re-runnable: drops and recreates everything each time.

BEGIN;

DROP TABLE IF EXISTS tickets;
DROP TABLE IF EXISTS items;

CREATE TABLE items (
  barcode       text PRIMARY KEY,
  name          text NOT NULL,
  current_stock integer NOT NULL
);

CREATE TABLE tickets (
  id         serial PRIMARY KEY,
  barcode    text NOT NULL,
  qty        integer NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Start above the threshold so the first poll fires nothing; you lower a row
-- to observe the downward-crossing fire once.
INSERT INTO items (barcode, name, current_stock) VALUES
  ('ABC-123', 'Widget',   15),
  ('DEF-456', 'Gadget',   40),
  ('GHI-789', 'Gizmo',     3);   -- already low: useful for list_low_stock

COMMIT;

-- Handy manual pokes while the watcher runs:
--   UPDATE items SET current_stock = 8 WHERE barcode = 'ABC-123';   -- fires
--   SELECT * FROM tickets;                                          -- observe act
