-- 0008: per-worker permission grants (issue model). A grant can now be scoped to
-- a single WORKER (worker_id set) instead of the whole session (worker_id NULL =
-- today's session-wide baseline). So agents working different aspects of one
-- issue-session don't inherit each other's approvals. Additive: every existing
-- grant row has NULL worker_id and stays session-wide.
ALTER TABLE session_grants ADD COLUMN worker_id TEXT REFERENCES workers(id);

-- Replace the (session, capability) active-uniqueness with (session, worker,
-- capability), so a session-wide grant and a per-worker grant for the same
-- capability can coexist. COALESCE(worker_id,'') buckets all session-wide rows
-- together (NULLs would otherwise each be distinct), preserving today's
-- one-active-session-wide-grant-per-capability invariant.
DROP INDEX idx_grants_active_uniq;
CREATE UNIQUE INDEX idx_grants_active_uniq
  ON session_grants(session_id, COALESCE(worker_id, ''), capability) WHERE status='active';
