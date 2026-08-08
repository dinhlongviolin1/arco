// freeze_migrate_test.go closes the build-guide-rev6 §E "Freeze:
// migrate-from-fixture (replay==fingerprint; one pool row;
// high-blast⇒not-compiled; DefaultTree==catalog)" debt.
//
// A fixture database is built at schema 0001 ONLY (the frozen init migration,
// applied verbatim from the embedded file so the fixture can never drift from
// the real 0001), seeded with session/worker/event/pool/grant rows in 0001's
// column shapes, closed, then reopened and migrated through ALL current
// migrations (0002+). The point: OLD DBs keep their meaning after every new
// migration.
//
// No replay-fingerprint helper exists in the codebase (the event log has no
// derived cursor state to rebuild), so "replay==fingerprint" is asserted as:
// the events table survives migration byte-identically — same count, same
// (id, kind, actor, payload) sequence — and re-reading it through the Reader's
// replay API (EventsSince) yields the same stream and cursor.
package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/permcompile"
)

// fixtureEventRow is the replay-relevant identity of one events row.
type fixtureEventRow struct {
	ID      int64
	Kind    string
	Actor   string
	Payload string
}

func dumpEventRows(t *testing.T, db *sql.DB) []fixtureEventRow {
	t.Helper()
	rows, err := db.Query(`SELECT id, kind, actor, payload FROM events ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []fixtureEventRow
	for rows.Next() {
		var r fixtureEventRow
		require.NoError(t, rows.Scan(&r.ID, &r.Kind, &r.Actor, &r.Payload))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// readCompiledPermissions parses the permcompile settings.json in dir into its
// allow/ask/deny lists. Shared with grant_security_test.go.
func readCompiledPermissions(t *testing.T, dir string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	require.NoError(t, err)
	var s struct {
		Permissions struct {
			Allow, Ask, Deny []string
		} `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal(b, &s))
	return map[string][]string{"allow": s.Permissions.Allow, "ask": s.Permissions.Ask, "deny": s.Permissions.Deny}
}

func TestMigrate_FromFixture_FreezeInvariants(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fixture.db")

	// ---- phase 1: build the fixture at schema 0001 ------------------------
	ms, err := loadMigrations()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(ms), 2, "this test exists to prove 0002+ preserve old DBs")
	require.Equal(t, 1, ms[0].version, "the first migration must be the frozen 0001 init")

	s1, err := Open(path)
	require.NoError(t, err)
	// Apply ONLY migration 0001, exactly as Store.Migrate would: the embedded
	// file's DDL plus the bookkeeping row with the file-bytes checksum, so the
	// later full Migrate resumes at 0002 (and would refuse on checksum drift).
	_, err = s1.DB().Exec(ms[0].sqlText)
	require.NoError(t, err)
	_, err = s1.DB().Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?,?,?,?)`,
		ms[0].version, ms[0].name, ms[0].sum, s1.now())
	require.NoError(t, err)

	const (
		ts       = "2026-01-01T00:00:00.000000000Z"
		sessID   = "01FIXTURESESSION0000000000"
		workerID = "01FIXTUREWORKER00000000000"
		poolID   = "pool-fixture"
	)
	// Seed rows in 0001 column shapes ONLY (no agent_ref, no intake_uid — those
	// columns arrive in 0003/0005). Raw SQL on purpose: Store's Go writers speak
	// the CURRENT schema and would not run on a 0001-era daemon.
	seed := func(q string, args ...any) {
		t.Helper()
		_, err := s1.DB().Exec(q, args...)
		require.NoError(t, err)
	}
	seed(`INSERT INTO sessions (id, goal, status, kind, perm_rev, permissions, last_activity_at, created_at)
	      VALUES (?,?,?,?,?,?,?,?)`, sessID, "ship the fixture", "active", "work", 2, "{}", ts, ts)
	seed(`INSERT INTO workers (id, workspace, state, owner_session, task, run_reason,
	      last_seen_at, last_event_at, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		workerID, "arco_"+workerID, "running", sessID, "fixture task", "dispatch", ts, ts, ts)
	seed(`INSERT INTO events (source, worker_id, session_id, kind, actor, payload, occurred_at, recorded_at)
	      VALUES ('internal',?,?,?,?,?,?,?)`,
		workerID, sessID, "dispatch_intent", "cli", `{"task":"fixture task","workspace":"arco_`+workerID+`"}`, ts, ts)
	seed(`INSERT INTO events (source, worker_id, session_id, kind, actor, payload, occurred_at, recorded_at)
	      VALUES ('internal',?,?,?,?,?,?,?)`,
		workerID, sessID, "dispatch_done", "", `{}`, ts, ts)
	seed(`INSERT INTO events (source, worker_id, session_id, kind, actor, payload, occurred_at, recorded_at)
	      VALUES ('internal',?,?,?,?,?,?,?)`,
		workerID, sessID, "state_change", "", `{"herdr_state":"working","target":"running"}`, ts, ts)
	seed(`INSERT INTO provider_pools (id, provider, clavis_profile, created_at) VALUES (?,?,?,?)`,
		poolID, "anthropic", "work-profile", ts)
	// Grants: a medium capability (should keep compiling to `ask`), and —
	// adversarially — a HIGH-BLAST capability, so the post-migration compile
	// must prove high-blast never reaches worker settings even when granted.
	seed(`INSERT INTO session_grants (id, session_id, capability, status, scope, granted_by, created_perm_rev, created_at)
	      VALUES (?,?,?,?,?,?,?,?)`, "grant-fixture-1", sessID, "net.fetch", "active", "session", "cli", 1, ts)
	seed(`INSERT INTO session_grants (id, session_id, capability, status, scope, granted_by, created_perm_rev, created_at)
	      VALUES (?,?,?,?,?,?,?,?)`, "grant-fixture-2", sessID, "git.push.main", "active", "session", "cli", 2, ts)

	preEvents := dumpEventRows(t, s1.DB())
	require.Len(t, preEvents, 3)
	require.NoError(t, s1.Close())

	// ---- phase 2: reopen and migrate through ALL current migrations --------
	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { s2.Close() })
	require.NoError(t, s2.Migrate(ctx))

	var applied int
	require.NoError(t, s2.DB().QueryRow(`SELECT COUNT(1) FROM schema_migrations`).Scan(&applied))
	require.Equal(t, len(ms), applied, "every embedded migration applied on top of the 0001 fixture")

	// Invariant 1 — replay == fingerprint: the event log is untouched (same
	// count, same (id, kind, actor, payload) sequence), and replaying it through
	// the Reader yields the same stream and final cursor.
	postEvents := dumpEventRows(t, s2.DB())
	require.Equal(t, preEvents, postEvents, "events must survive migration byte-identically")
	replayed, err := s2.Reader().EventsSince(0, 100)
	require.NoError(t, err)
	require.Len(t, replayed, len(preEvents))
	for i, ev := range replayed {
		require.Equal(t, preEvents[i].ID, ev.ID)
		require.Equal(t, preEvents[i].Kind, ev.Kind)
		require.Equal(t, preEvents[i].Actor, ev.Actor)
		require.Equal(t, preEvents[i].Payload, ev.Payload)
	}
	require.Equal(t, preEvents[len(preEvents)-1].ID, replayed[len(replayed)-1].ID, "replay cursor unchanged")

	// Old rows keep their meaning: the 0001-era worker reads back intact, with
	// the post-0001 columns at their backward-compatible defaults.
	w, err := s2.Reader().GetWorker(workerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State)
	require.Equal(t, "fixture task", w.Task)
	require.Equal(t, "", w.AgentRef, "0003 backfills agent_ref to '' (workspace-match fallback)")
	require.Nil(t, w.IntakeUID, "0005 backfills intake_uid to NULL (ungated legacy row)")

	// Invariant 2 — exactly one provider-pool row survives.
	var pools int
	require.NoError(t, s2.DB().QueryRow(`SELECT COUNT(1) FROM provider_pools`).Scan(&pools))
	require.Equal(t, 1, pools)
	p, err := s2.Reader().GetPool(poolID)
	require.NoError(t, err)
	require.Equal(t, "anthropic", p.Provider)
	require.Equal(t, "work-profile", p.ClavisProfile)

	// Invariant 3 — a HIGH-BLAST capability is NOT compiled into worker
	// settings, even though the fixture carries an active high-blast grant that
	// survived migration (and GrantedCapabilities therefore reports it).
	granted, err := s2.Reader().GrantedCapabilities(sessID)
	require.NoError(t, err)
	require.True(t, granted["net.fetch"], "the fixture's medium grant survived migration")
	require.True(t, granted["git.push.main"], "the adversarial high-blast grant survived migration")
	cat, err := s2.Reader().Catalog()
	require.NoError(t, err)
	cfgDir := t.TempDir()
	require.NoError(t, permcompile.Compile(cfgDir, t.TempDir(), granted, cat))
	perms := readCompiledPermissions(t, cfgDir)
	// git.push.main's tool patterns (mirrors permcompile's toolPatterns entry).
	hbPatterns := []string{
		"Bash(git push:* origin main:*)", "Bash(git push:* origin master:*)",
		"Bash(git push:* main:*)", "Bash(git push:* master:*)",
	}
	for _, pat := range hbPatterns {
		require.NotContains(t, perms["allow"], pat, "high-blast must never compile into allow")
		require.NotContains(t, perms["ask"], pat, "high-blast must never compile into ask")
		require.Contains(t, perms["deny"], pat, "high-blast patterns land in the deny belt")
	}
	require.Contains(t, perms["ask"], "WebFetch", "the granted medium capability still compiles to ask")

	// Invariant 4 — DefaultTree still matches the catalog after migration: it is
	// exactly the default_allowed rows, and never contains a high-blast one.
	tree, err := s2.Reader().DefaultTree()
	require.NoError(t, err)
	treeCaps := map[string]bool{}
	for _, row := range tree {
		require.True(t, row.DefaultAllowed)
		require.False(t, row.HighBlast, "DefaultTree must never carry a high-blast capability")
		treeCaps[row.Capability] = true
	}
	rows, err := s2.DB().Query(`SELECT capability FROM capability_catalog WHERE default_allowed<>0`)
	require.NoError(t, err)
	defer rows.Close()
	catCaps := map[string]bool{}
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		catCaps[c] = true
	}
	require.NoError(t, rows.Err())
	require.Equal(t, catCaps, treeCaps, "DefaultTree == the catalog's default_allowed set")
}
