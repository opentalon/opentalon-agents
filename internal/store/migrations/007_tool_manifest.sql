-- tool_manifest: the set of MCP tools the agent's tln_source calls, as a JSON
--   array of {"server","tool"} pairs, extracted from the source at
--   create/update time (tln-plugin's check action returns it). A derived index
--   over tln_source — kept so a later Timly API change can enumerate which
--   stored agents reference a given provider/operation and need migrating, e.g.
--     SELECT id, name FROM agents WHERE tool_manifest LIKE '%"tool":"list_items"%';
-- Defaults to '[]' so scans read a valid empty array and pre-existing rows are valid.
ALTER TABLE agents ADD COLUMN tool_manifest TEXT NOT NULL DEFAULT '[]';

-- api_version: the Timly API generation the agent was authored against (e.g.
--   "v1"). The engine cannot derive it (the version lives in the openapi-plugin
--   spec, not in the tln source), so it is stamped by the host at create/update
--   via the optional `api_version` arg; blank when the host does not supply one.
--   Pairs with tool_manifest to scope a migration to one API generation.
ALTER TABLE agents ADD COLUMN api_version TEXT NOT NULL DEFAULT '';
