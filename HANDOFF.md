# HANDOFF — arco

> For the next contributor/agent picking this up. Read this, then
> [`docs/implementation-plan.md`](docs/implementation-plan.md) (the authoritative build guide).

## What arco is
A self-hosted **Go daemon that supervises a fleet of coding-agent workers** and decides what
to do when they finish or block. It's driven from the CLI (Telegram + Web are optional).

- `clavis` (a sibling tool) launches workers · `herdr` (a sibling tool) herds them on each
  machine · **`arco` commands the ensemble.**
- Workers are coding agents (e.g. Claude Code) running in git worktrees. The daemon owns the
  truth about every worker in a durable **SQLite ledger** and calls a **short-lived LLM
  "brain" only when a decision is needed** — so nothing long-running is an LLM process
  (no leaks, bounded cost, crash-safe).

## Status: design complete + reviewed; **not yet built**
The architecture has been through several independent review rounds and is considered
build-ready. There is **no daemon code yet** — the repo currently holds the design/build
plan. The next step is to start building at **PASS-0**.

⚠️ Not safe to run against real repos/credentials until the **security preconditions**
(below, and in the plan) are met. See [`SECURITY.md`](SECURITY.md).

## Where to read (in order)
1. [`docs/implementation-plan.md`](docs/implementation-plan.md) — **the build guide.** Read
   the top sections in this order:
   - **REV 4** — resolved decisions, the **PASS-0 schema/contract freeze**, the reconciled
     build order (PASS-0 → PASS-1 → PASS-2 → PASS-3), and the 6 security preconditions.
   - **rev 4.1** — the final-review sign-off patches (6 must-fix-in-PASS-0 items).
   - **rev 4.2** — session hierarchy + worker ownership transfer (schema-freeze delta).
   - The **Tasks (1–27)** below carry inline `⚠ SUPERSEDED BY REV 4.x` markers where rev-4
     changed them — **always follow the rev-4 form, not the raw task text.**
2. [`docs/overview.md`](docs/overview.md) — readable overview + roadmap.
3. [`docs/design-blueprint.md`](docs/design-blueprint.md) — the design rationale (how other
   agent systems solve the same problems, and what we borrowed). *Note: it cites internal
   research/review artifacts that are not part of this repo — kept only as provenance.*

## Architecture in one paragraph
One long-lived **Go daemon** (systemd, `Restart=always`) owns a **SQLite ledger** (WAL,
single-writer) as the source of truth. `herdr`'s plugin-hook **pushes** worker
state-changes to the daemon (idempotent intake), backed by an **authoritative periodic
reconcile sweep** (so a dropped event never desyncs the fleet). When a decision is needed,
the daemon spawns a **short-lived brain call** (via `clavis`, on a cheap model by default)
that returns a **typed `StepResult`**; a worker's output is never applied directly — it's
classified → reconciled through a state machine → persisted → guarded → then drives the
next action. **Sessions** are the first-class unit (own workers, hold context, carry a
capability tree). Pure-Go SQLite (`modernc.org/sqlite`, cgo-free); `cobra` CLI.

## The resolved design decisions (don't relitigate without reason)
- **Sessions** are the unit of work + conversation; own workers (across machines); carry a
  versioned **capability tree**; are a data grouping, **never a resident process**.
- **Worker ownership is single-holder but transferable** — release = **pause** (kill the
  process, keep worktree/branch/summary), claim = recompile permissions + resume, via a
  protected `pool` sentinel. Enforced by single-writer + a `workers.rev` optimistic CAS +
  per-worker serialized queues (no races). *(rev 4.2; transfer ships in PASS-3.)*
- **Session hierarchy = single-parent tree** (`parent_session`): flat / supersession /
  nesting are one structure. **Ship flat in P2; depth-2 "supersession" next; deep nesting
  deferred.** The tree is a roll-up structure (context rolls up as capped summaries;
  authority flows down via child⊆parent trees; sibling resources arbitrated by ledger
  leases) — not a stack of LLMs.
- **Autonomy-first, shadow by default** — routine actions never gate; a stuck worker asks a
  *question* (answered two-level, daemon tries first), a dangerous action needs a *confirm*.
  Auto-answer is **shadow/draft in P2**, earned per class only after measured agreement.
- **Least privilege** — capability tree compiled onto each worker + re-checked at the
  daemon's authoritative boundary; high-blast capabilities are never handed to a worker.
- **Delegation (worker self-spawn) is OFF by default.** Hindsight/curator memory is
  **deferred past MVP** (P2 memory = manual). Brain runs on a **cheap model** by default.

## Build order (supersedes the raw 1–27 task numbering)
- **PASS-0 — freeze `schema.sql` + shared contracts.** Everything the REV 4 / rev 4.1 /
  rev 4.2 sections mark as "freeze now": immutable event log (two clocks, dedup),
  `brain_transcript_rows` + FTS5, `capability_catalog` + `session_grants`, escalations
  columns, `provider_pools` + leases, `budgets` + `breakers`, `vms` + `vm_observations`,
  `schema_migrations`, `usage` columns, `workers` (`rev` CAS + task/lineage/perm columns),
  `sessions` (`parent_session`, `kind CHECK('work','pool')` + a seeded pool row, `rev`),
  `budgets.scope += 'subtree'`, and the transfer/move event kinds. Plus the shared types:
  `Deps`, `VMClient`, `Store{Migrate, WithTx}`, `CapabilityOf`, and the split
  `AnswerQuestion`/`DecideConfirm` API. **Absorb the 6 rev-4.1 must-fixes here** (they're
  contract-shaped and expensive to change after rows exist).
- **PASS-1 — spine:** module/config → Store + migrations → ledger (workers+CAS, immutable
  events, sessions, escalations skeleton, capability catalog + `Allowed()`) → HTTP API over
  a unix socket + `/healthz` → CLI client.
- **PASS-2 — single-VM walking skeleton:** a local `VMClient` (liveness by workspace +
  boot-id + remote HEAD, **not PID**) → log normalizer → state fusion → spawn (create-before-
  side-effect, env-scrubbed) → executor (per-worker CAS, brain off the write path) →
  reconciler (intent-first + error/billing ladder) → reconcile sweep → brain (context
  assembly + `StepResult`) → escalations (**structural danger-class; autonomy in SHADOW**) →
  diff-gated completion → boot recovery (survive-and-reconcile). End-to-end test:
  `dispatch → hook → complete`, headless (no Telegram/Web).
- **PASS-3 — hardening:** budgets + breaker + `freeze`, provider-pool leases, per-VM
  admission, secret redaction, env-scrub + git-hardening + server-side branch protection,
  manual memory (24a), worker ownership transfer (pause/recompile/resume), depth-2
  supersession.
- **Later (own sub-plans):** Telegram add-on, Web add-on, hindsight/curator memory,
  cross-VM (SSH `VMClient` + provider pools) + Postgres/queue, deep session nesting.

## Security preconditions (before pointing arco at real repos/creds)
1. OS-user separation + a scrubbed worker spawn environment (strip provider/host tokens).
2. Hardened git + quarantine repo-shipped `.claude`/`.mcp.json` (host-escalation vector).
3. Managed-settings deny layer + no high-blast credentials on worker boxes + **server-side
   git branch protection** (the only non-advisory layer).
4. Auth on Telegram (sender allowlist), Web, and cross-machine event intake (source-bound).
5. Secret redaction at every ledger / memory / transcript egress.
6. Pinned spawn permission mode (interactive-in-pane, not headless `-p`) + structural
   action-class + network-fetch/spawn default-off.

## How the design was validated
Multiple independent review rounds across different model families (opus, fable, and a
non-Anthropic reviewer), including an 8-aspect review and a final sign-off (**GO with
fixes**, 26/27 blockers resolved) plus a dedicated review of the session-ownership/hierarchy
additions (**adopt-with-fixes**). All fixes are folded into the rev-4.x sections of the plan.

## How to contribute
- **Language/stack:** Go 1.22+, pure-Go SQLite (`modernc.org/sqlite`), `cobra`, stdlib
  `net/http`. Test-driven, one logical change per PR (see [`CONTRIBUTING.md`](CONTRIBUTING.md)).
- **`main` is protected** — branch → PR → merge (self-merge allowed; no direct pushes / force-pushes).
- Install hooks: `pip install pre-commit && pre-commit install` (runs **gitleaks** + hygiene).
  Never commit secrets or `.env` files (see [`.gitignore`](.gitignore), [`.gitleaks.toml`](.gitleaks.toml)).
- **Start at PASS-0.** Freeze the schema first; it's the cheapest place to get the contract right.

## Open items / decisions to confirm with the maintainer
- Whether to publish the internal review artifacts (currently out of the repo).
- P2 session-teardown default is **pause/kill** (release-to-pool arrives with transfer in PASS-3).
- Exact provider/model routing for the brain vs. workers (kept out of this repo).
