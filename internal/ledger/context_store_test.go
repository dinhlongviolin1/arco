package ledger

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// needleScrub redacts a fixed secret — enough to prove AppendMessage scrubs at
// the write chokepoint (row + FTS mirror).
type needleScrub struct{ needle string }

func (s needleScrub) Scrub(in string) (string, int) {
	if !strings.Contains(in, s.needle) {
		return in, 0
	}
	return strings.ReplaceAll(in, s.needle, "[redacted]"), 1
}
func (needleScrub) Version() string { return "test" }

func seedContextSession(t *testing.T, s *Store, id string) {
	t.Helper()
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: id, Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))
}

func appendMsg(t *testing.T, s *Store, m core.Message) int64 {
	t.Helper()
	var id int64
	require.NoError(t, s.WithTx(context.Background(), func(tx core.Tx) error {
		var err error
		id, err = tx.AppendMessage(m)
		return err
	}))
	return id
}

func TestContextStore_AppendAndRecent(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: "what's running?"})
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "arco", Content: "2 workers are active"})

	msgs, err := s.Reader().RecentMessages("S1", time.Time{}, 100)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	// chronological order (oldest first)
	require.Equal(t, "operator", msgs[0].Role)
	require.Equal(t, "what's running?", msgs[0].Content)
	require.Equal(t, "arco", msgs[1].Role)
	require.False(t, msgs[0].CreatedAt.IsZero(), "created_at round-trips")
}

func TestContextStore_ScopedBySession(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	seedContextSession(t, s, "S2")
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: "for s1"})
	appendMsg(t, s, core.Message{SessionID: "S2", Role: "operator", Content: "for s2"})

	msgs, err := s.Reader().RecentMessages("S1", time.Time{}, 100)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "for s1", msgs[0].Content)
}

func TestContextStore_SinceWindow(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: "old"})
	cut := time.Now().UTC().Add(50 * time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: "new"})

	msgs, err := s.Reader().RecentMessages("S1", cut, 100)
	require.NoError(t, err)
	require.Len(t, msgs, 1, "only messages at/after the cutoff")
	require.Equal(t, "new", msgs[0].Content)
}

// Deterministic sub-second boundary: a message written LATER can sort EARLIER as
// an RFC3339Nano string (Go trims fractional zeros), which a lexical WHERE would
// wrongly drop. The exact in-Go cutoff must include it.
func TestContextStore_SinceExactSubSecond(t *testing.T) {
	s := newTestStore(t)
	var clk time.Time
	s.SetClock(func() time.Time { return clk })
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	clk = base
	seedContextSession(t, s, "S1")
	clk = base.Add(100 * time.Millisecond) // "…05.1Z"
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: "first"})
	clk = base.Add(120 * time.Millisecond) // "…05.12Z" — later, but "…12Z" < "…1Z" lexically
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "arco", Content: "second"})

	// since == first message instant → BOTH are in-window (the old lexical compare
	// dropped "second").
	msgs, err := s.Reader().RecentMessages("S1", base.Add(100*time.Millisecond), 100)
	require.NoError(t, err)
	require.Len(t, msgs, 2, "a later sub-second message must not be dropped")
	require.Equal(t, "first", msgs[0].Content)
	require.Equal(t, "second", msgs[1].Content)

	// since strictly between the two → only "second".
	only, err := s.Reader().RecentMessages("S1", base.Add(110*time.Millisecond), 100)
	require.NoError(t, err)
	require.Len(t, only, 1)
	require.Equal(t, "second", only[0].Content)
}

func TestContextStore_LimitClampedToChronologicalTail(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	for _, c := range []string{"m1", "m2", "m3", "m4"} {
		appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: c})
	}
	msgs, err := s.Reader().RecentMessages("S1", time.Time{}, 2)
	require.NoError(t, err)
	require.Len(t, msgs, 2, "limit bounds the window")
	// the newest 2, returned oldest-first
	require.Equal(t, "m3", msgs[0].Content)
	require.Equal(t, "m4", msgs[1].Content)
}

func TestContextStore_LimitHardCap(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: "x"})
	// A caller asking for 10^6 rows is clamped server-side, not honored.
	msgs, err := s.Reader().RecentMessages("S1", time.Time{}, 1_000_000)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
}

func TestContextStore_Search(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: "deploy the auth service"})
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "arco", Content: "the database migration finished"})

	hits, err := s.Reader().SearchMessages("S1", "auth", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Contains(t, hits[0].Content, "auth service")

	// porter stemming: "migration" matches "migrations"/"migrate" family
	hits2, err := s.Reader().SearchMessages("S1", "migration", 10)
	require.NoError(t, err)
	require.Len(t, hits2, 1)

	none, err := s.Reader().SearchMessages("S1", "nonexistentword", 10)
	require.NoError(t, err)
	require.Empty(t, none)

	empty, err := s.Reader().SearchMessages("S1", "", 10)
	require.NoError(t, err)
	require.Empty(t, empty, "an empty query is a harmless no-op")
}

// A query with FTS5 special characters (-, :, quotes) must not error — it's
// sanitized into a literal phrase match.
func TestContextStore_SearchSpecialCharsSafe(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: "check the ci-cd pipeline status"})

	for _, q := range []string{"ci-cd", `a:b`, `"unbalanced`, "-minus", "ci-cd pipeline"} {
		hits, err := s.Reader().SearchMessages("S1", q, 10)
		require.NoError(t, err, "query %q must not error", q)
		_ = hits
	}
	// the hyphenated term is found as a phrase
	hits, err := s.Reader().SearchMessages("S1", "ci-cd", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
}

func TestContextStore_ScrubsAtWrite(t *testing.T) {
	s := newTestStore(t)
	s.SetScrubber(needleScrub{needle: "sk-SECRET"})
	seedContextSession(t, s, "S1")
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "operator", Content: "my token is sk-SECRET ok"})

	msgs, err := s.Reader().RecentMessages("S1", time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotContains(t, msgs[0].Content, "sk-SECRET", "the secret is scrubbed at write time")
	require.Contains(t, msgs[0].Content, "[redacted]")

	// The FTS mirror stored the scrubbed content too — the secret is unsearchable.
	hits, err := s.Reader().SearchMessages("S1", "sk-SECRET", 10)
	require.NoError(t, err)
	require.Empty(t, hits, "a scrubbed secret cannot be found via search")
}

func TestContextStore_TaintedRoundTrips(t *testing.T) {
	s := newTestStore(t)
	seedContextSession(t, s, "S1")
	appendMsg(t, s, core.Message{SessionID: "S1", Role: "arco", Content: "brain-sourced", Tainted: true})
	msgs, err := s.Reader().RecentMessages("S1", time.Time{}, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.True(t, msgs[0].Tainted, "the tainted flag round-trips")
}
