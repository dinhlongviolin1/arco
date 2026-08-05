package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestEvent_AppendAndEventsSince(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		for i := 0; i < 3; i++ {
			if _, _, _, err := tx.AppendEvent(core.Event{Kind: "note", Payload: "{}"}); err != nil {
				return err
			}
		}
		return nil
	}))
	evs, err := s.Reader().EventsSince(0, 100)
	require.NoError(t, err)
	require.Len(t, evs, 3)
	// ascending, contiguous cursor
	require.True(t, evs[0].ID < evs[1].ID && evs[1].ID < evs[2].ID)
}

func TestEvent_IdempotentDedupOnSourceID(t *testing.T) {
	s := newTestStore(t)
	e := core.Event{Source: "herdr:vm0", SourceEventID: "evt-1", SourceEventHash: "h1", Kind: "state_change", Payload: "{}"}

	var deduped, conflict bool
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, d, c, err := tx.AppendEvent(e)
		deduped, conflict = d, c
		return err
	}))
	require.False(t, deduped)
	require.False(t, conflict)

	// re-deliver identical id+hash → deduped, no new row
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, d, c, err := tx.AppendEvent(e)
		deduped, conflict = d, c
		return err
	}))
	require.True(t, deduped)
	require.False(t, conflict)

	var n int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(1) FROM events WHERE source='herdr:vm0' AND source_event_id='evt-1'`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestEvent_HashMismatchRecordsErrorAndSignalsConflict(t *testing.T) {
	s := newTestStore(t)
	base := core.Event{Source: "herdr:vm0", SourceEventID: "evt-9", SourceEventHash: "h1", Kind: "state_change", Payload: "{}"}
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, _, _, err := tx.AppendEvent(base)
		return err
	}))

	var conflict bool
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		bad := base
		bad.SourceEventHash = "DIFFERENT"
		_, _, c, err := tx.AppendEvent(bad)
		conflict = c
		return err
	}))
	require.True(t, conflict, "same id, different hash must signal conflict, not silent dedup")

	var errCount int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(1) FROM events WHERE kind='error'`).Scan(&errCount))
	require.Equal(t, 1, errCount)
}

func TestEvent_TwoClocksRoundTrip(t *testing.T) {
	s := newTestStore(t)
	// occurred_at deliberately precedes recorded_at (event time < learn time)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "note", Payload: "{}", OccurredAt: "2020-01-01T00:00:00.000000000Z"})
		return err
	}))
	evs, err := s.Reader().EventsSince(0, 10)
	require.NoError(t, err)
	require.Len(t, evs, 1)
	require.Equal(t, "2020-01-01T00:00:00.000000000Z", evs[0].OccurredAt)
	require.True(t, evs[0].RecordedAt > evs[0].OccurredAt, "recorded_at should be after the injected occurred_at")
}
