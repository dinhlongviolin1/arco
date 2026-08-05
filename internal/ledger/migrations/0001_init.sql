-- arco schema — migration 0001_init (frozen; build-guide-rev6 §B).
-- DDL ONLY. This file NEVER self-INSERTs into schema_migrations; Store.Migrate is
-- the sole writer of the bookkeeping row (checksum = sha256 of this file's bytes).
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;

CREATE TABLE schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  checksum   TEXT NOT NULL,
  applied_at TEXT NOT NULL
);

CREATE TABLE sessions (
  id               TEXT PRIMARY KEY,
  slug             TEXT UNIQUE,
  title            TEXT NOT NULL DEFAULT '',
  goal             TEXT NOT NULL DEFAULT '',
  status           TEXT NOT NULL DEFAULT 'open'
                     CHECK(status IN ('open','active','waiting','idle','done','archived')),
  kind             TEXT NOT NULL DEFAULT 'work' CHECK(kind IN ('work','pool')),
  parent_session   TEXT REFERENCES sessions(id),
  rev              INTEGER NOT NULL DEFAULT 0,
  perm_rev         INTEGER NOT NULL DEFAULT 0,
  mem_rev          INTEGER NOT NULL DEFAULT 0,
  permissions      TEXT NOT NULL DEFAULT '{}',
  context_summary  TEXT NOT NULL DEFAULT '',
  context_rev      INTEGER NOT NULL DEFAULT 0,
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

CREATE TABLE workers (
  id               TEXT PRIMARY KEY,
  title            TEXT NOT NULL DEFAULT '',
  vm               TEXT NOT NULL DEFAULT 'local',
  workspace        TEXT NOT NULL DEFAULT '',
  worktree         TEXT NOT NULL DEFAULT '',
  base_commit      TEXT NOT NULL DEFAULT '',
  head_commit      TEXT NOT NULL DEFAULT '',
  program          TEXT NOT NULL DEFAULT '',
  agent_kind       TEXT NOT NULL DEFAULT '',
  boot_id          TEXT NOT NULL DEFAULT '',
  pid              INTEGER,
  pid_start_time   TEXT NOT NULL DEFAULT '',
  state            TEXT NOT NULL
                     CHECK(state IN ('starting','running','waiting_for_user','waiting_for_confirmation',
                                     'blocked','completed_candidate','completed_verified','failed',
                                     'paused','killed','lost')),
  rev              INTEGER NOT NULL DEFAULT 0,
  stall_count      INTEGER NOT NULL DEFAULT 0,
  owner_session    TEXT NOT NULL REFERENCES sessions(id),
  session_perm_rev INTEGER NOT NULL DEFAULT 0,
  permissions_hash TEXT NOT NULL DEFAULT '',
  compiled_config_path TEXT NOT NULL DEFAULT '',
  task             TEXT NOT NULL DEFAULT '',
  run_reason       TEXT NOT NULL DEFAULT '',
  parent_worker_id TEXT REFERENCES workers(id),
  delegation_depth INTEGER NOT NULL DEFAULT 0,
  role             TEXT NOT NULL DEFAULT '',
  summary          TEXT NOT NULL DEFAULT '',
  last_seen_at     TEXT NOT NULL,
  last_event_at    TEXT NOT NULL,
  pooled_at        TEXT,
  created_at       TEXT NOT NULL
);
CREATE INDEX idx_workers_state ON workers(state);
CREATE INDEX idx_workers_vm ON workers(vm);
CREATE INDEX idx_workers_session ON workers(owner_session);
CREATE INDEX idx_workers_pooled ON workers(pooled_at) WHERE pooled_at IS NOT NULL;

CREATE TABLE events (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  source             TEXT NOT NULL DEFAULT 'internal',
  source_event_id    TEXT,
  source_event_hash  TEXT,
  worker_id          TEXT REFERENCES workers(id),
  session_id         TEXT REFERENCES sessions(id),
  kind               TEXT NOT NULL,
  actor              TEXT NOT NULL DEFAULT '',
  causation_event_id INTEGER REFERENCES events(id),
  correlation_id     TEXT,
  payload            TEXT NOT NULL DEFAULT '{}',
  occurred_at        TEXT NOT NULL,
  recorded_at        TEXT NOT NULL,
  UNIQUE(source, source_event_id)
);
CREATE INDEX idx_events_worker ON events(worker_id, id);
CREATE INDEX idx_events_session ON events(session_id, id);
CREATE INDEX idx_events_kind ON events(session_id, kind, id);
CREATE INDEX idx_events_recorded ON events(recorded_at, id);

CREATE TABLE brain_transcript_rows (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id    TEXT NOT NULL REFERENCES sessions(id),
  role          TEXT NOT NULL,
  content       TEXT NOT NULL,
  active        INTEGER NOT NULL DEFAULT 1,
  compacted     INTEGER NOT NULL DEFAULT 0,
  source_events TEXT NOT NULL DEFAULT '[]',
  tainted       INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL
);
CREATE INDEX idx_transcript_session ON brain_transcript_rows(session_id, active, id);
CREATE VIRTUAL TABLE transcript_fts USING fts5(
  content, session_id UNINDEXED, row_id UNINDEXED, tokenize='porter'
);

CREATE TABLE capability_catalog (
  capability      TEXT PRIMARY KEY,
  action_class    TEXT NOT NULL CHECK(action_class IN ('routine','ambiguous','danger')),
  tier            TEXT NOT NULL CHECK(tier IN ('low','medium','high_blast')),
  default_allowed INTEGER NOT NULL DEFAULT 0,
  high_blast      INTEGER NOT NULL DEFAULT 0,
  compiled_worker INTEGER NOT NULL DEFAULT 0,
  description     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE session_grants (
  id                   TEXT PRIMARY KEY,
  session_id           TEXT NOT NULL REFERENCES sessions(id),
  capability           TEXT NOT NULL REFERENCES capability_catalog(capability),
  status               TEXT NOT NULL DEFAULT 'active'
                         CHECK(status IN ('active','revoked','expired')),
  scope                TEXT NOT NULL DEFAULT 'session' CHECK(scope IN ('session','once')),
  source_escalation_id TEXT,
  granted_by           TEXT NOT NULL DEFAULT '',
  created_perm_rev     INTEGER NOT NULL,
  use_count            INTEGER NOT NULL DEFAULT 0,
  expires_at           TEXT,
  created_at           TEXT NOT NULL,
  revoked_at           TEXT
);
CREATE INDEX idx_grants_active ON session_grants(session_id, capability) WHERE status='active';

CREATE TABLE escalations (
  id                 TEXT PRIMARY KEY,
  worker_id          TEXT REFERENCES workers(id),
  session_id         TEXT REFERENCES sessions(id),
  kind               TEXT NOT NULL DEFAULT 'question' CHECK(kind IN ('question','confirm')),
  question_class     TEXT NOT NULL DEFAULT 'other'
                       CHECK(question_class IN ('clarify','proceed-confirmation','scope-change','resource','other')),
  action_class       TEXT NOT NULL DEFAULT 'ambiguous'
                       CHECK(action_class IN ('routine','ambiguous','danger')),
  tier               TEXT NOT NULL DEFAULT 'medium' CHECK(tier IN ('low','medium','high_blast')),
  capability         TEXT,
  action_fingerprint TEXT NOT NULL DEFAULT '',
  requested_event_id INTEGER REFERENCES events(id),
  classifier         TEXT NOT NULL DEFAULT '',
  classifier_version TEXT NOT NULL DEFAULT '',
  action             TEXT NOT NULL,
  detail             TEXT NOT NULL DEFAULT '{}',
  draft_answer       TEXT NOT NULL DEFAULT '',
  draft_confidence   REAL NOT NULL DEFAULT 0,
  brain_rationale    TEXT NOT NULL DEFAULT '',
  answered_by        TEXT NOT NULL DEFAULT '' CHECK(answered_by IN ('','brain','human')),
  status             TEXT NOT NULL DEFAULT 'pending'
                       CHECK(status IN ('pending','answered','approved','rejected','expired')),
  decision           TEXT NOT NULL DEFAULT '',
  answer_text        TEXT NOT NULL DEFAULT '',
  decided_by         TEXT NOT NULL DEFAULT '',
  once_or_always     TEXT NOT NULL DEFAULT 'once' CHECK(once_or_always IN ('once','always')),
  requested_at       TEXT NOT NULL,
  decided_at         TEXT,
  resumed_at         TEXT
);
CREATE UNIQUE INDEX idx_escalations_pending
  ON escalations(worker_id, COALESCE(capability,'')) WHERE status='pending';
CREATE INDEX idx_escalations_status ON escalations(session_id, status);

-- provider pools + leases: RATE-LIMIT / CONCURRENCY orchestration only (NO cost).
CREATE TABLE provider_pools (
  id                 TEXT PRIMARY KEY,
  provider           TEXT NOT NULL,
  org                TEXT NOT NULL DEFAULT '',
  clavis_profile     TEXT NOT NULL,
  model_class        TEXT NOT NULL DEFAULT '',
  max_active         INTEGER NOT NULL DEFAULT 35,
  max_starts_per_min INTEGER NOT NULL DEFAULT 10,
  state              TEXT NOT NULL DEFAULT 'ok' CHECK(state IN ('ok','cooldown','disabled')),
  cooldown_until     TEXT,
  created_at         TEXT NOT NULL
);
CREATE TABLE worker_pool_leases (
  id                       TEXT PRIMARY KEY,
  pool_id                  TEXT NOT NULL REFERENCES provider_pools(id),
  worker_id                TEXT REFERENCES workers(id),
  dispatch_intent_event_id INTEGER REFERENCES events(id),
  acquired_at              TEXT NOT NULL,
  expires_at               TEXT NOT NULL,
  released_at              TEXT
);
CREATE INDEX idx_leases_pool_active ON worker_pool_leases(pool_id) WHERE released_at IS NULL;

-- budgets + breakers + usage: CUT (cost tracking/metering deferred post-MVP).

CREATE TABLE vms (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  boot_id      TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL DEFAULT 'ok',
  last_seen_at TEXT NOT NULL,
  created_at   TEXT NOT NULL
);
CREATE TABLE vm_observations (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  vm_id          TEXT NOT NULL REFERENCES vms(id),
  observed_at    TEXT NOT NULL,
  agents_json    TEXT NOT NULL DEFAULT '[]',
  git_heads_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_vmobs_vm ON vm_observations(vm_id, observed_at);

CREATE TABLE memory_revisions (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  mem_rev    INTEGER NOT NULL,
  op         TEXT NOT NULL CHECK(op IN ('add','update','expire')),
  fact_id    TEXT NOT NULL DEFAULT '',
  topic      TEXT NOT NULL DEFAULT '',
  author     TEXT NOT NULL DEFAULT '' CHECK(author IN ('user','external','')),
  decided_by TEXT NOT NULL,
  at         TEXT NOT NULL
);

CREATE TABLE playbooks (
  id TEXT PRIMARY KEY, title TEXT NOT NULL, body TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'active' CHECK(state IN ('active','stale','archived')),
  pinned INTEGER NOT NULL DEFAULT 0, use_count INTEGER NOT NULL DEFAULT 0,
  last_used_at TEXT, created_at TEXT NOT NULL
);

-- SEED: the protected pool session (fixed 26-char Crockford ULID), before any worker can exist.
INSERT INTO sessions (id, slug, title, goal, status, kind, parent_session, permissions,
                      last_activity_at, created_at)
VALUES ('00000000000000000000000000', 'pool', 'Worker Pool (protected)', '', 'active', 'pool', NULL, '{}',
        '1970-01-01T00:00:00.000000000Z', '1970-01-01T00:00:00.000000000Z');

-- SEED: capability_catalog. high_blast=1 ⇒ compiled_worker=0 (never on a worker).
INSERT INTO capability_catalog (capability, action_class, tier, default_allowed, high_blast, compiled_worker, description) VALUES
 ('git.branch.create',   'routine','low',    1,0,1,'create a branch in-worktree'),
 ('git.commit',          'routine','low',    1,0,1,'commit in-worktree'),
 ('git.push.feature',    'routine','low',    1,0,1,'push its own feature branch'),
 ('git.pr.open',         'routine','low',    1,0,1,'open a PR'),
 ('git.pr.update',       'routine','low',    1,0,1,'update its own PR'),
 ('fs.worktree',         'routine','low',    1,0,1,'read/write inside its own worktree'),
 ('exec.tests',          'routine','low',    1,0,1,'run tests'),
 ('exec.build',          'routine','low',    1,0,1,'run build'),
 ('exec.lint',           'routine','low',    1,0,1,'run lint'),
 ('exec.install',        'routine','low',    1,0,1,'install deps'),
 ('handoff',             'routine','low',    1,0,0,'hand ownership back to arco'),
 ('git.pr.merge',        'ambiguous','medium',0,0,0,'merge a PR (arco-executed; server-side review backs it)'),
 ('git.push.shared',     'ambiguous','medium',0,0,0,'push a shared branch others use'),
 ('net.fetch',           'ambiguous','medium',0,0,0,'outbound network fetch (DEFAULT OFF)'),
 ('spawn.subworker',     'ambiguous','medium',0,0,0,'self-spawn a helper (DEFAULT OFF)'),
 ('memory.cross-project','ambiguous','medium',0,0,0,'recall memory outside {global, session.repo} (DEFAULT OFF)'),
 ('fleet.claim',         'ambiguous','high_blast',0,1,0,'claim a worker from the pool'),
 ('fleet.transfer',      'ambiguous','high_blast',0,1,0,'transfer a worker between sessions'),
 ('fleet.move',          'ambiguous','high_blast',0,1,0,'move a session subtree'),
 ('git.push.main',       'danger','high_blast',0,1,0,'push to main/protected (arco-executed after confirm)'),
 ('external.deploy',     'danger','high_blast',0,1,0,'deploy'),
 ('external.spend',      'danger','high_blast',0,1,0,'spend money / call a paid external API (safety cap)'),
 ('external.send',       'danger','high_blast',0,1,0,'send outbound comms'),
 ('secrets.read',        'danger','high_blast',0,1,0,'read secrets'),
 ('fs.destructive',      'danger','high_blast',0,1,0,'destructive delete outside worktree');
