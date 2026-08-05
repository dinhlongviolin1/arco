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
	now, err := time.Parse(time.RFC3339Nano, t.now())
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-window)
	pre := cutoff.Add(-time.Second).UTC().Format(secondFmt)
	rows, err := t.q.QueryContext(context.Background(),
		`SELECT recorded_at FROM events WHERE session_id=? AND kind='brain_intent' AND recorded_at>=?`,
		sessionID, pre)
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
