-- Indexes for the stale-brain-intent crash-recovery query (StaleBrainIntents),
-- which the sweep runs every pass. Without these it full-scans the events table
-- (the largest on a long-running daemon): the base predicate filters on
-- kind='brain_intent' (idx_events_kind leads with session_id, unusable here) and
-- the resolution check probes correlation_id (previously unindexed).
--
-- idx_events_kind_worker makes both the kind='brain_intent' scan and the
-- per-worker MAX(id) subquery sargable; the partial correlation index makes the
-- NOT EXISTS cid-sibling probe an index lookup (and stays small — only
-- brain-correlated events carry a correlation_id).
CREATE INDEX idx_events_kind_worker ON events(kind, worker_id, id);
CREATE INDEX idx_events_correlation ON events(correlation_id, id) WHERE correlation_id IS NOT NULL;
