-- D9 supervision modes (plan-rev7): every session carries a supervision_mode
-- gating how much AUTONOMY arco has over its workers (manual = observe + ledger
-- only; assist = notify + draft, never auto-act; auto = brain acting steps
-- execute). Operator-initiated actions are never gated by the mode. Default is
-- assist — the safe middle: arco surfaces decisions but a human executes them.
ALTER TABLE sessions ADD COLUMN supervision_mode TEXT NOT NULL DEFAULT 'assist'
  CHECK (supervision_mode IN ('auto','assist','manual'));
