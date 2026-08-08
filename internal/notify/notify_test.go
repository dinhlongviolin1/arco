package notify

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---- Level parsing & ordering -------------------------------------------

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want Level
	}{
		{"info", LevelInfo},
		{"warn", LevelWarn},
		{"urgent", LevelUrgent},
	}
	for _, c := range cases {
		got, err := ParseLevel(c.in)
		require.NoError(t, err, c.in)
		require.Equal(t, c.want, got, c.in)
	}
	_, err := ParseLevel("shouty")
	require.Error(t, err, "unknown level must be rejected")
	require.True(t, LevelInfo < LevelWarn && LevelWarn < LevelUrgent,
		"levels must be ordered info < warn < urgent")
}

// ---- No-op when unconfigured ---------------------------------------------

func TestNew_NoURLs_IsNoOp(t *testing.T) {
	s := New(nil, LevelInfo)
	require.NotNil(t, s, "New must never return a nil Sender")
	require.NoError(t, s.Send(context.Background(), Card{Level: LevelUrgent, Title: "x", Body: "y"}),
		"unconfigured sender must accept and drop cards without error")
}

// ---- Min-level filtering --------------------------------------------------

func TestFiltered_DropsBelowMin(t *testing.T) {
	rec := &Recorder{}
	f := Filtered(rec, LevelWarn)
	require.NoError(t, f.Send(context.Background(), Card{Level: LevelInfo, Title: "drop"}))
	require.NoError(t, f.Send(context.Background(), Card{Level: LevelWarn, Title: "keep-warn"}))
	require.NoError(t, f.Send(context.Background(), Card{Level: LevelUrgent, Title: "keep-urgent"}))
	got := rec.Cards()
	require.Len(t, got, 2)
	require.Equal(t, "keep-warn", got[0].Title)
	require.Equal(t, "keep-urgent", got[1].Title)
}

// ---- Decision card rendering (golden) --------------------------------------

func TestFormatEscalation_Golden(t *testing.T) {
	c := FormatEscalation(EscalationCard{
		WorkerID:   "w-1",
		TaskTail:   "fix the flaky auth test",
		Question:   "which sqlite driver?",
		Draft:      "use mattn/go-sqlite3",
		Confidence: 0.82,
		Rationale:  "it is already vendored",
	})
	require.Equal(t, LevelUrgent, c.Level)
	require.Equal(t, "arco: decision needed — w-1", c.Title)
	want := "task: fix the flaky auth test\n" +
		"question: which sqlite driver?\n" +
		"draft: use mattn/go-sqlite3 (confidence 0.82)\n" +
		"rationale: it is already vendored"
	require.Equal(t, want, c.Body)
}

func TestFormatEscalation_NilDraft_OmitsDraftLines(t *testing.T) {
	c := FormatEscalation(EscalationCard{
		WorkerID: "w-2",
		TaskTail: "migrate the ledger",
		Question: "worker is awaiting input",
	})
	require.Equal(t, LevelUrgent, c.Level)
	require.Equal(t, "arco: decision needed — w-2", c.Title)
	want := "task: migrate the ledger\n" +
		"question: worker is awaiting input"
	require.Equal(t, want, c.Body)
	require.NotContains(t, c.Body, "draft:")
	require.NotContains(t, c.Body, "rationale:")
}

// ---- Recorder is race-safe --------------------------------------------------

func TestRecorder_RaceSafe(t *testing.T) {
	rec := &Recorder{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rec.Send(context.Background(), Card{Level: LevelInfo, Title: "t"})
			_ = rec.Cards()
		}()
	}
	wg.Wait()
	require.Len(t, rec.Cards(), 16)
}
