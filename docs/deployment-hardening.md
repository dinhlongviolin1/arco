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

## 9. Task-S: live-herdr spike (gates the real worker-spawn path)

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

## 10. Cross-VM & scale (later)

- Cross-VM needs an SSH `VMClient` + the signed network intake (§5) between
  hosts.
- Beyond ~150 live workers or ~100 events/s sustained, move the ledger to
  Postgres + a queue (the `Store`/`Reader` ports were kept engine-agnostic for
  this).

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
- [ ] Task-S spike done before `use_local_vm` (§8/§9).
