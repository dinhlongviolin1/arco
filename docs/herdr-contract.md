# herdr contract (Task-S spike result)

Confirmed against **herdr 0.7.5** on a live server (the `LocalVMClient`
integration was previously spike-TBD). This records the real CLI/JSON contract
arco depends on and the integration items that remain.

> **Re-verified 2026-08-10 (rev7/T4.1) against herdr 0.7.5** — the version this
> host still runs (`herdr --version` → `herdr 0.7.5`). **0.8.0 is NOT installed
> here**, so nothing below describes 0.8.0; see "Pending 0.8.0 re-verification"
> at the end for the exact points to re-check on upgrade. The re-check used
> READ-ONLY commands only (`--version`, `agent list`, `agent get <target>`,
> `api schema --json`) because live panes on this host are running real work.
> API `protocol` 17, `schema_version` 1, 89 methods.

## Commands arco uses

| arco call | herdr command | notes |
|---|---|---|
| `ListAgents` | `herdr agent list` | JSON by default — **no `--json` flag** (still rejected at 0.7.5: `usage: herdr agent list`). |
| `Prompt` | `herdr agent prompt <TARGET> <TEXT> [--wait --until <s> --timeout <ms>]` | `TARGET` is a herdr agent target (e.g. `pane_id`), not arco's workspace. |
| `Kill` | `herdr workspace close <WORKSPACE_ID>` | **corrected 2026-08-10**: `LocalVMClient.Kill` derives `workspace_id` from the pane target (`"wS:pN"` → `wS`) and closes the workspace — it does NOT send keys. arco no longer calls `agent send-keys` anywhere. |
| launch | `herdr agent start <NAME> --kind <KIND> --pane <ID> [--timeout <MS>] [-- <AGENT_ARG>...]` | wired since Task-S. `--kind` ∈ claude/codex/gemini/…; `-- <args>` is where `permcompile.LaunchArgs` go; requires an existing pane at a shell prompt. `timeout_ms` must be **> 3000 and ≤ 300000** (schema). |
| launch (workspace) | `herdr workspace create [--no-focus] [--label <L>] [--cwd <D>] [--env K=V …]` | `WorkspaceCreateParams = {cwd, env (map), focus (default **false**), label}` — **not focusing is the API default**, `--no-focus` restates it. Supports arco's focus invariant (deployment-hardening §0). |
| liveness wait | `herdr agent wait <TARGET> --until <state> --timeout <ms>` | states below. |
| — (read-only probe) | `herdr agent get <TARGET>` | returns one agent: `{"result":{"agent":{…AgentInfo…},"type":"agent_info"}}`. Same field set as a `agent list` entry. |

Other subcommands: `read`, `rename`, `focus`, `attach`, `explain`; socket API via
`herdr api snapshot` / `herdr api schema [--json]` (`api schema` DOES accept
`--json`, unlike `agent list`).

## `agent list` output (real envelope)

```json
{"id":"cli:agent:list","result":{"type":"agent_list","agents":[
  {"agent":"claude","agent_status":"idle","pane_id":"wB:p1",
   "workspace_id":"wB","terminal_id":"term_…","cwd":"…","revision":12,
   "state_change_seq":587,"tab_id":"wB:t1","terminal_title":"…",
   "terminal_title_stripped":"…","focused":false,"foreground_cwd":"…",
   "agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"…uuid…"}}
]}}
```

- **Wrapped** in `result.agents[]` — NOT a bare array.
- Agent identity fields: `agent` (kind), `agent_status`, `pane_id`,
  `workspace_id`, `terminal_id`, `cwd`, `revision`, `state_change_seq`.
- **No `boot_id` / `pid_start_time` / worker PID** — herdr exposes no PID
  identity. `terminal_id` (stable per pane) is the closest identity token; arco
  carries it in `AgentObs.BootID` for the sweep's reuse guard.
- **2026-08-10 re-check — fields the spike's example omitted** (live output +
  `success_response.$defs.AgentInfo`): `agent_session`
  (`{agent, kind, source, value}` — the agent's own session id, e.g. Claude's
  conversation UUID), `focused` (bool), `foreground_cwd`,
  `terminal_title_stripped`, plus schema-declared `display_agent`, `name`,
  `title`, `interactive_ready`, `launch_pending`, `screen_detection_skipped`,
  `state_labels` (map), `tokens` (map, ≤32 keys). arco parses only the subset it
  needs and ignores the rest — additive fields are not a breaking change, but
  `agent_session.value` is the first stable **agent-side** identity herdr has
  exposed and is worth considering if the sweep's reuse guard is ever revisited.
- **Only these are `required`:** `terminal_id`, `agent_status`, `workspace_id`,
  `tab_id`, `pane_id`, `focused`, `revision`. `agent`, `cwd`, `terminal_title`
  and friends are **nullable** — a parser must tolerate `null`/absent for the
  agent KIND and cwd (arco's `herdrAgent` uses zero-values, so this holds).

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

## Socket event stream (`events.subscribe`) — the D1/T3.6 contract

`internal/herdrsock` (T2.1, #82) subscribes to herdr's NDJSON unix socket; T3.6's
human-activity back-off rides the same stream. Pinned from
`herdr api schema --json` (protocol 17) plus a live socket probe on 2026-08-08,
re-checked 2026-08-10:

- **Subscription `type` values are DOTTED; emitted event `type` values are
  UNDERSCORED.** You subscribe to `pane.agent_status_changed` and receive
  `pane_agent_status_changed`. Both spellings are load-bearing — a client that
  reuses one string for both silently receives nothing.
- The 26 subscribable kinds (`request.$defs.Subscription`): `workspace.{created,
  updated,metadata_updated,renamed,moved,closed,focused}`,
  `worktree.{created,opened,removed}`, `tab.{created,closed,focused,renamed,
  moved}`, `pane.{created,closed,updated,focused,moved,exited,agent_detected,
  output_matched,agent_status_changed,scroll_changed}`, `layout.updated`.
- **`pane.agent_status_changed` and `pane.scroll_changed` subscriptions REQUIRE
  an existing `pane_id`.** A bare (unfiltered) entry is rejected for the whole
  `events.subscribe` call — so arco subscribes the pane-lifecycle kinds
  unfiltered and adds per-pane subscriptions as panes appear, retrying the full
  subscribe without the bare entry if it is refused. This is the single most
  breakage-prone item in the socket contract; re-verify it on any upgrade.
- **arco consumes:** `pane_agent_status_changed` (the D1 status signal) and
  `pane_focused` / `pane_scroll_changed` (T3.6 human activity). Workspace/tab
  focus events carry no pane and therefore no worker, so the daemon drops them.
- `agent.list` is the resync-on-reconnect snapshot; polling stays the
  authoritative fallback, so a socket outage degrades latency, not correctness.

## Agent manifests (the srt rollout seam)

Method names, corrected 2026-08-10: **`server.agent_manifests`** and
**`server.reload_agent_manifests`** (the strings `agent_manifest_status` /
`agent_manifest_reload` are the RESPONSE type tags, not the methods). The
response carries `AgentManifestInfo = {agent, source, source_kind,
local_override_shadowing_remote, active_version, cached_remote_version,
remote_last_checked_unix, remote_update_result, remote_update_error, warning}` —
`local_override_shadowing_remote` is what tells you a local manifest is winning
over the bundled one. See open item 4 for why this is the sandbox rollout path.

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
2. **Launch path — IMPLEMENTED and LIVE-VERIFIED** (2026-08-10 status: item (a)
   below is CLOSED — deployment-hardening §11 records the full repo-spawn
   running end-to-end on vm0 under `use_local_vm = true`, and §9 is marked DONE.
   Items (b) and (c) remain open; `use_local_vm` still ships default-off).
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
3. ~~**`send-keys` key token.** Confirm `C-c` (Ctrl-C) is accepted for the
   best-effort `Kill`.~~ **MOOT for `Kill` (2026-08-10).** `LocalVMClient.Kill`
   became `workspace close <workspace_id>` (derived from the pane target), so
   arco calls `agent send-keys` nowhere. The token question is only re-opened if
   a future path needs key injection: `AgentSendKeysParams = {target,
   keys: [string]}` in `api schema` declares **no enum** for key tokens, so the
   accepted spelling of `C-c`/`esc` is NOT verifiable from the schema and would
   need a live send against a scratch pane — do not probe it against a working
   pane.
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

## Pending 0.8.0 re-verification (opened 2026-08-10)

The rev-7 task table assumed this doc would be refreshed against **herdr 0.8.0**.
**0.8.0 is not installed on this host** (`herdr --version` → `herdr 0.7.5`), and
this refresh invented no 0.8.0 behavior: everything above is 0.7.5, protocol 17.
On upgrade, re-run the read-only probes (`herdr --version`, `herdr agent list`,
`herdr agent get <target>`, `herdr api schema --json`) and re-check exactly
these points — each one is load-bearing in arco code:

| # | Contract point | 0.7.5 state | Where it bites if it changed |
|---|---|---|---|
| 1 | **`agent list --json` rejection** | rejected — `usage: herdr agent list`; list is JSON by default | `LocalVMClient.ListAgents` deliberately omits `--json`. If 0.8.0 ADDS the flag, omitting it is still fine; if 0.8.0 makes list non-JSON without it, every sweep loses liveness. |
| 2 | **`agent list` envelope shape** | wrapped `result.agents[]` + `"type":"agent_list"`, NOT a bare array | `herdrListResp`. A bare array or a renamed key parses to zero agents → every live worker missed → false `lost`/`failed` finalization. Check `agent get`'s `result.agent` + `"type":"agent_info"` at the same time. |
| 3 | **`AgentInfo` field names + required set** | `pane_id`, `workspace_id`, `terminal_id`, `agent_status` present and required (`agent`/`cwd` nullable) | `terminal_id` carries `AgentObs.BootID` (the pane-reuse guard) and `pane_id` is the worker's `AgentRef`. Renaming either breaks correlation and the identity-strict orphan reaper. |
| 4 | **`agent_status` enum (wait states)** | exactly `idle\|working\|blocked\|done\|unknown`; only `done` is terminal | `terminalHerdrStatus`, `PromptReady`'s `--wait --until working`, and `agent wait --until`. A NEW state defaults to "alive, non-terminal" — safe but silently unhandled; a REMOVED/renamed `working` breaks initial prompt delivery confirmation. |
| 5 | **`send-keys` key tokens** | no enum in `api schema`; unverifiable read-only. Moot today — `Kill` is `workspace close` | Only if a future path needs key injection (open item 3). |
| 6 | **`agent prompt` `--` end-of-options guard** | **UNVERIFIED at 0.7.5** — cannot be probed read-only (running `agent prompt` writes to a live pane). Dashboard §2 records it as apparently fixed at herdr HEAD (0.8.0, upstream #1878). The SOCKET API is unambiguous by construction (`AgentPromptParams = {target, text, wait}` — `text` is a field, not argv), so the hazard is CLI-layer only | This is a **NAMED GATE for cross-VM** (deployment-hardening §10): a task text starting with `--` is parsed as a flag → herdr exits 0 → arco marks the prompt delivered and the task is silently lost. Re-verify on a SCRATCH pane (never a working one), then close §10's gate or switch `Prompt` to the socket API. |
| 7 | **Per-pane subscription requirement** | `pane.agent_status_changed` / `pane.scroll_changed` REQUIRE an existing `pane_id`; a bare entry fails the whole `events.subscribe` | `internal/herdrsock` carries a bare-entry retry specifically for this. If 0.8.0 allows bare entries, the retry is dead code (harmless); if the dotted/underscored split changes, the subscriber goes silent and T3.6's back-off stops firing — a silent loss of a SAFETY control. |
| 8 | **`agent start` params** | `{name, kind, pane_id, args, timeout_ms}`; `timeout_ms` > 3000 and ≤ 300000; name 1–32 chars, lowercase-initial; **no command/exec override** | `Launch` + `herdrAgentName`. The missing exec override is why the srt sandbox rollout is a manifest (open item 4) — if 0.8.0 adds one, `vm.SandboxWrap` can finally prefix the launch argv and the sandbox becomes real confinement instead of a boot gate. |
| 9 | **`workspace create --env` semantics** | env is a map in the API; whether it REPLACES or AUGMENTS the pane env is still unconfirmed (open item 2b) | P1's scrubbed-spawn-env guarantee holds fully only if it replaces. Cheap to settle on 0.8.0 with a scratch workspace. |
| 10 | **API surface version** | `protocol` 17, `schema_version` 1, 89 methods; manifest methods `server.agent_manifests` / `server.reload_agent_manifests` | A protocol bump is the cheapest single signal that any of 1–9 may have moved. |

Until that pass runs, treat this document as **0.7.5-accurate and 0.8.0-unknown**.
