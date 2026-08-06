-- Worker↔agent correlation ref (Task-S spike / herdr-contract.md): herdr
-- identifies an agent by its pane_id, not arco's "arco_<ulid>" workspace, so the
-- sweep must correlate liveness by a ref captured at launch. Empty '' means "not
-- launched via the arco-owned spawn path yet" → the sweep falls back to the
-- workspace match (backward compatible; Fake/Prompt-model workers keep working).
ALTER TABLE workers ADD COLUMN agent_ref TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_workers_agent_ref ON workers(agent_ref) WHERE agent_ref <> '';
