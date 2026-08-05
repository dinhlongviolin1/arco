# arco Plan Hardening Report (rev 5)

> Pre-build adversarial review of `docs/implementation-plan.md` (rev 4.2). Produced before any daemon
> code exists, because every blocker below is cheapest to fix in the PASS-0 freeze and a table-rebuild
> to fix once rows exist. **Verdict: GO** — no finding invalidates the architecture (single-writer
> ledger, derived-state reconcile, `VMClient` cross-VM seam, shadow-first autonomy). Every blocker is a
> *specification hole or an unreconciled supersession*, not a design flaw.

## Method
- **16 Claude domain reviewers** (security, concurrency, scale, data-model, session-ownership,
  session-hierarchy, capability, crash-safety, cost, contracts, brain, autonomy, memory, build-order,
  ux, observability) → **each blocker/major adversarially verified** by an independent refuter
  (default-REJECT) → synthesized. 96 agents, 97 raw findings, **55 survived verification**.
- **3 qwen3.8-max cross-family passes** (via `clavis qwen-1 -- --model opus`) on the areas that most
  benefit from a non-Anthropic perspective: security+capabilities, concurrency+crash+schema, and the
  newest/least-reviewed rev-4.2 session ownership+hierarchy additions. 24 findings.
- Reviewers were told the plan uses **layered supersession** (REV 4 / 4.1 / 4.2 override earlier task
  text) and to flag a stale artifact only when the supersession itself is incomplete or contradictory.

All line numbers refer to `docs/implementation-plan.md` unless noted.

## Cross-reviewer convergence (highest confidence)
- **Runtime permission-freshness gap — hit by 4 independent reviewers.** `perm_rev` bumps on
  Grant/Revoke but **nothing recompiles/CAS-updates a running worker at runtime** — only boot (Task 27)
  and Claim/Transfer (PASS-3) re-sync. Depending on direction it breaks, a worker is either **silently
  stalled** (dispatch gate closes, no re-opener → session hangs) or **silently wide** (revoked capability
  stays live in the on-disk config). → **B7 + M5 + open-q #3.**
- **`Allowed()` is not authoritative over worker-direct actions** — 2 reviewers. It gates only
  arco-executed side effects; a worker performs fs/bash/`git`/`gh` live in-process and can `curl .../merge`
  or `gh api` with no `git` subcommand for the hook to see. → **B8.**
- **Provider-pool lease lifecycle** (leak on crash, un-frozen columns, no reacquire on Claim/Resume) —
  2 reviewers. → **B10-lease + M4.**
- **Task 23 still bakes `net-fetch` into the compiled `allow` set** (contradicts rev-4.1 default-off) and
  is *missing* the SUPERSEDED marker Task 20 got — 2 reviewers. → **B-net-fetch / M11.**

---

## Blockers (must fix in the rev-5 editing pass, before code)

**B1. PASS-0 schema is prose-only; the only concrete DDL is the stale rev-3 block, and two schema sources are unreconciled.**
Where: header L211 + raw block L213-332; PASS-0 L40,48,124; Task 2 L445-447.
The rev-3 block is `CREATE TABLE IF NOT EXISTS` throughout, has `events.active/compacted`, `cost_usd REAL`,
nullable `owner_session`, zero CHECK enums, and none of the ~12 frozen tables. A fresh DB via the Task 2
path has no seeded `kind='pool'` row → the first `CreateWorker` fails NOT NULL + FK on `owner_session`.
**Fix:** produce ONE frozen `0001_init.sql` as real DDL; declare a single source of truth (generate
`schema.sql` from replayed migrations, or drop it and have `Open` run migrations); stamp the rev-3 block
`SUPERSEDED — do NOT embed`; put the fixed-ULID `kind='pool'` INSERT at the end, after sessions/workers;
migrate-from-fixture test asserts replay==schema and exactly one pool row before any worker insert.

**B2. The "locked" Key Interfaces block contradicts the PASS-0 type freeze for `Store` and `VMClient`.**
Where: locked marker L344 + Store iface L348-351; PASS-0 L51; G8 L87.
The block still exposes `WithWrite(func(*sql.Tx))` + `DB() *sql.DB` (the storage-engine leak that sinks
the P3 Postgres swap) and shows no `VMClient`; entity methods are ctx-less on `*Ledger`. The L344 banner
names only OpenEscalation/Decision/Spawn/AppendEvent — silent on Store and VMClient.
**Fix:** replace with `Store{ Migrate(ctx) error; WithTx(ctx, func(Tx) error) error }` where `Tx` is an
arco-owned interface (no `*sql.Tx`); re-declare CreateWorker/GetWorker/AppendEvent/TransitionWorker/
EventsSince/Grant as ctx-taking methods on `Tx`/sub-stores; add the frozen `VMClient`
(ListAgents/GitHeads/Prompt/Kill/Diff) and route Sweep/Spawn through it; extend the L344 marker.

**B3. `Deps` is frozen-by-name but defined nowhere** (used at 387,394,395,413,416,417,422,425,426).
**Fix:** add a `Deps` sketch in the plan's vocabulary — e.g.
`type Deps struct { Store Store; VM VMClient; Cfg Config; Exec *Exec; Classify func(NormalizedEntry)(CatalogRow,bool); Clock func() time.Time; Log *slog.Logger; Redact func(string)(string,int) }`
— built once in `daemon.go`, passed read-only; finalize the field set in PASS-0.

**B4. Secret redaction must move to write-time (ingress) for the immutable ledger; it has no package, owner, or test.**
Where: precond 5 L67; L58/59; Task 24a L597-599; File Structure L169-207; Task 4/6/21 tests.
`events` is IMMUTABLE with raw `payload`, but redaction is specified "at egress" — leaving the raw secret
on disk forever. No `internal/redact/`, no call in AppendEvent/intake/transcript/brain-assembly; nothing
redacts before the `transcript_fts` build. The **brain prompt to a third-party LLM is the largest exfil
surface** and isn't listed as a redaction point.
**Fix:** add `internal/redact/redact.go` `Scrub(s)(string,int)` (URL creds, GH/Telegram/Anthropic token
prefixes, high-entropy, configurable) + golden-corpus test; make it a required WRITE-boundary call in
AppendEvent, the transcript/FTS/topic-file writers, **and before `Invoke`**, each with an S1 test that a
planted token is absent from persisted bytes AND from a MemorySearch snippet; keep egress redaction ALSO
as defense-in-depth; state that `~/.arco` (0700) + `arco.db`/`logs/*.jsonl` (0600) owned by the arco OS
user is the actual at-rest guarantee.

**B5. Repo-shipped `.claude/`, `.mcp.json`, `settings.local.json`, and git hooks are never quarantined; they out-precede arco's compiled config.**
Where: precond 2 L64; Task 10 AddWorktree L496-500; Task 23 Compile L589-594.
A repo `settings.local.json` overrides arco's `settings.json`; a repo `.mcp.json` auto-registers MCP tools
that escape `Allowed()` entirely; repo hooks + `core.fsmonitor` are untouched.
**Fix:** add a PASS-3 quarantine task after AddWorktree, before Compile/launch: delete/rename repo
`.claude/` (incl. `settings.local.json`) + `.mcp.json`; `git config --unset core.fsmonitor` + disarm repo
hooks + `.gitattributes` filter/smudge/clean, submodules, `protocol.file`, `hooksPath`; each a quarantine
event; S1 test with a malicious fixture repo. `.mcp.json` tools escape `Allowed()`, so quarantine is the
*only* defense for that surface.

**B6. arco's OWN compiled `settings.json` + PreToolUse hook live inside the worker-writable worktree — the worker can self-widen live.** *(qwen)*
Where: Global Constraints paths L162; Task 23 `Compile(worktree,…)` L398,589-594.
The PreToolUse hook is an external script re-executed per tool call — rewriting it neutralizes enforcement
with no restart; the worker can also drop a higher-precedence `settings.local.json`. This is distinct from
B5 (repo-shipped configs): here it is *arco's* config that is worker-writable.
**Fix:** stage compiled config + hook script **outside the checkout** (e.g. `~/.arco/workers/<ulid>/`),
owned by the daemon user, unwritable by the worker user; point Claude Code at it via `--settings` /
managed-settings precedence. Specify the quarantine-vs-arco-config path coexistence.

**B7. `child ⊆ parent` is checked only on widen; a parent Revoke leaves descendants holding the lost capability.**
Where: Decision B L113-114; Revoke L375; Allowed L376; L165.
`Allowed()` reads the session's own derived tree, not the ancestor chain; `Revoke(P,X)` has no descendant
cascade → a live authority-escalation-via-stale-inheritance that falsifies "authority flows down."
**Fix:** make Revoke transitively cascade over the subtree (reuse the move-subtree recursive-CTE): remove
the cap from every descendant holding it, append a per-node revoke event, bump each `perm_rev`; the
freshness gate + stale-config regen then recompile affected workers. **Open q #2:** cascade-materialize vs
`Allowed()` walks the ancestor chain live.

**B8. `Allowed()` is not in the path of worker-direct tool calls; medium-tier caps with live creds have only the admittedly-bypassable hook.** *(2 reviewers)*
Where: L158,165; Allowed L376; grant tiering L24; precond 3 L65; Compile L592.
High-blast is covered (removed-from-worker + no-creds + branch protection on main). But MEDIUM-tier caps
(merge PR, push shared feature) are compiled into the worker's `allow` set and run with real creds;
server-side branch protection covers main, not arbitrary PR merges or unprotected branches.
**Fix (do NOT blanket-reclassify medium as high-blast):** (1) require every worker-executed medium-tier
cap to be backed by a *server-side non-advisory* control covering ALL targets the grant reaches
(extend branch-protection/required-review, or fine-grained token / GitHub-App scoping); (2) review
invariant — no capability may rely on the worker-side hook OR the compiled allow-rule as its sole gate;
(3) narrow L165/591: `Allowed()` is authoritative only for arco-executed actions. Reframe the model as
**compiled config = prevention, arco = detection+response**, and build the detection path
(normalizer/fusion scans transcript tails for deny-listed calls → auto-pause + danger-class escalation).

**B9. `herdr.Prompt` — the most frequent side effect — is never bracketed by `prompt_intent`/`prompt_done`, and boot recovery doesn't re-drive a dangling prompt.**
Where: G5 L85; Task 12 L516,518; Task 18 L554. (`events.kind` is plain TEXT, no CHECK — no enum rebuild.)
**Fix:** every Prompt = `prompt_intent`(embed intent ULID + acting owner in the prompt text) → Prompt →
normalizer confirms the echoed ULID → `prompt_done`. Extend Task 18: intent without done → scan the
normalized transcript for the ULID; append `prompt_done` if present else re-drive exactly once. Add the
kinds to the L276 comment. S1: intent precedes Prompt, done follows only on observed delivery.

**B10. Worker-agent LLM spend is never written to `usage`; budgets meter only the cheap brain, so the expensive fleet evades every cap.**
Where: PASS-0 L47; Task 11 L505 (only usage producer); L45,46,56.
`provider_pools.daily_usd_cap` + per-worker/session/pool budgets meter near-zero and never trip while a
session burns arbitrary provider money.
**Fix:** define how clavis/herdr surface per-turn token/cost (an `agent_usage` NormalizedEntry kind or a
herdr usage manifest); the reconciler writes `usage` rows with `call_kind='agent'`, worker/session/provider;
budget/breaker checks aggregate over ALL call_kinds. **Open q #1:** if worker spend is genuinely
unobservable in P2, ship pool/session/worker USD budgets *brain-only with a loud caveat*.
**Lease sub-blocker** *(2 reviewers)*: freeze `worker_pool_leases` columns (`pool_id`, `worker_id`
nullable-until-bound, `dispatch_intent_event_id`, `acquired_at`, `expires_at`); add boot reconciliation +
TTL reaper that releases any lease with no matching non-terminal worker / pending intent; Claim/Resume
must re-acquire a lease and pass admission before launch (see M4). Test: kill -9 between lease and intent
→ lease gone after boot.

**B11. StepResult kinds `handoff` and `final_output` have no reconciler branch.**
Where: enum L384; Task 12 branches L516-519. `final_output` (a core P2 completion signal) is a silent
no-op → worker stalls/loops; `handoff` parses in P2 with no handler.
**Fix:** `final_output` → drive `completed_candidate` then the diff gate; P2 `handoff` → reject/escalate
until PASS-3; `Validate` maps any unhandled-in-P2 kind to an `error` event, never a silent drop; state the
exhaustive kind→branch table in Task 12.

**B12. Disjointness rests on convention: the brain resolver still returns the same `Decision` type `DecideEscalation` turns into a standing Grant, and its name collides with the human store API.**
Where: locked L380-381,387; Task 22 L585; L344.
**Fix:** give the resolver a distinct return type with NO scope/yesNo/grant field —
`type DraftAnswer struct { Text string; Confidence float64; Rationale string; Tainted bool }` — and rename
it (`ResolveQuestion`/`DraftAnswer`). Freeze: no function accepts a `DraftAnswer` (or any brain-sourced
value) into `DecideConfirm`/`Grant`. New structural test: `DraftAnswer` has no scope field.

**B13. Task 24 S1 tests still mandate the brain auto-apply path rev-4.1 killed — a prompt-injection-to-persistent-memory hole.**
Where: Task 24 header L597 vs S1 list L599 ("verbatim-preference auto-apply whitelist").
A subagent building from the tests ships auto-apply of brain-derived facts into always-hot USER.md/MEMORY.md.
**Fix:** rewrite Task 24 S1 into 24a-only tests (Load/Read/Search with redaction assertion, per-fact
provenance round-trip, `mem_rev`/`memory_revisions`, and an EXPLICIT test that NO auto-apply path exists —
any author incl. "verbatim-preference" requires a human decision); delete the whitelist phrasing; add 24b
(pending-queue, zero auto-apply) + 24c (curator) stubs.

**B14. Pending escalations are never settled on release/claim/transfer — a stale `confirm` can authorize an action under the NEW owner.** *(qwen)*
Where: Decision A L98-107; DecideConfirm L44,380; Task 15 L539.
`waiting_for_user` is a legal (quiescent) transfer source, so a worker with an open question/confirm can
change owners; a human deciding it post-transfer re-submits a resume and, for `confirm yes`, authorizes a
danger action / promotes a standing Grant on the OLD session. L104's execute-time re-validation covers
worker-directed side effects, not arco-side `DecideConfirm`.
**Fix:** in the same tx as the ownership CAS, cancel-or-reanchor all pending escalations for the worker
(the `worker_released`/`worker_transferred` event carries their IDs); `DecideConfirm` re-checks `Allowed()`
against the worker's CURRENT owner tree at decision time and refuses if the owner changed; `always` grants
must target the current owner or be rejected.

---

## Major

**M1. The only non-advisory controls (managed-settings deny, server-side branch protection, OS-user separation) are unbuilt one-liners, while `exec tests/build/lint/install` is default-on = arbitrary code execution.** *(CONFIRMED)* Where: precond 3 L65; DefaultTree L571; PASS-3 L74. Promote each to a dedicated PASS-3 task + S1 acceptance test (worker `git push origin main` rejected server-side even with a forged local config; managed-settings deny beats a repo `settings.local.json`). Because exec is default-on, env-scrub + branch protection are load-bearing. **Open q #4.**

**M2. Tasks 7/10/18 specify central-PID / local-git liveness with no ⚠ SUPERSEDED banner — an implementer builds the exact model rev-4 bans.** *(CONFIRMED)* Where: L55; G4 L86 (omits 7/10/18); Task 7 L479-480; Task 18 L554. Add banners: Task 7 → signals via `VMClient.ListAgents/GitHeads`, identity = vm+workspace+boot_id+pid_start_time+remote HEAD; Task 18 → boot recovery keys off boot_id+pid_start_time (survives PID reuse across reboot); Task 10 → reframe as the `LocalVMClient` impl of GitHeads/Prompt/Kill/Diff. Add all three to G4.

**M3. Decided-but-not-yet-resumed escalation is a crash window: the resume job is in-memory only and not in boot recovery.** *(CONFIRMED)* Where: L54; Task 18 L554; Task 27 L616. Crash after `DecideEscalation` commits but before the in-memory resume runs → worker wedged in `waiting_for_user` forever. Fix: record a durable resume intent in the decision commit; boot recovery scans `escalations WHERE status IN (answered,approved,rejected) AND resumed_at IS NULL` and re-submits, idempotent via the `prompt_intent` ULID.

**M4. Claim→Resume is an un-gated spawn: it omits re-acquiring the provider-pool lease and passing budget/breaker admission.** *(PLAUSIBLE; 2 reviewers)* Where: Decision A L99-100; L45/56/106. A Claim storm or reaper churn re-spawns past `max_active`/`max_starts_per_min` or through a hard breaker. Fix: Resume acquires a fresh lease against the NEW owner's pool + passes per-VM admission + budget/breaker BEFORE launch; on denial for an AUTOMATED Claim, yield `admission-denied` and leave the worker paused-in-pool. Preserve L56's carve-out: a HUMAN resume is still permitted under a hard breaker.

**M5. Revoke does not narrow a running worker: the dispatch gate only stalls new prompts and `settings.json` is not hot-reloaded.** *(PLAUSIBLE; 4 reviewers — see convergence)* Where: L99,103; Task 27 L616; L375. Fix: on Revoke, for each running worker under the owner whose COMPILED config contains the revoked non-high-blast cap, enqueue pause→recompile→resume via `Exec.Submit` (+ `workers.rev` CAS); high-blast revokes need no restart (never compiled; enforced live by `Allowed()`). Test: a running worker cannot exercise a now-revoked worker-side cap after recompile. **Open q #3.**

**M6. Time-based grant expiry (`expires_at`) is invisible to the freshness gate and the compiled config.** *(PLAUSIBLE)* Where: L43; L103; sweep L416. Expiry is not a grant/revoke event → never bumps `perm_rev` → stale config keeps the expired grant as `allow`. Fix: `Allowed()` evaluates status+scope+use_count+expires_at live against now(); fold an expiry check into the sweep → on lapse bump `perm_rev`, append `revoke(expired)`, trigger the M5 recompile. **Open q #7.**

**M7. `events.kind` vocabulary is never consolidated (and is deliberately un-CHECK'd), so a typo'd/missing kind is silently mis-dispatched.** *(PLAUSIBLE)* Where: L50; L276-280; G5 L85; L128. Fix: PASS-0 consolidates one authoritative Go `const` enum + a table of every intent/result pair (rev-3 set + `prompt_intent/prompt_done` + `worker_*_intent/ed` + `session_moved` + `memory_diff`); NO SQLite CHECK (keeps the append-log extensible); reconciler/normalizer switches have a default case recording an `error` event for an unknown kind.

**M8. Task 4's step still says `INSERT OR IGNORE`, contradicting its own banner, and the hash-mismatch return contract is undefined.** *(PLAUSIBLE)* Where: banner L457; step L460; sig L357; intake L473. Fix: delete the stale `INSERT OR IGNORE` text; define `AppendEvent`'s contract — hash match → `deduped=true`; hash MISMATCH → append an `error` event + return a DISTINCT conflict signal (not deduped); other constraint error → return err. Amend intake so the conflict signal still enqueues a Reconcile. Same-id/different-hash S1 test.

**M9. A worker hitting a billing wall / sustained 429 is only a generic `error`, so the reconciler can re-prompt it into the wall repeatedly.** *(PLAUSIBLE; P2-scoped)* Where: L159; Task 12 L516; NormalizedEntry L402; L519. Fix: add distinct `billing`/`rate_limited` NormalizedEntry kinds + fusion classification; hard billing → park `blocked` + alert (mirror the brain path); reserve jitter-backoff for transient `rate_limited` with a bounded attempt cap; defer pool-billing-state to PASS-3.

**M10. `AnswerQuestion` name collides (brain resolver vs human store API); the split store API has no concrete Go sigs.** *(PLAUSIBLE)* Rename the resolver (B12); add `l.AnswerQuestion(ctx,id,text string,scope Scope) error` / `l.DecideConfirm(ctx,id string,yes bool,scope Scope) error`.

**M11. Security-critical `CapabilityOf(action)→catalog row` has no signature; input type and fail-open/closed behavior are unfrozen (where "NL can only raise" safety lives).** *(PLAUSIBLE; 2 reviewers)* Where: L51,57; L369-376. Fix: freeze `func CapabilityOf(e NormalizedEntry) (CatalogRow, bool)` keyed on the already-frozen `NormalizedEntry`; `CatalogRow{ Capability; ActionClass; Tier; DefaultAllowed bool; HighBlast bool }`; `classified=false` = unclassifiable → **fail-closed** (`ask`/deny, never routine) → NULL capability (matches G2's `COALESCE`). Also fix Task 23 S1 (remove `net-fetch` from allow) + add its SUPERSEDED marker.

**M12. Checkpoint archives all `active=1` rows (including turns appended mid-summarization) and leaves the in-flight-call CAS-failure path undefined.** *(PLAUSIBLE)* Where: L386; L54; L229. Fix: Checkpoint snapshots the max transcript row-id it read and soft-archives `WHERE active=1 AND id <= snapshot`; concurrently-appended turns stay active at the current rev; reserve the `context_rev` CAS for checkpoint-vs-checkpoint; a decision assembled at rev N re-validates `context_rev` at apply time, drops + re-enqueues one re-decide on mismatch.

**M13. Release "reuses Task 17" but contradicts it on worktrees; the pool TTL reaper is a no-op with no durable clock.** *(qwen; 2 passes)* Where: L99,105 vs Task 17 L549. Task 17's Pause is specified+tested to REMOVE the worktree; Release must KEEP worktree+branch. And Release already *is* pause (process dead), so "TTL→Pause" does nothing; no `pooled_at` clock survives restart. Fix: define `Pause(id, keepWorktree bool)` (or Release as its own op) + fix Task 17's test; record `pooled_at`; spec the reaper's real terminal action (kill row + worktree/branch GC + event) via `workers.rev` CAS so a concurrent Claim wins cleanly.

**M14. Session teardown fan-out has no join, and execute-time re-validation doesn't cover teardown.** *(qwen)* Where: L104,116; Task 17/20. Nothing notices "last worker quiesced" → sessions hang in `active` forever; a prompt job queued ahead of a pause job still fires `herdr.Prompt` into a mid-teardown session (P2 teardown doesn't change owner, so owner+rev re-validation misses it). Fix: after each per-worker teardown job commits, submit a coalesced session-completion-check job (guarded by `sessions.rev` CAS) that closes the session when no non-terminal workers remain; extend execute-time re-validation to drop side effects when a `session_teardown_intent` is open for the owner.

**M15. Transfer stages the new permission config on disk BEFORE the ownership CAS, and CAS-failure semantics are unspecified (contradicts "no side effect without a successful CAS").** *(qwen)* Where: Decision A L101 vs L54. On live CAS failure the worktree holds session B's config while the owner is still A; a naive retry resumes under the wrong tree. Fix: stage at a temp path; the ownership CAS is one tx that also updates `permissions_hash`+`compiled_config_path`; only then atomically install + resume; on CAS failure discard staging and re-drive from the intent (never resume from a stale stage). Spec the same for Release.

**M16. Orphaned pre-created worktrees are never GC'd after a crash.** *(qwen)* Where: L54; Task 18 L554; Task 14 L531. Crash between `git worktree add arco_<ulid>` and the `worktree`-column write → boot marks the worker `failed` (correct) but the on-disk worktree is referenced by nothing. Fix: boot recovery enumerates worktrees matching the deterministic `arco_<ulid>` pattern and removes any whose ULID has no non-terminal worker row (safe because IDs are pre-generated). kill-9 test between add and the row write.

**M17. Escalation timeout→auto-pause never CLOSES the pending row, so a re-asked question dedups into a stale row and becomes invisible forever.** *(qwen)* Where: rev-4.1 G2 L83; L57; Task 15 L539. Fix: timeout/answer closes the row (`status='expired'/'answered'`) in the SAME tx as the pause/transition; `OpenEscalation` = `INSERT … ON CONFLICT DO NOTHING` returning the surviving row (a bare INSERT errors the reconciler on every benign dedup). See open-q #5 on whether the index needs `kind`.

**M18. Depth and fan-in caps are not enforced at `CreateSession` — depth-3 is constructible today despite "deferred."** *(qwen)* Where: Decision B L111,116; L120. The cycle/depth check is specified only for move-subtree (which doesn't ship); create-with-parent has no depth check, fan-in check, or self-parent guard. Fix: in the `CreateSession` tx enforce `max_depth` (2 while supersession is the feature), a PINNED `max_children_per_session` (a number, not "~6–15"), and `parent_session ≠ id`.

**M19. Authority-laundering up the rollup: `Tainted` is behavioral (persuades the LLM), not structural.** *(qwen)* Where: Decision B L113-114; L57. A jailbroken child shapes its ~2K summary; the root brain consuming rollups can be steered into `fleet.*`/ownership actions that are routine-class and fully legal under the root's own tree. Fix: make it structural — a brain call whose assembled context includes rollup summaries may not emit `fleet.*`/ownership side effects (shadow/draft only), or any such op force-escalates to a human confirm; add to the disjointness test set (the "3 tests").

**M20. Rollup cost controls are unpinned and compete with the hot path for the global brain semaphore.** *(qwen)* Where: Decision B L112; L154; L46. Fan-in cap, rollup interval, and per-session brain-rate cap are all un-numeric → cost unbounded; rollup calls draw from the same `MaxBrainCalls` semaphore as per-worker decisions and can starve the hot path without tripping a breaker. Fix: pin the numbers at freeze; charge rollup usage with `call_kind='rollup'`; give worker-decision calls admission priority over rollups.

**M21. No operator escape hatch to lift a hard-freeze; `/healthz` is a static stub; no CLI to LIST pending escalations / shadow drafts; operability magic numbers have no defaults.** *(mixed CONFIRMED/PLAUSIBLE)* Where: L56, L51, L80, L463-467, L89, L439. Fixes: add `arco unfreeze` (alias `breaker reset`); wire self-liveness to systemd `Type=notify`+`WatchdogSec` (ping only while the write queue drains AND last sweep < K×SweepInterval AND DB ping ok); add `arco escalations [--pending]` + `arco autonomy [status|drafts]`; add an operability-defaults table (`SweepInterval=30s, MaxSpawns=8, MaxBrainCalls=4, stall-N=3, breaker=5 restarts/10min`) overridable via `ARCO_*`/TOML.

---

## Minor / robustness
- **Byte-stability purity** (Task 21 L578): forbid wall-clock/`now`/random/env in `AssembleContext` (pass an event-pinned timestamp); sort roster + lists by ULID; sleep between the two byte-identical calls in the test.
- **Always-hot file reads race memory writes**: define byte-stability over `(ledger state, mem_rev)`; read USER.md/MEMORY.md pinned to `mem_rev`.
- **>150-worker SQLite→Postgres boundary unenforced**: add a fleet-wide `max_live_workers` admission gate; add `LivenessMissThreshold`+`SuspectTimeout` (must exceed one SweepInterval) to the defaults.
- **Breaker reset/hysteresis + soft/hard trip fractions + `daily_usd_cap` reset semantics undefined**; drop "disable hindsight" (a P2 no-op) for a P2-real soft action.
- **Money-type**: freeze `budgets`/`breakers` thresholds as `*_microusd INTEGER` to match `usage.cost_microusd`; compare in integer microusd.
- **`DefaultTree()` vs `capability_catalog`**: derive `DefaultTree()` from the catalog (single source); test that no `high_blast=1` cap appears in any compiled allow list.
- **`question_class` value set** never enumerated: one frozen `escalations` DDL, enumerate `question_class` with a CHECK, document the status lifecycle per kind.
- **Delegation caps vs session caps** composition: state a self-spawned worker is a leaf under the SAME `owner_session` (never auto-creates a child session), or define the compose order. **Open q #6.**
- **`sessions.rev` CAS reused for rebind/status/rollup**: give rollup a non-CAS or separate `rollup_rev` path; move-subtree CAS-checks all touched rows in one tx.
- **Spawn adopt-or-abort launch-in-flight TOCTOU**: bounded settle/re-scan of `ListAgents` before aborting; verify no OS process holds `workspace=arco_<ulid>` before `failed`.
- **Malformed-ladder transcript hygiene**: record malformed outputs only as `error` events, never `brain_msg`; reprompt appends a corrective, not an identical prompt; any `Billing=true` at any rung short-circuits to park-blocked.
- **24a manual write interface**: freeze `ApplyMemoryDiff(diff, decidedBy) error` requiring a human `decided_by`, rejecting `author=brain`, bumping `mem_rev`+`memory_revisions`; enumerate `author`/`trust`/`state` as validated enums.
- **Telegram SSE consumer has no durable cursor**: persist last-acked `events.id`; idempotent discrete posts keyed on escalation id.
- **General-topic mirror doubles send volume**: account for page+mirror in the ~18/min bucket; assert no session binds `tg_topic_id=1`.
- **systemd unit never produced**: add a deploy task producing the `.service` unit; layer systemd `StartLimitIntervalSec/Burst` (outer) against arco's boot-timestamp breaker (inner → freeze, not exit).

---

## PASS-0 schema & contract deltas (the expensive-to-change freeze)

**SQL — produce ONE frozen `0001_init.sql` (delete/stamp the rev-3 block L213-332):**
- [ ] `events`: remove `active`/`compacted`; keep raw `payload`; `UNIQUE(source, source_event_id)`; add `source_event_hash`; `kind TEXT NOT NULL` with NO CHECK; IMMUTABLE.
- [ ] Seed exactly one fixed-ULID `kind='pool'` row (parent_session NULL, all NOT NULL cols populated) at the END of the migration.
- [ ] `escalations`: one consolidated DDL; partial-unique `ON escalations(worker_id, COALESCE(capability,'')) WHERE status='pending'` (drop `session`+`action_fingerprint` from the index; keep `action_fingerprint` as a stored column); add `question_class` with an enumerated CHECK; timeout/answer closes the row (M17); document the status lifecycle per kind. **Open q #5** on whether `kind` joins the index.
- [ ] `usage`: `cost_microusd INTEGER`, `call_kind`, `worker_id`, `session_id`, `provider`; add `call_kind='agent'` + `'rollup'` producers.
- [ ] `budgets` + `breakers`: thresholds as `*_microusd INTEGER`.
- [ ] Add every frozen table absent from the rev-3 block: `capability_catalog`, `session_grants`(+`expires_at`), `provider_pools`, `worker_pool_leases`(+`pool_id`,`worker_id` nullable,`dispatch_intent_event_id`,`acquired_at`,`expires_at`), `budgets`, `breakers`, `vms`, `vm_observations`, `brain_transcript_rows`, `transcript_fts`, `schema_migrations`, `memory_revisions`.
- [ ] `sessions`: add `pooled_at` handling for the pool reaper (column or indexed query over `worker_released`).
- [ ] `schema_migrations` bootstrap row; declare one authority.
- [ ] Migrate-from-fixture test: replay==schema AND exactly one `kind='pool'` row before any worker insert.

**Go interfaces — reconcile the "locked" block (L344-358) to PASS-0 (L51):**
- [ ] Replace `Store{WithWrite(func(*sql.Tx)); DB()}` with `Store{ Migrate(ctx) error; WithTx(ctx, func(Tx) error) error }`; `Tx` arco-owned (no `*sql.Tx`).
- [ ] Re-declare CreateWorker/GetWorker/ListWorkers/TransitionWorker/AppendEvent/EventsSince/Grant as ctx-taking methods on `Tx`/sub-stores; `EventsSince(ctx, cursor, limit)` bounded.
- [ ] **Add `expectedRev` to the CAS mutators** and a typed `ErrRevMismatch`: `TransitionWorker(ctx, id, to, expectedRev, e)`, `Checkpoint(ctx, sessionID, expectedCtxRev, watermark)`, `Grant/Revoke → (newPermRev, error)`, `AssembleContext → (prompt, ctxRev, watermark, error)`.
- [ ] Add `VMClient` (ListAgents/GitHeads/Prompt/Kill/Diff); route Sweep/Spawn through it; extend the L344 banner to name Store + VMClient.
- [ ] Define `type Deps struct {…}`.
- [ ] Freeze `CapabilityOf(e NormalizedEntry) (CatalogRow, bool)` + `type CatalogRow struct {…}`; unclassifiable = fail-closed.
- [ ] Rename the brain resolver off `AnswerQuestion`; `type DraftAnswer struct { Text; Confidence; Rationale; Tainted }` (no scope field); concrete Go sigs for `AnswerQuestion(ctx,id,text,scope)` / `DecideConfirm(ctx,id,yes,scope)`.
- [ ] Consolidated `event_kind` Go const enum; add `prompt_intent|prompt_done` to the L276 comment.
- [ ] Add `internal/redact/` with `Scrub(s)(string,int)`; wire as a write-boundary call in AppendEvent + transcript/FTS/topic-file writers + before `Invoke`, plus egress read paths.
- [ ] Add `agent_usage` + `billing`/`rate_limited` to `NormalizedEntry.Kind`.

---

## Open questions for the maintainer
1. **Is per-worker agent LLM spend observable in P2?** (clavis/herdr usage manifest or NormalizedEntry?) If not, ship pool/session/worker USD budgets **brain-only with a loud caveat** — confirm. *(blocks B10.)*
2. **`Allowed()` and the child⊆parent theorem (B7):** cascade-materialize a revoke into descendant rows (cheap reads) OR make `Allowed()` walk the ancestor chain live (cheap writes)? Pick one.
3. **Revoke on a running worker (M5):** pause→recompile→resume, or hard kill? What widen-window latency is acceptable?
4. **Should `exec tests/build/lint/install` stay default-on (M1)?** It is arbitrary code execution; if it stays on, env-scrub + server-side branch protection are load-bearing and must be built to spec.
5. **One-open-escalation-per-worker (B6/M17):** can a worker hold at most one pending escalation (drop `action_fingerprint`; `kind` does not participate), or must it hold a pending question AND confirm simultaneously (index needs `kind`)?
6. **Delegation vs session caps:** does a self-spawned worker stay a leaf under the same `owner_session`, or may it create a child session?
7. **Grant expiry (M6):** will any P2 grant-creation path set `expires_at` (e.g. `arco grant --expires`)? If not, the expiry reaper is defined-but-inert.
8. **`daily_usd_cap` reset + freeze durability:** UTC-midnight vs rolling-24h; does a hard-freeze auto-recover at rollover or require `arco unfreeze`? Soft/hard trip fractions + hysteresis.
9. **Final `Deps` field set** and whether reads are ctx-threaded in P2 or documented ctx-free.
