package reconcile

// Supplementary T3.1 tests (beyond the committed guideline file): mapping
// edges and the across-restart idempotency pin.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// A failure-ish conclusion escalates even while other checks are still running.
func TestCIVerify_FailureWinsOverPending(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkCandidate(t, e, s, "/wt/ci-mix", "abadcafe4")
	fake.Agents = nil

	var calls []ciCall
	out := `{"total_count":2,"check_runs":[
	  {"name":"build","status":"in_progress","conclusion":null},
	  {"name":"test","status":"completed","conclusion":"timed_out"}]}`
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	n, _ := countArtifacts(t, s, id)
	require.Equal(t, 0, n)
	escs, err := s.Reader().ListEscalations(core.EscalationFilter{WorkerID: id, Status: "pending"})
	require.NoError(t, err)
	require.Len(t, escs, 1, "a timed_out check escalates even with runs still pending")
	require.Contains(t, escs[0].Detail, "test", "the failed check is named")
}

// neutral / skipped conclusions are non-blocking: all-completed with them is green.
func TestCIVerify_NeutralAndSkippedAreNonBlocking(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkCandidate(t, e, s, "/wt/ci-neutral", "beefbeef5")
	fake.Agents = nil

	var calls []ciCall
	out := `{"total_count":3,"check_runs":[
	  {"name":"build","status":"completed","conclusion":"success"},
	  {"name":"lint","status":"completed","conclusion":"neutral"},
	  {"name":"docs","status":"completed","conclusion":"skipped"}]}`
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	n, _ := countArtifacts(t, s, id)
	require.Equal(t, 1, n)
	escs, err := s.Reader().ListEscalations(core.EscalationFilter{WorkerID: id, Status: "pending"})
	require.NoError(t, err)
	require.Empty(t, escs)
}

// A candidate with no recorded head commit has nothing addressable to poll.
func TestCIVerify_EmptyHeadSkipsPoll(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkCandidate(t, e, s, "/wt/ci-nohead", "temp")
	_, err := s.DB().Exec(`UPDATE workers SET head_commit='' WHERE id=?`, id)
	require.NoError(t, err)
	fake.Agents = nil

	var calls []ciCall
	out := ciJSONSuccess
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}

	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	require.Empty(t, calls, "no head commit → no poll")
}

// Malformed runner output is a transient (runner-error-like): no event, no
// escalation, and a later well-formed response still lands the artifact.
func TestCIVerify_MalformedJSONIsTransient(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkCandidate(t, e, s, "/wt/ci-garbled", "d00dfeed6")
	fake.Agents = nil

	var calls []ciCall
	out := `not json at all`
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	n, _ := countArtifacts(t, s, id)
	require.Equal(t, 0, n)
	escs, err := s.Reader().ListEscalations(core.EscalationFilter{WorkerID: id, Status: "pending"})
	require.NoError(t, err)
	require.Empty(t, escs)

	out = ciJSONSuccess
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	n, _ = countArtifacts(t, s, id)
	require.Equal(t, 1, n)
}

// The artifact dedup is ledger-backed, not in-memory: a fresh Engine over the
// SAME store (a daemon restart) must not duplicate the artifact.
func TestCIVerify_ArtifactIdempotentAcrossRestart(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkCandidate(t, e, s, "/wt/ci-reboot", "0ddba1l7")
	fake.Agents = nil

	var calls []ciCall
	out := ciJSONSuccess
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)

	// "Restart": a brand-new Engine sharing the ledger, fresh in-memory state.
	e2 := New(s, fake)
	t.Cleanup(e2.Exec.Stop)
	e2.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}
	_, err = e2.Sweep(context.Background())
	require.NoError(t, err)

	n, _ := countArtifacts(t, s, id)
	require.Equal(t, 1, n, "one artifact per (worker, head) across restarts")
}
