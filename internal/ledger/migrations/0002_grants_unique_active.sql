-- 0002: enforce at most one ACTIVE grant per (session, capability) at the DB
-- level, so idempotent Grant is safe even if the single-writer invariant ever
-- loosens (defense-in-depth; qwen code-review). DDL only; Store.Migrate records
-- the bookkeeping row.
CREATE UNIQUE INDEX idx_grants_active_uniq
  ON session_grants(session_id, capability) WHERE status='active';
