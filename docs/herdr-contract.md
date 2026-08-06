# herdr contract (Task-S spike result)

Confirmed against **herdr 0.7.5** on a live server (the `LocalVMClient`
integration was previously spike-TBD). This records the real CLI/JSON contract
arco depends on and the integration items that remain.

## Commands arco uses

| arco call | herdr command | notes |
|---|---|---|
| `ListAgents` | `herdr agent list` | JSON by default — **no `--json` flag** (it's rejected: `usage: herdr agent list`). |
| `Prompt` | `herdr agent prompt <TARGET> <TEXT> [--wait --until <s> --timeout <ms>]` | `TARGET` is a herdr agent target (e.g. `pane_id`), not arco's workspace. |
| `Kill` | `herdr agent send-keys <TARGET> <KEY>...` | canonical Escape is `esc`; `C-c` (Ctrl-C) still to confirm as a key token. |
| launch (NOT wired) | `herdr agent start <NAME> --kind <KIND> --pane <ID> [-- <AGENT_ARG>...]` | `--kind` ∈ claude/codex/gemini/…; `-- <args>` is where `permcompile.LaunchArgs` go; requires an existing pane at a shell prompt. |
| liveness wait | `herdr agent wait <TARGET> --until <state> --timeout <ms>` | states below. |

Other subcommands: `agent get <target>`, `read`, `rename`, `focus`, `attach`,
`explain`; socket API via `herdr api snapshot` / `herdr api schema [--json]`.

## `agent list` output (real envelope)

```json
{"id":"cli:agent:list","result":{"type":"agent_list","agents":[
  {"agent":"claude","agent_status":"idle","pane_id":"wB:p1",
   "workspace_id":"wB","terminal_id":"term_…","cwd":"…","revision":12,
   "state_change_seq":587,"tab_id":"wB:t1","terminal_title":"…"}
]}}
```

- **Wrapped** in `result.agents[]` — NOT a bare array.
- Agent identity fields: `agent` (kind), `agent_status`, `pane_id`,
  `workspace_id`, `terminal_id`, `cwd`, `revision`, `state_change_seq`.
- **No `boot_id` / `pid_start_time` / worker PID** — herdr exposes no PID
  identity. `terminal_id` (stable per pane) is the closest identity token; arco
  carries it in `AgentObs.BootID` for the sweep's reuse guard.

## `agent_status` enum (authoritative, from `api schema`)

`idle | working | blocked | done | unknown` — exactly five. Only **`done`** means
the agent finished; `idle/working/blocked/unknown` are alive; a pane absent from
`agent list` is gone. (The pre-spike code's assumed `gone/exited/dead` do not
exist.)

## What the spike FIXED in `LocalVMClient`

- `agent list` (dropped the invalid `--json`).
- Parse the `result.agents[]` envelope with real field names.
- `terminalHerdrStatus = {done}` (was `{done,gone,exited,dead}`).
- Map `workspace_id`→`AgentObs.Workspace`, `terminal_id`→`AgentObs.BootID`.

## Integration items still open (before `use_local_vm` is the default)

1. **Worker↔agent correlation.** herdr identifies agents by
   `workspace_id`/`pane_id`; arco identifies workers by its own
   `workspace = "arco_<ulid>"`. They don't match. arco must capture herdr's
   `pane_id` (and `workspace_id`/`terminal_id`) **at launch** and store them on
   the worker row, then target Prompt/Kill/liveness by `pane_id` and correlate
   `ListAgents` results by the stored ids — not by arco's workspace string.
2. **Launch path.** `reconcile.Dispatch`/`Delegate` currently only `Prompt` an
   existing pane. The real spawn is `herdr agent start <name> --kind <kind>
   --pane <id> -- <args>`, where `<args>` are `permcompile.LaunchArgs` and the
   process env is `spawnenv.Scrub`'d and a provider lease is acquired first. This
   needs a new `core.VMClient` launch method + a herdr pane to start into.
3. **`send-keys` key token.** Confirm `C-c` (Ctrl-C) is accepted for the
   best-effort `Kill`, or use the documented canonical key names.
