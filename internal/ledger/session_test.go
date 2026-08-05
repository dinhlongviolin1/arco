package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestSession_StatusTransitionAndCAS(t *testing.T) {
	s := newTestStore(t)
	id := newWork(t, s)

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetSessionStatus(id, core.SessionActive, 0, core.Event{Kind: "session_open", SessionID: id})
	}))
	got, _ := s.Reader().GetSession(id)
	require.Equal(t, core.SessionActive, got.Status)
	require.Equal(t, int64(1), got.Rev)

	// illegal done->active (drive to done first)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetSessionStatus(id, core.SessionDone, 1, core.Event{Kind: "note", SessionID: id})
	}))
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetSessionStatus(id, core.SessionActive, 2, core.Event{Kind: "note", SessionID: id})
	})
	require.ErrorIs(t, err, core.ErrIllegalTransition)
}

func TestSession_PoolIsProtected(t *testing.T) {
	s := newTestStore(t)

	// status transition on the pool sentinel is rejected
	err := s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetSessionStatus(core.PoolSessionID, core.SessionIdle, 0, core.Event{Kind: "note"})
	})
	require.ErrorIs(t, err, core.ErrProtectedPool)

	// granting on the pool is rejected (else `arco grant pool` = fleet-wide escalation)
	err = s.WithTx(context.Background(), func(tx core.Tx) error {
		_, e := tx.Grant(core.PoolSessionID, "git.commit", "cli", core.Event{Kind: "grant"})
		return e
	})
	require.ErrorIs(t, err, core.ErrProtectedPool)
}

func TestSession_ResolveByIDAndSlug(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Slug: "arco-p2", Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))
	byID, err := s.Reader().ResolveSession("01AAAAAAAAAAAAAAAAAAAAAAAA")
	require.NoError(t, err)
	require.Equal(t, "arco-p2", byID.Slug)
	bySlug, err := s.Reader().ResolveSession("arco-p2")
	require.NoError(t, err)
	require.Equal(t, "01AAAAAAAAAAAAAAAAAAAAAAAA", bySlug.ID)
}
