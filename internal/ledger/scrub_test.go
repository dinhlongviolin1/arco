package ledger

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/redact"
)

// A secret in an event payload must be scrubbed at the write chokepoint, so it
// never lands verbatim in the immutable log.
func TestAppendEvent_ScrubsPayloadAtRest(t *testing.T) {
	s := newTestStore(t)
	s.SetScrubber(redact.New())
	const token = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		_, _, _, err := tx.AppendEvent(core.Event{Kind: "note", Payload: `{"leak":"` + token + `"}`})
		return err
	}))

	var stored string
	require.NoError(t, s.DB().QueryRow(`SELECT payload FROM events WHERE kind='note'`).Scan(&stored))
	require.NotContains(t, stored, token, "raw secret must not be at rest in the ledger")
	require.Contains(t, stored, "[REDACTED:", "redaction marker present")
}
