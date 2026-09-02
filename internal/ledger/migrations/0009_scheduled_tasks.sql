-- 0009: scheduled (recurring/planned) tasks. Each task is a recurring UNATTENDED
-- agent run: when due, arco runs the chat brain (Converse) with the full tool
-- surface + the task's own durable memory, in the task's own session/topic, so a
-- monitoring task can inspect and (confirm-gated) act on the fleet. Additive: a
-- new table only, nothing existing is touched.
CREATE TABLE scheduled_tasks (
  id           TEXT PRIMARY KEY,                        -- ULID
  name         TEXT NOT NULL,                           -- human label + topic name
  schedule     TEXT NOT NULL,                           -- canonical schedule spec (cron "0 8 * * *" or interval "30m")
  prompt       TEXT NOT NULL,                           -- the agentic instruction the run receives
  session_id   TEXT NOT NULL REFERENCES sessions(id),   -- the task's own session: its topic + durable memory
  enabled      INTEGER NOT NULL DEFAULT 1,
  next_run     TEXT NOT NULL,                           -- RFC3339Nano UTC — when it next fires
  last_run     TEXT,                                    -- RFC3339Nano UTC — last fire (NULL until first run)
  last_status  TEXT NOT NULL DEFAULT '',                -- '' | ok | error
  last_result  TEXT NOT NULL DEFAULT '',                -- last run's short outcome (for /schedule list)
  created_at   TEXT NOT NULL
);

-- The scheduler's hot query: enabled tasks ordered by when they are next due.
CREATE INDEX idx_scheduled_due ON scheduled_tasks(enabled, next_run);
