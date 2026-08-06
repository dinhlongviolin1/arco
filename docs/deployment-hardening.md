# Deployment & hardening runbook

The **operator's** guide to running `arco` safely against real repos and
credentials. arco enforces and *verifies* its half of the PASS-3 security
preconditions in code; the steps below are the half that lives outside the
process — OS accounts, credential placement, server-side settings — that arco
cannot perform for you. Each section names the code control that assumes it, so
you can see what backs the requirement.

> **Do not point arco at real repos/creds until every CRITICAL item here is
> done.** `arco` runs a boot **preflight** (`internal/preflight`) that refuses to
> start on the checks it *can* verify locally; the rest are on you.

---

## 0. What arco already enforces (no action needed)

These are implemented and tested — listed so you don't re-do them:

| Control | Where | Guarantee |
|---|---|---|
| Boot preflight | `internal/preflight`, wired in `daemon.Run` | Refuses to start as root, without `git`, or with a network intake and no signing secret. |
| Scrubbed subprocess env | `internal/spawnenv`, `vm.LocalVMClient.newCmd` | Strips provider/git/cloud/DB/`ARCO_*` secrets from subprocesses arco launches on the worker/agent path (via `vm.newCmd`). (`quarantine`'s `git config`/`rev-parse` calls run arco-owned on a fresh gitdir and are not scrubbed — no worker exposure.) |
| Write-time secret redaction | `internal/redact`, ledger write chokepoint | Secrets in event payloads / worker task / session goal are redacted at rest. |
| Repo-config quarantine | `internal/quarantine` | Renames aside repo-shipped `.claude`/`.mcp.json`/`.gitattributes`/`.gitmodules`/`.lfsconfig`, disables repo hooks + fsmonitor. |
| Compiled worker permissions | `internal/permcompile` | `settings.json` + PreToolUse hook staged **outside** the worktree; high-blast caps never granted. |
| Managed deny layer (content) | `permcompile.ManagedSettings` | Deny-only policy artifact for the managed path (see §3). |
| Signed intake | `api` + `ARCO_INTAKE_SECRET` | HMAC-SHA256 on `POST /v1/events`; fails closed if TCP is set without a secret. |

---

## 1. OS-user separation (precondition P1) — CRITICAL

Run arco as a **dedicated non-root user** that is **not** the user your workers
run as, and that has no ambient high-blast credentials.

```
# dedicated service account, no login, no sudo
sudo useradd --system --home /var/lib/arco --shell /usr/sbin/nologin arco
```

- The systemd unit must set `User=arco` (never root). arco's preflight
  `not_root` check refuses euid 0, but it cannot verify you didn't also put it in
  group 0 or grant it capabilities — don't.
- Workers should run as a **separate** unprivileged user so a worker escape
  cannot read arco's state dir or ledger.
- **Backing code:** `preflight.not_root`; `spawnenv.Scrub` keeps arco's own env
  out of children, but only OS-user separation stops a worker from reading files
  arco can read.

## 2. State-dir & socket permissions — treat as REQUIRED

Keep the arco state dir (holds the ledger + compiled worker configs) and the
unix-socket dir at `0700`, owned by the `arco` user:

```
sudo install -d -o arco -g arco -m 700 /var/lib/arco
```

- The **socket dir's `0700` is the ONLY thing authenticating the local mutating
  routes** (`dispatch`/`verify`/`escalations{answer,confirm}` are unauthenticated
  over the socket — see §5). Any local user who can reach the socket can drive
  arco, so treat this as required, not optional.
- arco creates these `0700` when it owns them, but will not chmod a dir you
  point it at that it may not own. Preflight surfaces `state_dir_private` /
  `socket_dir_private` as **warnings** in the log (not a hard stop) if they're
  group/world-readable — do not ignore them.

## 3. Managed-settings deny layer (precondition P3) — CRITICAL for untrusted repos

arco writes a **deny-only** `managed-settings.json` next to each worker's
compiled config (`permcompile.Compile`). Deploy it to Claude Code's **managed
settings path** (root-owned) so its denies cannot be overridden by a repo
`settings.local.json` or `--dangerously-skip-permissions`.

- Copy/symlink arco's generated `managed-settings.json` to the OS managed path
  documented by your Claude Code version, owned by root and read-only to the
  worker user.
- **Verify** afterward: as the worker user, a repo that ships
  `settings.local.json` allowing a high-blast tool must still be denied.
- **Backing code:** `permcompile.ManagedSettings` (deny-only; covers high-blast
  tool shapes + static dangerous shapes).

## 4. No high-blast creds on worker boxes + server-side branch protection (P3) — CRITICAL

The compiled worker config and the PreToolUse hook are **advisory** (a worker
runs live, in-process). The only **non-advisory** control for a worker-executed
push is server-side:

- **Branch protection** on every repo arco touches: require PRs, block direct
  pushes to `main`/protected branches, no force-push, apply to admins. Configure
  it in GitHub/GitLab — arco cannot set it.
- **Per-worker fine-grained tokens** (B8): give each worker a token scoped to its
  repo(s) with the minimum permissions; never a broad PAT. Do not place
  deploy/cloud/admin credentials on worker machines at all.
- **Backing code:** `permcompile` denies `git push … main` shapes best-effort;
  branch protection is what actually holds.

## 5. Signed / network intake (precondition P4)

The event intake (`POST /v1/events`) is unauthenticated over the **local unix
socket** (trusted via the `0700` dir). For **any network / cross-VM** intake:

- Set `ARCO_INTAKE_SECRET` (≥16 bytes; keep it out of the repo and out of
  worker env). arco refuses to start with `tcp_addr` set but no secret.
- herdr / any cross-VM poster must send `X-Arco-Signature: sha256=<hmac>` over
  the exact request body.
- **Not yet wired:** binding a TCP listener. When you add one, do **not** expose
  the raw mux — `dispatch`/`verify`/`escalations{answer,confirm}` are
  unauthenticated (socket-trust). Serve only `/v1/events` (+ read-only GETs) over
  TCP, or gate every mutating route behind the HMAC (see the scope warning in
  `daemon.Run`).

## 6. Telegram / Web auth (precondition P4, add-ons — off by default)

Both are opt-in and ship off. Before enabling either:

- **Telegram:** a sender allowlist (only approved chat/user IDs act); the bot
  token stays out of the repo and worker env.
- **Web:** authenticated + source-bound; never bind it unauthenticated on a
  routable interface.

## 7. Secret redaction egress (precondition P5) — enforced, verify your patterns

Redaction is on at the ledger, the brain prompt, and worker task/session goal.
Review `internal/redact` patterns against the credential shapes your environment
actually uses (it's deterministic/best-effort — see the residual-gap notes) and
add any provider-specific token shapes you rely on.

## 8. Pinned spawn contract (precondition P6)

`permcompile.LaunchArgs` produces the pinned launch flags (settings outside the
worktree, non-bypass permission mode, allow/deny tool lists). **Wiring these into
the actual worker launch is gated on the herdr launch contract** — see §9.

---

## 9. Task-S: live-herdr spike (gates the real worker-spawn path) — ✅ DONE

**Completed + the real spawn path is live-verified end-to-end (§11).** herdr 0.7.5's
contract is confirmed (`docs/herdr-contract.md`), `vm.LocalVMClient.Launch` is
implemented, and `use_local_vm=true` runs the real repo-spawn on vm0. The original
spike notes are kept below for provenance; §11 is the reproducible procedure.

`vm.LocalVMClient` maps herdr's `agent list --json` / `agent prompt` /
`send-keys` against herdr's *documented* contract; the exact JSON fields and the
**launch** invocation are unconfirmed against a live herdr. Until this spike is
done, arco defaults to the **Fake** VMClient and the real spawn path (which would
apply `LaunchArgs` + the scrubbed env + a provider lease at launch) is not wired.

Spike checklist:

1. On a host with a real herdr, confirm the JSON schema of `agent list --json`
   (field names/types for workspace, liveness/status, boot id, pid-start).
2. Confirm the invocation that **launches** a worker agent (so arco can pass
   `permcompile.LaunchArgs` + a `spawnenv.Scrub`'d env at start, not just prompt
   an existing pane).
3. Adjust `vm.LocalVMClient` to the confirmed contract; add an
   `agent launch`-style method to `core.VMClient` and wire
   `permcompile.LaunchArgs` + `spawnenv.Scrub` + provider-lease acquisition into
   `reconcile.Dispatch`/`Delegate` at the real launch.
4. Set `use_local_vm = true` / `ARCO_LOCAL_VM=1` only after the above passes.

## 10. Cross-VM & scale (later — foundation landed)

- **Landed (PR #69): the injection-safe SSH command layer.** `vm.NewRemote(host,
  herdr)` runs the SAME herdr/git chain on a remote host over ssh: `LocalVMClient`
  is now transport-agnostic (a pluggable `runner` — local by default, ssh when
  remote). Every remote argument is POSIX-shell-quoted (`shellQuote`: single-quote
  wrap + `'\''` escape), because `ssh host cmd` runs the command through the remote
  LOGIN SHELL — an unquoted arg is a remote-code-injection hole, and worker
  tasks/labels are untrusted input. The host itself is validated (leading `-`
  rejected — ssh option injection, CVE-2023-51385 class) and a `--` end-of-options
  separator is emitted. Verified end-to-end: a 25-token hostile corpus (command
  substitution, backticks, quotes, newlines, globs, redirection, control chars)
  round-trips byte-exact through a REAL shell back into the intended argv; the ssh
  client's own env is scrubbed (P1). BatchMode pins it non-interactive. **Scope of
  the guarantee:** the SHELL layer is injection-safe. A separate, pre-existing layer
  (identical on local) is the herdr CLI's own flag parsing of positional values
  (e.g. a task text beginning with `--`); that needs a herdr-contract `--`
  end-of-options guard (noted on `Prompt`), not quoting. That misparse is a LIVE
  silent-task-loss bug even locally (task `--help` → herdr prints help, exits 0,
  prompt marked delivered), so the herdr `agent prompt --` spike is a NAMED GATE
  for enabling cross-VM — don't let this scoping quietly downgrade it. Also noted: NUL bytes are
  rejected by exec before any shell, and Linux's 128 KB argv-element cap bounds
  very large quote-dense payloads (per-task failure, not injection).
- **Cross-host VALIDATED against a real second host (vm1).** Env-gated integration
  tests (`ssh_integration_test.go`, run with `ARCO_TEST_SSH_HOST=<host>` — no
  hostnames hardcoded) exercise the REAL transport over the network: the
  herdr-chain `ListAgents` against a remote fake herdr; a 10-payload hostile
  injection corpus (`$(rm -rf /)`, backticks, newlines, `-oProxyCommand=...`)
  delivered byte-exact to the remote through the real login shell; and real remote
  git (`GitHeads` reading a repo created on the remote). All pass; remote
  scaffolding self-cleans.
- **Still needed before live cross-VM:** the spawn path's worktree provisioning
  runs on THIS host (needs a remote variant), a VM-routing/admission policy for
  multiple hosts, the signed cross-host intake (§5) from remote workers' hooks, and
  herdr installed + configured on the remote host. Until then `NewRemote` is a
  cross-host-validated building block, deliberately not wired into the daemon.
- Beyond ~150 live workers or ~100 events/s sustained, move the ledger to
  Postgres + a queue (the `Store`/`Reader` ports were kept engine-agnostic for
  this).

## 11. Live deployment (VERIFIED end-to-end on vm0, herdr 0.7.5)

The full repo-spawn is live-verified: `dispatch(repo)` → clone-per-worker →
quarantine → permcompile → herdr launch → **scoped pool credentials** →
authenticated agent → **autonomous task completion** in the isolated clone. The
reproducible procedure:

**Prereqs on the host:** `herdr` (0.7.x) running, `clavis` with at least one
profile (`clavis list`), `git`. arco talks to the local herdr over its socket.

**1. Config** (`~/.arco/config.toml`, outside the repo — never commit it):
```toml
use_local_vm = true      # real herdr LocalVMClient (else Fake)
herdr_bin    = "herdr"
default_pool = "p1"      # every repo-spawn leases from this pool
lease_ttl    = "1h"
```
`chmod 700 ~/.arco` (preflight requires the state/socket dir be `0700`).

**2. Start the daemon:** `arco daemon --config ~/.arco/config.toml`
(preflight fails fast if `herdr` is missing or `default_pool` doesn't exist yet —
so create the pool first, or seed it, then start).

**3. Create the credential pool** — a worker leased from it launches Claude Code
with that clavis profile's scoped creds (NOT arco's own — P1):
```
arco pool create p1 --profile deepseek-1 --max-active 10
```

**4. Dispatch a repo task** (the autonomous path):
```
arco dispatch "…the task…" --repo https://github.com/you/repo.git --new
```
→ worker `running`; it clones, authenticates as the pool's profile, and completes
the task in `~/.arco/workers/<id>/worktree` with no human in the loop.

**Worker auth model (how §1's P1 is honored while the worker still authenticates):**
`spawnenv.Scrub` strips ALL provider creds from the launch env (so a worker never
inherits arco's key); Spawn then injects the *pool's* clavis-profile env
(`clavis env <profile> --json --reveal-key`) — a **scoped** credential, delivered
via herdr `workspace create --env` which **replaces** the pane env. Caveat: `--env`
puts the token in the herdr `create` argv (visible in `ps` briefly) — acceptable
for a scoped, low-privilege pool key on a single-user host; use a dedicated,
tightly-scoped clavis profile per pool.

**Live-caught gotchas (already handled in code, noted for operators):**
- herdr `agent start <name>` needs a lowercase name (arco lowercases the ULID).
- `agent prompt`/`send-keys` target a **pane_id**, not the workspace label
  (arco correlates + prompts by the captured pane_id).
- herdr returns some errors as `{"error":…}` with **exit 0** (arco treats these
  as failures) and its only env mechanism is `--env` (argv).
- The initial task prompt races the agent's TUI boot → arco delivers it async with
  a `--wait`-confirmed retry.

**Known follow-ups (non-blocking):** narrow initial-prompt double-submit edge; a
delivery holding an Exec slot on the rare never-boots path; the legacy prompt-path
`dispatch` (no `--repo`) doesn't fit real herdr (use `--repo`); no `pool
delete/update` CLI.

## 12. Known limitations & prioritized follow-ups (whole-system capstone audit)

Two integrated-system audits (opus) confirmed the crash-safety, concurrency, and
secret-at-rest core sound. Round one fixed five findings (PRs #55–#58: worker
terminalization can't wedge or resurrect a corpse; worker Read is worktree-gated;
the local hook signs intake under P4; EscalationTimeout auto-pauses stuck waiting
workers). A second whole-system audit (post-#62) fixed four more:
- **HIGH — scoped provider creds leaked into the ledger/LLM via herdr `--env`
  argv rendered into launch error strings** → redacted at the source (PR #63).
- **MED — a human answer could drive a POOL-OWNED worker to running**, bypassing
  the pool-inert invariant every brain path enforces → `decide()` now guards it.
- **MED — the escalation resume event was recorded UNATTRIBUTED** (NULL worker_id,
  invisible to the brain/audit) → `decide()` stamps it to the worker.
- **LOW — a `completed_candidate` worker's miss counter grew unboundedly** (no
  legal edge to lost, so finalize no-ops) → the counter is reset each sweep.

The rest are **explicitly scoped-out known limitations** — none block the core
spawn→autonomous-completion loop, but an operator should know them:

- ~~Human-answer delivery is not wired to the agent (MED).~~ **DONE (PR #65).**
  The API answer/confirm routes now go through `Engine.AnswerQuestion`/
  `DecideConfirm`, which — when the decision RESUMES the worker (→ running, never a
  pool worker, MED-4) — deliver the answer text (or an approval signal) to the
  worker's pane via `PromptReady`, async through the per-worker Exec, best-effort
  (a delivery failure is an error event, not a decision failure). A rejected
  confirm blocks the worker and delivers nothing.
- **A crash between `dispatch_done` and initial-task delivery strands a taskless
  `running` worker (MED, audit round 2).** Spawn commits `running` then delivers
  the first prompt async via Exec; a crash in that window leaves a live, idle,
  lease-holding worker that boot `Recover` (which only re-drives `starting`) never
  re-prompts. **Deliberately NOT auto-recovered** — an attempt (a `prompt_delivered`
  marker + a sweep re-delivery of the dangling `prompt_intent`) was built and
  **rejected in review as unsafe**: a Claude agent that already FINISHED the task
  stays alive at its input prompt reporting herdr status `idle` (the process does
  not exit → it never becomes `done`), which is **indistinguishable** from a
  never-delivered idle agent. Re-typing `w.Task` in that state re-executes a
  completed, possibly side-effectful task (open a PR, push, delete) — the same
  double-dispatch class that got the naive brain re-drive rejected. A safe
  auto-recovery needs a positive "this agent never received input" signal (agent
  working-history), which the current herdr signals (`alive` + git HEAD) don't
  provide — HEAD can be unchanged for a finished no-commit task. **Operator remedy
  today:** the stranded worker is visible as `running`+idle with HEAD==base; `arco
  kill <id>` it and re-dispatch. Revisiting this needs richer agent-status history
  (track first-observed-working), a larger design task.

- **Agent actuation is PARTIAL (MED-3).** Two pieces have landed: `arco kill
  <worker>` terminates a worker and stops its agent (`VM.Kill` = `herdr workspace
  close`, derived from the pane_id; PR #60), and the **sweep now reaps orphaned
  agents of TERMINAL workers** (PR #62) — a crash between `arco kill`'s commit and
  its best-effort agent-stop, or any lingering `lost`/`failed` pane, is cleaned up
  on the next sweep. The reaper is *identity-strict*: it closes a pane only on a
  positive `terminal_id` match (never on a recycled/ambiguous ref). Identity is
  established **at launch** (captured from herdr right after `agent start`,
  persisted by `BindLaunch`) and thereafter only *confirmed* by liveness — never
  *established* by it — so a stranger occupying a recycled pane can never be
  recorded as a worker's identity and then wrongly closed (dual-review closed both
  the inert-guard and first-observe-poisoning paths). A worker whose launch-capture
  didn't resolve an id stays unidentifiable and is simply left for manual cleanup —
  a rare, non-destructive miss chosen over any risk of closing an innocent
  workspace. **Auto-kill-on-pause LANDED (PR #67):** the identity-strict reaper now
  reclaims a **paused** worker's idle agent too (a worker paused by
  `escalation_timeout`/`pool_ttl` has only an idle agent burning quota; its
  worktree/work-product is preserved), and the liveness loop excludes those
  workers so an auto-killed agent is not mistaken for a death and finalized to
  `lost` (the coupling those two changes had to land together). **Exception:** a
  paused worker with a PENDING escalation keeps its agent — an operator approval
  re-prompts the same pane (a reconnect, e.g. an `AuditDeniedAttempt` danger
  confirm), so reaping it would silently discard the approval; those stay
  liveness-tracked. This closed the
  earlier worry that killing a paused agent would "strand" the worker — there is no
  resume-by-RECONNECT to lose; resume is via relaunch. **Resume via relaunch is intentionally NOT built** —
  analysis showed it has no clean use case given the three (and only three) ways a
  worker becomes `paused`: (1) `AuditDeniedAttempt` danger-confirm → resume is the
  operator **approval**, which re-prompts the live pane and **works** (its agent is
  kept, above); (2) `reapEscalations` timeout → the escalation is expired before
  pausing, so a relaunch would re-deliver the original task and the agent would
  re-ask the same question and re-time-out — relaunch doesn't help; (3) `reapPooled`
  pool-TTL → spare pool capacity, whose operation is claim+dispatch, not
  resume-of-original. So the real "resume" (approve a danger-paused worker) is done;
  a bespoke relaunch path would add untested launch/delivery surface for a scenario
  that doesn't benefit. Operator remedy for a timed-out/pooled paused worker:
  inspect its preserved worktree, `arco kill` + re-dispatch. (`ClaimWorker` exists
  in the ledger but is intentionally unrouted for the same reason — a claimed pool
  worker would need a relaunch to be useful.) A second (pre-existing,
  minor) follow-up: the *operator* `arco kill` path `VM.Kill`s `w.AgentRef` with no
  identity gate, so killing a worker that had mis-adopted a recycled pane could close
  the wrong workspace — only the *unattended* reaper is identity-strict; the operator
  path trusts explicit intent. **Operationally: `arco kill <id>` reclaims a worker's
  agent, and terminal + paused orphans self-clean on the next sweep.**
- **Scoped creds pass via `herdr --env` argv (MED-5).** herdr's only env
  mechanism is `workspace create --env KEY=VALUE`, so a pool's clavis token is
  briefly visible in `/proc/<herdr-pid>/cmdline` during the create call. On a
  single-user host this adds no attacker (same user already owns the token); on a
  shared host, use `hidepid` and a tightly-scoped per-pool profile. A leak-free
  path needs a herdr env-file/stdin mechanism (herdr change).
- **Inert config knobs.** `crash_loop_restarts`/`crash_loop_window` (a
  crash-loop breaker), a global `max_spawns` cap, and `stall_n` stall-detection
  are defined in config but **not yet enforced** (only per-VM/per-session caps +
  the brain-billing park limit loops today). `escalation_timeout` IS now enforced
  (§11 / PR #58).
- **Minor:** the initial task-delivery prompt has a narrow double-submit edge if
  the agent finishes a turn within the readiness poll gap; a worktree Read that
  resolves outside via a symlink (e.g. `node_modules`→shared store) is denied
  (allowlist it if it bites); a crash in the ~ms between `agent start` and the
  ref-commit leaks that one agent (documented in `herdr-contract.md`).

---

## Pre-flight checklist (before real repos/creds)

- [ ] arco runs as a dedicated **non-root** user; workers run as a **separate**
      unprivileged user (§1).
- [ ] State dir + socket dir are `0700`, arco-owned (§2).
- [ ] Managed deny layer deployed to the root-owned managed path and **verified**
      non-overridable (§3).
- [ ] Server-side **branch protection** on all target repos; per-worker
      fine-grained tokens; no deploy/admin creds on worker boxes (§4).
- [ ] `ARCO_INTAKE_SECRET` set (≥16 bytes) **iff** any network intake; TCP mux
      split not exposed raw (§5).
- [ ] Telegram/Web left off, or enabled with auth (§6).
- [ ] Redaction patterns reviewed for your credential shapes (§7).
- [x] Task-S spike done (§9); real spawn path live-verified — follow §11 to deploy
      `use_local_vm` with a scoped clavis-profile pool.
