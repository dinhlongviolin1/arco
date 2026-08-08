# arco rev-7 — Implementation Plan

High-level plan: `docs/plan-rev7.md` (decisions D1–D9). This document turns it into phases → milestones → tasks with test-first acceptance gates.

Execution model: each task is implemented by a **clavis `qwen-1` sub-agent** in an isolated **git worktree**, guided by **pre-written tests** (test = the conditional evaluation of done). The agent pushes a branch + opens a PR; the orchestrator validates (build, `go vet`, `go test -race ./...`, diff review), fixes directly on the branch when the fix is known, then squash-merges. Main must stay green at every merge.

Conventions:
- Branch: `task/<id>-<slug>` · Worktree: `.wt/<id>` (git worktrees live under the repo in `.wt/`, gitignored)
- Every task ships tests. Pre-written guideline tests live in the task's test files before the agent starts; the agent makes them pass and may add more, never weaken them.
- New schema = new migration file (`internal/ledger/migrations/000N_*.sql`); the frozen `0001_init.sql` is never edited.
- Every ledger-visible action carries an actor (`operator | brain | reconcile`) so D9's "who answered" guarantee holds.

---

## M1 — Trust & Floor (phase H1)
**Milestone: arco pings your phone with decision cards, yields when you're driving, and has no dead knobs.**

| ID | Task | Key seams | Acceptance (tests) |
|---|---|---|---|
| T1.1 | Notifier: shoutrrr → ntfy decision cards. New `internal/notify` (interface + shoutrrr adapter + no-op). Emit on: escalation created (card: worker, task tail, question, draft, rationale), escalation answered/expired, worker completed_verified / failed / lost. Config `[notify] urls=[]`, `min_level`. | new pkg; hooks in reconcile + escalation write paths | unit: card rendering (golden), level filter, no-op when unconfigured; fake Sender records calls on each lifecycle transition; race-safe |
| T1.2 | `arco status` CLI: one-screen fleet table (sessions, workers by state, pending escalations w/ age, pool leases, daemon health) via existing HTTP API. `--json`. | internal/cli, internal/api (new GET /status) | API handler unit test (seeded ledger → DTO); CLI golden-table test; zero-state test |
| T1.3 | Enforce `stall_n` (consecutive no-progress sweeps → worker `blocked` + escalation) and delete dead knobs `crash_loop_restarts`, `max_spawns` (or wire — decision: delete). Write `workers.stall_count`. | internal/reconcile/sweep.go, internal/config | sweep-table tests: N-1 no-op, N triggers once, progress resets counter; config parse rejects removed knobs |
| T1.4 | Escalation DTO carries `brain_rationale` + `draft_confidence`; `arco escalations` prints them. | internal/api/server.go DTOs, internal/cli | DTO round-trip test incl. nil-draft case; CLI golden |
| T1.5 | D9 supervision modes: migration `sessions.supervision_mode` (`auto\|assist\|manual`, default `assist`); `arco mode <session> <mode>`; reconcile/brain gate: `manual` = observe+ledger only (no spawn/kill/redeliver/brain drafts/notify), `assist` = notify+draft, never auto-act; every event/intent row records actor. | migration 0005, internal/core, reconcile guards, cli | mode-matrix table test (action × mode → allowed?); actor recorded on answer path; migration up test |
| T1.6 | SO_PEERCRED intake binding: UDS intake resolves peer UID; worker events must arrive from the worker's UID (map recorded at spawn); mismatch → 403 + audit event. Keep HMAC. | internal/api (listener), internal/vm spawn records uid | integration test over real UDS: same-uid pass; forged-worker-id fail (simulate via recorded-uid mismatch); non-cred transport unchanged |
| T1.7 | Debt: the 3 missing §E tests + kill-9 crash matrix (SIGKILL daemon at: pre-intent, post-intent/pre-execute, post-execute/pre-result → restart → reconcile converges, no duplicate side effects). | test/ | the matrix itself |

## M2 — Signals & Containment (phase H2)

| ID | Task | Key seams | Acceptance |
|---|---|---|---|
| T2.1 | herdr `events.subscribe` client: long-lived NDJSON Unix-socket subscriber (`pane.agent_status_changed`, `pane.focused`, `workspace.focused`, `tab.focused`, `pane.scroll_changed`) feeding fusion as signals; `agent.list` resync on reconnect; polling stays as fallback. | new internal/herdrsock; internal/fusion | fake socket server: event → fusion signal; reconnect + resync; malformed frames ignored; subscription filter correctness |
| T2.2 | Sandbox wrapper: optional `srt` prefix at the launch seam (config `[sandbox] enabled, policy_path`), off by default; preflight check that srt exists when enabled. | internal/vm/local.go launch argv; internal/preflight | argv-construction tests (enabled/disabled/policy); preflight failure test |
| T2.3 | Secrets: read arco's own secrets from `$CREDENTIALS_DIRECTORY` (systemd-creds) with env fallback; worker cred handoff via 0600 file under private dir instead of `--env` argv (kills MED-5); packaging: unit file `LoadCredential=` lines. | internal/config, internal/spawnenv, internal/vm | cred-dir precedence tests; spawn argv contains no secret material (regression grep test); file perms 0600 asserted |
| T2.4 | Brain context fix: assemble Facts + ContextSummary + memory tier-1/2 into brain prompt (budgeted); raise event tail caps to budget-based. | internal/reconcile/brain_apply.go | prompt-assembly tests: facts/summary present, budget respected, starving inputs degrade gracefully; golden prompt |
| T2.5 | `/metrics` (promhttp): workers by state, escalations pending/age, brain calls/tokens, sweep duration, notify failures. | internal/api | scrape test asserts metric names/values from seeded state |

## M3 — Proof & Fleet (phase H3)

| ID | Task | Key seams | Acceptance |
|---|---|---|---|
| T3.1 | Verification leg 1 — CI check-runs: on `completed_candidate`, poll the branch's GitHub check-runs (gh api); success → `verification_artifact` event with check summary; failure → escalation. Config-gated. | internal/reconcile/verify.go | fake gh transport: success/failure/pending paths; artifact event idempotent |
| T3.2 | Verification leg 2 — in-daemon merge queue: FIFO per repo, serialized rebase → test cmd → ff-merge; conflict/red kicks back to worker (event + escalation); emits `verification_artifact`. `arco queue` CLI. | new internal/mergeq | real-git-fixture tests: clean merge; conflict kickback; red-test kickback; FIFO ordering; crash-restart resumes queue safely |
| T3.3 | Cross-VM wiring: route spawn/kill/list through the validated SSH command layer per `vms` table; per-VM herdr socket path; preflight per VM. | internal/vm (ssh client exists), internal/reconcile | contract tests against fake ssh runner; live CROSS-HOST smoke doc §10 update |
| T3.4 | HKDF per-worker intake keys; remove workspace-name fallback (server.go:508-528). | internal/api | derive/verify tests; old fallback path returns 403 (regression); key rotation test |
| T3.5 | Earn-out: per `question_class` agreement tracking (draft vs final answer); `arco autonomy` report; promotion to auto-answer only when class ≥ threshold AND T3.1/T3.2 live AND session mode `auto`. | internal/reconcile, migration | agreement bookkeeping tests; promotion gate matrix (below threshold / no verification / wrong mode → never auto) |
| T3.6 | D9 completion: human-activity back-off — focus/scroll/Done→Idle events (from T2.1) arco didn't cause start an activity timer that demotes `auto`→`assist` for the session; never call `pane.focus` in auto. | internal/fusion, reconcile | self-caused-event exclusion test; timer demote/restore tests |
| T3.7 | herdr plugin: thin manifest+script exposing `arco status --json` in herdr UI. | new plugin/ dir | script smoke test |

T3.6 shipped as `internal/reconcile/activity.go`: the daemon feeds every herdr
pane activity event (focus/scroll, T2.1) to `Engine.ApplyHumanActivity`, which
resolves the worker by `AgentRef` and drops its session `auto`→`assist` with
actor `activity-backoff` (assist/manual are operator statements — never
touched; an unknown pane is a silent no-op). Every arco path that touches a pane
calls `NoteSelfPaneOp` first, so the echo herdr pushes for arco's own prompt is
excluded for `self_op_window` (5s). `Sweep` restores `auto` after
`activity_restore_after` (20m) of quiet, and ONLY for sessions the back-off
itself demoted — an operator's assist stands. That demotion set is in-memory: a
daemon restart leaves those sessions in `assist`, which fails toward less
autonomy. **Invariant:** arco never calls a pane-focus op (in `auto` least of
all), so a focus event always means a human is present.

T3.2 shipped as `internal/mergeq`: config gate `merge_queue` (default off, with
optional `merge_queue_test_cmd`) has the sweep ticker drain a ledger-backed FIFO
queue, strictly one item per `ProcessNext`. Items are EVENT-SOURCED
(`mergeq_enqueued`/`mergeq_merged`/`mergeq_kicked` events reconstructed via
`EventsSince` — no new migration), so a restarted daemon resumes exactly where
it left off. Integration happens in a scratch clone of the worktree's `origin`
(the worker's own worktree is never touched): clone → merge the head fetched
from the worktree → optional test gate → push main. A landed merge appends ONE
`verification_artifact` (deduped on `mergeq:<worker>:<head>`, like T3.1);
conflict/red-gate/denied-push kick the item back with one pending `confirm`
escalation carrying the git error — a non-bare target refusing its checked-out
branch is a kickback, never a crash (see deployment-hardening §11). Merging is
evidence only: the worker stays `completed_candidate`. CLI: `arco queue
<worker>` / `arco queue list` over `POST|GET /v1/queue` (503 when disabled).

T3.1 shipped as `internal/reconcile/civerify.go` (not verify.go — the human diff-gate stays untouched): config gate `ci_check_runs` (default off) has the sweep poll `gh api .../check-runs` inside the candidate's worktree; green → one ledger-deduped `verification_artifact` per (worker, head SHA), idempotent across restarts; red → one pending `confirm` escalation; pending/zero-runs/gh-error → retry next sweep. CI success is evidence only — the worker stays `completed_candidate`.

## M4 — Close-out

| ID | Task |
|---|---|
| T4.1 | Docs: deployment-hardening §13 (rev-7 controls), herdr-contract refresh (0.8.0), dashboard §2 percentages re-scored |
| T4.2 | Full matrix: `go test -race ./...`, kill-9 matrix, live single-VM smoke (§11 rerun), MED-5 closed verification |
| T4.3 | Product review package for the operator (what shipped, what moved, open questions) — then joint review |

Dependency edges: T3.5 ⇐ (T3.1 ∨ T3.2); T3.6 ⇐ T2.1; everything else parallelizable within its milestone. Merge order within a milestone: smallest-risk first.

---

## Factory workflow (per task)

1. Orchestrator: `git worktree add .wt/<id> -b task/<id>-<slug> origin/main`
2. Orchestrator writes guideline tests into the worktree (failing or skeleton) + `TASK.md` brief.
3. `clavis qwen-1` headless in the worktree: implement to green (`go build`, `go vet`, `go test -race ./...`), commit, push, `gh pr create`.
4. Orchestrator validates: fresh `go test -race ./...` in worktree, diff review against brief, no test weakening, no unrelated files. Known fixes applied directly to the branch (no re-ask). Unknown/architectural problems → re-brief agent once, else orchestrator implements.
5. Squash-merge PR, delete branch, remove worktree, resync main, next task.
6. Ledger of work: PR per task, this doc's IDs in PR titles (`feat(rev7/T1.1): …`).
