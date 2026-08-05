# arco Build Guide — rev 6 (consolidated)

> **This is the authoritative build guide.** It flattens the rev-1→rev-5 layers into one document:
> resolved decisions, the frozen `0001_init.sql` (schema + seed data), the frozen Go contracts, and the
> reconciled PASS-0→PASS-3 task list with stale text removed. The layered
> [`implementation-plan.md`](implementation-plan.md), [`hardening-report-rev5.md`](hardening-report-rev5.md),
> and [`memory-links-rev5.md`](memory-links-rev5.md) are retained as **provenance appendices** (how the
> decisions were reached); where they conflict with this guide, **this guide wins.**
>
> Status: **design complete + hardened (5 review rounds) + consolidated. Not yet built. Start at PASS-0.**
> Not safe against real repos/creds until the 6 security preconditions (PASS-3) are met.

## How to read
1. **§A Decisions** — the 9+3 rev-5 open questions, resolved. Several are marked `PROVISIONAL — confirm`:
   they are my best-judgment defaults from the review, safe to build against, cheap to flip if the
   maintainer overrides. Nothing downstream is silently decided.
2. **§B Frozen schema** — `0001_init.sql` in full, including the `capability_catalog` seed rows and the
   `pool` session seed. This IS the freeze; do not drift.
3. **§C Frozen Go contracts** — `Deps`, `Store`/`Tx`, `VMClient`, `CapabilityOf`/`CatalogRow`, `DraftAnswer`,
   the split escalation API, the `event_kind` enum, `WorkerState`, and the pinned defaults table.
4. **§D Build order** — PASS-0 → PASS-1 → PASS-2 → PASS-3, every task numbered and assigned, TDD S1–S5
   shape preserved, all rev-5 work items included.
5. **§E Testing** — the consolidated test inventory (incl. the rev-5-added tests).

---

## §A — Decisions (rev-5 open questions, resolved)

Legend: **FROZEN** = locked; **PROVISIONAL — confirm** = my default, override anytime before that task is built.

1. **Worker-agent LLM spend metering (rev5 oq#1 → B10).** `usage` accepts `call_kind='agent'` rows from
   the start (freeze-safe regardless). *Whether* clavis/herdr can surface per-turn cost is verified in the
   PASS-0.5 integration spike (Task S). If unavailable, pool/session/worker USD budgets ship **brain-only
   with a loud `budget_scope_note` caveat**; token/cost breakers still trip on brain + any agent rows we do
   get. **PROVISIONAL — confirm** the brain-only fallback is acceptable if the spike finds no agent usage.
2. **B7 revoke propagation — cascade vs ancestor-walk.** **Cascade-materialize.** `Allowed()` stays an
   O(1) single-row read on the per-event hot path; a parent `Revoke` cascades over the subtree (recursive
   CTE) at revoke time (rare), removing the capability from every descendant grant + bumping each
   `perm_rev`. **FROZEN** (hot-path cost is the deciding factor).
3. **M5 revoke on a running worker.** **pause → recompile → resume** for non-high-blast caps (graceful,
   preserves work); high-blast needs no restart (never compiled; enforced live by `Allowed()`). `kill`
   remains the operator emergency primitive. **FROZEN.**
4. **M1 `exec tests/build/lint/install` default-on?** **Stays default-on** (it is the point of a coding
   agent). Consequence, accepted: env-scrub + server-side branch protection + OS-user separation become
   **load-bearing**, built to spec in PASS-3, and are the literal acceptance criteria for "real repos/creds."
   **PROVISIONAL — confirm** you accept default-on exec rather than defaulting these to `ask`.
5. **B6/M17 escalation dedup index.** **One pending escalation per worker.** Index =
   `(worker_id, COALESCE(capability,'')) WHERE status='pending'`; `kind` does NOT participate;
   `action_fingerprint` is a stored audit column, not in the index. A worker blocked at an `ask` cannot
   emit a second escalation. **FROZEN.**
6. **Delegation shape.** A self-spawned worker is a **leaf under the same `owner_session`** (never
   auto-creates a child session) in P2 — keeps the delegation caps and session-tree caps orthogonal.
   **FROZEN** for P2.
7. **Grant expiry.** Freeze `session_grants.expires_at` now; **no P2 path sets it** (the expiry reaper is
   defined-but-inert in P2). `arco grant --expires` is a P3 addition. **FROZEN.**
8. **Budget reset + freeze durability.** `daily_usd_cap` resets at **UTC midnight**. Soft trip at **0.8×**
   cap, hard at **1.0×**; hysteresis re-arm at **0.5×**. A hard freeze persists in `breakers` and requires
   explicit `arco unfreeze` — it does **not** auto-recover at rollover (safety over convenience).
   **PROVISIONAL — confirm** the trip/hysteresis fractions.
9. **`Deps` field set + ctx.** `Deps{ Store; VM VMClient; Cfg Config; Exec *Exec; Classify CapabilityOf;
   Clock func() time.Time; Log *slog.Logger; Redact Scrubber }` — `Classify` **is** `CapabilityOf` (one
   function, not two). `ctx context.Context` is threaded explicitly on all `Store`/`Tx`/`VMClient` methods;
   `Deps` is passed read-only. **FROZEN.**

Memory-links (from `memory-links-rev5.md`):
- **M-1 nodes table:** **skip** — the `MEMORY.md` index line is the node record at <10² nodes. **FROZEN.**
- **M-2 `rel` typing:** **untyped `'ref'` MVP**; name the relation in prose next to the link (dodges the
  CHECK-enum freeze; typed rels can be added later without a rebuild). **FROZEN.**
- **M-3 `scope` in the always-hot index:** frontmatter only unless `cache_read` telemetry shows the index
  tag is free. **FROZEN.**

New decisions this consolidation forces:
- **D-lost:** add **`lost`** to `WorkerState` (a worker whose process+workspace `VMClient` cannot find
  across `LivenessMissThreshold` sweeps; distinct from `failed`/`killed`). `suspect_missing` is a transient
  in-memory sub-state, NOT a persisted status. **FROZEN** (CHECK-enum value — must be in `0001_init.sql`).
- **D-spike:** add **Task S (PASS-0.5): freeze + verify the clavis/herdr contract** before PASS-2 (opus P1-3):
  hook registration + stable `source_event_id`, transcript format/location per agent, structured StepResult
  return, prompt-echo observability (B9), usage surfacing (B10, oq#1), and the pinned `interactive-in-pane`
  spawn mode (never headless `-p`). **FROZEN** as a gate on PASS-2.

---

## §B — Frozen schema (`internal/ledger/migrations/0001_init.sql`)

Single source of truth: **migrations**. `Open()` runs `schema_migrations`-tracked migrations; there is no
separately-embedded `schema.sql` (B1/P2-3 resolved). `memory_links` is deliberately **NOT** here — it is a
derived index added by a later migration and rebuilt from md files (zero-migration rollback by design).

```sql
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;

-- ---- migration bookkeeping ------------------------------------------------
CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  checksum   TEXT NOT NULL,           -- sha256 of the migration text
  applied_at TEXT NOT NULL            -- UTC RFC3339Nano
);

-- ---- sessions (first-class unit; single-parent tree; protected pool) ------
CREATE TABLE sessions (
  id               TEXT PRIMARY KEY,                                  -- ULID
  slug             TEXT UNIQUE,
  title            TEXT NOT NULL DEFAULT '',
  goal             TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL DEFAULT 'open'
                     CHECK(status IN ('open','active','waiting','idle','done','archived')),
  kind             TEXT NOT NULL DEFAULT 'work' CHECK(kind IN ('work','pool')),
  parent_session   TEXT REFERENCES sessions(id),                     -- NULL = root
  rev              INTEGER NOT NULL DEFAULT 0,                        -- CAS: rebind/status/rollup
  perm_rev         INTEGER NOT NULL DEFAULT 0,                        -- bumps on grant/revoke/expiry
  mem_rev          INTEGER NOT NULL DEFAULT 0,                        -- bumps on sanctioned memory write
  permissions      TEXT NOT NULL DEFAULT '{}',                       -- DERIVED cache of the grant rows
  context_summary  TEXT NOT NULL DEFAULT '',
  context_rev      INTEGER NOT NULL DEFAULT 0,                        -- CAS: checkpoint
  facts            TEXT NOT NULL DEFAULT '',
  progress         TEXT NOT NULL DEFAULT '',
  repo             TEXT NOT NULL DEFAULT '',
  default_vm       TEXT NOT NULL DEFAULT '',
  pinned           INTEGER NOT NULL DEFAULT 0,
  notify_level     TEXT NOT NULL DEFAULT 'important'
                     CHECK(notify_level IN ('all','important','silent')),
  tg_topic_id      INTEGER,
  tg_status_msg_id INTEGER,
  stall_count      INTEGER NOT NULL DEFAULT 0,
  last_activity_at TEXT NOT NULL,
  created_at       TEXT NOT NULL,
  closed_at        TEXT
);
CREATE INDEX idx_sessions_status ON sessions(status);
CREATE INDEX idx_sessions_parent ON sessions(parent_session);
CREATE UNIQUE INDEX idx_sessions_topic ON sessions(tg_topic_id) WHERE tg_topic_id IS NOT NULL;

-- ---- workers --------------------------------------------------------------
CREATE TABLE workers (
  id               TEXT PRIMARY KEY,                                  -- ULID (pre-generated)
  title            TEXT NOT NULL DEFAULT '',
  vm               TEXT NOT NULL DEFAULT 'local',
  workspace        TEXT NOT NULL DEFAULT '',                          -- deterministic: arco_<ulid>
  worktree         TEXT NOT NULL DEFAULT '',
  base_commit      TEXT NOT NULL DEFAULT '',
  head_commit      TEXT NOT NULL DEFAULT '',
  program          TEXT NOT NULL DEFAULT '',
  agent_kind       TEXT NOT NULL DEFAULT '',                          -- claude|qwen|... selects normalizer
  boot_id          TEXT NOT NULL DEFAULT '',                          -- host boot id (liveness identity)
  pid              INTEGER,
  pid_start_time   TEXT NOT NULL DEFAULT '',                          -- guards PID reuse across reboot
  state            TEXT NOT NULL
                     CHECK(state IN ('starting','running','waiting_for_user','waiting_for_confirmation',
                                     'blocked','completed_candidate','completed_verified','failed',
                                     'paused','killed','lost')),
  rev              INTEGER NOT NULL DEFAULT 0,                        -- optimistic CAS (no side effect w/o it)
  stall_count      INTEGER NOT NULL DEFAULT 0,
  owner_session    TEXT NOT NULL REFERENCES sessions(id),            -- single owner; pool = the sentinel
  session_perm_rev INTEGER NOT NULL DEFAULT 0,                        -- owner tree rev this worker compiled at
  permissions_hash TEXT NOT NULL DEFAULT '',                          -- hash of compiled tree (dispatch gate)
  compiled_config_path TEXT NOT NULL DEFAULT '',                      -- OUTSIDE the worktree (~/.arco/workers/<id>/)
  task             TEXT NOT NULL DEFAULT '',
  run_reason       TEXT NOT NULL DEFAULT '',
  parent_worker_id TEXT REFERENCES workers(id),                       -- delegation lineage
  delegation_depth INTEGER NOT NULL DEFAULT 0,
  role             TEXT NOT NULL DEFAULT '',
  summary          TEXT NOT NULL DEFAULT '',
  last_seen_at     TEXT NOT NULL,
  last_event_at    TEXT NOT NULL,
  pooled_at        TEXT,                                              -- set on release→pool (TTL reaper clock)
  created_at       TEXT NOT NULL
);
CREATE INDEX idx_workers_state ON workers(state);
CREATE INDEX idx_workers_vm ON workers(vm);
CREATE INDEX idx_workers_session ON workers(owner_session);
CREATE INDEX idx_workers_pooled ON workers(pooled_at) WHERE pooled_at IS NOT NULL;

-- ---- events (IMMUTABLE, append-only, two clocks, namespaced dedup) ---------
CREATE TABLE events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,                -- monotonic SSE cursor
  source            TEXT NOT NULL DEFAULT 'internal',                 -- internal|herdr:<vm>|cli|tg|web
  source_event_id   TEXT,                                            -- stable id from origin; NULL internal
  source_event_hash TEXT,                                            -- hash of the SCRUBBED canonical payload
  worker_id         TEXT REFERENCES workers(id),
  session_id        TEXT REFERENCES sessions(id),
  kind              TEXT NOT NULL,                                    -- see event_kind enum (§C); NO CHECK (extensible)
  actor             TEXT NOT NULL DEFAULT '',                         -- who caused it (session/worker/cli/brain)
  causation_event_id INTEGER REFERENCES events(id),
  correlation_id    TEXT,
  payload           TEXT NOT NULL DEFAULT '{}',                       -- already Scrub()'d at write time
  occurred_at       TEXT NOT NULL,                                    -- event time
  recorded_at       TEXT NOT NULL,                                    -- wall clock we learned it
  UNIQUE(source, source_event_id)
);
CREATE INDEX idx_events_worker ON events(worker_id, id);
CREATE INDEX idx_events_session ON events(session_id, id);
CREATE INDEX idx_events_kind ON events(session_id, kind, id);
CREATE INDEX idx_events_recorded ON events(recorded_at, id);

-- ---- brain transcript (soft-archive lives HERE, not on events) -------------
CREATE TABLE brain_transcript_rows (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id   TEXT NOT NULL REFERENCES sessions(id),
  role         TEXT NOT NULL,                                         -- system|assistant|tool|user
  content      TEXT NOT NULL,                                         -- normalized, already Scrub()'d
  active       INTEGER NOT NULL DEFAULT 1,                            -- 1=live in transcript
  compacted    INTEGER NOT NULL DEFAULT 0,                            -- 1=folded into context_summary
  source_events TEXT NOT NULL DEFAULT '[]',                           -- provenance (event ids)
  tainted      INTEGER NOT NULL DEFAULT 0,                            -- advisory-only (brain answer/rollup/external)
  created_at   TEXT NOT NULL
);
CREATE INDEX idx_transcript_session ON brain_transcript_rows(session_id, active, id);
CREATE VIRTUAL TABLE transcript_fts USING fts5(
  content, session_id UNINDEXED, row_id UNINDEXED,
  tokenize='porter'
);  -- indexes SCRUBBED normalized content only; never raw events.payload

-- ---- capability catalog (the structural classifier's DATA) ----------------
CREATE TABLE capability_catalog (
  capability      TEXT PRIMARY KEY,
  action_class    TEXT NOT NULL CHECK(action_class IN ('routine','ambiguous','danger')),
  tier            TEXT NOT NULL CHECK(tier IN ('low','medium','high_blast')),
  default_allowed INTEGER NOT NULL DEFAULT 0,                          -- in DefaultTree()?
  high_blast      INTEGER NOT NULL DEFAULT 0,                          -- never compiled onto a worker
  compiled_worker INTEGER NOT NULL DEFAULT 0,                          -- 0=arco-executed-only
  description     TEXT NOT NULL DEFAULT ''
);

-- ---- grants (rows, not JSON; sessions.permissions is a derived cache) ------
CREATE TABLE session_grants (
  id                 TEXT PRIMARY KEY,
  session_id         TEXT NOT NULL REFERENCES sessions(id),
  capability         TEXT NOT NULL REFERENCES capability_catalog(capability),
  status             TEXT NOT NULL DEFAULT 'active'
                       CHECK(status IN ('active','revoked','expired')),
  scope              TEXT NOT NULL DEFAULT 'session' CHECK(scope IN ('session','once')),
  source_escalation_id TEXT,
  granted_by         TEXT NOT NULL DEFAULT '',                         -- NEVER a brain/rollup value
  created_perm_rev   INTEGER NOT NULL,
  use_count          INTEGER NOT NULL DEFAULT 0,
  expires_at         TEXT,                                            -- P2: never set (reaper inert)
  created_at         TEXT NOT NULL,
  revoked_at         TEXT
);
CREATE INDEX idx_grants_active ON session_grants(session_id, capability) WHERE status='active';

-- ---- escalations (autonomy-first; one pending per worker) ------------------
CREATE TABLE escalations (
  id                TEXT PRIMARY KEY,
  worker_id         TEXT REFERENCES workers(id),
  session_id        TEXT REFERENCES sessions(id),
  kind              TEXT NOT NULL DEFAULT 'question' CHECK(kind IN ('question','confirm')),
  question_class    TEXT NOT NULL DEFAULT 'other'
                      CHECK(question_class IN ('clarify','proceed-confirmation','scope-change',
                                               'resource','other')),
  action_class      TEXT NOT NULL DEFAULT 'ambiguous'
                      CHECK(action_class IN ('routine','ambiguous','danger')),
  tier              TEXT NOT NULL DEFAULT 'medium' CHECK(tier IN ('low','medium','high_blast')),
  capability        TEXT,                                             -- NULL for free-text questions
  action_fingerprint TEXT NOT NULL DEFAULT '',                        -- audit only; NOT in the dedup index
  requested_event_id INTEGER REFERENCES events(id),
  classifier        TEXT NOT NULL DEFAULT '',
  classifier_version TEXT NOT NULL DEFAULT '',
  action            TEXT NOT NULL,
  detail            TEXT NOT NULL DEFAULT '{}',
  draft_answer      TEXT NOT NULL DEFAULT '',                          -- SHADOW: the brain's advisory draft
  draft_confidence  REAL NOT NULL DEFAULT 0,
  brain_rationale   TEXT NOT NULL DEFAULT '',
  answered_by       TEXT NOT NULL DEFAULT '' CHECK(answered_by IN ('','brain','human')),
  status            TEXT NOT NULL DEFAULT 'pending'
                      CHECK(status IN ('pending','answered','approved','rejected','expired')),
  decision          TEXT NOT NULL DEFAULT '',
  answer_text       TEXT NOT NULL DEFAULT '',
  decided_by        TEXT NOT NULL DEFAULT '',
  once_or_always    TEXT NOT NULL DEFAULT 'once' CHECK(once_or_always IN ('once','always')),
  requested_at      TEXT NOT NULL,
  decided_at        TEXT,
  resumed_at        TEXT                                              -- durable resume marker (boot recovery)
);
CREATE UNIQUE INDEX idx_escalations_pending
  ON escalations(worker_id, COALESCE(capability,'')) WHERE status='pending';
CREATE INDEX idx_escalations_status ON escalations(session_id, status);

-- ---- provider pools + leases (leases acquired BEFORE dispatch_intent) ------
CREATE TABLE provider_pools (
  id                 TEXT PRIMARY KEY,
  provider           TEXT NOT NULL,
  org                TEXT NOT NULL DEFAULT '',
  clavis_profile     TEXT NOT NULL,
  model_class        TEXT NOT NULL DEFAULT '',
  max_active         INTEGER NOT NULL DEFAULT 35,
  max_starts_per_min INTEGER NOT NULL DEFAULT 10,
  daily_usd_cap_microusd INTEGER NOT NULL DEFAULT 0,
  state              TEXT NOT NULL DEFAULT 'ok'
                       CHECK(state IN ('ok','cooldown','billing','disabled')),
  cooldown_until     TEXT,
  created_at         TEXT NOT NULL
);
CREATE TABLE worker_pool_leases (
  id                     TEXT PRIMARY KEY,
  pool_id                TEXT NOT NULL REFERENCES provider_pools(id),
  worker_id              TEXT REFERENCES workers(id),                 -- nullable until bound
  dispatch_intent_event_id INTEGER REFERENCES events(id),
  acquired_at            TEXT NOT NULL,
  expires_at             TEXT NOT NULL,                               -- TTL; boot recovery reaps
  released_at            TEXT
);
CREATE INDEX idx_leases_pool_active ON worker_pool_leases(pool_id) WHERE released_at IS NULL;

-- ---- budgets + breakers (integer microusd; scope incl. subtree) -----------
CREATE TABLE budgets (
  id            TEXT PRIMARY KEY,
  scope         TEXT NOT NULL CHECK(scope IN ('fleet','session','pool','worker','subtree')),
  scope_id      TEXT NOT NULL DEFAULT '',
  soft_usd_microusd INTEGER NOT NULL DEFAULT 0,
  hard_usd_microusd INTEGER NOT NULL DEFAULT 0,
  soft_tokens   INTEGER NOT NULL DEFAULT 0,
  hard_tokens   INTEGER NOT NULL DEFAULT 0,
  window        TEXT NOT NULL DEFAULT 'utc_day' CHECK(window IN ('utc_day','rolling_24h','total')),
  created_at    TEXT NOT NULL
);
CREATE TABLE breakers (
  id         TEXT PRIMARY KEY,
  scope      TEXT NOT NULL CHECK(scope IN ('fleet','session','pool','worker','subtree')),
  scope_id   TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL DEFAULT 'closed' CHECK(state IN ('closed','soft','hard')),
  reason     TEXT NOT NULL DEFAULT '',
  tripped_at TEXT,
  cleared_at TEXT
);

-- ---- observation plane -----------------------------------------------------
CREATE TABLE vms (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  boot_id     TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL DEFAULT 'ok',
  last_seen_at TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
CREATE TABLE vm_observations (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  vm_id       TEXT NOT NULL REFERENCES vms(id),
  observed_at TEXT NOT NULL,
  agents_json TEXT NOT NULL DEFAULT '[]',
  git_heads_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_vmobs_vm ON vm_observations(vm_id, observed_at);

-- ---- usage (integer microusd; every call_kind) ----------------------------
CREATE TABLE usage (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  worker_id    TEXT,
  session_id   TEXT,
  provider     TEXT NOT NULL DEFAULT '',
  model        TEXT NOT NULL DEFAULT '',
  call_kind    TEXT NOT NULL
                 CHECK(call_kind IN ('brain_decision','brain_classify','rollup','agent',
                                      'checkpoint','hindsight')),
  event_id     INTEGER REFERENCES events(id),
  prompt_hash  TEXT NOT NULL DEFAULT '',                              -- over the exact bytes sent (post-Scrub)
  context_rev  INTEGER NOT NULL DEFAULT 0,
  perm_rev     INTEGER NOT NULL DEFAULT 0,
  input_tok    INTEGER NOT NULL DEFAULT 0,
  output_tok   INTEGER NOT NULL DEFAULT 0,
  cache_read   INTEGER NOT NULL DEFAULT 0,
  cache_write  INTEGER NOT NULL DEFAULT 0,
  cost_microusd INTEGER NOT NULL DEFAULT 0,
  success      INTEGER NOT NULL DEFAULT 1,
  billing_error INTEGER NOT NULL DEFAULT 0,
  at           TEXT NOT NULL
);
CREATE INDEX idx_usage_session ON usage(session_id, at);
CREATE INDEX idx_usage_worker ON usage(worker_id, at);
CREATE INDEX idx_usage_at ON usage(at);

-- ---- memory revisions (provenance for the manual memory writes) -----------
CREATE TABLE memory_revisions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  mem_rev    INTEGER NOT NULL,
  op         TEXT NOT NULL CHECK(op IN ('add','update','expire')),
  fact_id    TEXT NOT NULL DEFAULT '',
  topic      TEXT NOT NULL DEFAULT '',
  author     TEXT NOT NULL DEFAULT '' CHECK(author IN ('user','external','')),  -- NEVER 'brain'
  decided_by TEXT NOT NULL,                                            -- human required
  at         TEXT NOT NULL
);

-- ---- playbooks (P3 learning loop) -----------------------------------------
CREATE TABLE playbooks (
  id TEXT PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'active' CHECK(state IN ('active','stale','archived')),
  pinned INTEGER NOT NULL DEFAULT 0, use_count INTEGER NOT NULL DEFAULT 0,
  last_used_at TEXT, created_at TEXT NOT NULL
);

-- ==== SEED: the protected pool session (fixed well-known ULID), LAST ========
-- Must be inserted AFTER sessions exists and BEFORE any worker can be created.
INSERT INTO sessions (id, slug, title, goal, status, kind, parent_session, permissions,
                      last_activity_at, created_at)
VALUES ('00000000000000000000000POOL', 'pool', 'Worker Pool (protected)', '', 'active', 'pool', NULL, '{}',
        '1970-01-01T00:00:00.000000000Z', '1970-01-01T00:00:00.000000000Z');

-- ==== SEED: capability_catalog ============================================
-- default_allowed=1 → in DefaultTree(); high_blast=1 → arco-executed-only, never on a worker;
-- compiled_worker=1 → written into the worker's allow/ask set (else arco boundary only).
INSERT INTO capability_catalog (capability, action_class, tier, default_allowed, high_blast, compiled_worker, description) VALUES
 ('git.branch.create',   'routine','low',    1,0,1,'create a branch in-worktree'),
 ('git.commit',          'routine','low',    1,0,1,'commit in-worktree'),
 ('git.push.feature',    'routine','low',    1,0,1,'push its own feature branch'),
 ('git.pr.open',         'routine','low',    1,0,1,'open/update a PR'),
 ('git.pr.update',       'routine','low',    1,0,1,'update its own PR'),
 ('fs.worktree',         'routine','low',    1,0,1,'read/write inside its own worktree'),
 ('exec.tests',          'routine','low',    1,0,1,'run tests'),
 ('exec.build',          'routine','low',    1,0,1,'run build'),
 ('exec.lint',           'routine','low',    1,0,1,'run lint'),
 ('exec.install',        'routine','low',    1,0,1,'install deps'),
 ('git.pr.merge',        'ambiguous','medium',0,0,0,'merge a PR (arco-executed; server-side review backs it)'),
 ('git.push.shared',     'ambiguous','medium',0,0,0,'push a shared/feature branch others use'),
 ('net.fetch',           'ambiguous','medium',0,0,0,'outbound network fetch (DEFAULT OFF; rev-4.1)'),
 ('spawn.subworker',     'ambiguous','medium',0,0,0,'self-spawn a helper (DEFAULT OFF; depth 1, <=2 children)'),
 ('handoff',             'routine','low',    1,0,0,'hand ownership back to arco'),
 ('memory.cross-project','ambiguous','medium',0,0,0,'recall memory outside {global, session.repo} (DEFAULT OFF)'),
 ('fleet.claim',         'ambiguous','high_blast',0,1,0,'claim a worker from the pool'),
 ('fleet.transfer',      'ambiguous','high_blast',0,1,0,'transfer a worker between sessions'),
 ('fleet.move',          'ambiguous','high_blast',0,1,0,'move a session subtree'),
 ('git.push.main',       'danger','high_blast',0,1,0,'push to main/protected (arco-executed after confirm)'),
 ('external.deploy',     'danger','high_blast',0,1,0,'deploy'),
 ('external.spend',      'danger','high_blast',0,1,0,'spend money / paid API beyond budget'),
 ('external.send',       'danger','high_blast',0,1,0,'send outbound comms'),
 ('secrets.read',        'danger','high_blast',0,1,0,'read secrets'),
 ('fs.destructive',      'danger','high_blast',0,1,0,'destructive delete outside worktree');

INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (1, '0001_init', 'sha256:PLACEHOLDER', '1970-01-01T00:00:00.000000000Z');
```

**Freeze invariants (tests in Task 2 / PASS-0):** replay of all migrations == the committed schema
fingerprint; a fresh DB has exactly one `kind='pool'` row before any worker insert; every
`high_blast=1` catalog row has `compiled_worker=0`; `DefaultTree()` == the set of `default_allowed=1` rows.

---

## §C — Frozen Go contracts

`*sql.DB`/`*sql.Tx` never appear in an interface — they live inside the sqlite impl (P3 Postgres swap).
All CAS mutators take an `expectedRev` and return `ErrRevMismatch`; no side effect without a successful CAS.

```go
// ---- storage seam ---------------------------------------------------------
type Store interface {
    Migrate(ctx context.Context) error                     // runs schema_migrations
    WithTx(ctx context.Context, fn func(Tx) error) error   // synchronous; hooks fire strictly AFTER commit
    Reader() Reader                                         // WAL concurrent reads (no *sql.DB leak)
}
var ErrRevMismatch = errors.New("optimistic CAS rev mismatch")

// Tx is arco-owned; entity ops hang off it (or off sub-stores derived from it).
type Tx interface {
    CreateWorker(w Worker) error
    GetWorker(id string) (Worker, error)
    TransitionWorker(id string, to WorkerState, expectedRev int64, e Event) error   // CAS
    AppendEvent(e Event) (cursor int64, deduped bool, conflict bool, err error)      // ON CONFLICT DO NOTHING; conflict=hash mismatch
    CreateSession(s Session) error
    SetSessionStatus(id string, to SessionStatus, expectedRev int64, e Event) error  // CAS
    Grant(sessionID, cap string, e Event) (newPermRev int64, err error)              // cascades allowed at boundary
    Revoke(sessionID, cap string, e Event) (newPermRev int64, err error)             // cascades to subtree (dec #2)
    Checkpoint(sessionID string, expectedCtxRev int64, watermark int64) error        // CAS; fold rows id<=watermark
    // ... AttachWorker, OpenEscalation, DecideConfirm/AnswerQuestion, lease ops, usage, etc.
}
type Reader interface {
    ListWorkers(f WorkerFilter) ([]Worker, error)
    EventsSince(cursor int64, limit int) ([]Event, error)   // bounded
    Allowed(sessionID, cap string) (bool, error)            // O(1) own-tree read (cascade keeps it O(1))
    // ... GetSession, ListSessions, transcript reads, etc.
}

// ---- VM seam (LocalVMClient now; SSHVMClient P3). Identity, NOT central PID ----
type VMClient interface {
    ListAgents(ctx context.Context) ([]AgentObs, error)     // {workspace, boot_id, pid_start_time, alive}
    GitHeads(ctx context.Context, worktrees []string) (map[string]string, error)
    Prompt(ctx context.Context, workspace, text string) error   // text embeds a prompt_intent ULID (B9)
    Kill(ctx context.Context, workspace string) error
    Diff(ctx context.Context, worktree, base, head string) (Diff, error)
}

// ---- injection bundle (built once in daemon.go, passed read-only) ----------
type Deps struct {
    Store    Store
    VM       VMClient
    Cfg      Config
    Exec     *Exec
    Classify CapabilityOf          // == the structural classifier below (one function)
    Clock    func() time.Time
    Log      *slog.Logger
    Redact   Scrubber
}

// ---- structural classifier (data lives in capability_catalog) --------------
type CatalogRow struct {
    Capability     string
    ActionClass    string  // routine|ambiguous|danger
    Tier           string  // low|medium|high_blast
    DefaultAllowed bool
    HighBlast      bool
    CompiledWorker bool
}
type CapabilityOf func(e NormalizedEntry) (row CatalogRow, classified bool)  // classified=false ⇒ FAIL-CLOSED (ask/deny)

// ---- redaction (write-time; deterministic + versioned) ---------------------
type Scrubber interface {
    Scrub(s string) (clean string, n int)   // deterministic; version pinned in Cfg
    Version() string
}

// ---- escalations: brain draft is a DISTINCT type with NO authority field ---
type DraftAnswer struct {                    // what the two-level resolver returns
    Text       string
    Confidence float64
    Rationale  string
    Tainted    bool                          // always true for brain output
}
func ResolveQuestion(deps Deps, esc Escalation) (DraftAnswer, bool)  // bool = escalate-to-human
// The human decision API is SEPARATE and the only path to a grant:
//   (Tx) AnswerQuestion(id, text string, scope Scope) error
//   (Tx) DecideConfirm(id string, yes bool, scope Scope) error   // re-checks Allowed() vs CURRENT owner (B14)
// FROZEN: no function accepts a DraftAnswer (or any brain-sourced value) into DecideConfirm/Grant.

// ---- StepResult: exhaustive kinds; reconciler has a branch for each --------
type StepResult struct { Kind, Worker, Instruction, Reason string }
// Kind ∈ run_again | dispatch | handoff | final_output | question | confirm
//   final_output → completed_candidate + diff gate;  handoff → P2 reject/escalate (PASS-3 feature)
//   unhandled kind → error event, never a silent drop.
```

**`WorkerState`** (incl. `lost`): `starting, running, waiting_for_user, waiting_for_confirmation, blocked,
completed_candidate, completed_verified, failed, paused, killed, lost`.

**`event_kind`** (Go `const` enum; reconciler/normalizer switches have a default → `error` event):
`state_change, dispatch_intent, dispatch_done, prompt_intent, prompt_done, brain_intent, brain_decision,
brain_msg, chat_in, chat_out, question_req, question_ans, question_esc, confirm_req, confirm_dec, grant,
revoke, session_open, session_close, session_moved, worker_release_intent, worker_released,
worker_claim_intent, worker_claimed, worker_transfer_intent, worker_transferred, memory_diff, kill_intent,
kill_done, reconcile, resume_intent, error, note`.

**Pinned defaults** (Task 1 `Config`, all overridable via `ARCO_*` / TOML):

| Knob | Default | Knob | Default |
|---|---|---|---|
| `SweepInterval` | 30s | `stall_N` | 3 |
| `MaxSpawns` | 8 | `MaxBrainCalls` | 4 |
| `crash_loop_breaker` | 5 restarts / 10min | `LivenessMissThreshold` | 3 sweeps |
| `SuspectTimeout` | 2×SweepInterval | `checkpoint_threshold` | 40 transcript rows |
| `escalation_timeout` | 30min → auto-pause | `auto_answer_budget_N` | 10/session (inert in P2 shadow) |
| `max_children_per_session` | 8 | `rollup_interval` | 5min |
| `per_session_brain_rate` | 6/min | `pool_ttl` | 24h → pause reaper |
| `lease_ttl` | 15min | budget soft/hard/re-arm | 0.8× / 1.0× / 0.5× |

---

## §D — Build order (PASS-0 → PASS-3; every task numbered + assigned)

TDD S1–S5 shape preserved (failing test → red → implement → green → commit); fakes over mocks for
externals (`test/bin/{clavis,herdr}`); no LLM/provider calls in tests. Detailed S1–S5 substeps for the
carried-over tasks live in [`implementation-plan.md`](implementation-plan.md) **as corrected by the freeze
above** — where they differ, this guide wins.

**PASS-0 — freeze (this document).** Land `0001_init.sql` (§B) + the Go contracts (§C) + a migrate-from-fixture
test asserting the freeze invariants. No behavior. Record the §A decisions. *(Absorbs rev-4.1 G-items +
rev-5 B1/B2/B3/B6-schema/M7/M11 + D-lost.)*

**PASS-0.5 — Task S (clavis/herdr contract spike, GATES PASS-2).** Freeze + verify against a real
clavis/herdr: hook registration + stable `source_event_id`; per-agent transcript format/location;
`interactive-in-pane` spawn (never headless `-p`); **B9** prompt-echo observability (a `Prompt` with an
embedded ULID appears in the normalized transcript); **B10/oq#1** whether per-turn agent usage/cost is
surfaced. Output: a frozen `VMClient` fake matching reality + a go/no-go on agent-spend metering.

**PASS-1 — spine.** T1 module/config (+defaults table) → T2 Store+migrations (`Migrate`/`WithTx`, single-writer)
→ **T-redact `internal/redact/`** (wired into `AppendEvent` now, not PASS-3) → T3 workers CRUD + CAS
transitions → T4 immutable event log (`ON CONFLICT DO NOTHING`; hash-mismatch→`error`+conflict signal;
hash over scrubbed canonical bytes) → T20a sessions CRUD + **capability_catalog load + `Allowed()`** +
`CapabilityOf` (fail-closed) → T5 API over unix socket (0600, dedicated group) + `/healthz` → T-client CLI.

**PASS-2 — single-VM walking skeleton (headless).** T10 `LocalVMClient` (GitHeads/Prompt/Kill/Diff) → T9
normalize (+`billing`/`rate_limited`/`agent_usage` kinds, M9/B10) → T8 fusion (VMClient liveness, not PID) →
T6 intake `POST /v1/events` (idempotent) → T14 spawn (pre-gen ULID → lease → `dispatch_intent`+`CreateWorker`
→ side-effect → `dispatch_done`; env-scrubbed; config staged **outside** the worktree, B6) → T23 permcompile
(`--settings`; net-fetch OFF; high-blast never compiled) → T13 Exec (per-worker CAS queues, cross-worker
parallel) → T12 reconciler (intent-first; **all** StepResult branches incl. `final_output`/`handoff`, B11;
`prompt_intent/done`, B9; billing/rate ladder, M9) → T22 two-level resolver (`ResolveQuestion`→`DraftAnswer`,
SHADOW: drafts stored in `escalations.draft_answer`, human decides) → T15 escalations (split
`AnswerQuestion`/`DecideConfirm`; one-pending-per-worker; timeout→close row + auto-pause; durable
`resumed_at`) → T7 reconcile sweep (VMClient, `suspect_missing`→`lost`) → T11 brain (byte-stable assemble;
`prompt_hash` post-Scrub; error/billing ladder) → T21 transcript + checkpoint (watermark; `context_rev` CAS)
→ T16 diff-gate → T17 pause/resume (`Pause(id, keepWorktree bool)`) → T18 boot recovery (survive-and-reconcile
+ decided-but-unresumed escalations M3 + orphan-worktree GC M16 + lease reap B10) → T27 session boot + perm
re-sync + **runtime recompile-on-Grant/Revoke** (M5, coalesced) → T24a memory (manual; `[[wikilinks]]` +
derived `memory_links`; `Scrub`; `trust:external`+`Tainted`) → **integration test `dispatch→hook→complete`**.

**PASS-3 — hardening + security preconditions (the "real repos/creds" gate).** Budgets+breakers+`arco freeze`/
`unfreeze` (UTC-day reset; 0.8/1.0/0.5) → provider-pool leases at scale → per-VM admission → **6 preconditions
as dedicated tasks each with an S1 test**: (P1) OS-user separation + scrubbed spawn env; (P2) **quarantine
task** (repo `.claude`/`.mcp.json`/`settings.local.json`/hooks/`core.fsmonitor`/`.gitattributes`); (P3)
managed-settings deny + no high-blast creds + server-side branch protection + per-worker fine-grained tokens
(B8) + provider-side spend caps (B10); (P4) auth on Telegram/Web/cross-VM intake (source-bound) + caller-class
on mutating endpoints (high-blast `Grant` = local-CLI only); (P5) redaction egress defense-in-depth + golden
corpus; (P6) pinned spawn mode + net-fetch/spawn default-off + **B8 detection path** (fusion scans transcript
tails for deny-listed calls → auto-pause + danger escalation) → worker ownership transfer (release/claim/
transfer + pool TTL reaper + execute-time owner re-validation + B14 pending-escalation settlement) → depth-2
supersession (rollup + fan-in cap `max_children_per_session` + coalescing + `call_kind='rollup'` admission
priority, M19/M20) → `arco unfreeze`/`escalations`/`autonomy` CLI + systemd `Type=notify` `WatchdogSec` +
`.service` unit (M21).

**Later (own sub-plans):** SSH `VMClient` + cross-VM + provider pools at scale + Postgres/queue (>150 workers
or >100 events/s); Telegram (notify_class + coalescer); Web; scheduler; hindsight 24b (proposals, zero
auto-apply); curator + playbooks 24c; depth-3 + move-subtree; typed memory `rel`.

---

## §E — Testing inventory (consolidated; includes the rev-5-added tests)
- **Freeze:** migrate-from-fixture (replay==fingerprint; one pool row; high-blast⇒not-compiled; DefaultTree==catalog).
- **Concurrency (`-race`):** per-worker serialize + cross-worker parallel; two racing mutations → one `ErrRevMismatch`.
- **Crash matrix (kill-9 at every intent→done boundary):** dispatch, prompt (B9), kill, release/claim/transfer,
  grant-expiry, decided-but-unresumed escalation (M3), orphan worktree (M16), leaked lease (B10). One table-driven harness.
- **Security:** redaction golden corpus (planted token absent from persisted bytes AND MemorySearch snippet AND brain prompt);
  malicious-fixture-repo (`.mcp.json`+`settings.local.json` → worker with neither active); worker cannot exercise a revoked cap
  after recompile (M5); high-blast `Grant` rejected off the local-CLI caller class.
- **Autonomy disjointness (structural):** `DraftAnswer` has no scope field; the auto-answer path calls only `Prompt`;
  `DecideConfirm` rejects `decided_by='brain'`; a rollup-context brain call cannot emit `fleet.*` (M19).
- **Byte-stability:** `AssembleContext` byte-identical across two calls at the same `(ledger state, mem_rev)`, with a sleep between.
- **Memory-links:** `memory_links` rebuilt-from-files only (no write API); depth-1 neighbors returned; kill-criterion instrumentation logs neighbor-hop follows.
- **Integration:** `dispatch → hook → complete` headless (no Telegram/Web), incl. re-POST dedup.

