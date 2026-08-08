// killnine_test.go closes the build-guide-rev6 §E "Crash matrix (kill-9 at
// every intent→done boundary)" debt for the dispatch and prompt (B9) receipt
// windows.
//
// Simulation technique: a REAL ledger DB file is driven through phase 1 with
// one Store/Engine + vm.Fake, then ABANDONED at the crash point (the post-crash
// steps are simply never run; the store is closed). Reopening the SAME file
// with a fresh Store + a fresh vm.Fake is the restarted daemon's view of the
// world: the fake's empty call log proves the new process has performed zero
// side effects. Because every receipt-window boundary is a committed tx, this
// exercises the same durability semantics as a SIGKILL between the two txs.
//
// Matrix rows:
//   - W1 pre-intent:                crash before the dispatch tx commits
//   - W2 post-intent / pre-execute: dispatch_intent committed, launch never ran
//   - W3 post-execute / pre-result: launch ran, dispatch_done never committed
//   - W4 prompt_intent redrive:     a brain classification lost mid-flight is
//     re-driven at most once; a committed prompt_intent is never re-executed
//
// Every row ends with an idempotence coda: a SECOND Recover+Sweep changes
// neither the ledger state nor the fake's call counts.
package test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// knClock is a settable test clock (mirrors the reconcile-package test clock)
// so the stale-brain-intent grace can be crossed deterministically.
type knClock struct{ ns atomic.Int64 }

func (c *knClock) now() time.Time   { return time.Unix(0, c.ns.Load()).UTC() }
func (c *knClock) set(tm time.Time) { c.ns.Store(tm.UnixNano()) }

// knOpen opens (or reopens) the ledger DB file at path and migrates it. No
// cleanup is registered: phase-1 stores are closed EXPLICITLY — that close is
// the crash point.
func knOpen(t *testing.T, path string) *ledger.Store {
	t.Helper()
	s, err := ledger.Open(path)
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	return s
}

// knRestart is "the restarted daemon": a fresh Store over the SAME DB file and
// a FRESH vm.Fake whose empty Prompts()/Launched() logs represent the new
// process having performed no side effects yet.
func knRestart(t *testing.T, path string) (*reconcile.Engine, *ledger.Store, *vm.Fake) {
	t.Helper()
	s := knOpen(t, path)
	fake := vm.NewFake()
	e := reconcile.New(s, fake)
	t.Cleanup(func() { e.Exec.Stop(); s.Close() })
	return e, s, fake
}

// knSeedIntent commits exactly what Dispatch's phase-1 tx commits (engine.go
// Dispatch): the session row, the worker row in `starting`, and the
// dispatch_intent event — and NOTHING else (no VM launch, no dispatch_done).
// The shapes mirror engine.go so the constructed state is indistinguishable
// from a daemon killed right after that tx committed.
func knSeedIntent(t *testing.T, s *ledger.Store, task string) (sessionID, workerID string) {
	t.Helper()
	workerID = ulid.Make().String()
	sessionID = ulid.Make().String()
	workspace := "arco_" + workerID
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{
			ID: sessionID, Goal: task, Status: core.SessionActive, Kind: core.SessionKindWork,
		}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{
			ID: workerID, OwnerSession: sessionID, State: core.WorkerStarting,
			Workspace: workspace, Task: task, RunReason: "dispatch",
		}); err != nil {
			return err
		}
		_, _, _, err := tx.AppendEvent(core.Event{
			Kind: "dispatch_intent", SessionID: sessionID, WorkerID: workerID, Actor: "cli",
			Payload: fmt.Sprintf(`{"task":%q,"workspace":%q}`, task, workspace),
		})
		return err
	}))
	return sessionID, workerID
}

// knEventCount returns the total number of ledger events (idempotence probe).
func knEventCount(t *testing.T, s *ledger.Store) int {
	t.Helper()
	evs, err := s.Reader().EventsSince(0, 10000)
	require.NoError(t, err)
	return len(evs)
}

// knEventKinds returns the kinds of all ledger events in id order.
func knEventKinds(t *testing.T, s *ledger.Store) []string {
	t.Helper()
	evs, err := s.Reader().EventsSince(0, 10000)
	require.NoError(t, err)
	kinds := make([]string, 0, len(evs))
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

// W1 pre-intent: the daemon was killed BEFORE the dispatch tx committed, so
// the ledger holds no worker and no dispatch_intent. Restart must be a clean
// no-op: no worker invented, no prompt sent. The row's value is the harness
// proving "empty ledger converges" — kept deliberately cheap.
func TestKillNine_W1_PreIntent_EmptyLedgerConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arco.db")
	s1 := knOpen(t, path)
	// Crash before any dispatch tx: nothing was ever written.
	require.NoError(t, s1.Close())

	e, s2, fake := knRestart(t, path)
	require.NoError(t, e.Recover(context.Background()))
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)

	ws, err := s2.Reader().ListWorkers(core.WorkerFilter{})
	require.NoError(t, err)
	require.Empty(t, ws, "recovery must not invent a worker")
	require.Empty(t, fake.Prompts(), "no prompt on an empty ledger")
	require.Empty(t, fake.Launched(), "no launch on an empty ledger")
	require.Zero(t, res.Observed)
	require.Zero(t, res.Transitions)
	require.Zero(t, knEventCount(t, s2))

	// Idempotence coda: a second Recover+Sweep changes nothing.
	require.NoError(t, e.Recover(context.Background()))
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	ws, err = s2.Reader().ListWorkers(core.WorkerFilter{})
	require.NoError(t, err)
	require.Empty(t, ws)
	require.Empty(t, fake.Prompts())
	require.Zero(t, knEventCount(t, s2))
}

// W2 post-intent / pre-execute: the worker row (starting) + dispatch_intent
// are committed, but the daemon died BEFORE VM.Prompt — the launch never
// happened and there is no dispatch_done. On restart with the agent NOT alive,
// Recover must park the worker `failed` (boot_recovery receipt) and MUST NOT
// re-execute the committed intent (zero Prompts on the fresh fake): an intent
// is a receipt to RESOLVE by liveness, never a command to blindly replay.
func TestKillNine_W2_PostIntentPreExecute_ParksFailedWithoutReExecuting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arco.db")
	s1 := knOpen(t, path)
	_, workerID := knSeedIntent(t, s1, "build the feature")
	// Crash between the intent tx and the VM launch.
	require.NoError(t, s1.Close())

	e, s2, fake := knRestart(t, path) // fresh fake: the agent is NOT alive

	require.NoError(t, e.Recover(context.Background()))

	w, err := s2.Reader().GetWorker(workerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerFailed, w.State, "intent-without-done + dead agent ⇒ parked failed")
	require.Empty(t, fake.Prompts(), "the committed dispatch_intent must NOT be re-executed")
	require.Empty(t, fake.Launched())

	// The receipt trail: dispatch_intent, then the boot_recovery reconcile event
	// — and still no dispatch_done (the launch truly never happened).
	kinds := knEventKinds(t, s2)
	require.Contains(t, kinds, "dispatch_intent")
	require.NotContains(t, kinds, "dispatch_done")
	evs, err := s2.Reader().EventsSince(0, 100)
	require.NoError(t, err)
	var bootRecovery bool
	for _, ev := range evs {
		if ev.Kind == "reconcile" && strings.Contains(ev.Payload, `"boot_recovery":true`) {
			bootRecovery = true
		}
	}
	require.True(t, bootRecovery, "Recover must record its boot_recovery receipt")

	// Idempotence coda: a second Recover+Sweep changes nothing.
	rev, events := w.Rev, knEventCount(t, s2)
	require.NoError(t, e.Recover(context.Background()))
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	w2, err := s2.Reader().GetWorker(workerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerFailed, w2.State)
	require.Equal(t, rev, w2.Rev, "no rev churn on an already-recovered worker")
	require.Equal(t, events, knEventCount(t, s2), "no new events on the second pass")
	require.Empty(t, fake.Prompts())
}

// W3 post-execute / pre-result: same committed intent state as W2, but the
// launch DID happen (the restarted fake lists the agent alive) and the daemon
// died before dispatch_done. Recover must ADOPT the live agent (starting →
// running) without a duplicate launch — zero Prompts on the fresh fake — and a
// subsequent Sweep keeps it running.
func TestKillNine_W3_PostExecutePreResult_AdoptsWithoutDuplicateLaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arco.db")
	s1 := knOpen(t, path)
	_, workerID := knSeedIntent(t, s1, "build the feature")
	// Crash between the VM launch and the dispatch_done tx.
	require.NoError(t, s1.Close())

	e, s2, fake := knRestart(t, path)
	// The restarted daemon's fake observes the phase-1 launch still alive.
	fake.Agents = []core.AgentObs{{Workspace: "arco_" + workerID, Alive: true}}

	require.NoError(t, e.Recover(context.Background()))

	w, err := s2.Reader().GetWorker(workerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State, "intent-without-done + live agent ⇒ adopted running")
	require.Empty(t, fake.Prompts(), "adoption must not duplicate the launch prompt")
	require.Empty(t, fake.Launched())
	require.NotContains(t, knEventKinds(t, s2), "dispatch_done",
		"adoption records a boot_recovery reconcile event, not a synthetic dispatch_done")

	// A subsequent sweep keeps the adopted worker running.
	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.Observed)
	require.Zero(t, res.Transitions)
	w, err = s2.Reader().GetWorker(workerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w.State)

	// Idempotence coda: a second Recover+Sweep changes nothing.
	rev, events := w.Rev, knEventCount(t, s2)
	require.NoError(t, e.Recover(context.Background()))
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
	w2, err := s2.Reader().GetWorker(workerID)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, w2.State)
	require.Equal(t, rev, w2.Rev)
	require.Equal(t, events, knEventCount(t, s2))
	require.Empty(t, fake.Prompts())
}

// knSeedRunningWithBrainIntent drives phase 1 for W4: a REAL dispatch (the
// pre-crash daemon launched the worker — one prompt on the phase-1 fake), then
// the brain-classification receipts exactly as brain_apply.go writes them: a
// brain_intent carrying a correlation id (brainClassify's intent tx), and —
// when withPromptIntent — the prompt_intent sharing that cid (preparePrompt's
// tx, committed right before the un-recallable VM.Prompt). The store is then
// closed: the crash point.
func knSeedRunningWithBrainIntent(t *testing.T, path string, clk *knClock, withPromptIntent bool) (workerID string) {
	t.Helper()
	s1 := knOpen(t, path)
	s1.SetClock(clk.now)
	fake1 := vm.NewFake()
	e1 := reconcile.New(s1, fake1)

	res, err := e1.Dispatch(context.Background(), "", "long-running task", true)
	require.NoError(t, err)
	require.Equal(t, core.WorkerRunning, res.State)
	require.Len(t, fake1.Prompts(), 1, "phase 1: the pre-crash daemon delivered the initial task")

	cid := ulid.Make().String()
	require.NoError(t, s1.WithTx(context.Background(), func(tx core.Tx) error {
		// brainClassify's intent receipt (brain_apply.go).
		if _, _, _, err := tx.AppendEvent(core.Event{
			Kind: "brain_intent", WorkerID: res.WorkerID, SessionID: res.SessionID, Actor: "brain",
			CorrelationID: cid, Payload: `{"state":"running"}`,
		}); err != nil {
			return err
		}
		if withPromptIntent {
			// preparePrompt's receipt for the un-recallable prompt (brain_apply.go):
			// committed under the write lock RIGHT before VM.Prompt fires.
			if _, _, _, err := tx.AppendEvent(core.Event{
				Kind: "prompt_intent", WorkerID: res.WorkerID, SessionID: res.SessionID, Actor: "brain",
				CorrelationID: cid, Payload: fmt.Sprintf(`{"instruction":%q}`, "keep going"),
			}); err != nil {
				return err
			}
		}
		return nil
	}))

	e1.Exec.Stop()
	require.NoError(t, s1.Close()) // SIGKILL
	return res.WorkerID
}

// knBrainRestart is knRestart plus a brain wired to answer run_again, so a
// re-driven classification produces exactly one observable Prompt.
func knBrainRestart(t *testing.T, path string, clk *knClock, calls *atomic.Int32) (*reconcile.Engine, *ledger.Store, *vm.Fake) {
	t.Helper()
	e, s, fake := knRestart(t, path)
	s.SetClock(clk.now)
	e.Brain = reconcile.BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: func(context.Context, string, ...string) ([]byte, error) {
			calls.Add(1)
			return []byte(`{"kind":"run_again","instruction":"resume"}`), nil
		}}
	return e, s, fake
}

// W4: the prompt_intent redrive is not a duplicate. Two crash points inside
// one brain classification:
//
//   - crash BEFORE preparePrompt committed prompt_intent (the classification
//     was lost mid-flight): the dangling brain_intent must be re-driven by
//     redriveStaleBrainIntents (inside Sweep, once past BrainIntentGrace) AT
//     MOST once — exactly one Prompt on the fresh fake across TWO consecutive
//     sweeps, because claimRedrive + the cid-resolution keep later sweeps out.
//
//   - crash AFTER prompt_intent(cid) committed but before the prompt was
//     delivered / any resolving brain_decision landed: the cid-sibling proves
//     the classification already ACTED, so the redrive must NOT fire — zero
//     Prompts. This is the documented at-most-once trade for un-recallable
//     side effects (reader.go StaleBrainIntents): a prompt lost inside the
//     intent→delivery window is never re-sent, it is not a duplicate-send bug.
func TestKillNine_W4_PromptIntentRedrive(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	t.Run("crash mid-classification: re-driven exactly once across two sweeps", func(t *testing.T) {
		clk := &knClock{}
		clk.set(base)
		path := filepath.Join(t.TempDir(), "arco.db")
		workerID := knSeedRunningWithBrainIntent(t, path, clk, false)

		var calls atomic.Int32
		e, s2, fake := knBrainRestart(t, path, clk, &calls)
		fake.Agents = []core.AgentObs{{Workspace: "arco_" + workerID, Alive: true}}
		// Cross the (floored, 5-minute) BrainIntentGrace so the dangling intent is stale.
		clk.set(base.Add(10 * time.Minute))

		// Sweep #1 (inside Recover) re-drives the lost classification…
		require.NoError(t, e.Recover(context.Background()))
		e.Exec.Wait()
		// …sweep #2 must not re-drive it again (idempotence).
		res, err := e.Sweep(context.Background())
		require.NoError(t, err)
		e.Exec.Wait()
		require.Zero(t, res.BrainRedrives, "the re-driven classification resolved its cid; no second re-drive")

		require.Equal(t, int32(1), calls.Load(), "the lost classification is re-driven exactly once")
		require.Len(t, fake.Prompts(), 1, "exactly one prompt across two sweeps")
		require.Contains(t, fake.Prompts()[0].Text, "[arco-intent]", "the re-driven prompt carries the intent marker (B9)")
		w, err := s2.Reader().GetWorker(workerID)
		require.NoError(t, err)
		require.Equal(t, core.WorkerRunning, w.State)

		// Idempotence coda: another Recover+Sweep still changes nothing.
		events := knEventCount(t, s2)
		require.NoError(t, e.Recover(context.Background()))
		_, err = e.Sweep(context.Background())
		require.NoError(t, err)
		e.Exec.Wait()
		require.Equal(t, int32(1), calls.Load())
		require.Len(t, fake.Prompts(), 1)
		require.Equal(t, events, knEventCount(t, s2))
	})

	t.Run("prompt_intent receipt committed: never re-driven, zero duplicate prompts", func(t *testing.T) {
		clk := &knClock{}
		clk.set(base)
		path := filepath.Join(t.TempDir(), "arco.db")
		workerID := knSeedRunningWithBrainIntent(t, path, clk, true)

		var calls atomic.Int32
		e, s2, fake := knBrainRestart(t, path, clk, &calls)
		fake.Agents = []core.AgentObs{{Workspace: "arco_" + workerID, Alive: true}}
		clk.set(base.Add(10 * time.Minute))

		require.NoError(t, e.Recover(context.Background()))
		e.Exec.Wait()
		res, err := e.Sweep(context.Background())
		require.NoError(t, err)
		e.Exec.Wait()

		// The committed prompt_intent is the cid-sibling that proves "already
		// acted": the intent is not dangling, so nothing is re-driven and the
		// un-recallable prompt is never duplicated (at-most-once by design).
		require.Zero(t, res.BrainRedrives)
		require.Zero(t, calls.Load(), "brain never re-invoked for a resolved classification")
		require.Empty(t, fake.Prompts(), "a committed prompt_intent must never be re-executed")
		w, err := s2.Reader().GetWorker(workerID)
		require.NoError(t, err)
		require.Equal(t, core.WorkerRunning, w.State)

		// Idempotence coda.
		events := knEventCount(t, s2)
		require.NoError(t, e.Recover(context.Background()))
		_, err = e.Sweep(context.Background())
		require.NoError(t, err)
		e.Exec.Wait()
		require.Zero(t, calls.Load())
		require.Empty(t, fake.Prompts())
		require.Equal(t, events, knEventCount(t, s2))
	})
}
