# opentalon-agents

OpenTalon plugin: **persistent, LLM-authored automations written in the Talon language.** Describe a task in chat; the LLM authors it as Talon source; the plugin validates and stores it, and runs it deterministically — no model in the loop at run time.

## Architecture

- **This plugin owns the agent**: it stores each agent's Talon source + triggers, records runs, and (from Phase 2) watches data on a schedule. State lives in its own SQLite/Postgres store.
- **It links no `talon-language` code.** The language is reached purely as a runtime proxy: during a bidi call the plugin invokes `host.RunAction("talon-plugin", …)` — `check` to validate source, `execute_workflow` to run it. `talon-plugin` stays a generic, agent-agnostic language gateway.
- Because reaching talon-plugin needs a live `HostCaller`, the plugin advertises `supports_callbacks: true` and handles every action on the bidi path.

## Actions (LLM-visible)

`create` · `list` · `show` · `run` · `update` · `enable` · `disable` · `delete`

`create`/`update` validate the Talon source via `talon-plugin.check` before storing — invalid source is rejected with diagnostics. `run` executes the stored source via `talon-plugin.execute_workflow` and records a run.

## Status

**Phase 1** (this release): scaffold, store + migrations, CRUD, and inline `run`. Schedules, polls, webhooks, and the autonomous tick engine follow in Phases 2–4 (some gated on `talon-language` #126–128).

## Config

Delivered by the host via `OPENTALON_CONFIG`:

```yaml
plugins:
  agents:
    enabled: true
    github: "opentalon/opentalon-agents"
    ref: "master"
    config:
      db:
        driver: sqlite          # or "postgres"
        dsn: "./agents.db"      # sqlite path, or postgres URL
      talon_plugin_name: talon-plugin   # capability name of the loaded talon-plugin
```

Requires `talon-plugin` (with the `check` action) to be loaded in the same host.

## Develop

```
make build   # build the plugin binary
make test    # unit tests (store round-trip; action layer with a fake HostCaller)
make vet
```
