package reconcile

// GUIDELINE TESTS — rev7 T3.1 (verification leg 1: CI check-runs).
//
// Pinned surface:
//   - type CIRunner func(ctx context.Context, dir string, args ...string) ([]byte, error)
//   - type CICfg struct { Enabled bool; Runner CIRunner } (at least these fields)
//   - Engine.CI CICfg — polled inside e.Sweep for completed_candidate workers only.
//
// Pinned semantics:
//   - Runner is invoked with dir == the worker's Worktree and the worker's
//     HeadCommit somewhere in args (gh resolves owner/repo from the worktree).
//   - Runner output is GitHub's check-runs JSON:
//     {"total_count":N,"check_runs":[{"name":...,"status":...,"conclusion":...}]}
//   - all completed+success  → exactly ONE "verification_artifact" event (idempotent
//     across sweeps); worker STAYS completed_candidate (human verify gate untouched).
//   - any failure-ish conclusion → ONE pending "confirm" escalation (kind column is
//     CHECK-constrained to question|confirm), no artifact event.
//   - any run still pending / zero check-runs → no event, no escalation (wait).
//   - runner error (network/gh hiccup) → no event, no escalation (transient).
//   - gate off (Enabled=false) → runner never called; nil Runner must not panic.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
)

type ciCall struct {
	dir  string
	args []string
}

func fakeCIRunner(calls *[]ciCall, out *string, errOut *error) CIRunner {
	return func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		*calls = append(*calls, ciCall{dir: dir, args: args})
		if errOut != nil && *errOut != nil {
			return nil, *errOut
		}
		return []byte(*out), nil
	}
}

// mkCandidate seeds a worker in completed_candidate with a known worktree+head.
func mkCandidate(t *testing.T, e *Engine, s *ledger.Store, worktree, head string) string {
	t.Helper()
	id := mkRunning(t, e, s, worktree, head)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		w, err := tx.GetWorker(id)
		if err != nil {
			return err
		}
		return tx.TransitionWorker(id, core.WorkerCompletedCandidate, w.Rev,
			core.Event{Kind: "state_change", WorkerID: id, SessionID: w.OwnerSession, Payload: "{}"})
	}))
	return id
}

func countArtifacts(t *testing.T, s *ledger.Store, workerID string) (int, core.Event) {
	t.Helper()
	evs, err := s.Reader().RecentWorkerEvents(workerID, 100)
	require.NoError(t, err)
	n, last := 0, core.Event{}
	for _, ev := range evs {
		if ev.Kind == "verification_artifact" {
			n++
			last = ev
		}
	}
	return n, last
}

const ciJSONSuccess = `{"total_count":2,"check_runs":[
  {"name":"build","status":"completed","conclusion":"success"},
  {"name":"test","status":"completed","conclusion":"success"}]}`

const ciJSONFailure = `{"total_count":2,"check_runs":[
  {"name":"build","status":"completed","conclusion":"success"},
  {"name":"test","status":"completed","conclusion":"failure"}]}`

const ciJSONPending = `{"total_count":2,"check_runs":[
  {"name":"build","status":"completed","conclusion":"success"},
  {"name":"test","status":"in_progress","conclusion":null}]}`

const ciJSONEmpty = `{"total_count":0,"check_runs":[]}`

func TestCIVerify_SuccessEmitsOneArtifactAndKeepsHumanGate(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkCandidate(t, e, s, "/wt/ci-ok", "cafebabe1")
	fake.Agents = nil // agent gone — expected for a candidate

	var calls []ciCall
	out := ciJSONSuccess
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)

	// Runner saw the worker's worktree and head SHA.
	require.NotEmpty(t, calls, "CI runner must be invoked for a completed_candidate worker")
	require.Equal(t, "/wt/ci-ok", calls[0].dir)
	require.Contains(t, strings.Join(calls[0].args, " "), "cafebabe1")

	n, ev := countArtifacts(t, s, id)
	require.Equal(t, 1, n, "exactly one verification_artifact event")
	require.True(t, json.Valid([]byte(ev.Payload)), "artifact payload must be JSON")
	require.Contains(t, ev.Payload, "success")

	// CI success is evidence, NOT verification — the human diff-gate (Verify)
	// remains the only path to completed_verified.
	require.Equal(t, core.WorkerCompletedCandidate, mustWorker(t, s, id).State)

	// Idempotent: a second sweep must not duplicate the artifact.
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	n, _ = countArtifacts(t, s, id)
	require.Equal(t, 1, n, "artifact event must be idempotent across sweeps")
}

func TestCIVerify_FailureOpensOneConfirmEscalation(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkCandidate(t, e, s, "/wt/ci-bad", "deadbeef2")
	fake.Agents = nil

	var calls []ciCall
	out := ciJSONFailure
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)

	n, _ := countArtifacts(t, s, id)
	require.Equal(t, 0, n, "no artifact on CI failure")

	escs, err := s.Reader().ListEscalations(core.EscalationFilter{WorkerID: id, Status: "pending"})
	require.NoError(t, err)
	require.Len(t, escs, 1, "CI failure must escalate")
	require.Equal(t, "confirm", escs[0].Kind, "escalations.kind is CHECK-constrained to question|confirm")
	require.Contains(t, strings.ToLower(escs[0].Action+" "+escs[0].Detail), "ci")

	// Second sweep: still exactly one pending escalation (dedup).
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	escs, err = s.Reader().ListEscalations(core.EscalationFilter{WorkerID: id, Status: "pending"})
	require.NoError(t, err)
	require.Len(t, escs, 1)
	require.Equal(t, core.WorkerCompletedCandidate, mustWorker(t, s, id).State)
}

func TestCIVerify_PendingWaitsThenSucceeds(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkCandidate(t, e, s, "/wt/ci-wait", "feedface3")
	fake.Agents = nil

	var calls []ciCall
	out := ciJSONPending
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}

	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	n, _ := countArtifacts(t, s, id)
	require.Equal(t, 0, n, "in-progress checks: no artifact yet")
	escs, err := s.Reader().ListEscalations(core.EscalationFilter{WorkerID: id, Status: "pending"})
	require.NoError(t, err)
	require.Empty(t, escs, "in-progress checks: no escalation")

	// Zero check-runs is also "pending" (checks may not have been created yet).
	out = ciJSONEmpty
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	n, _ = countArtifacts(t, s, id)
	require.Equal(t, 0, n, "zero check-runs treated as pending, not success")

	// Checks complete later → artifact appears.
	out = ciJSONSuccess
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	n, _ = countArtifacts(t, s, id)
	require.Equal(t, 1, n)
}

func TestCIVerify_RunnerErrorIsTransientNoise(t *testing.T) {
	e, s, fake := newEngine(t)
	id := mkCandidate(t, e, s, "/wt/ci-err", "badc0ffee")
	fake.Agents = nil

	var calls []ciCall
	out := ""
	rerr := error(context.DeadlineExceeded)
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, &rerr)}

	_, err := e.Sweep(context.Background())
	require.NoError(t, err, "a gh/network hiccup must not fail the sweep")
	n, _ := countArtifacts(t, s, id)
	require.Equal(t, 0, n)
	escs, err := s.Reader().ListEscalations(core.EscalationFilter{WorkerID: id, Status: "pending"})
	require.NoError(t, err)
	require.Empty(t, escs, "transient runner errors must not escalate")

	// Recovery: once the runner works again, the artifact lands.
	rerr = nil
	out = ciJSONSuccess
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	n, _ = countArtifacts(t, s, id)
	require.Equal(t, 1, n)
}

func TestCIVerify_GateOffAndNonCandidatesNeverPolled(t *testing.T) {
	e, s, fake := newEngine(t)
	cand := mkCandidate(t, e, s, "/wt/ci-off", "aaaa1111")
	run := mkRunning(t, e, s, "/wt/ci-run", "bbbb2222")
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + run, Alive: true}}

	var calls []ciCall
	out := ciJSONSuccess

	// Disabled gate: runner never invoked, even with a candidate present.
	e.CI = CICfg{Enabled: false, Runner: fakeCIRunner(&calls, &out, nil)}
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Empty(t, calls, "Enabled=false must mean zero runner calls")
	n, _ := countArtifacts(t, s, cand)
	require.Equal(t, 0, n)

	// Enabled but nil runner: sweep must not panic or error.
	e.CI = CICfg{Enabled: true, Runner: nil}
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)

	// Enabled with runner: only the candidate is polled, never the running worker.
	e.CI = CICfg{Enabled: true, Runner: fakeCIRunner(&calls, &out, nil)}
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	for _, c := range calls {
		require.NotEqual(t, "/wt/ci-run", c.dir, "running workers are not CI-polled")
	}
}
