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

## arco ships a herdr plugin

`plugin/arco-status/` is a thin plugin (manifest + one shell script) that puts
the fleet snapshot in the herdr UI. Link it with
`herdr plugin link $(pwd)/plugin/arco-status`; its single action, `status`,
runs `arco status --json` (resolved via PATH) and passes stdout through, so
`herdr plugin action invoke status --plugin arco-status` and `herdr plugin log`
show the raw `StatusResp`. Manifest schema (herdr 0.7.5): `herdr-plugin.toml`
with required `id`/`name`/`version`/`min_herdr_version`, optional `platforms`,
and `[[actions]]` entries of `id`/`title`/`command` where `command` is an argv
**array** whose `"./"`-relative argv[0] resolves against the plugin root.

## Integration items still open (before `use_local_vm` is the default)

1. **Worker↔agent correlation.** herdr identifies agents by
   `workspace_id`/`pane_id`; arco identifies workers by its own
   `workspace = "arco_<ulid>"`. They don't match. arco must capture herdr's
   `pane_id` (and `workspace_id`/`terminal_id`) **at launch** and store them on
   the worker row, then target Prompt/Kill/liveness by `pane_id` and correlate
   `ListAgents` results by the stored ids — not by arco's workspace string.
   **Consequence — do NOT set `use_local_vm` before this is wired:** the sweep
   looks up liveness by `w.Workspace` ("arco_<ulid>") against herdr's
   `workspace_id` ("wB"), which never matches, so every live worker would miss
   and be false-finalized `Lost`/`Failed` (and a launch-error fallback →
   `Failed`). It is not merely inert; enabling it early nukes the fleet.
2. **Launch path — IMPLEMENTED (Fake-tested), live-verify pending.**
   `Engine.Spawn` (repo dispatch) now composes provision → quarantine → compile →
   `LaunchArgs` → `spawnenv.Scrub` → `VMClient.Launch`, and `LocalVMClient.Launch`
   implements the real chain to the CONFIRMED shapes:
   `workspace create --no-focus --label <name> --cwd <workdir> [--env …]` →
   resolve `workspace_id` by label (`workspace list`) → resolve `pane_id`
   (`pane list`) → `agent start <name> --kind <kind> --pane <pane_id> -- <args>`;
   ref = `pane_id`. IDs are parsed only from the read-only list envelopes
   (confirmed), not create responses. **Remaining live-verify (why `use_local_vm`
   stays default-off + guarded):** (a) that create→list→start works end-to-end on
   a live server (needs spawning a real agent); (b) whether
   `workspace create --env` REPLACES or AUGMENTS the pane's shell env — P1's
   scrubbed-spawn-env fully holds only if it replaces (else herdr's own env may
   leak to the worker); (c) **pane-readiness race** — `agent start` requires the
   pane at a shell prompt, but Launch fires `pane list`+`agent start` right after
   `workspace create` returns; if create returns before the pane's shell is
   ready, `agent start` may land early — a readiness guard (`agent wait` / retry,
   or confirming create blocks until prompt-ready) is needed. Also: the
   launch-ERROR liveness fallback in `Engine.Spawn` correlates by arco's
   workspace, but `LocalVMClient.ListAgents` reports herdr's `workspace_id`, so it
   won't match for the real client — a half-spawned agent whose ref-capture
   errored would be false-`Failed` (gated behind `use_local_vm`; fix when lifting
   the gate). A provider lease at spawn is still TODO (no pool selection policy).
3. **`send-keys` key token.** Confirm `C-c` (Ctrl-C) is accepted for the
   best-effort `Kill`, or use the documented canonical key names.
4. **Sandbox (`srt`) — arco cannot wrap the launch argv; the rollout is a herdr
   agent manifest.** T2.2 landed the opt-in sandbox config (`[sandbox] enabled,
   policy_path` + `ARCO_SANDBOX`/`ARCO_SANDBOX_POLICY`), the boot gate
   (preflight `sandbox_srt_present`: CRITICAL when enabled, so a config that
   promises confinement can never boot without the binary), and the pure argv
   transformer `vm.SandboxWrap(enabled, srtBin, policyPath, argv)` →
   `srt [--settings <path>] <argv…>`. What it did **not** do is prefix
   `LocalVMClient.Launch`'s command, because herdr owns that command:
   `agent start <NAME> --kind <KIND> --pane <ID> [--timeout <MS>]
   [-- <AGENT_ARG>…]` (confirmed on herdr 0.7.5 via `agent start --help`, and
   `AgentStartParams = {name, kind, pane_id, args, timeout_ms}` in
   `herdr api schema --json`). `--kind` is a **closed enum** of supported agent
   kinds (`pi, claude, codex, gemini, cursor, …`) whose canonical executable
   herdr resolves and launches itself; the trailing `--` args are ARGUMENTS TO
   THAT executable, not a command line. There is no per-start command/exec
   override, so an `srt` prefix on that argv would be passed to the agent as a
   flag instead of caging it — and it would break `agent start`'s detection
   contract ("success means the expected agent was detected in the pane").
   **Rollout path (operator work, not arco code):** herdr resolves agent kinds
   from **agent manifests** (`agent_manifest_status` / `agent_manifest_reload`
   in the API; `AgentManifestInfo` reports `source`, `source_kind` and
   `local_override_shadowing_remote`, i.e. a local manifest may shadow the
   bundled one). Define a manifest whose command is srt-wrapped — e.g. a
   `claude` override running `srt --settings <policy> claude …` — reload it, and
   point arco's worker `kind` at it. `SandboxWrap` stays the single place that
   knows srt's argv shape, ready for the day a start-time command override (or a
   manifest-generation step) exists; the guideline tests pin its properties, not
   srt's flag spelling. Second-layer isolation (`systemd-run --user` transient
   units, plan-rev7 D2) is likewise out of this seam's reach for the same reason.
