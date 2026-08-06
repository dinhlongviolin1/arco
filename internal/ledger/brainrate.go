package ledger

import (
	"context"
	"time"
)

// CountRecentBrainCalls counts brain_intent events for sessionID within the last
// window (per-session brain-rate admission). Same lexically-safe strategy as the
// lease start-rate window: a coarse whole-second SQL prefilter (RFC3339Nano
// trims fractional zeros, so a full-precision string range isn't chronologically
// ordered) plus an exact in-Go instant compare. The prefilter bounds the scanned
// set to ~one window of this session's brain_intent events.
func (t *txn) CountRecentBrainCalls(sessionID string, window time.Duration) (int, error) {
	return t.countRecentEventsByKind("session_id", sessionID, "brain_intent", window)
}

// CountRecentRollups counts rollup_intent events for one PARENT WORKER within
// the last window — the coalescing denominator for supersession rollup (≤1 per
// PARENT per interval). Per-parent, not per-session: a depth-2 session can hold
// more than one parent (a child that itself delegated), and per-session
// coalescing would let one starve the other (opus review).
func (t *txn) CountRecentRollups(parentWorkerID string, window time.Duration) (int, error) {
	return t.countRecentEventsByKind("worker_id", parentWorkerID, "rollup_intent", window)
}

// countRecentEventsByKind counts events of one kind scoped by a single column
// (session_id or worker_id) within the last window. Coarse whole-second SQL
// prefilter (RFC3339Nano trims fractional zeros → a full-precision string range
// isn't chronologically ordered) + exact in-Go instant compare; the prefilter
// bounds the scanned set to ~one window.
func (t *txn) countRecentEventsByKind(scopeCol, scopeVal, kind string, window time.Duration) (int, error) {
	now, err := time.Parse(time.RFC3339Nano, t.now())
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-window)
	pre := cutoff.Add(-time.Second).UTC().Format(secondFmt)
	// scopeCol is a fixed internal literal ("session_id"|"worker_id"), never user
	// input — safe to interpolate; scopeVal/kind stay parameterized.
	rows, err := t.q.QueryContext(context.Background(),
		`SELECT recorded_at FROM events WHERE `+scopeCol+`=? AND kind=? AND recorded_at>=?`,
		scopeVal, kind, pre)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil {
			return 0, err
		}
		ts, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			continue
		}
		if !ts.Before(cutoff) {
			n++
		}
	}
	return n, rows.Err()
}
