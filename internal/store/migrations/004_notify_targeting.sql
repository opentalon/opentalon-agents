-- notify targeting: extend agent_notifications so a notification can go to more
-- than the creator's conversation. Both columns are addressless — they name
-- audiences and channels by KIND, never a chat id or email; recipients are
-- resolved host-side at fire time.
--
--   - recipients_json  JSON array of {kind, role?}: who to notify. kind is one
--                      of "creator"/"me" (the stored delivery target),
--                      "responsible" (the person responsible for each fired
--                      item, resolved per item), or "role" (a named role/team,
--                      resolved to its members). Empty preserves the historical
--                      default: the creator.
--   - channels_json    JSON array of delivery channels ("in_app" / "email").
--                      Empty defaults to in-app only.
ALTER TABLE agent_notifications ADD COLUMN recipients_json TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_notifications ADD COLUMN channels_json TEXT NOT NULL DEFAULT '';
