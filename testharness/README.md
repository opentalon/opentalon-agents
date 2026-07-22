# testharness — deterministic local E2E for the stock watcher

Runs the opentalon-agents watcher end-to-end against **Postgres** (deterministic
data), through a tiny custom **MCP server**, in the **opentalon host** with the
**console channel**. No brave-search, no external services.

```
testharness/
  seed.sql          # items + tickets tables, deterministic rows
  mcp/              # custom MCP server (own go module), Postgres-backed
  vcr-proxy/        # Anthropic record/replay proxy (own go module)
  seed-agent/       # writes the watcher agent directly (deterministic gate)
  ci/config.yaml    # host config used by CI; a template for local runs
  ci/cassette.json  # committed VCR cassette for the authoring leg
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
Override the port with `ADDR=:9000` (update your host config's url to match).

**4. Write the host config.** Use `ci/config.yaml` as a template (it wires the
console channel + agents/talon/mcp plugins + `agents-tick` scheduler). For a
local run, swap the `/work/...` paths for your local paths and drop the
container-only `state:`/`log:` blocks.

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

## In CI

`.github/workflows/e2e.yml` runs this stack headless against the **published
host container** (`ghcr.io/opentalon/opentalon`), so nothing here needs building
the host. Three jobs:

- **deterministic** (PR gate) — seeds the watcher directly with
  `go run ./testharness/seed-agent` (the exact agent the LLM authors, no model),
  then drives tick → drop stock → assert one ticket. Reliable.
- **vcr-replay** (PR, label-gated) — pipes the authoring prompt to the console;
  the host calls Anthropic, but its `base_url` points at the **vcr-proxy**
  replaying `ci/cassette.json`. Real authoring path (chat → LLM → Talon agent),
  deterministic, **no secret**. Same tick → drop → assert. Opt-in because it's
  slow (see below).
- **cassette-check** (PR gate) — fast, no stack: fails if the committed
  cassette's `prompt_hash` doesn't match `sha256(internal/plugin/prompt.txt)`,
  i.e. the authoring prompt changed but the cassette wasn't re-recorded.
- **vcr-record** (release + manual) — same authoring, but the proxy runs in
  record mode against real Anthropic, stamps the current `prompt_hash`, and on a
  **published release** commits the refreshed cassette to master. Manual
  dispatch instead uploads it as a `vcr-cassette` artifact for review. Needs the
  `ANTHROPIC_API_KEY` repo secret. We re-record only when it matters — a prompt
  change (surfaced by cassette-check) or a release — not on a nightly timer,
  mirroring opentalon's VCR flow.

Beyond the ticket, `run-e2e.sh` asserts the authoring leg directly against
`agents.db`: the `stock-abc` agent row exists and at least one run reached
`completed` — so a ticket arriving by any other path can't pass the LLM leg.

### Running vcr-replay on a PR

`vcr-replay` builds the host image and clones/builds the console/talon/mcp
plugins, so it takes several minutes — too slow for every PR. It's **opt-in via
the `e2e-vcr` label**:

- **Add the `e2e-vcr` label** to a PR to run it — `gh pr edit <n> --add-label e2e-vcr`
  (or the GitHub UI). Adding the label re-triggers the workflow immediately, no
  new push needed; it re-runs on each later push while the label stays on.
- Remove the label to stop it running on subsequent pushes.
- **`deterministic` still runs on every PR** (it skips pure label events); only
  `vcr-replay` is gated.
- No label needed off-PR: `workflow_dispatch` runs it on demand from the Actions
  tab, and the nightly schedule runs `vcr-record`.

All three stand up Postgres, **datalevin-server** (from the
`opentalon/talon-language` repo — talon-plugin's backend at `:8898`) and the MCP
server, mount the plugin binary + rendered `ci/config.yaml` into the container,
and run `ci/run-e2e.sh`.

`ci/config.yaml` uses a 10s tick + 10s poll interval so the crossing is observed
in a couple of minutes; `seed-agent` takes `AGENT_INTERVAL` to match.

### VCR cassette

The cassette records the Anthropic responses for the authoring turn(s). The
proxy (`testharness/vcr-proxy`) sits at `<base_url>/v1/messages` and, like the
host's own in-process VCR player, replays interactions **in order**, ignoring
the request body (a request-hash mismatch only logs a warning). So the cassette
is valid as long as the host makes the same sequence of LLM calls — a prompt or
model change invalidates it, which the `cassette-check` job surfaces.

The cassette carries a `prompt_hash` = `sha256(internal/plugin/prompt.txt)`
(computed by `ci/prompt-hash.sh`). The **cassette-check** job recomputes it on
every PR and fails if it drifts, so a prompt change can't silently ship against
a stale recording — that red check is the signal to re-record.

Two ways to (re)record, both needing the `ANTHROPIC_API_KEY` secret/key:

- **CI (easiest):** trigger the workflow with `workflow_dispatch`. The
  `vcr-record` job runs the full stack, records, stamps the current
  `prompt_hash`, and uploads a `vcr-cassette` artifact. Download it, drop it at
  `testharness/ci/cassette.json`, and commit. (On a **published release** the
  same job commits the refreshed cassette to master automatically.)
- **Locally:** stand up the stack yourself (Postgres seeded, datalevin, MCP,
  assembled `WORK`, built `HOST_IMAGE` — see `ci/e2e.yml` steps), then:
  ```
  MODE=vcr-record ANTHROPIC_API_KEY=sk-ant-... WORK=... HOST_IMAGE=... \
    MCP_LOG=/tmp/mcp.log DATALEVIN_LOG=/tmp/dl.log \
    bash testharness/ci/run-e2e.sh
  ```
  The proxy writes `testharness/ci/cassette.json` on exit. Stamp the current
  prompt hash before committing (CI does this for you):
  ```
  jq --arg h "$(bash testharness/ci/prompt-hash.sh)" '. + {prompt_hash:$h}' \
    testharness/ci/cassette.json > c.tmp && mv c.tmp testharness/ci/cassette.json
  ```

The cassette stores only response bodies + a request hash — no API key.

## Smoke-test the MCP server alone

Without the host, confirm tools work:
```
(cd testharness/mcp && go run .) &
# then use any MCP client against http://localhost:8765/sse
```
```
