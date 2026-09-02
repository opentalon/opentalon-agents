-- config: opaque JSON blob of the host wizard's structured selections
--   (template key + slot values: area, category, ticket_template_id,
--   assignee, recipient, channel, autonomy…). The plugin never interprets
--   or queries it — it stores it verbatim and echoes it back so the host
--   can rehydrate its wizard for editing. The executable artifact remains
--   tln_source; this is edit-time metadata only.
-- Defaults to '' so scans read a plain string and pre-existing rows are valid.
ALTER TABLE agents ADD COLUMN config TEXT NOT NULL DEFAULT '';
