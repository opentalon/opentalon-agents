# opentalon-agents

OpenTalon plugin for **persistent, LLM-authored automations written in the [Tln](https://github.com/opentalon/tln-language) language.**

A user describes a task in chat — *"monitor the stock item with barcode ABC-123; when stock drops below 10, open a refill ticket"* — the LLM authors it **as Tln source**, and this plugin stores it and runs it **deterministically and autonomously**: no model in the loop at run time.

> **Status.** Phase 1 (create/validate/run agents) is shipped. The autonomous watcher engine (polling + reactive evaluation) is Phase 2, in progress — see [Roadmap](#roadmap). The design below is the target; sections are marked _shipped_ / _planned_.

---

## Why Tln (and not the LLM) at run time

The LLM is great at *authoring* an automation from a fuzzy request, but you don't want a model re-deciding what to do every 5 minutes forever. So the LLM writes the logic **once**, in Tln — a small deterministic language — and from then on the plugin executes it mechanically. Authoring is probabilistic; running is deterministic and cheap.

---

## Architecture

`opentalon-agents` is an **external gRPC plugin**. It owns everything about an agent — the stored Tln source, triggers, run history, and watcher state — in its own SQLite/Postgres store. It **links no `tln-language` code at all**: it reaches the language only by calling **`tln-plugin`** actions *through the host*. And it doesn't run its own scheduler — it rides the host's.

```mermaid
flowchart LR
  U["User (chat)"] -->|create / run| O
  SCHED["host scheduler<br/>(agents.tick every 1m)"] --> O

  subgraph HOST["opentalon host"]
    O["Orchestrator<br/>(dispatch + credentials)"]
  end

  O <-->|gRPC bidi + HostCaller| A["opentalon-agents<br/>(agent logic + state)"]
  A --> DB[("SQLite / Postgres<br/>agents · runs · state")]

  A -.->|"host.RunAction<br/>check / evaluate"| T["tln-plugin<br/>(generic language gateway)"]
  T -.-> LANG[["Tln language<br/>Session · on-blocks"]]
  A -.->|"host.RunAction<br/>poll / act"| MCP["MCP servers<br/>(inventory, tickets, …)"]
  T -.->|"MCP steps in fired workflows"| MCP
```

**Separation of concerns**

| Piece | Responsibility |
|-------|----------------|
| **opentalon-agents** | Agent lifecycle & state: store Tln source + triggers, poll, map data → facts, keep the fact snapshot, record runs. All "agent" logic lives here. |
| **tln-plugin** | A *generic, agent-agnostic* gateway to the Tln language. Exposes `check` (validate source) and `evaluate` (reactively run source against facts). Knows nothing about agents. |
| **opentalon host** | Loads plugins, exposes their actions to the LLM, dispatches calls (with credentials), and fires the periodic `tick` via its scheduler. |

---

## How the host invokes this plugin

Nothing is hardcoded in core — the host learns about the plugin from config, then drives it two ways.

**1. Registration** — declare it in the host `config.yaml`. On startup the host launches the binary and calls `Capabilities()`, which advertises the capability name `agents`, its actions, the authoring prompt, and `supports_callbacks: true`.

```yaml
plugins:
  agents:
    enabled: true
    plugin: "./plugins/opentalon-agents/opentalon-agents"   # or github/ref
    expose_http: true                 # only if using webhook triggers
    config:
      db: { driver: sqlite, dsn: ./agents.db }
      tln_plugin_name: tln-plugin
      webhook_secret: "${AGENTS_WEBHOOK_SECRET}"   # shared bearer for the webhook endpoint
      notify_plugin_name: _notify                  # host entrypoint for pushed notifications
  tln-plugin:
    enabled: true
    github: opentalon/tln-plugin
    ref: v0.2.0        # provides `check` + `evaluate`

scheduler:
  jobs:
    - name: agents-tick
      interval: "1m"
      action: agents.tick     # <capability-name>.<action>
```

**2. LLM-initiated** _(shipped)_ — the advertised actions become tools (`agents.create`, `agents.run`, …). When the user asks for an automation, the LLM calls `agents.create`; because we declare `supports_callbacks`, the host dispatches over **ExecuteBidi**, so our handler gets a **live `HostCaller`** to reach `tln-plugin`.

**3. Autonomous tick** _(engine implemented; requires the `scheduler.jobs` entry above)_ — the LLM is *not* involved. The `scheduler.jobs` entry fires `agents.tick` on a timer; the scheduler calls it through the orchestrator, again over bidi with a live `HostCaller`. The tick is **unscoped** (no user/group), so it sweeps all agents system-wide.

---

## A real example: the stock watcher

**In chat:** *"Create an agent that watches stock item barcode ABC-123 and opens a refill ticket when it drops below 10."*

The LLM calls **`agents.create`** with a name, the Tln source, and a poll trigger:

**`tln_source`** — the logic, authored by the LLM:

```tln
# React to changes in an item's stock. Fire ONCE on the downward crossing
# below 10 (prev >= 10 and new < 10) — not every tick while it stays low.
on change attr "current_stock" {
  when prev_value >= 10 and new_value < 10
  workflow "Refill stock"
}

# The action that runs when the on-block fires.
workflow "Refill stock" {
  step "ticket" {
    mcp "tickets" "create" {
      title "Refill needed for ABC-123"
      item  step("trigger").result.entity     # the item that crossed the threshold
      qty   50
    }
  }
}
```

**`triggers`** — how the engine feeds it data (structured config, also from the LLM):

```json
[
  {
    "type": "poll",
    "config": {
      "server": "inventory",
      "tool": "get-item",
      "args": { "barcode": "ABC-123" },
      "interval": "5m",
      "value_path": "item.current_stock",
      "id_field": "item.barcode",
      "attribute": "current_stock"
    }
  }
]
```

On `create`, the source is validated (`tln-plugin.check`) before it's stored — invalid Tln is rejected with compile diagnostics so the LLM can fix and retry.

---

## What happens on each tick — and where facts come in

A **fact** is an EAV triple: *(entity, attribute, value)* — e.g. *(item ABC-123, `current_stock`, 8)*. The watcher works by turning each poll into a fact and letting Tln's `Session` react to **changes** in that fact. The **snapshot** is the set of facts the agent remembers between ticks; it's what makes the watcher *edge-triggered* (fire once on the crossing) and *restart-safe* (a value that hasn't changed since last time fires nothing).

```mermaid
sequenceDiagram
  autonumber
  participant S as host scheduler
  participant O as orchestrator
  participant A as opentalon-agents
  participant M as inventory MCP
  participant T as tln-plugin

  S->>O: RunAction(agents.tick)
  O->>A: ExecuteBidi(tick) + live HostCaller
  Note over A: find agents whose poll is due
  A->>O: RunAction(inventory.get-item {barcode: ABC-123})
  O->>M: get-item
  M-->>A: { item: { barcode: "ABC-123", current_stock: 8 } }
  Note over A: MAP — extract value_path (8),<br/>barcode → entity id 1 (registry),<br/>build Fact{1, current_stock, 8}
  A->>O: RunAction(tln-plugin.evaluate<br/>{source, snapshot:{1:{current_stock:15}}, facts:[{1,current_stock,8}]})
  O->>T: ExecuteBidi(evaluate) + live HostCaller
  Note over T: hydrate Session from snapshot (15),<br/>Assert(8): 15→8 crosses < 10 → on-block fires
  T->>O: RunAction(tickets.create {title, item, qty})
  O-->>T: ok
  T-->>A: { firings: ["Refill stock"], snapshot: {1:{current_stock:8}} }
  Note over A: persist snapshot + next_poll_at,<br/>record a run
```

Step by step:

1. **Poll** — the engine calls the item's MCP tool through the host: `inventory.get-item{barcode: ABC-123}` → `{ "item": { "current_stock": 8 } }`.
2. **Map → fact** — it extracts the value at `value_path` (`8`), maps the external id (`ABC-123`) to a small integer via a per-agent registry, and builds a fact: `Fact{RecordID: "1", Attribute: "current_stock", Value: 8}`. *(The int mapping is required because Tln's snapshot is keyed by integer entity id.)*
3. **Evaluate** — it calls `tln-plugin.evaluate` with the stored `source`, the prior `snapshot` (last known stock, e.g. 15), and the new `facts`. tln-plugin hydrates a `Session` from the snapshot, asserts the new fact, and the `on change` block sees `15 → 8`: the `when prev_value >= 10 and new_value < 10` guard holds, so it fires and runs `"Refill stock"` — whose `mcp "tickets" "create"` step is dispatched back through the host.
4. **Persist** — the engine stores the returned snapshot (`current_stock: 8`), schedules the next poll, and records a run.

Because it's edge-triggered: `8 → 8` (unchanged) fires nothing; `8 → 7` doesn't re-fire (it didn't cross *down through* 10 again); only a fresh `≥10 → <10` transition opens another ticket. Restart is safe too — the snapshot is reloaded from the DB, so replaying the last value fires nothing.

---

## A real example: the watcher that investigates and asks you

The stock watcher above takes a **fixed** action on fire (open a ticket) — no model at run time. Some tasks instead need *judgement* when they fire: *"stock is trending down — which items are actually at risk given lead times, and should I reorder or just alert you?"* That's an LLM turn with a **question back to the user**, triggered by a deterministic signal. Opt in with the **`escalate`** argument.

**In chat:** *"Watch all my inventory SKUs, and when any of them drops below 20, look into which ones are genuinely at risk and check with me before ordering anything."*

The LLM authors the **same kind of watcher** — detection stays deterministic — but leaves the reaction to an escalation turn:

**`tln_source`** — a coarse, deterministic trip. The `when` threshold is a literal (a watcher can't compare against another fact), so the *nuanced* per-SKU reasoning is deferred to the escalation turn:

```tln
on change attr "current_stock" {
  when prev_value >= 20 and new_value < 20
  workflow "Flag for review"
}

# No fixed action here — the reaction IS the escalation turn the plugin starts
# when this block fires. Keep the workflow a light marker (some Tln builds
# want at least one step); the real work happens in the assistant turn.
workflow "Flag for review" {
  step "note" { mcp "log" "info" { message "SKU crossed the review threshold" } }
}
```

**`triggers`** — one poll fans out over every SKU in the response (`items_path`), so the *same* on-block fires per item that crosses:

```json
[
  {
    "type": "poll",
    "config": {
      "server": "inventory",
      "tool": "list-items",
      "interval": "10m",
      "items_path": "items",
      "value_path": "current_stock",
      "id_field": "barcode",
      "attribute": "current_stock"
    }
  }
]
```

**`escalate`** — the opt-in. Detection stays deterministic; only the reaction becomes model-driven, so it's rate-limited:

```json
{ "enabled": true, "max_per_window": 5, "window_seconds": 3600 }
```

`create` also captures the caller's `session_id` (injected by the host) — that's the channel the escalation turn runs in and pushes its reply to.

### What happens when it fires

When a SKU crosses `20 → below`, the engine records the firing **and** starts a background assistant turn in the user's session (via the host's built-in `_escalate` entrypoint — enabled with `orchestrator.escalation.enabled`). The turn is seeded with a synthesized prompt built from the agent's stored `description` (the user's original ask), what tripped, and the observed facts:

```
Your background agent "sku-reorder-watch" just fired and escalated to you.

What the user originally asked for:
watch all inventory SKUs; when any drops below 20, check which are at risk and ask me before ordering

What tripped the watcher:
- on-block "on change attr \"current_stock\"" for ABC-123 (workflow)
- on-block "on change attr \"current_stock\"" for XYZ-9 (workflow)

Latest observed values (facts):
[{"record_id":"1","attribute":"current_stock","value":18},{"record_id":"4","attribute":"current_stock","value":6}]

Investigate what is going on — you may fan out focused sub-agent checks to look into
each affected entity — then decide what, if anything, should be done. Come back to the
user with a short summary and ask how they would like to proceed. Do not take
irreversible action without confirming with the user first.
```

The assistant then **investigates with sub-agent checks** (one per at-risk SKU — supplier lead time, open POs), synthesizes, and **asks the user** — its reply is pushed to their channel tagged agent-originated (`source: agent`, `agent_id`, `trigger: poll`):

> Two SKUs crossed the line. **XYZ-9** is the real risk: 6 units left, ~3-week supplier lead time, no open PO — it'll stock out before a reorder lands. **ABC-123** (18 units) is comfortably covered by an open PO arriving Friday.
>
> Want me to raise a reorder for **XYZ-9** now (I'd suggest 200 units to cover the lead time), or just keep alerting you? ABC-123 I'd leave alone.

The user replies in the same conversation — *"Reorder XYZ-9, 200 units."* — and the **next turn acts** (opens the PO through the appropriate tool). Detection stayed deterministic and cheap; the LLM only ran once the signal tripped, and nothing irreversible happened without a human OK.

```mermaid
sequenceDiagram
  autonumber
  participant A as opentalon-agents
  participant O as orchestrator
  participant E as _escalate host
  participant Sub as sub-agent checks
  participant U as user channel

  Note over A: watcher fires, XYZ-9 crosses 20 to 6,<br/>escalate on and within rate limit
  A->>O: RunAction _escalate.turn<br/>session_id, prompt, entity_id, group_id,<br/>source agent, agent_id, trigger poll
  O->>E: start background turn, seed prompt hidden
  E->>Sub: investigate each at-risk SKU, lead time and open POs
  Sub-->>E: XYZ-9 at risk, ABC-123 covered
  E-->>U: push reply tagged source agent, ask reorder XYZ-9 or just alert
  U->>O: Reorder XYZ-9, 200 units
  O-->>U: next turn opens the PO
```

Guardrails: escalation is **opt-in per agent**, **edge-triggered** (fires on the crossing, not every tick), and **rate-limited** (`max_per_window` / `window_seconds`, per-agent or via the plugin defaults). Turns cost tokens and are billed to the agent owner's chat budget, so a flapping signal can't run away.

---

## Proactive notifications: the agent that just tells you

Escalation is the expensive answer to *"let me know"*. Most of the time the user doesn't want an investigation — they want **a message**: *"tell me when stock drops below 10."* That's the **`notify`** argument.

```json
{ "enabled": true }
```

or, with wording of your own:

```json
{ "enabled": true, "template": "{{agent_name}}: {{firings}}\n{{facts}}" }
```

When a notify-enabled agent fires — or, for a `schedule` agent, after each run — the plugin **pushes a plain message**. By default it goes to the conversation the agent was created in. No model runs, no turn starts, nothing is billed.

**The LLM never sees an address.** `create` / `update` declare `session_id`, `channel_id`, `conversation_id`, `sender_id` as `InjectContextArgs`; the host fills in whichever it can resolve, and the plugin stores them in `agent_notifications` alongside the opt-in. At fire time the engine calls the host's notify entrypoint (`_notify.send`, configurable via `notify_plugin_name`) with that stored target plus provenance (`source: agent`, `agent_id`, `trigger`). `show` reports *that* notifications are on and *which kinds* of audience/channel, never *where* they go — so a chat address can't leak into a future Tln program.

### Recipients & channels

`notify` defaults to the creator, in-app — but a firing often needs to reach **the person responsible for the item**, or **a role/team**, over **email** as well as in-app. Add `recipients` and `channels`, which name audiences and delivery methods **by kind, never by address**:

```json
{
  "enabled": true,
  "recipients": [{ "kind": "responsible" }, { "kind": "role", "role": "procurement" }],
  "channels": ["in_app", "email"]
}
```

- **`recipients`** (default `[{"kind":"creator"}]`):
  - `creator` (alias `me`) — the stored delivery target (the conversation the agent was authored in).
  - `responsible` — the person responsible for **each fired item**, resolved per item at fire time. The engine reverses the agent's entity registry to the fired item's external id and hands it to the host, which resolves the address; this needs an `id_field` on the trigger (a watch with no item id has nobody to attribute to, and that recipient is skipped).
  - `role` — a named role/team; the host resolves its members. Requires `role`.
- **`channels`** (default `["in_app"]`): any of `in_app`, `email`. Each recipient is delivered over each selected channel (so `2 recipients × 2 channels` fans out to four host sends).

The boundary is unchanged: recipients are resolved **host-side at fire time** via the same `_notify.send` entrypoint (now also carrying `recipient_kind`, `role`, `item_id`, `delivery_channel`). No chat id, email, or role membership ever enters the Tln source or the LLM context. Because only a `creator` recipient needs a stored conversation, a notify that targets **only** responsible/role can be enabled from a non-interactive context; targeting the creator (the default) still requires a conversation. Each delivery is independent — one recipient failing is logged and skipped, never aborting the rest of the fan-out or the tick.

**Operator requirement:** the host's `_notify` entrypoint is opt-in and ships dark, like `_escalate`. Run the current host — `ghcr.io/opentalon/opentalon:latest`, rebuilt from `master` on every merge, which carries the entrypoint ([opentalon#322](https://github.com/opentalon/opentalon/pull/322)) — and set `orchestrator.notify.enabled: true` in the host config. Without the entrypoint the call fails and is logged; with it present but disabled, every send comes back `{delivered:false,reason:"disabled"}`. Either way the tick that produced the firing is unaffected.

This is what closes the hole the old pattern left: previously an agent could only push by baking `mcp "<channel>" "send" { chat_id "..." }` into its source, which hardcodes the destination and breaks the moment the user switches channel.

```mermaid
sequenceDiagram
  autonumber
  participant U as user chat
  participant A as opentalon-agents
  participant N as _notify host
  Note over U,A: create time
  U->>A: agents.create, notify true<br/>host injects session_id, channel_id,<br/>conversation_id, sender_id
  A->>A: store target in agent_notifications
  Note over A: fire time, no LLM
  A->>N: RunAction _notify.send<br/>stored target, text,<br/>source agent, agent_id, trigger
  N-->>U: message pushed to the same conversation
```

| | `notify` | `escalate` |
|---|---|---|
| Reaction | fixed message, pushed | full assistant turn |
| Cost | none | tokens, billed to the owner |
| Can ask a question | no | yes |
| Bounded by | edge-triggered firing | firing **+** rate limit |

Both are opt-in and independent; an agent may use either, both, or neither. A delivery failure (host without a notify entrypoint, channel gone) is **logged and dropped** — never retried, since a stale alert is worse than no alert, and never allowed to fail the tick that produced it. Enabling `notify` to the **creator** from a context with no resolvable conversation is rejected at create time rather than stored as an agent that can't reach anyone (a notify that targets only a role/responsible person has no such requirement).

**Targeting** ([#47](https://github.com/opentalon/opentalon-agents/issues/47)): beyond the creator, a notification can fan out to the **responsible person per fired item** and to **named roles/teams**, over **in-app and/or email** — see [Recipients & channels](#recipients--channels) above. Recipients are named by kind and resolved host-side at fire time, so the address boundary holds.

**Not covered:** two-way Q&A from a background agent (the agent pushes; the user replies in chat and the LLM handles it from there) and autonomous self-update — both deliberately out of scope, see [#28](https://github.com/opentalon/opentalon-agents/issues/28).

---

## Actions

| Action | Description |
|--------|-------------|
| `create` | Author an agent from Tln source (+ optional triggers, + optional `escalate`, + optional `notify`). Validated via `tln-plugin.check` before storing. |
| `list` / `show` | Inspect agents (`show` returns the full Tln source, plus the escalation and notification config when enabled — never the delivery address). |
| `run` | Execute an agent's program now (inline) and return the result. _shipped_ |
| `update` | Replace the Tln source / triggers / escalation / notification setting (re-validated). |
| `enable` / `disable` / `delete` | Lifecycle. |
| `tick` | Hidden (`UserOnly`) — fired by the host scheduler to drive watchers (poll → map → evaluate). _implemented_ |

`group_id` / `entity_id` are injected by the host per call; every operation is group-scoped. `create` / `update` additionally take the caller's delivery context (`session_id`, `channel_id`, `conversation_id`, `sender_id`) so escalations and notifications can reach the creator later. All actions run on the bidi path (a live `HostCaller` is needed to reach `tln-plugin`).

---

## Roadmap

- **Phase 1 — _shipped_**: plugin scaffold, SQLite/Postgres store + migrations, agent CRUD, inline `run` (validate via `check`, execute via `execute_workflow`).
- **Phase 2 — _in progress_**: the watcher/tick engine. Tracked in [#1](https://github.com/opentalon/opentalon-agents/issues/1): poll trigger + state ([#3](https://github.com/opentalon/opentalon-agents/issues/3), [#4](https://github.com/opentalon/opentalon-agents/issues/4)), `tlnproxy.Evaluate` ([#5](https://github.com/opentalon/opentalon-agents/issues/5)), poller/mapper/engine ([#6](https://github.com/opentalon/opentalon-agents/issues/6)–[#8](https://github.com/opentalon/opentalon-agents/issues/8)), tick + scheduler wiring ([#9](https://github.com/opentalon/opentalon-agents/issues/9)), prompt + E2E ([#10](https://github.com/opentalon/opentalon-agents/issues/10), [#11](https://github.com/opentalon/opentalon-agents/issues/11)). Depends on `tln-plugin`'s `evaluate` action (**done**, `v0.2.0`).
- **Phase 3 — webhook triggers _implemented_**: push data instead of polling. Declare a `webhook` trigger (mapping only), set `expose_http: true` + a `webhook_secret`, and POST to the endpoint below. The handler enqueues into `pending_events`; the next tick drains and evaluates it (the HTTP request has no `HostCaller`, so evaluation is deferred to the tick).
- **Phase 4 — _implemented_** ([#13](https://github.com/opentalon/opentalon-agents/issues/13)): **`schedule` (cron) triggers** (one-shot `workflow` agent on a 5-field cron, tracked via `next_cron_at`, run through `execute_workflow`); **create-time trigger validation**; **multi-entity polls** — a poll trigger with `items_path` maps every element of a list to a fact (value_path/id_field per item), capped by `max_items_per_poll` (drops are logged, never silent); and a **configurable backoff cap** (`max_backoff_seconds`, default 30m).
- **Phase 5 — escalation & sub-agent mode _implemented_** ([#30](https://github.com/opentalon/opentalon-agents/issues/30)): a **hybrid** reaction. Detection stays deterministic (the tick), but an agent can opt into `escalate` so that when its watcher fires, instead of only running a fixed Tln action, the plugin starts a full assistant **reasoning turn** in the creator's session (via the host's built-in `_escalate` entrypoint — requires `opentalon` ≥ `v0.0.22` with `orchestrator.escalation.enabled`). That turn can investigate (including fanning out sub-agent checks), decide, and **ask the user** what to do; its reply is pushed back to the user's channel tagged as agent-originated (`source: agent`, `agent_id`, `trigger`). Opt-in per agent, edge-triggered, and rate-limited (`escalation_max_per_window` / `escalation_window_seconds`, per-agent overridable).

- **Phase 6 — proactive notifications _implemented_** ([#28](https://github.com/opentalon/opentalon-agents/issues/28)): an agent can opt into `notify` and **push a message to its creator** when it fires (or after each scheduled run) — model-free and free of charge, the cheap counterpart to `escalate`. The delivery target (`session_id` / `channel_id` / `conversation_id` / `sender_id`) is captured from host-injected context at create time into `agent_notifications`, so the LLM never sees or hardcodes a chat address; at fire time the engine calls the host's `_notify.send` entrypoint (`notify_plugin_name`; requires the current host image with `orchestrator.notify.enabled`) with that target plus `source: agent` / `agent_id` / `trigger`. Delivery failures are logged and dropped, never retried and never fatal to the tick.

## Durability & restart

The plugin holds **no in-memory agent state** — the engine is DB-driven. Every `agents.tick` re-queries the DB for enabled, due agents (poll / schedule / queued webhooks) and processes them. So after a plugin or host restart, agents resume automatically:

- **agents** (source, triggers, enabled) and **agent_state** (`facts_snapshot_json`, `entity_map_json`, `next_poll_at`, `next_cron_at`, `consecutive_failures`) are persisted; each `evaluate` re-hydrates the Session from the snapshot, so an unchanged value fires nothing (no false re-fires on restart).
- **pending_events** (queued webhooks) survive and drain on the next tick.
- **agent_escalations** / **agent_notifications** (opt-in config + the creator's session / delivery target) are persisted too, so an agent keeps reaching the right person across restarts — no live session is required at fire time.

The only external requirement is that the host keeps firing `agents.tick` — its `scheduler.jobs` entry lives in host config (dynamic jobs persist in `dataDir/scheduler/jobs.yaml`), so it resumes on host restart too. Nothing needs to "bring agents back online."

## Webhooks

With `expose_http: true`, the host reverse-proxies `/<config-key>/*` to the plugin's private listener. An external system pushes data with:

```
POST /agents/v1/hooks/<agent-name>?user_id=<owner>
Authorization: Bearer <webhook_secret>
Content-Type: application/json

{ "barcode": "ABC-123", "stock": 8 }
```

- The shared **`webhook_secret`** gates the endpoint (401 otherwise; 503 if unset). The **`user_id`** param (query or a top-level body field) scopes the lookup to that user's agent named in the path.
- The body is mapped to a fact by the agent's `webhook` trigger config (`value_path`/`id_field`/`attribute`) and evaluated on the next tick — same reactive semantics as polling. Returns `202 {"status":"queued"}`.

### Query agents

`GET /v1/agents` (same `Authorization: Bearer <webhook_secret>`) lists agent summaries — never the Tln source. AND-combined filters:

```
GET /agents/v1/agents?group_id=<g>&entity_id=<user>&name=<substr>&enabled=true
```

`group_id` (tenant), `entity_id` (creating user), `name` (case-insensitive substring), `enabled`. Fetch a single agent's full source via the `show` action.

---

## Develop

```
make build   # build the plugin binary
make test    # unit tests (store round-trip; action layer with a fake HostCaller)
make vet
```

Requires `tln-plugin` (≥ `v0.2.0`, provides `check` + `evaluate`) loaded in the same host. `opentalon-agents` itself imports **no** `tln-language` — all language access is via `host.RunAction("tln-plugin", …)`.

### End-to-end tests

A full-stack E2E (host + plugins + stub MCP) lives in `testharness/` and runs via `.github/workflows/e2e.yml`. The fast **deterministic** job runs on every PR. The **vcr-replay** job — real chat → LLM → Tln authoring, replayed from a committed cassette — is slow, so it's opt-in: add the **`e2e-vcr`** label to a PR to run it (`gh pr edit <n> --add-label e2e-vcr`). The committed cassette carries a `prompt_hash`; a cheap **cassette-check** job fails any PR where the authoring prompt changed but the cassette wasn't re-recorded. Re-recording against the real model happens on a published release (or manual dispatch), not nightly. See `testharness/README.md`.
