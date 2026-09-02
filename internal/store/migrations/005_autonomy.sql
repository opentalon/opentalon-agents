-- autonomy: how much a workflow may do on its own — one of
--   'notify' (tell only) | 'ask' (propose, wait for OK) | 'act' (act, then report).
-- Set by the host wizard's autonomy step and surfaced on the workflow roster.
-- Defaults to 'ask' to match the product default (and template default_autonomy);
-- pre-existing rows adopt that same default.
ALTER TABLE agents ADD COLUMN autonomy TEXT NOT NULL DEFAULT 'ask';
