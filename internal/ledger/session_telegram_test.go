package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// SetSessionTelegram binds the forum topic id and status-card message id, and a
// nil pointer leaves that column unchanged (COALESCE semantics) — so the topic
// id set on first use survives a later status-card-only update.
func TestSetSessionTelegram_PartialUpdateCoalesces(t *testing.T) {
	s := newTestStore(t)
	id := "sess-tg"
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: id, Status: core.SessionOpen, Kind: core.SessionKindWork, Title: "t", Goal: "g"})
	}))

	// fresh session: both columns NULL
	sess, err := s.Reader().GetSession(id)
	require.NoError(t, err)
	require.Nil(t, sess.TGTopicID)
	require.Nil(t, sess.TGStatusMsgID)

	// set topic id only
	topic := int64(555)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetSessionTelegram(id, &topic, nil)
	}))
	sess, _ = s.Reader().GetSession(id)
	require.NotNil(t, sess.TGTopicID)
	require.EqualValues(t, 555, *sess.TGTopicID)
	require.Nil(t, sess.TGStatusMsgID, "status msg id untouched")

	// set status msg id only — topic id must be preserved (nil = leave)
	msg := int64(42)
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.SetSessionTelegram(id, nil, &msg)
	}))
	sess, _ = s.Reader().GetSession(id)
	require.NotNil(t, sess.TGTopicID, "topic id preserved across a status-only update")
	require.EqualValues(t, 555, *sess.TGTopicID)
	require.NotNil(t, sess.TGStatusMsgID)
	require.EqualValues(t, 42, *sess.TGStatusMsgID)
}
