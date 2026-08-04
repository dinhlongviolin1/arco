# Castellan Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Rev 3 (2026-08-04)** — adds five design threads on top of the rev-2 hardened skeleton: (1) **sessions** as the first-class unit of work+conversation that owns workers across VMs; (2) a **context-balance policy** (durable per-session brain transcript + opportunistic warm prefix, never a warm process); (3) a **4-tier memory** system with off-hot-path hindsight write-back; (4) a **Telegram forum-topic UX** (one topic per session); and (5) **autonomy-first control** — no per-action approval gates, a per-session **capability tree** compiled into each worker's Claude Code permission config, two-level question answering, and tiered grants. The rev-2 TDD task structure is preserved verbatim; rev-3 work is additive (new Tasks 20–27) plus targeted edits to the schema, the worker-state enum, the interfaces, and the approvals task (now "escalations"). See "Revisions (rev 3)" below.
>
> **Rev 2 (2026-08-04)** — hardened after an independent architecture review (Fable). Changes vs rev 1 are summarized in "Revisions from review" below and folded into the tasks/schema. The core shape is unchanged and endorsed; the revisions defend the two things that *justify* the short-lived-brain bet: crash-safe intent recording and cost/idempotency economics.

**Goal:** Build `castellan` — a self-hosted Go daemon that durably supervises a fleet of coding-agent workers (spawned via `clavis`, herded by `herdr`), reconciles their state through a state machine, invokes a short-lived LLM "brain" only for decisions, and is driven headless from a CLI (with optional Telegram + Web clients).

**Architecture:** One long-lived Go daemon owns a SQLite ledger (the single source of truth for *decisions*, reconciled against external reality). `herdr`'s plugin-hook pushes worker state-changes to the daemon's HTTP endpoint *as an optimization over an authoritative periodic reconcile sweep*; when a decision is needed the daemon spawns a *short-lived* LLM call via `clavis` that returns a typed `StepResult`. Decisions serialize per-worker but run concurrently across workers. Nothing long-running is an LLM process.

**Tech Stack:** Go 1.22+, `modernc.org/sqlite` (pure-Go, no cgo), stdlib `net/http` (ServeMux) + SSE, `spf13/cobra` (CLI), `BurntSushi/toml` (config), `oklog/ulid/v2` (IDs), `stretchr/testify` (tests). Optional add-ons: `go-telegram/bot`; stdlib `html/template` + htmx (Web — no JS build). Shells out to `clavis` and `herdr`.

## Revisions (rev 3)
1. **Sessions are first-class** — a `sessions` table is the unit of *work* and *conversation*. A session OWNS a group of workers (possibly across VMs — ownership is a FK in the central ledger, placement is the worker's `vm` column), has its OWN durable brain transcript + rolling summary, and carries a versioned `permissions` blob. `jobs` is subsumed: `workers.owner_job` → `owner_session`; the old `jobs` progress/facts fields move onto the session. *(Schema; new Task 20; Tasks 3/14 reference `owner_session`.)*
2. **Context-balance policy** — the brain is stateless as a *process* but keeps a durable per-session *transcript* (`events kind='brain_msg'` with Hermes-style `active/compacted` soft-archive + FTS5) reassembled byte-stably each event. Prompt-cache is opportunistic (1h TTL only during an active burst; measured via existing `usage.cache_read/write`), never a keepalive. Checkpoint folds the transcript into a bounded `context_summary` on idle/threshold/state-change. *(New Task 21; schema `sessions.context_summary/context_rev`.)*
3. **4-tier memory + hindsight write-back** — (T1) whole-file `USER.md` always-hot in the static prefix; (T2) `MEMORY.md` index always-hot + topic files retrieved JIT via a `memory_read` tool; (T3) per-session `facts`+`context_summary`+FTS5 over archived events; (T4) playbooks (existing). Sessions never write user-memory mid-flight; an off-hot-path **hindsight pass** proposes typed memory diffs (`add|update(supersedes)|expire`, bi-temporal provenance) that land as escalations; a curator ages topic facts (ACTIVE→STALE→ARCHIVED, pinned/referenced-protected). *(New Task 24; files under `~/.castellan/memory/`.)*
4. **Telegram forum-topic UX** — one private forum supergroup; castellan creates one **topic per session** (`sessions.tg_topic_id`); a pinned, `editMessageText`-updated status card per topic; the General topic (id 1) is the fleet console + mute-safe escalation mirror; per-session `notify_level`. Generalizes 1:1 to the Web UI (both render `events WHERE session_id=?`). *(New Task 26; replaces the thin Telegram sub-plan.)*
5. **Autonomy-first control (replaces per-action approvals).** Three action classes: **routine** (act, no gate), **stuck/ambiguous** (ask a question — "small feedback"), **danger/irreversible** (confirm). **Two-level autonomy**: a worker's question is answered FIRST by castellan's brain from session context + user memory + playbooks; escalate to the HUMAN only if castellan is itself uncertain OR the action is danger-class. "approvals" → "escalations". *(Task 15 reframed; new Task 22 two-level resolver.)*
6. **Per-session capability tree, inherited by workers, compiled to worker config.** A versioned `permissions` blob on the session; every grant/revoke is an event. Enforcement is **two layers**: (a) at spawn, compile the session tree into the worker's own Claude Code permission config — `settings.json` `permissions.{allow,deny,ask}` + a `PreToolUse` hook + `--allowedTools/--disallowedTools` written onto the machine so the worker self-limits; (b) castellan re-checks the tree at its OWN action boundary (authoritative, because worker-side hooks have known gaps — [claude-code#37210](https://github.com/anthropics/claude-code/issues/37210)). Inheritance is narrow-only downward: worker ⊆ session ⊆ global-default+grants. **High-blast capabilities are not granted to the worker at all** — castellan executes them itself after a confirm. *(New Tasks 20/23; schema `sessions.permissions`.)*
7. **Grant tiering.** Escalations offer `[✅ This once] [✅ Always for session] [❌ No] [👀 Diff]`; default once. Medium-risk (merge PR, push shared feature) may be promoted to a standing session grant by one tap; **high-blast (push main, deploy, spend, destructive delete) are per-action-once only** — a standing grant requires a deliberate CLI `castellan grant`, never a phone tap. Async posture: standing-grant routine-above-default so the fleet doesn't stall while the operator is away. *(Task 15/23; `escalations.once_or_always` + a `tier` guard.)*

## REV 4 — final review hardening (2026-08-04) · BUILD FROM HERE

Eight-aspect final review before code: **opus×3, fable×2, codex×3** (qwen hit its weekly quota; Codex substituted). Full findings on disk: `specs/hermes-analysis/review2-{security,concurrency,scale,memory,datamodel,autonomy,ux,planexec}.md` + `qwen-design-review.md`. This section is the authoritative build guide; where it conflicts with rev-1/2/3 task text below, **rev 4 wins.**

**Unanimous verdict:** the architecture (durable Go daemon + SQLite ledger + short-lived brain + sessions + capability tree) is sound and **unchanged**. The P2 headless core is buildable — but **NOT in the 1→27 order** and **NOT against real repos/creds** until a schema/contract freeze + security preconditions land. Every fix is mechanical.

### The five decisions — RESOLVED (operator away; my calls)
1. **Two-level autonomy → build the resolver, DEFAULT to SHADOW/DRAFT for all classes in P2.** No unattended auto-answer until measured per question-class via `castellan autonomy promote` (gate: ≥50 samples, ≥95% agreement, 0 danger-misses). `proceed-confirmation` and `scope-change` classes **never** auto (P2/P3).
2. **Delegation (worker self-spawn) → DEFAULT OFF.** Per-session grant only; depth **1**; ≤**2** children/parent; ≤**5** live workers/session; ≤**20** spawns/session/day.
3. **Hindsight + LLM curator + playbook-learning → DEFERRED past MVP.** P2 memory is **manual-only** (Task 24a).
4. **Trust-boundary hardening → ACCEPTED as mandatory** before running against real repos/creds (the 6 preconditions below).
5. **Byte-stable transcript → keep the cheap invariant + prompt-hash + cache telemetry only; NO cache-plan engine** until measured `cache_read` proves payoff.

### PASS-0 — SCHEMA & CONTRACT FREEZE (do before ANY task; datamodel+scale reviews)
Freeze `schema.sql`, then no more `CREATE TABLE IF NOT EXISTS` drift:
- **events is IMMUTABLE** — remove `active/compacted`; add `source`, `UNIQUE(source, source_event_id)` (namespaced), `source_event_hash` (on hash-mismatch record an `error` event, use `ON CONFLICT DO NOTHING` not blanket `OR IGNORE`), `actor`, `causation_event_id`, `correlation_id`. `EventsSince(cursor, limit)` bounded.
- **`brain_transcript_rows`** — separate table + **`transcript_fts` FTS5** over it (index normalized content, **never** raw `events.payload`; large turns spill to files).
- **`capability_catalog`** (capability→action_class{routine|ambiguous|danger}, tier{low|medium|high_blast}, default_allowed, high_blast) + **`session_grants`** (rows not JSON: status, scope{session|once}, source_escalation_id, granted_by, expires_at, use_count, created_perm_rev). `sessions.permissions` = derived cache only.
- **`escalations`** += `capability`, `action_fingerprint`, `requested_event_id`, `classifier`(+version), `decision`, `answer_text`, `decided_by`; unique-pending index (session, capability, fingerprint). Split API: `AnswerQuestion(id,text,scope)` vs `DecideConfirm(id,yesNo,scope)` (drop the shared underspecified `Decision`).
- **`provider_pools`** (provider, org, clavis_profile, model_class, max_active=35, max_starts_per_min, daily_usd_cap, state) + **`worker_pool_leases`** (acquired BEFORE dispatch_intent).
- **`budgets`** + **`breakers`** (fleet/session/pool/worker; soft/hard usd+tokens). **`vms`** + **`vm_observations`** (observation plane).
- **`usage`** += provider, call_kind, event_id, prompt_hash, context_rev, perm_rev, success, billing_error, `cost_microusd` (integers; dollars display-only).
- **`schema_migrations`** + versioned embedded migrations (`0001_init.sql`…) + migrate-from-fixture test.
- **`sessions`** += `mem_rev` + **`memory_revisions`** table. **`workers`** += `task`, `run_reason`, `parent_worker_id`, `delegation_depth`, `role`, **`rev` (optimistic CAS)**, `session_perm_rev`, `permissions_hash`, `compiled_config_path`; **`owner_session` NOT NULL**.
- **enum CHECKs** on every status/class/tier column; UTC RFC3339Nano timestamps; the missing hot-query indexes (session_grants active; escalations pending per session/worker; events(session,kind,id) + events(recorded_at,id); usage(session,at)/(worker,at)/(at); unique sessions(tg_topic_id)).
- **Freeze shared types:** `Deps`; `VMClient` (scale review §1: `ListAgents/GitHeads/Prompt/Kill/Diff`); `Store{Migrate(ctx); WithTx(ctx, func(Tx))}` + per-entity sub-stores (keep `*sql.DB/*sql.Tx` INSIDE the sqlite impl); `CapabilityOf(action)→catalog row` (the structural classifier).

### New / changed behavior (folds every review's blockers into rev-3 Tasks)
- **Concurrency (fable):** `TransitionWorker` = optimistic **`workers.rev` CAS**; **no side effect without a successful CAS**. One mutation domain per worker — Sweep/answer/verify/pause/kill/boot all via `Exec.Submit(workerID,…)`. **Spawn reorder:** pre-gen ULID + deterministic `workspace=castellan_<ulid>`; `dispatch_intent`+`CreateWorker` BEFORE any external side effect; adopt-or-abort recovery. **Escalations open-and-return** (never hold a queue slot/semaphore on a human); decision re-submits a resume job; channel is watcher-only. Fix `Store` reentrancy. Per-session lock around assemble/append/checkpoint with `context_rev` CAS.
- **Cross-VM liveness (scale):** rewrite Tasks 7/10/18 behind **`VMClient`** (`LocalVMClient` now; `SSHVMClient` P3). Identity = `vm + workspace + boot_id + pid_start_time + remote HEAD` — **never central PID**. Batched per-VM observation; `suspect_missing`→`lost`.
- **Rate limits + cost (scale):** provider-pool **leases before spawn**; per-pool 429/cooldown/billing state; **fleet/session/pool/worker budgets + breaker** with `castellan freeze` (soft: halve spawn rate + disable hindsight; hard: park new work, keep sweeps/pause/kill/human-resume). Per-VM admission gates.
- **Autonomy (fable):** danger-class/tier assigned **structurally from `capability_catalog`** at `OpenEscalation` (NL can only *raise*, never lower); **disjointness invariant** — a brain answer is advisory text that can never reach `Grant()`/confirm (3 tests). Graduation modes shadow→draft→per-class-auto; **escalation timeout → auto-pause**; per-session auto-answer budget; FYI-visible even when silent (CLI surface too).
- **Memory — split Task 24:** **24a (P2)** manual bounded memory: `USER.md`≤4KiB + `MEMORY.md` index≤4KiB always-hot; topic files JIT via `memory_read`≤6K chars; FTS5 recall top-5 ≤500 chars w/ source IDs; **secret redaction at every egress**; per-fact frontmatter provenance. **24b (post-MVP)** hindsight = proposals to a pending queue, **zero auto-apply**. **24c (P3)** LLM curator + playbook learning. No `author=brain` fact auto-applies; Checkpoint must not launder brain answers (`SourceEvents`+`Tainted`).
- **Security:** the 6 preconditions below + **secret redaction on the permanent ledger** (rev-3 dropped it) + structural action-class.
- **UX — all OFF-BY-DEFAULT add-on path, does NOT block P2 core:** static **`notify_class {page|card|digest}`** table; Telegram coalescer = per-topic ≤1 edit/60s **AND** global ~18/min token bucket + outbox (never backpressures the write path); General digest as default surface; split buttons (question=`[Send draft]`+ForceReply, confirm=`[✅once][✅always][❌][👀diff]`); `SSE id == events.id`.

### Security preconditions — before real repos/creds (6)
1. OS-user separation + **scrubbed spawn env** (strip GH/Telegram/brain tokens from worker env).
2. Hardened git + **quarantine repo-shipped `.claude`/`.mcp.json`** (repo `core.fsmonitor`/hooks are a host-escalation vector).
3. Managed-settings deny layer + **no high-blast creds on worker boxes** + **server-side git branch protection** (only non-advisory layer).
4. **Auth on Telegram (sender allowlist), Web, and cross-VM intake** (source-bound + signed).
5. **Secret redaction** at ledger + memory + transcript egress.
6. **Pinned spawn contract** (deny-rules+hooks survive `--dangerously-skip-permissions`, but headless `-p` aborts on unlisted `ask` tools — pin the mode) + structural action-class + `network-fetch`/`spawn` default-off.

### Reconciled P2 build order (SUPERSEDES the 1→27 numbering)
- **PASS-0** freeze + define `Deps`/split-`Decision`/`VMClient`/`Store`/`CapabilityOf`.
- **PASS-1 spine:** module/config → Store+migrations → ledger (workers+CAS, immutable events, sessions, escalations skeleton) → API over unix socket + `/healthz` → CLI client.
- **PASS-2 single-VM supervise (walking skeleton):** `LocalVMClient` → normalize → fusion → spawn (reordered, env-scrubbed) → executor (per-worker CAS, brain off write-path) → reconciler (intent-first + error/billing ladder) → sweep → brain (assembly + StepResult) → escalations (**structural class; autonomy SHADOW**) → diff-gate → boot recovery. End-to-end test `dispatch→hook→complete`, headless.
- **PASS-3 hardening:** budgets+breaker+`freeze` → provider-pool leases → per-VM admission → secret redaction → env-scrub/git-hardening + server-side branch protection → memory 24a.
- **Then P3 sub-plans:** `SSHVMClient`+cross-VM, provider pools at scale, Telegram (notify_class+coalescer), Web, hindsight 24b, curator/playbooks 24c, Postgres/queue (cross at >150 live workers or >100 events/s sustained).

### Deferred to sub-plans (P2 stays headless-complete without them)
Telegram · Web · hindsight (24b) · curator+playbook-learning (24c) · cross-VM SSH + provider-pool routing + Postgres/queue · scheduler.

### rev 4.1 — qwen3.8-max sign-off patches (verdict: **GO WITH FIXES**; full report `specs/hermes-analysis/review2-qwen-rev4.md`)
Independent meta-review verified **26/27 blockers resolved** and all 5 decisions correct+consistent. Absorb these **in the PASS-0 editing pass** (contract-shaped — expensive after rows exist):
1. **Complete the PASS list (G1).** Place: **capability/catalog + `Allowed()` implementation** in PASS-1 (catalog is frozen in PASS-0 but never *implemented* in a PASS, yet PASS-2 needs it); **permcompile (tree→worker config, Task 23)** + **intake `POST /v1/events` (Task 6, it IS the hook the PASS-2 `dispatch→hook→complete` test fires)** + **pause/resume (Task 17, needed by "escalation timeout→auto-pause")** + **session boot/`perm_rev` re-sync (Task 27)** in PASS-2. Explicitly defer SSE (19) + session CLI/rollup (25). State that the reconciler is built against frozen interfaces with fakes (it's listed before brain/escalations).
2. **Fix the escalation dedup invariant (G2)** before freezing the index: free-text questions have **NULL capability** and SQLite treats NULLs as distinct → the unclassifiable-question class never dedups; and the index is session- not worker-scoped. Use `worker_id` + `COALESCE(capability,'')` in the partial unique index (or enforce one-open-per-worker in `OpenEscalation`).
3. **Name the pinned spawn mode (G3):** **interactive-in-pane, NEVER headless `-p`** (headless aborts on unlisted `ask` tools → the whole observe-`prompt_wait`/answer-via-`herdr.Prompt` path breaks). Add a normalizer test that a real `ask` prompt yields `prompt_wait`.
4. **Add to the freeze: `question_class` on escalations (G6)** (the graduation report groups by it) and **`prompt_intent`/`prompt_done` event kinds (G5)** — `herdr.Prompt` is the most frequent side effect and is still fire-and-hope; embed an intent ULID in the prompt text so the normalizer proves delivery.
5. **Mark the 7 superseded task texts (G4)** with a "SUPERSEDED BY REV 4 §…" line so a subagent doesn't build the stale versions: Task 20 default tree (net-fetch/spawn ✓→off), Task 22 (day-1 auto-answer→shadow), Task 24 (hindsight+auto-apply whitelist→24a manual), Task 21 (flips `active/compacted` on events→immutable+transcript table), Task 4 (`INSERT OR IGNORE`→`ON CONFLICT`), Key Interfaces (`chan Decision`→split `AnswerQuestion`/`DecideConfirm`; old `Spawn` order→reordered).
6. **One sentence in the Store contract (G8):** `WithTx` is synchronous; reconcile-enqueue / SSE-broadcast / watcher-wake fire strictly **after** commit; no reentrancy.

Recommended same pass (cheap): realpath-canonicalize the fs hook + worktrees off any path to `~/.castellan` (G7); fusion ignores event-carried `herdr_state` older than the worker high-water mark (G9); `[castellan-auto]` provenance marker on machine answers (G10); enumerate magic-number config defaults — stall N, SweepInterval, MaxSpawns/MaxBrainCalls (PE-S3); add symmetric **demotion** rules to `castellan autonomy promote` (one human-override→draft, one danger-miss→shadow). Also refresh the overview doc to rev 4 (still shows rev-3 day-1-autonomy + `Interruption` StepResult).

---

## Revisions from review (rev 2)
1. **Idempotent, crash-safe event intake** — herdr sends a stable `source_event_id`; intake dedups on admit. Push is now an *optimization over* an authoritative **periodic reconcile sweep** that repairs the ledger from process-liveness + git HEAD even when POSTs are dropped. *(New Task 7; Tasks 4, 6 changed.)*
2. **Persist brain-call INTENT before invoking** + a **malformed-output / error / billing ladder** (re-prompt → fallback model → record `empty_response` & alert; never crash-loop; no hard `max_tokens` on the StepResult call; **no retry on a billing wall**). *(Task 12 reordered + expanded.)*
3. **Execution concurrency**: decisions serialize **per-worker**, run **concurrently across workers**, and brain calls run **off** the single reconcile/write path. Spawn throttle + decorrelated-jitter backoff for provider 429s. *(New Task 13; Global Constraints.)*
4. **Event log fixed**: append-only with **two clocks** `occurred_at` (event time) + `recorded_at` (wall clock we learned it) — no `valid_until` invalidation on an append-only log. *(Schema; Task 4.)*
5. **Log-normalization layer**: one `NormalizedEntry` stream per agent type (claude vs qwen) feeding fusion, instead of parsing raw transcript tails. *(New Task 9; Task 8 consumes it.)*
6. **Pause/resume + worktree locking** to park idle workers and reclaim PTYs/worktrees at scale. *(New Task 17.)*
7. **Cost discipline** for the short-lived bet: default the brain to a *cheap* model; record `cache_read`/`cache_write` tokens so we can see if reassembled prefixes hit cache. *(Global Constraints; Task 11.)*
8. Escalations (questions/confirms) wake via **channel/event**, not DB polling *(Task 15)*. Diff/review pipeline gates `completed_candidate→completed_verified` *(Task 16)*. Daemon restart = **survive-and-reconcile** + crash-loop breaker *(Task 18)*. SQLite→Postgres/queue migration boundary called out.

## Global Constraints
- **Go 1.22+** (`net/http.ServeMux` method+path routing).
- **Pure-Go SQLite only** (`modernc.org/sqlite`) — cgo-free, cross-compiles to the VMs.
- **SQLite WAL, single *writer* goroutine.** All writes serialize through one queue. Readers use WAL concurrent reads. **This is a single-host mitigation** — see "Scale boundary" below; do not treat SQLite as multi-host coordination.
- **Execution model:** the write/reconcile path is single-threaded and fast; **brain calls and worker spawns run in worker goroutines OFF that path.** Decisions for one worker serialize (per-worker mutex/queue); different workers proceed concurrently. A global semaphore caps concurrent spawns and concurrent brain calls.
- **CLI-first, headless-complete**: daemon + `castellan` CLI fully functional with **zero optional deps active** (no Telegram/Web).
- **All critical state durable** in SQLite; in-memory structures are caches rebuilt from the ledger on boot.
- **Worker IDs are generated ULIDs**, never titles.
- **A worker's output is never applied directly** — classified → reconciled through the state machine → persisted → guarded → then drives the next action.
- **The brain is short-lived and cheap.** Each call starts, decides, exits. **Default brain model = a cheap/fast profile** (not Opus) on the per-event hot path; escalate to a strong model only for explicitly hard decisions. Every brain call records token usage **including `cache_read_tokens`/`cache_write_tokens`**. **Never set a hard `max_tokens` on the StepResult generation** (a reasoning model can burn the cap on thinking and truncate the JSON). **Never retry into a billing-wall / balance error.**
- **Idempotency:** every externally-sourced event carries a stable ID; intake is dedup-on-admit. Every side-effecting action (spawn/dispatch/kill) records **intent → execute → typed result** so replay after a crash can distinguish "decided" from "done."
- **Scale boundary (decide before P3 hardens the schema):** SQLite+WAL is the P2 single-host store. Cross-VM (P3) moves the write path behind an interface (`Store`) so it can be backed by Postgres (or a small queue) without touching call sites. Keep large transcripts/logs **as files on disk**, never as SQLite blobs.
- Paths: config `~/.castellan/config.toml`; DB `~/.castellan/castellan.db`; socket `~/.castellan/castellan.sock`; worker logs `~/.castellan/logs/<worker_id>.jsonl`; user memory `~/.castellan/memory/{USER.md,MEMORY.md,<topic>.md}`; compiled worker permission configs staged under the worktree (`.claude/settings.json` + hook script).
- **Sessions are the unit of work + conversation** (rev 3). Every worker has an `owner_session`; routing, context, cost, escalations, and the Telegram topic all key off the session. A session is a *data grouping + context scope*, **not** an active process — it never becomes a resident orchestrator (that would reintroduce the banned long-lived-LLM model). The daemon still owns routing; the reconciler still serializes per-worker; the brain is still short-lived per-event.
- **Autonomy-first, not approval-gated** (rev 3). Workers act on **routine** actions with no gate. Only two things interrupt: a **stuck/ambiguous** worker asks a *question*, and a **danger/irreversible** action needs a *confirm*. Questions are answered **two-level**: castellan's brain answers from session context + user memory + playbooks FIRST; it escalates to the human only when it is itself uncertain (confidence below a threshold, or conflicting signals) **or** the action is danger-class. A brain-answered question is logged as an event with its rationale so a wrong auto-answer is auditable and reversible.
- **Capability tree per session, compiled onto the worker** (rev 3). Default capabilities = "anything inside its own worktree + open/update PRs; NOT merge, NOT main/shared, NOT deploy/spend/send/read-secrets." The operator widens a session with grants. Enforcement is defense-in-depth: (1) at spawn the session tree compiles to the worker's Claude Code config (`permissions.{allow,deny,ask}` + `PreToolUse` hook + `--allowedTools/--disallowedTools`) so the worker self-limits; (2) **castellan re-checks at its own action boundary and is authoritative** — worker-side hooks have documented bypass gaps. **High-blast capabilities are never compiled into the worker**; castellan performs those itself only after a confirm, so a jailbroken/obfuscated worker command cannot reach them. Inheritance is narrow-only: `worker ⊆ session ⊆ global-default+grants`; a child can never widen a parent. The blob is versioned; every grant/revoke is an append-only event.

---

## File Structure

```
castellan/
  go.mod  go.sum
  cmd/castellan/main.go
  internal/
    config/config.go
    ledger/
      schema.sql   db.go(Store iface + sqlite impl, single-writer queue)
      models.go    workers.go   events.go(idempotent append + two-clock)
      sessions.go(session CRUD + lifecycle + owner_session)   permissions.go(capability tree + versioned blob + grant/revoke events)
      escalations.go(was approvals.go: questions + confirms, channel-wakeup)   usage.go   playbooks.go
    intake/
      http.go(POST /v1/events, dedup-on-admit)   hook.go(herdr manifest w/ stable id)
    reconcile/
      sweep.go(periodic authoritative repair: liveness+HEAD)
      machine.go(event → per-worker serialized decision)
      exec.go(per-worker queues + global spawn/brain semaphores + jitter backoff)
    fusion/state.go(Resolve over NormalizedEntry + signals)
    normalize/normalize.go(NormalizedEntry; per-agent normalizers: claude.go, qwen.go)
    worker/
      herdr.go   git.go(worktree + per-path lock + before/after HEAD + diff)
      spawn.go   lifecycle.go(pause/resume/kill/restart)
    brain/
      step.go(StepResult + Parse/Validate)   context.go(assemble: core+user-mem+session-transcript+recitation+refs; byte-stable)
      transcript.go(per-session brain-msg log: active/compacted soft-archive + checkpoint/fold)
      answer.go(two-level question resolver: brain-answer vs escalate-to-human)
      invoke.go(short-lived clavis call; intent-first; error/billing ladder; opportunistic cache-control)
    permcompile/compile.go(session tree → worker .claude/settings.json + PreToolUse hook + allow/deny flags)
    memory/ store.go(USER.md/MEMORY.md/topic files + FTS5 recall)   hindsight.go(off-hot-path extraction → typed diffs)   curator.go(ACTIVE→STALE→ARCHIVED aging)
    api/ server.go  handlers.go  sse.go
    client/client.go
    cli/ root.go daemon.go status.go workers.go dispatch.go session.go grant.go answer.go logs.go pause.go hook.go
    telegram/bot.go topics.go       # OPTIONAL add-on: forum-topic-per-session (sub-plan)
    web/server.go web/templates/*   # OPTIONAL add-on (sub-plan)
  test/bin/{clavis,herdr}           # fake binaries for tests
  test/integration_test.go
```

---

## Data Model (`internal/ledger/schema.sql`, embedded)

```sql
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;

-- rev 3: SESSIONS are the first-class unit of work + conversation. A session owns
-- a group of workers (possibly across VMs). Subsumes the old `jobs` table.
CREATE TABLE IF NOT EXISTS sessions (
  id              TEXT PRIMARY KEY,              -- ULID
  slug            TEXT UNIQUE,                   -- human handle e.g. 'castellan-p2'
  title           TEXT NOT NULL DEFAULT '',
  goal            TEXT NOT NULL DEFAULT '',      -- standing mandate (was jobs.plan)
  status          TEXT NOT NULL DEFAULT 'open',  -- open|active|waiting|idle|done|archived
  facts           TEXT NOT NULL DEFAULT '',      -- session memory: durable decisions/constraints (md)  (was jobs.facts)
  progress        TEXT NOT NULL DEFAULT '',      -- Goal/Constraints/Progress/Decisions/Next/Critical (was jobs.progress)
  context_summary TEXT NOT NULL DEFAULT '',      -- rolling brain-transcript summary, hard-capped ~2K tokens
  context_rev     INTEGER NOT NULL DEFAULT 0,    -- bumps on every checkpoint (prefix-cache invalidation marker)
  permissions     TEXT NOT NULL DEFAULT '{}',    -- versioned capability tree (JSON); see permissions.go
  perm_rev        INTEGER NOT NULL DEFAULT 0,    -- bumps on every grant/revoke
  repo            TEXT NOT NULL DEFAULT '',      -- affinity hint (not a constraint)
  default_vm      TEXT NOT NULL DEFAULT '',
  pinned          INTEGER NOT NULL DEFAULT 0,
  notify_level    TEXT NOT NULL DEFAULT 'important', -- all|important|silent (Telegram routing)
  tg_topic_id     INTEGER,                       -- Telegram forum topic binding
  tg_status_msg_id INTEGER,                      -- pinned live status-card message id
  stall_count     INTEGER NOT NULL DEFAULT 0,
  last_activity_at TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  closed_at       TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_topic ON sessions(tg_topic_id);

CREATE TABLE IF NOT EXISTS workers (
  id            TEXT PRIMARY KEY,               -- ULID
  title         TEXT NOT NULL DEFAULT '',
  vm            TEXT NOT NULL DEFAULT 'local',
  workspace     TEXT NOT NULL DEFAULT '',       -- herdr pane/workspace target
  worktree      TEXT NOT NULL DEFAULT '',
  base_commit   TEXT NOT NULL DEFAULT '',
  head_commit   TEXT NOT NULL DEFAULT '',       -- last observed HEAD (for diff/verify)
  program       TEXT NOT NULL DEFAULT '',       -- clavis profile + args used to launch
  agent_kind    TEXT NOT NULL DEFAULT '',       -- claude|qwen|... selects the normalizer
  pid           INTEGER,
  state         TEXT NOT NULL,                  -- WorkerState enum
  stall_count   INTEGER NOT NULL DEFAULT 0,
  owner_session TEXT REFERENCES sessions(id),   -- rev 3: was owner_job
  permissions   TEXT NOT NULL DEFAULT '{}',     -- rev 3: effective (narrowed) tree compiled onto this worker
  summary       TEXT NOT NULL DEFAULT '',
  last_seen_at  TEXT NOT NULL,                  -- last reconcile/observe (liveness)
  last_event_at TEXT NOT NULL,
  created_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workers_state ON workers(state);
CREATE INDEX IF NOT EXISTS idx_workers_vm ON workers(vm);
CREATE INDEX IF NOT EXISTS idx_workers_session ON workers(owner_session);

-- append-only event log. NEVER updated/deleted. Two clocks.
CREATE TABLE IF NOT EXISTS events (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,   -- monotonic SSE cursor
  source_event_id TEXT UNIQUE,                         -- stable id from origin (herdr/cli); NULL for internal
  worker_id       TEXT REFERENCES workers(id),
  session_id      TEXT REFERENCES sessions(id),        -- rev 3: was job_id
  kind            TEXT NOT NULL,   -- state_change|dispatch_intent|dispatch_done|brain_intent|brain_decision|
                                   -- brain_msg(session transcript turn)|chat_in|chat_out|
                                   -- question_req|question_ans(brain)|question_esc(human)|confirm_req|confirm_dec|
                                   -- grant|revoke|session_open|session_close|memory_diff|
                                   -- kill_intent|kill_done|reconcile|error|note
  payload         TEXT NOT NULL DEFAULT '{}',
  active          INTEGER NOT NULL DEFAULT 1,   -- rev 3: brain_msg soft-archive (1=live in transcript)
  compacted       INTEGER NOT NULL DEFAULT 0,   -- rev 3: 1=folded into context_summary (still FTS-searchable, never deleted)
  occurred_at     TEXT NOT NULL,   -- when the fact became true (event time; may be < recorded_at)
  recorded_at     TEXT NOT NULL    -- wall clock we learned it
);
CREATE INDEX IF NOT EXISTS idx_events_worker ON events(worker_id, id);
CREATE INDEX IF NOT EXISTS idx_events_session ON events(session_id, id);
-- brain transcript retrieval = events WHERE session_id=? AND kind='brain_msg' AND active=1
-- FTS5 virtual table over events.payload for archived-event recall (memory_search tool).
-- dedup-on-admit: INSERT OR IGNORE on source_event_id makes re-delivery a no-op.
-- NOTE (rev 3): the old `jobs` table is removed; its fields live on `sessions` (goal/facts/progress/stall_count).

-- rev 3: was `approvals`. Autonomy-first: this is NOT a per-action gate. Rows are created
-- only for a stuck worker's QUESTION or a danger-class CONFIRM (see Global Constraints).
CREATE TABLE IF NOT EXISTS escalations (
  id           TEXT PRIMARY KEY,
  worker_id    TEXT REFERENCES workers(id),
  session_id   TEXT REFERENCES sessions(id),
  kind         TEXT NOT NULL DEFAULT 'question',  -- question | confirm
  action_class TEXT NOT NULL DEFAULT 'ambiguous', -- routine(never a row) | ambiguous | danger
  tier         TEXT NOT NULL DEFAULT 'medium',    -- low | medium | high_blast (gates 'always')
  action       TEXT NOT NULL,
  detail       TEXT NOT NULL DEFAULT '{}',
  answered_by  TEXT NOT NULL DEFAULT '',          -- '' | 'brain' | 'human'
  brain_rationale TEXT NOT NULL DEFAULT '',       -- audit: why the brain auto-answered
  status       TEXT NOT NULL DEFAULT 'pending',   -- pending|answered|approved|rejected
  once_or_always TEXT NOT NULL DEFAULT 'once',    -- once|always → promotes to a standing session grant
  requested_at TEXT NOT NULL,
  decided_at   TEXT
);
CREATE INDEX IF NOT EXISTS idx_escalations_status ON escalations(status);

CREATE TABLE IF NOT EXISTS usage (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  worker_id    TEXT,
  session_id   TEXT,                               -- rev 3: cost rollup per session
  model        TEXT NOT NULL,
  input_tok    INTEGER NOT NULL DEFAULT 0,
  output_tok   INTEGER NOT NULL DEFAULT 0,
  cache_read   INTEGER NOT NULL DEFAULT 0,   -- did our reassembled prefix hit cache?
  cache_write  INTEGER NOT NULL DEFAULT 0,
  cost_usd     REAL NOT NULL DEFAULT 0,
  at           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS playbooks (       -- P3 learning loop
  id TEXT PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'active', pinned INTEGER NOT NULL DEFAULT 0,
  use_count INTEGER NOT NULL DEFAULT 0, last_used_at TEXT, created_at TEXT NOT NULL
);
```

**WorkerState** (`models.go`): `starting, running, waiting_for_user, waiting_for_confirmation, blocked, completed_candidate, completed_verified, failed, paused, killed`. Legal transitions enforced in `workers.go`. **Rev 3:** the broad `waiting_for_approval` is dropped. `waiting_for_user` = the worker asked a question that reached the human (most escalations); `waiting_for_confirmation` = a narrow danger-class confirm is pending. A question that castellan's own brain answers never enters a waiting state — the worker keeps running.

**SessionStatus** (`models.go`, rev 3): `open, active, waiting, idle, done, archived`. `active` = has running workers; `waiting` = blocked on a question/confirm or user input; `idle` = all workers terminal, goal unverified; `done` = verified; `archived` = closed by curator (Telegram topic closed, not deleted).

**ActionClass / Capability** (`models.go`, rev 3): `ActionClass ∈ {routine, ambiguous, danger}`; capability tree is a nested map compiled in `permissions.go`. Default-on vs default-off and the high-blast set are defined in Task 20.

---

## Key Interfaces (locked)

> ⚠ **SUPERSEDED BY REV 4.1 (read the REV 4 section at the top first):** `OpenEscalation` returns immediately — no blocking `chan Decision`; a decision re-submits a resume job (watcher-only channel). Split the decision API into **`AnswerQuestion(id, text, scope)`** and **`DecideConfirm(id, yesNo, scope)`** — drop the shared `Decision`. `action_class`/`tier`/`capability` are computed **structurally** from `capability_catalog` (never caller- or brain-supplied). `Spawn` order = pre-gen ULID + `workspace=castellan_<ulid>` → write `dispatch_intent` + `CreateWorker` (with a **scrubbed `cmd.Env` allowlist**) **before** any external side effect → adopt-or-abort recovery (no trailing `AttachWorker`). `AppendEvent` uses `ON CONFLICT(source, source_event_id) DO NOTHING` (not `INSERT OR IGNORE`).

```go
// ledger — Store is an interface so P3 can swap SQLite for Postgres without touching callers.
type Store interface {
    WithWrite(func(*sql.Tx) error) error              // serialized single-writer
    DB() *sql.DB                                       // reads
}
func Open(path string) (*Ledger, error)
func (l *Ledger) CreateWorker(w Worker) error
func (l *Ledger) GetWorker(id string) (Worker, error)
func (l *Ledger) ListWorkers(f WorkerFilter) ([]Worker, error)
func (l *Ledger) TransitionWorker(id string, to WorkerState, e Event) error
func (l *Ledger) AppendEvent(e Event) (cursor int64, deduped bool, err error)  // INSERT OR IGNORE on source_event_id
func (l *Ledger) EventsSince(cursor int64) ([]Event, error)

// sessions (rev 3)
func (l *Ledger) CreateSession(s Session) error
func (l *Ledger) GetSession(id string) (Session, error)
func (l *Ledger) ResolveSession(ref string) (Session, error)          // slug | id | tg_topic_id
func (l *Ledger) ListSessions(f SessionFilter) ([]Session, error)
func (l *Ledger) SetSessionStatus(id string, to SessionStatus, e Event) error
func (l *Ledger) AttachWorker(sessionID, workerID string) error       // sets owner_session

// permissions (rev 3) — versioned capability tree; narrow-only inheritance
type Capability string  // "git.pr.merge", "external.deploy", ...
type Tree map[Capability]bool
func DefaultTree() Tree                                               // the ✓ set
func HighBlast(c Capability) bool                                     // push.main|deploy|spend|destructive-delete
func (t Tree) Narrow(child Tree) (Tree, error)                        // err if child widens (worker ⊆ session)
func (l *Ledger) Grant(sessionID string, c Capability, tier string, e Event) error   // bumps perm_rev
func (l *Ledger) Revoke(sessionID string, c Capability, e Event) error
func (l *Ledger) Allowed(sessionID string, c Capability) (bool, error)  // authoritative boundary check (layer 2)

// escalations (rev 3, was approvals) — channel-wakeup, autonomy-first
type Escalation struct { ID, WorkerID, SessionID, Kind, ActionClass, Tier, Action string; OnceOrAlways string }
func (l *Ledger) OpenEscalation(e Escalation) (chan Decision, error)  // question|confirm; subscriber wakes on decision
func (l *Ledger) DecideEscalation(id string, d Decision) error        // 'always' + non-high-blast → Grant standing

// brain (extended, rev 3)
type StepResult struct { Kind, Worker, Instruction, Reason string } // Kind: run_again|handoff|dispatch|final_output|question|confirm
func AssembleContext(l *Ledger, s Session, w Worker) (string, error) // core + USER.md + MEMORY.md-index + session transcript(active) + recitation + refs; byte-stable
func Checkpoint(l *Ledger, s Session) error                          // fold active brain_msg rows → context_summary; soft-archive; bump context_rev
func AnswerQuestion(deps Deps, esc Escalation) (Decision, bool)      // two-level: brain answers from ctx+memory+playbooks; bool=escalate-to-human

// memory (rev 3) — 4-tier
func LoadUserMemory() (userMD string, indexMD string, err error)     // whole-file T1 + T2 index (always-hot)
func MemoryRead(topic string) (string, error)                        // T2 JIT tool
func MemorySearch(q string) ([]Hit, error)                           // FTS5 over topic files + archived events
type MemoryDiff struct { Op string; Text string; Supersedes string; ObservedAt time.Time } // add|update|expire
func Hindsight(deps Deps, s Session) ([]MemoryDiff, error)           // off-hot-path; proposals become escalations
func Curate(deps Deps) error                                         // ACTIVE→STALE→ARCHIVED aging of topic facts

// permcompile (rev 3)
func Compile(worktree string, effective Tree) error                  // writes .claude/settings.json + PreToolUse hook
func Flags(effective Tree) (allowed, disallowed []string)            // --allowedTools/--disallowedTools

// normalize
type NormalizedEntry struct { Kind string; Text string; Tool string; At time.Time } // Kind: assistant|tool_call|tool_result|prompt_wait|error
type Normalizer interface { Parse(raw string) []NormalizedEntry; State(entries []NormalizedEntry) (herdrState string, waitingInput bool) } // rev 3: waitingInput = worker is prompting for human input (question/confirm), not the old "approval"
func For(agentKind string) Normalizer

// fusion
type Signals struct { Norm []NormalizedEntry; HerdrState string; Alive bool; HeadChanged bool }
func Resolve(w Worker, s Signals) (state WorkerState, ambiguous bool)

// brain (rev 3: StepResult + AssembleContext updated below in the rev-3 block; `interruption` → `question`/`confirm`)
func ParseStep(raw string) (StepResult, error)
type InvokeResult struct { Step StepResult; Usage Usage; Err error; Malformed bool; Billing bool }
func Invoke(ctx context.Context, cfg BrainCfg, prompt string) InvokeResult   // intent already persisted by caller

// reconcile
func Sweep(deps Deps) error                            // authoritative: for each live worker, observe liveness+HEAD+norm → Resolve → repair
func Reconcile(deps Deps, e Event) error               // event-driven fast path
type Exec struct{ ... }                                // per-worker queues + global semaphores + jitter backoff
func (x *Exec) Submit(workerID string, fn func())      // serialize per worker, parallel across workers

// worker
func Spawn(deps Deps, sessionID, clavisProfile, task string) (Worker, error)   // rev 3: narrows session tree → permcompile.Compile onto worktree → records dispatch_intent → spawn → dispatch_done → AttachWorker
func (h Herdr) List() ([]HerdrAgent, error)
func (h Herdr) Prompt(target, text string) error
func Pause(deps Deps, id string) error                 // commit dirty → detach → remove worktree → state=paused
func Resume(deps Deps, id string) error
```

---

## Tasks

> Tasks 1–18 are the **P2 walking skeleton, hardened**. Optional add-ons (Telegram, Web, scheduler, playbooks, cross-VM/P3) are scoped at the end; each gets its own granular sub-plan.

### Task 1: Module scaffold + config + CLI root
**Files:** `go.mod`, `cmd/castellan/main.go`, `internal/cli/{root,daemon}.go`, `internal/config/config.go(+_test)`
- [ ] **S1 failing test** `config_test.go`: load a temp `config.toml` (`clavis_profile="deepseek-1"`, `brain_profile="deepseek-1"`) → fields set; omitted `socket`/`brain_model` get defaults (`brain_model` defaults to a **cheap** tier, not opus).
- [ ] **S2** `go test ./internal/config/` → FAIL.
- [ ] **S3** implement `Config{DBPath,Socket,TCPAddr,ClavisProfile,BrainProfile,BrainModel,HerdrBin,MaxSpawns,MaxBrainCalls,SweepInterval, Telegram{Enabled,Token}, Web{Enabled,Addr}}`; `Load(path)` TOML+env `CASTELLAN_*`+defaults.
- [ ] **S4** `go test` → PASS.
- [ ] **S5** `main.go`→`cli.Execute()`; cobra root + `version` + `daemon` stub.
- [ ] **S6 commit** `feat: scaffold + config (cheap brain default)`

### Task 2: Ledger `Store` (WAL, single-writer queue) + migrations
**Files:** `ledger/schema.sql`, `db.go(+_test)`
- [ ] **S1 test**: `Open` temp DB → WAL on, `workers`/`events` exist; 50 concurrent `WithWrite` increments → exactly 50 (serialized).
- [ ] **S2** FAIL. **S3** implement `modernc.org/sqlite`, `//go:embed schema.sql`, one writer goroutine draining a channel; `Store` interface. **S4** PASS.
- [ ] **S5 commit** `feat: ledger Store, WAL, serialized writer`

### Task 3: Worker model + CRUD + guarded transitions
**Files:** `models.go`, `workers.go(+_test)`
- [ ] **S1 test**: create `starting`; `TransitionWorker → running` ok; illegal `killed→running` → `ErrIllegalTransition`; an event row appears.
- [ ] **S2–4** implement enum + `legalTransitions` + `CreateWorker/GetWorker/ListWorkers` + `TransitionWorker` (tx: guard→update→append event). PASS.
- [ ] **S5 commit** `feat: worker CRUD + guarded transitions`

### Task 4: Event log — idempotent append + two clocks + cursor
> ⚠ **SUPERSEDED BY REV 4.1:** use **`ON CONFLICT(source, source_event_id) DO NOTHING`**, NOT blanket `INSERT OR IGNORE` (which would hide `CHECK`/`NOT NULL` violations as false dedups). Identity is namespaced `UNIQUE(source, source_event_id)`; store `source_event_hash` and on a same-id-different-hash arrival record an `error` event instead of silently deduping. `EventsSince(cursor, limit)` is bounded. `events` is **immutable** — no `active/compacted` columns (those live on `brain_transcript_rows`, Task 21).
**Files:** `events.go(+_test)`
- [ ] **S1 tests**: append 3 → `EventsSince(0)` returns 3 ascending; **re-appending the same `source_event_id` returns `deduped=true` and does NOT add a row**; `occurred_at` may precede `recorded_at` and both round-trip. *(No `valid_until` — append-only.)*
- [ ] **S2–4** implement `AppendEvent` as `INSERT OR IGNORE`; detect dedup via `RowsAffected`. PASS.
- [ ] **S5 commit** `feat: idempotent two-clock append-only event log`

### Task 5: API server (unix socket) + healthz + client
**Files:** `api/server.go(+_test)`, `client/client.go`
- [ ] **S1 test**: server on temp socket; `client.Health()` ok.
- [ ] **S2–4** `net.Listen("unix",…)` + ServeMux + `GET /healthz`; client over unix transport. PASS.
- [ ] **S5 commit** `feat: API over unix socket + client`

### Task 6: Idempotent event intake (`POST /v1/events`)
**Files:** `intake/http.go(+_test)`, `api/handlers.go`
**Interfaces:** consumes `AppendEvent` (dedup). Produces the herdr-hook target.
- [ ] **S1 tests**: POST `{source_event_id, worker_ref, herdr_state, occurred_at, transcript_ref}` → event appended, `last_event_at` updated, a Reconcile enqueued; **re-POST same `source_event_id` → 200 + `deduped:true`, no second event, no second reconcile**; unknown worker → 202 + `note` event.
- [ ] **S2–4** implement: parse → `AppendEvent` → if `deduped` return early → else enqueue `Reconcile`. Return JSON `{deduped}`.
- [ ] **S5 commit** `feat: idempotent event intake endpoint`

### Task 7: Authoritative reconcile sweep (steady-state repair)
**Files:** `reconcile/sweep.go(+_test)`
**Interfaces:** `Sweep(deps)`. This is the *truth*; push is an optimization over it.
- [ ] **S1 test**: seed 2 workers — one whose fake process is dead + HEAD changed, one alive; run `Sweep`; dead+changed → `completed_candidate` (or `failed` if no HEAD change), alive stays; a `reconcile` event is recorded; runs even though **no** intake event arrived.
- [ ] **S2–4** implement: for each non-terminal worker, gather `Signals` (process liveness via PID, `git rev-parse HEAD` vs `head_commit`, normalized transcript tail) → `fusion.Resolve` → `TransitionWorker` if changed; update `last_seen_at`. Wire a ticker at `cfg.SweepInterval` in `daemon.go`.
- [ ] **S5 commit** `feat: authoritative periodic reconcile sweep`

### Task 8: State fusion resolver
**Files:** `fusion/state.go(+_test)`
- [ ] **S1 table tests**: herdr=`blocked`+alive→`blocked`; alive+HeadChanged+no input marker→`completed_candidate`; norm shows an input/question prompt→`waiting_for_user` (or `waiting_for_confirmation` for a danger-class marker); dead+no HEAD change→`failed`; conflicting→`ambiguous=true`.
- [ ] **S2–4** implement precedence: explicit input/wait markers (from `NormalizedEntry`) > herdr hook state > liveness > HEAD; `ambiguous` when signals disagree/unknown.
- [ ] **S5 commit** `feat: multi-signal fusion resolver`

### Task 9: Log-normalization layer
**Files:** `normalize/normalize.go`, `normalize/claude.go`, `normalize/qwen.go(+_test)`
**Interfaces:** `NormalizedEntry`, `Normalizer`, `For(agentKind)`.
- [ ] **S1 tests**: a claude transcript sample → entries incl. a `prompt_wait` when it's awaiting input; a qwen sample (different shape) → equivalent entries via its own normalizer; `For("claude")` vs `For("qwen")` pick the right one.
- [ ] **S2–4** implement per-agent parsers producing a common stream; `State()` derives `(herdrState, waitingInput)`. Fusion (Task 8) consumes `NormalizedEntry`, never raw tails.
- [ ] **S5 commit** `feat: per-agent log normalization feeding fusion`

### Task 10: herdr wrapper + git (worktree + per-path lock + diff)
**Files:** `worker/herdr.go`, `worker/git.go(+_test)`
- [ ] **S1 tests** (temp git repo, fake `herdr` on PATH): `git.AddWorktree` under a **per-path lock** (concurrent adds to same repo serialize); records `base_commit`; `Diff(base,head)` returns numstat; `herdr.List/Prompt` shell out correctly.
- [ ] **S2–4** implement `Mutex`-per-repo-path map; worktree add/remove; HEAD read; diff (full for one, `--numstat` bulk). PASS.
- [ ] **S5 commit** `feat: herdr wrapper + locked worktrees + diff`

### Task 11: Brain — StepResult + context + short-lived invoke (cost-aware)
**Files:** `brain/step.go`, `context.go`, `invoke.go(+_test)`
- [ ] **S1 tests**: `ParseStep` extracts from fenced JSON, errors on garbage (→ `Malformed`); `AssembleContext` includes core block + worker-roster recitation + reference IDs (not payloads), under a char cap; `Invoke` (fake `clavis` echoing canned StepResult) returns parsed step + usage **including cache_read/write**; a fake that emits a balance error → `InvokeResult.Billing=true`; **no `--max-tokens` flag is passed** on the call (assert argv).
- [ ] **S2–4** implement; `Invoke` runs `clavis <brain_profile> -- --model <cfg.BrainModel> -p <prompt>` with a `context.Context` timeout, captures stdout, parses; classify billing/malformed. Records a `usage` row.
- [ ] **S5 commit** `feat: cost-aware short-lived brain invoker`

### Task 12: Reconciler — decision path (intent-first + error ladder)
**Files:** `reconcile/machine.go(+_test)`
**Interfaces:** `Reconcile(deps, e)`. Runs the decision via `Exec` (Task 13), off the write path.
- [ ] **S1 tests**:
  - unambiguous fusion → `TransitionWorker`, **no brain call** (mock asserts not called);
  - ambiguous → **`brain_intent` event (assembled prompt + intended action) is persisted BEFORE `Invoke` is called** (assert ordering via a hook), then `brain_decision` after;
  - `Malformed` → re-prompt once → still bad → fallback model → still bad → record `error`/`empty_response` + alert, worker parked `blocked` (never crash-loop);
  - `Billing` → **no retry**, worker parked `blocked`, alert;
  - `question` step → **Task 22 two-level resolve first**: if brain auto-answers, log `question_ans` + rationale and `herdr.Prompt` the answer (worker never waits); else open a `question` escalation + `waiting_for_user`;
  - `confirm` step (danger-class) → open a `confirm` escalation + `waiting_for_confirmation` (never auto-answered);
  - `dispatch`/`run_again` → `herdr.Prompt`;
  - stall_count increments; after N stalls a replan brain call fires.
- [ ] **S2–4** implement the ladder + intent-first ordering; per-worker serialize via `Exec.Submit(worker.ID, …)`.
- [ ] **S5 commit** `feat: reconciler decision path (intent-first, error/billing ladder)`

### Task 13: Execution engine (per-worker serialize / cross-worker parallel / throttle / backoff)
**Files:** `reconcile/exec.go(+_test)`
- [ ] **S1 tests**: two `Submit`s for the **same** worker run strictly in order; `Submit`s for **different** workers run concurrently (assert overlap); a global semaphore caps concurrent funcs at `cfg.MaxBrainCalls`/`MaxSpawns`; `Backoff(attempt)` yields decorrelated jitter (bounded, non-constant).
- [ ] **S2–4** implement per-worker FIFO goroutine (map of chan), global weighted semaphore, jitter backoff helper used by `Invoke`/`Spawn` on 429/5xx.
- [ ] **S5 commit** `feat: per-worker serialized, cross-worker parallel executor + jitter backoff`

### Task 14: Worker spawn (intent→execute→result) + CLI dispatch + integration
**Files:** `worker/spawn.go(+_test)`, `cli/{dispatch,workers,status,logs}.go`, `test/integration_test.go`
- [ ] **S1 unit test** (fake clavis/herdr): `Spawn` appends **`dispatch_intent`** → creates worktree → launches clavis → `CreateWorker(starting)` → **`dispatch_done`**; a crash between intent and done leaves a recoverable `dispatch_intent` with no `dispatch_done`.
- [ ] **S1 integration test**: start daemon (temp DB/socket, fake bins, **no Telegram/Web**); `castellan dispatch "do X"` → worker created; POST a herdr hook (idle+HEAD change) → worker reaches `completed_candidate`; `castellan workers` lists it; re-POST same event id → no change.
- [ ] **S2–4** implement spawn + CLI over `client`; wire `daemon.go` (ledger+api+intake+Exec+sweep ticker).
- [ ] **S5 commit** `feat: crash-safe spawn + CLI walking skeleton`

### Task 15: Escalations (channel-wakeup, not polling) + `castellan answer` — autonomy-first (rev 3)
**Files:** `ledger/escalations.go` (was `approvals.go`), `cli/answer.go`, `api/handlers.go(+_test)`
> Reframed rev 3: this is NOT a per-action approval gate. A row exists only for a stuck worker's **question** or a **danger-class confirm**. Routine actions never create a row. The two-level resolver (Task 22) tries to brain-answer a question *before* a human row is ever opened.
- [ ] **S1 tests**: `OpenEscalation(kind=question)` → subscriber on an in-memory **notify channel** wakes on decision (assert no busy-poll); `POST /v1/answer {id,decision,scope}` sets status+`decided_at`, appends `question_esc`/`confirm_dec` event, transitions worker out of `waiting_for_user`/`waiting_for_confirmation`; **`scope=always` on a non-high-blast tier promotes to a standing session `Grant`; `scope=always` on a `high_blast` tier is REJECTED at the API (must use `castellan grant`)**; a `confirm` for a danger action requires an explicit decision (never auto).
- [ ] **S2–5** implement escalation registry with per-row channels; tier guard on `always`; CLI `answer <id> <yes|no> [--always]`. commit `feat: autonomy-first escalations (questions+confirms) via channel wakeup`.

### Task 16: Diff/review gate (`completed_candidate → completed_verified`)
**Files:** `worker/git.go` (extend), `cli/workers.go` (show diff), `api/handlers.go(+_test)`
- [ ] **S1 tests**: a `completed_candidate` worker exposes a diff (base→head) via `GET /v1/workers/{id}/diff`; `castellan verify <id>` (or an auto-verifier hook) transitions to `completed_verified`; without verification it stays `candidate`.
- [ ] **S2–5** implement diff endpoint + verify transition. commit `feat: diff-gated completion`.

### Task 17: Pause/resume + reclaim
**Files:** `worker/lifecycle.go(+_test)`, `cli/pause.go`
- [ ] **S1 tests**: `Pause` on a running worker commits dirty changes to its branch, detaches/kills the process, removes the worktree (under the per-path lock), state→`paused`; `Resume` refuses if the branch is checked out elsewhere, else re-creates the worktree + relaunches, state→`running`.
- [ ] **S2–5** implement. commit `feat: pause/resume to reclaim worktrees/PTYs`.

### Task 18: Boot recovery + survive-and-reconcile + crash-loop breaker
**Files:** `cli/daemon.go` (extend), `reconcile/sweep.go` (reuse), test
- [ ] **S1 tests**: pre-seed a `running` worker with a **live** PID + a herdr pane → on boot it is **kept** (survive-and-reconcile), and the first `Sweep` re-attaches it; a `running` worker with a **dead** PID → `failed` + recovery event; a `dispatch_intent` with no `dispatch_done` → resolved (re-attach if the process exists, else mark `failed`, never double-spawn); pending escalations (questions/confirms) survive; if the daemon restarts >N times in M minutes, the breaker refuses auto-actions and alerts.
- [ ] **S2–5** implement boot sweep + intent/done reconciliation + a restart-rate breaker (persist boot timestamps). commit `feat: survive-and-reconcile boot recovery + crash-loop breaker`.

### Task 19: SSE stream (`GET /v1/stream?since=`) with pre-side-effect dedup
**Files:** `api/sse.go(+_test)`
- [ ] Broker replays events after `since` then live-tails; **dedup by event id happens before any side effect**, not just in the UI. TDD + commit `feat: replayable SSE stream`.

---

## Rev 3 tasks (sessions, context, memory, autonomy, permissions, Telegram)

> **Ordering for a fresh build:** Task 20 (sessions + permission tree) belongs conceptually right after Task 3 (workers) since `owner_session` and the compiled tree are referenced by Spawn (Task 14). Tasks 21–24 layer onto the brain/reconciler (Tasks 11–12). Task 26 is an optional client like Telegram. Numbered 20+ to preserve rev-2 task identity for in-flight executors.

### Task 20: Sessions + capability tree (schema is foundational; do before Task 14 in a fresh build)
> ⚠ **SUPERSEDED BY REV 4.1 (decision #2 + precond #6):** `DefaultTree()` has **`net-fetch` and `spawn-subworker` OFF by default** (NOT in the ✓ set). Capabilities are **rows** — `capability_catalog` (capability→action_class/tier/high_blast) + `session_grants` (status/scope/expires_at/use_count/created_perm_rev); `sessions.permissions` is only a derived cache. Delegation caps live here: **depth 1, ≤2 children/parent, ≤5 live/session, ≤20 spawns/session/day.** Split implementation: the **catalog + `Allowed()`** land in PASS-1; grants/UI in PASS-3.
**Files:** `ledger/sessions.go`, `ledger/permissions.go(+_test)`, `cli/session.go`, `cli/grant.go`
- [ ] **S1 tests**: `CreateSession` → `open`; `ResolveSession` by slug/id/topic-id; `SetSessionStatus` legal transitions (`open→active→waiting→idle→done→archived`); illegal `done→active` without reopen → error; `AttachWorker` sets `owner_session` + index.
- [ ] **S1 tests (permissions)**: `DefaultTree()` has the ✓ set (branch.create, commit, push.feature, pr.open/update, fs in-worktree, exec tests/build/lint/install/net-fetch, spawn-subworker capped, handoff) and the ✗ set off; `Narrow(child)` errors if child adds a capability the parent lacks (worker ⊄ session); `Grant`/`Revoke` bump `perm_rev` and append `grant`/`revoke` events; `HighBlast()` true for push.main/deploy/spend/destructive-delete; `Allowed()` is the authoritative check.
- [ ] **S2–4** implement CRUD + lifecycle + tree ops; CLI `session new|ls|show|close`, `grant <session> <capability> [--tier]` (the ONLY path to a standing high-blast grant).
- [ ] **S5 commit** `feat: sessions + versioned capability tree (rev 3)`

### Task 21: Per-session brain transcript + context assembly + checkpoint (context-balance policy)
> ⚠ **SUPERSEDED BY REV 4.1 (PASS-0 + decision #5):** `events` is IMMUTABLE — transcript turns live in a separate **`brain_transcript_rows`** table (+ `transcript_fts`); `Checkpoint` flips `active/compacted` **there**, NEVER on `events`. Keep only the cheap byte-stable-assembly invariant + `prompt_hash` + `cache_read/write` telemetry — **no cache-plan/marker engine** until measured cache hits justify it. Memory caps apply (USER.md/MEMORY.md ≤4KiB always-hot; transcript tail bounded).
**Files:** `brain/transcript.go`, `brain/context.go(+_test)`
- [ ] **S1 tests**: appending `brain_msg` rows then `AssembleContext` returns core + `USER.md` + `MEMORY.md` index + active transcript + roster recitation at the tail + reference IDs (not payloads), under a char cap, **byte-identical across two calls with the same ledger state** (assert prefix stability); `Checkpoint` folds active rows into `context_summary` (structured Goal/Constraints/Progress/Decisions/Next/Critical), flips them `active=0, compacted=1` (still present, FTS-searchable — never deleted), caps the summary ~2K tokens, bumps `context_rev`; a checkpoint whose summarizer fails falls back to deterministic anchor extraction (never blocks the session).
- [ ] **S2–4** implement; assembly prepends a REFERENCE-ONLY provenance marker to `context_summary`; opportunistic `cache_control` on the static prefix only when the session had an event in the last hour (recorded via `usage.cache_read/write`; no keepalive pings).
- [ ] **S5 commit** `feat: durable session brain-transcript + byte-stable assembly + checkpoint`

### Task 22: Two-level question resolver (brain-answers-first)
> ⚠ **SUPERSEDED BY REV 4.1 (decision #1):** build the resolver but **DEFAULT to SHADOW/DRAFT for ALL classes in P2** — it drafts an answer + rationale but the human still decides; **no unattended auto-answer ships in P2.** Danger-class/tier are computed **structurally from `capability_catalog`** (NL can only *raise* severity); the **disjointness invariant** holds — a brain answer is advisory text that can NEVER reach `Grant()`/confirm. Add an **escalation timeout → auto-pause** and a **per-session auto-answer budget** (force-escalate after N). Auto-answer is earned later per question-class via `castellan autonomy promote` (≥50 samples, ≥95% agreement, 0 danger-misses) with symmetric demotion (1 human-override→draft, 1 danger-miss→shadow). `proceed-confirmation`/`scope-change` never auto. Machine answers carry a `[castellan-auto]` provenance marker and are FYI-visible even when silent.
**Files:** `brain/answer.go(+_test)`
- [ ] **S1 tests**: `AnswerQuestion` on an ambiguous worker question → brain assembles session context + user memory + relevant playbooks, returns a `Decision` with a confidence + `brain_rationale`; **when confidence ≥ threshold AND action_class≠danger → auto-answer (returns escalate=false)**, logs `question_ans`; when confidence < threshold OR conflicting signals OR danger-class → escalate=true (a human row is opened); a danger-class action is NEVER auto-answered regardless of confidence.
- [ ] **S2–4** implement the resolver; wire it into Task 12's `question` branch; record the rationale so a wrong auto-answer is auditable/reversible.
- [ ] **S5 commit** `feat: two-level autonomy — castellan answers routine worker questions`

### Task 23: Compile session tree → worker Claude Code config (defense-in-depth enforcement)
**Files:** `permcompile/compile.go(+_test)`, `worker/spawn.go` (extend)
> Verified mechanism: `settings.json` `permissions.{allow,deny,ask}` (eval order deny→ask→allow, hooks first, deny wins even in bypass) + `PreToolUse` hook (`permissionDecision:"deny"` / exit 2) + `--allowedTools/--disallowedTools`. Worker-side hooks have documented bypass gaps (claude-code#37210) → **castellan's own `Allowed()` boundary check (Task 20) is authoritative; high-blast capabilities are never compiled onto the worker at all.**
- [ ] **S1 tests**: `Compile(worktree, tree)` writes `.claude/settings.json` with tests/build/lint/install/net-fetch and in-worktree fs in `allow`, out-of-worktree writes + push.main + deploy + spend + read-secrets in `deny`, medium-risk (merge/shared-push) in `ask`; writes a `PreToolUse` hook script that denies out-of-worktree paths + parses `Bash`/`gh` command strings for blocked git subcommands; `Flags(tree)` returns matching `--allowedTools/--disallowedTools`; **a high-blast capability produces a `deny` rule AND is asserted absent from allow (worker cannot reach it even if it tries)**; Spawn narrows session→worker tree and compiles before launch.
- [ ] **S2–4** implement; document that command-string matching is best-effort and the daemon boundary is the real gate.
- [ ] **S5 commit** `feat: compile session capability tree onto worker (settings.json + PreToolUse hook + flags)`

### Task 24: 4-tier memory + hindsight write-back + curator
> ⚠ **SUPERSEDED BY REV 4.1 (decision #3) — SPLIT INTO 24a/24b/24c:** **24a (P2, the only part that ships now)** = manual-only memory: `USER.md`≤4KiB + `MEMORY.md` index≤4KiB always-hot, topic files JIT via `memory_read`≤6K chars, FTS5 recall top-5 ≤500 chars w/ source IDs, **secret redaction at every egress**, per-fact frontmatter provenance (fact_id/state/author/trust/observed_at/valid_from/supersedes/pinned), `mem_rev` + `memory_revisions`. **NO `author=brain` auto-apply, NO verbatim-preference whitelist, NO LLM curator, NO hindsight in P2.** **24b** (hindsight = proposals to a pending queue, zero auto-apply) = post-MVP. **24c** (LLM curator + playbook learning) = P3. Checkpoint must not launder brain answers into summaries (`SourceEvents`+`Tainted`).
**Files:** `memory/store.go`, `memory/hindsight.go`, `memory/curator.go(+_test)`
- [ ] **S1 tests**: `LoadUserMemory` returns whole `USER.md` + `MEMORY.md` index; `MemoryRead(topic)` loads a topic file JIT; `MemorySearch` FTS5 over topic files + archived events; `Hindsight(session)` (fake cheap model) proposes typed `MemoryDiff`s with bi-temporal provenance and **runs off the hot path (after checkpoint/close), never inside a brain decision**; diffs land as `memory_diff` escalations (human-approved by default; verbatim-preference auto-apply whitelist); `Curate` ages topic facts ACTIVE→STALE→ARCHIVED with pinned/referenced protection, never-used≠stale, reactivation-on-recall.
- [ ] **S2–4** implement file store + FTS5 + hindsight extraction + curator; memory writes batch at checkpoint boundaries (bump a `mem_rev`) so they are the one sanctioned prefix-cache break.
- [ ] **S5 commit** `feat: 4-tier memory + off-hot-path hindsight write-back + curator`

### Task 25: Session-aware CLI + cost rollup + dispatch-into-session
**Files:** `cli/dispatch.go` (extend), `cli/status.go` (extend), `cli/session.go`
- [ ] **S1 tests**: `dispatch --new "<goal>"` creates a session + first worker; `dispatch --session <slug> "<task>"` attaches; `status` groups workers under sessions and rolls up `usage` per session; a bare instruction with no session context prompts to pick among candidate sessions (never silently guesses).
- [ ] **S2–5** implement; commit `feat: session-aware dispatch + per-session cost rollup`.

### Task 26: Telegram forum-topic client (optional add-on)
**Files:** `telegram/bot.go`, `telegram/topics.go(+_test)`
> One private forum supergroup; bot admin with `can_manage_topics`. One topic per session (`sessions.tg_topic_id`), General = topic id 1 (messages have no `message_thread_id` — handle it). Group rate limit ~20 msg/min across ALL topics → coalesce.
- [ ] **S1 tests** (fake Bot API): creating a session → `createForumTopic` → stores `tg_topic_id` + a pinned status-card message id; a worker state change edits the card via `editMessageText` (no notify); an escalation posts an inline keyboard `[✅ This once][✅ Always for session][❌ No][👀 Diff]` with `callback_data` ≤64B; **the "Always" button is omitted/disabled for `high_blast` tier**; an incoming message with `message_thread_id=T` resolves to the session bound to T (General → fleet-console commands); a critical escalation is mirrored into General (mute-safe); `notify_level` gates discrete messages.
- [ ] **S2–5** implement SSE→Telegram consumer + topic lifecycle (create on open, `closeForumTopic` on done — never delete); commit `feat: Telegram forum-topic-per-session client`.

### Task 27: Boot recovery for sessions + orphaned compiled configs
**Files:** `cli/daemon.go` (extend), test
- [ ] **S1 tests**: on boot, sessions with only terminal workers → `idle`; a session with a live worker stays `active`; a pending `confirm` escalation survives restart; a stale compiled `.claude/settings.json` in a reclaimed worktree is regenerated from the current session tree (never trust an on-disk config across a grant/revoke — check `perm_rev`).
- [ ] **S2–5** implement; commit `feat: session boot recovery + permission-config re-sync`.

---

## Optional add-ons & P3 (own sub-plans)
- **Telegram** (`internal/telegram`, Task 26): forum-topic-per-session; SSE → status-card edits + escalation inline keyboards; General = fleet console + mute-safe escalation mirror. Off by default. *Now a core rev-3 task (26); sub-plan `castellan-telegram.md` for hardening.*
- **Web UI** (`internal/web`): stdlib `html/template` + htmx + SSE; **session board** (columns = status) / session detail (renders `events WHERE session_id=?`, same data as the Telegram topic) / escalations with the same buttons. No JS build. Off by default. *Sub-plan `castellan-web.md`.*
- **Scheduler** (`internal/scheduler`): durable cron; castellan schedules its own follow-ups; NL→cron via a brain call; reminder-context injection when a task fires. *Sub-plan `castellan-scheduler.md`.*
- **Playbook learning loop** (`ledger/playbooks.go` + `memory/curator.go`): ACTIVE→STALE→ARCHIVED, pinned/referenced protection, reactivation, human-approved promotion — shares the curator with the memory tiers (Task 24). *Sub-plan `castellan-playbooks.md`.*
- **Cross-VM (P3)**: swap `Store` behind Postgres/queue; each VM's hook posts to one authed TCP endpoint; per-VM liveness + aggregation; provider-pool routing for rate limits. **Session ownership stays a central-ledger FK — a session owning worker A in vm1 + worker B in vm2 is just two `workers` rows with different `vm` and the same `owner_session`.** *Sub-plan `castellan-multivm.md`.*
- **Memory system** (Task 24): the 4 tiers (T1 whole-file `USER.md` + T2 `MEMORY.md` index/topic files + T3 per-session facts/summary/FTS5 + T4 playbooks) that `AssembleContext` pulls from, plus off-hot-path hindsight write-back. *Now a core rev-3 task (24).*

## Testing Strategy
- **Unit**: real SQLite in `t.TempDir()` (pure-Go, no mocks for the ledger); table tests for fusion/normalize/brain-parsing/config.
- **Fakes over mocks for externals**: `test/bin/{clavis,herdr}` shell scripts on PATH emit canned output; exercises real `exec.Command` paths, **no LLM/herdr and no provider calls** in tests (cost/flakiness/rate limits — [[agent-fanout-quota-discipline]]).
- **Integration**: Task 14 end-to-end (dispatch→hook→complete, incl. dedup) guards the skeleton in CI.
- **Concurrency**: Task 13 asserts per-worker ordering + cross-worker overlap under `-race`.

## Self-Review (coverage vs blueprint + review)
- Core shape §3.1 → Tasks 1–5,14. ✅  main-agent mgmt §3.2 (assembly, short-lived, cost) → Tasks 11, Global Constraints. ✅
- Typed StepResult + manager-owned routing + stall replan §3.5 → Tasks 11–12. ✅
- Ledger schema §3.7 (rich states, two-clock append log, sessions, escalations, usage+cache tokens) → Tasks 2–4,11,15,20; schema. ✅
- **Review fixes:** idempotent intake (T4/6), reconcile sweep (T7), intent-first + error/billing ladder (T12), execution concurrency + backoff (T13), log normalization (T9), pause/resume (T17), diff gate (T16), survive-and-reconcile + breaker (T18), channel escalations (T15), two-clock log (schema/T4), cost/cache measurement (Global/T11), Store interface for the Postgres boundary. ✅
- **Rev 3 threads:** sessions + capability tree (T20), context-balance transcript/checkpoint (T21), two-level autonomy (T22), permission-compile-to-worker (T23), 4-tier memory + hindsight + curator (T24), session-aware CLI + cost rollup (T25), Telegram forum-topic client (T26), session boot recovery (T27); autonomy-first control + escalations reframe (Global Constraints, schema, WorkerState enum, T12/T15). ✅
- Deferred by design (headless-complete without them): Web, scheduler, cross-VM → sub-plans. Telegram + memory are now core rev-3 tasks but remain OFF-by-default / non-blocking for the CLI. ✅
