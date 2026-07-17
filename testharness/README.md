# testharness — deterministic local E2E for the stock watcher

Runs the opentalon-agents watcher end-to-end against **Postgres** (deterministic
data), through a tiny custom **MCP server**, in the **opentalon host** with the
**console channel**. No brave-search, no external services.

```
testharness/
  seed.sql          # items + tickets tables, deterministic rows
  mcp/              # custom MCP server (own go module), Postgres-backed
  host-config.yaml  # blocks to merge into ../opentalon/config.yaml
```

## Data flow

```
console (you type)
  -> host + LLM  -> agents.create (Talon source + poll trigger)
scheduler.tick (1m)
  -> agents.tick -> poll: host.RunAction("mcp","testdb__get_item",{barcode})
                      -> mcp-plugin -> testharness MCP -> Postgres
                    map value_path -> fact -> talon-plugin.evaluate
                    on downward crossing < 10 -> workflow ->
                      host.RunAction("mcp","testdb__create_ticket",{barcode,qty})
                      -> Postgres tickets row
```

The agent's own store is SQLite (`agents.db`); **Postgres is only the inventory
data** behind the MCP server.

## MCP tools

| Tool | Args (all strings) | Returns |
|------|--------------------|---------|
| `get_item` | `barcode` | `{barcode, name, current_stock}` |
| `list_low_stock` | `threshold` | `{items:[{barcode,current_stock}]}` |
| `create_ticket` | `barcode`, `qty` | `{ticket_id, barcode, qty}` |

Namespaced by mcp-plugin as `testdb__<tool>`; a poll/workflow reaches them as
`server: "mcp", tool: "testdb__get_item"`.

## Run order

**1. Seed Postgres** (once; re-run to reset):
```
createdb opentalon_test          # first time only
psql -d opentalon_test -f testharness/seed.sql
```
DSN defaults to `postgres://<you>@localhost:5432/opentalon_test?sslmode=disable`;
override with `DATABASE_URL`.

**2. Build the agents plugin:**
```
make build                       # -> bin/opentalon-agents
```

**3. Start the MCP server** (leave running). It's a nested go module, so run
from inside it:
```
cd testharness/mcp && go run .   # listens :8765, SSE at /sse
```
Override the port with `ADDR=:9000` (update host-config.yaml's url to match).

**4. Merge `host-config.yaml`** into `../opentalon/config.yaml** (keep your
`models:`/`routing:` blocks; swap `channels:` to console; add the `plugins:`
and `scheduler:` blocks).

**5. Start the host:**
```
cd ../opentalon
make build            # host binary; clones+builds console/talon/mcp plugins on first run
./opentalon -config config.yaml
```

## Drive it

In the console, author the watcher:

> Create an agent named stock-abc that watches inventory item barcode ABC-123 and opens a refill ticket for 50 units when its stock drops below 10. Poll the `mcp` server tool `testdb__get_item` with arg barcode=ABC-123 every 1 minute; the stock value is at `current_stock`.

The LLM calls `agents.create` (validated via `talon-plugin.check`). First tick
polls stock=15 → no fire (edge-triggered). Now cross the threshold:

```
psql -d opentalon_test -c "UPDATE items SET current_stock = 8 WHERE barcode='ABC-123';"
```

Within a minute the tick sees `15 → 8`, the `on change` block fires once, and a
ticket appears:

```
psql -d opentalon_test -c "SELECT * FROM tickets;"
```

Edge semantics to verify: `8 → 7` does **not** re-fire; only a fresh `≥10 → <10`
crossing opens another ticket. Restart the host mid-run — the snapshot reloads
from `agents.db`, so replaying `8` fires nothing.

## Smoke-test the MCP server alone

Without the host, confirm tools work:
```
(cd testharness/mcp && go run .) &
# then use any MCP client against http://localhost:8765/sse
```
```
