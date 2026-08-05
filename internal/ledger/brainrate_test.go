package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestRecentWorkerEvents_OrderAndLimit(t *testing.T) {
	s := newTestStore(t)
	sid := ulid.Make().String()
	wid := ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sid, Goal: "g", Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{ID: wid, OwnerSession: sid, State: core.WorkerStarting, Workspace: "arco_" + wid, Task: "t", RunReason: "x"}); err != nil {
			return err
		}
		for i := 0; i < 5; i++ {
			if _, _, _, err := tx.AppendEvent(core.Event{Kind: "note", WorkerID: wid, SessionID: sid, Payload: "{}"}); err != nil {
				return err
			}
		}
		// an event for a DIFFERENT worker must not leak in
		other := ulid.Make().String()
		if err := tx.CreateWorker(core.Worker{ID: other, OwnerSession: sid, State: core.WorkerStarting, Workspace: "arco_" + other, Task: "t", RunReason: "x"}); err != nil {
			return err
		}
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "note", WorkerID: other, SessionID: sid, Payload: "{}"})
		return err
	}))

	// limit caps the tail and results are chronological (id ascending)
	evs, err := s.Reader().RecentWorkerEvents(wid, 3)
	require.NoError(t, err)
	require.Len(t, evs, 3, "limit respected")
	require.Less(t, evs[0].ID, evs[1].ID)
	require.Less(t, evs[1].ID, evs[2].ID)
	for _, ev := range evs {
		require.Equal(t, wid, ev.WorkerID, "no other worker's events leak in")
	}
}

func TestBrainRate_CountWindowAndKind(t *testing.T) {
	s := newTestStore(t)
	clk := &testClock{t: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	s.SetClock(clk.now)
	sid := ulid.Make().String()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: sid, Goal: "g", Status: core.SessionActive, Kind: core.SessionKindWork})
	}))

	// three brain_intent + one unrelated event, all "now"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		for i := 0; i < 3; i++ {
			if _, _, _, err := tx.AppendEvent(core.Event{Kind: "brain_intent", SessionID: sid, Actor: "brain", Payload: "{}"}); err != nil {
				return err
			}
		}
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "state_change", SessionID: sid, Payload: "{}"})
		return err
	}))

	count := func() int {
		var n int
		require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
			var err error
			n, err = tx.CountRecentBrainCalls(sid, time.Minute)
			return err
		}))
		return n
	}
	require.Equal(t, 3, count(), "only brain_intent events within the window count")

	// slide past the window → the three age out
	clk.advance(2 * time.Minute)
	require.Equal(t, 0, count(), "events older than the window are excluded")

	// a different session is unaffected
	require.Equal(t, 0, func() int {
		var n int
		require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
			var err error
			n, err = tx.CountRecentBrainCalls("other", time.Minute)
			return err
		}))
		return n
	}())
}
