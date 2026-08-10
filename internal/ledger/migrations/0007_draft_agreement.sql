-- rev7/T3.5 autonomy earn-out: per-question_class tally of HUMAN decisions on
-- DRAFTED escalations — did the human's decision agree with the brain's draft?
-- A dedicated tally table (not an events replay) so DraftAgreement is one
-- indexed-row read on every sweep and survives restart by construction. Rows
-- are written ONLY inside the human decide() tx; a brain auto-answer never
-- touches it (it would ratify itself). The class enum mirrors escalations'
-- frozen 0001 CHECK.
CREATE TABLE draft_agreement (
  question_class TEXT PRIMARY KEY
    CHECK(question_class IN ('clarify','proceed-confirmation','scope-change','resource','other')),
  agree INTEGER NOT NULL DEFAULT 0 CHECK(agree >= 0),
  total INTEGER NOT NULL DEFAULT 0 CHECK(total >= agree)
);

-- Operator-facing alias: 0001 named the standing-grant table session_grants,
-- but the audit surface (T3.5 guideline tests, operator SQL) addresses it as
-- plain `grants`. A view aliases the name without a table rebuild; SQLite
-- views are read-only without INSTEAD OF triggers, so no write path can ever
-- target the alias.
CREATE VIEW grants AS SELECT * FROM session_grants;
