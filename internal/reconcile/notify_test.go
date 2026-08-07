package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// waitCards polls the recorder until it holds at least n cards (notify emits may
// be post-commit/off the write path) or times out.
func waitCards(t *testing.T, rec *notify.Recorder, n int) []notify.Card {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		cards := rec.Cards()
		if len(cards) >= n {
			return cards
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d cards, have %d: %+v", n, len(cards), cards)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func lastCard(t *testing.T, rec *notify.Recorder, n int) notify.Card {
	t.Helper()
	cards := waitCards(t, rec, n)
	return cards[len(cards)-1]
}

// Escalation opened (worker awaiting input) → urgent decision card carrying the
// worker id and the question.
func TestNotify_EscalationOpened_UrgentCard(t *testing.T) {
	e, s, _ := newEngine(t)
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/n1", "base")

	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))

	c := lastCard(t, rec, 1)
	require.Equal(t, notify.LevelUrgent, c.Level)
	require.Contains(t, c.Title, id, "card must name the worker")
	require.Contains(t, c.Body, "worker is awaiting input", "card must carry the question")

	// One-pending-per-worker: replaying the same signal must not spam a second card.
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))
	require.Len(t, rec.Cards(), 1, "duplicate waiting signal must not re-notify")
}

// Human answers the question → info card mentioning the resolution.
func TestNotify_AnswerQuestion_InfoCard(t *testing.T) {
	e, s, _ := newEngine(t)
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/n2", "base")
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))
	waitCards(t, rec, 1) // the opened card

	pend, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
	require.NoError(t, err)
	require.Len(t, pend, 1)

	require.NoError(t, e.AnswerQuestion(context.Background(), pend[0].ID, "use sqlite", core.ScopeOnce))

	c := lastCard(t, rec, 2)
	require.Equal(t, notify.LevelInfo, c.Level)
	require.Contains(t, c.Title, "answered")
	require.Contains(t, c.Body, id, "card must name the worker")
}

// Escalation expires past EscalationTimeout → warn card.
func TestNotify_EscalationExpired_WarnCard(t *testing.T) {
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	clk := &settableClock{}
	clk.set(base)
	e, s, _ := newEngine(t)
	s.SetClock(clk.now)
	e.EscalationTimeout = 30 * time.Minute
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/n3", "base")
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))
	waitCards(t, rec, 1) // the opened card

	clk.set(base.Add(31 * time.Minute))
	_, err := e.Sweep(context.Background())
	require.NoError(t, err)

	c := lastCard(t, rec, 2)
	require.Equal(t, notify.LevelWarn, c.Level)
	require.Contains(t, c.Title, "expired")
	require.Contains(t, c.Body, id, "card must name the worker")
}

// Worker finalized lost → warn card.
func TestNotify_WorkerLost_WarnCard(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 2
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/n4", "base")
	fake.Agents = nil // missing every sweep, no HEAD change

	for i := 0; i < 2; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerLost, w.State)

	c := lastCard(t, rec, 1)
	require.Equal(t, notify.LevelWarn, c.Level)
	require.Contains(t, c.Title, "lost")
	require.Contains(t, c.Title, id, "card must name the worker")
}

// Worker verified (candidate → completed_verified via Verify) → info card.
func TestNotify_WorkerVerified_InfoCard(t *testing.T) {
	e, s, fake := newEngine(t)
	e.MissThreshold = 2
	rec := &notify.Recorder{}
	e.Notify = rec
	id := mkRunning(t, e, s, "/wt/n5", "base")
	fake.Agents = nil
	fake.Heads["/wt/n5"] = "advanced" // HEAD moved before it vanished

	for i := 0; i < 2; i++ {
		_, err := e.Sweep(context.Background())
		require.NoError(t, err)
	}
	w, _ := s.Reader().GetWorker(id)
	require.Equal(t, core.WorkerCompletedCandidate, w.State)

	require.NoError(t, e.Verify(context.Background(), id, w.Rev, "human"))

	var found *notify.Card
	for _, c := range waitCards(t, rec, 1) {
		if c.Level == notify.LevelInfo &&
			strings.Contains(c.Title, id) && strings.Contains(c.Title, "verified") {
			cc := c
			found = &cc
		}
	}
	require.NotNil(t, found, "an info card naming the worker and 'verified' must be emitted")
}

// Worker parked failed on launch error → warn card.
func TestNotify_WorkerFailed_WarnCard(t *testing.T) {
	e, s, fake := newEngine(t)
	rec := &notify.Recorder{}
	e.Notify = rec
	fake.PromptErr = errors.New("clavis boom")

	res, err := e.Dispatch(context.Background(), "", "task", true)
	require.NoError(t, err)
	w, _ := s.Reader().GetWorker(res.WorkerID)
	require.Equal(t, core.WorkerFailed, w.State)

	var found *notify.Card
	for _, c := range waitCards(t, rec, 1) {
		if c.Level == notify.LevelWarn &&
			strings.Contains(c.Title, res.WorkerID) && strings.Contains(c.Title, "failed") {
			cc := c
			found = &cc
		}
	}
	require.NotNil(t, found, "a warn card naming the worker and 'failed' must be emitted")
}

// A nil Notify sender must never panic anywhere on the lifecycle paths.
func TestNotify_NilSender_Safe(t *testing.T) {
	e, s, fake := newEngine(t)
	require.Nil(t, e.Notify)
	id := mkRunning(t, e, s, "/wt/n6", "base")
	require.NoError(t, e.ApplyEvent(context.Background(), EventInput{
		WorkerID: id, Alive: true, WaitingInput: true,
	}))
	pend, err := s.Reader().ListEscalations(core.EscalationFilter{Status: "pending", WorkerID: id})
	require.NoError(t, err)
	require.Len(t, pend, 1)
	require.NoError(t, e.AnswerQuestion(context.Background(), pend[0].ID, "ok", core.ScopeOnce))
	fake.Agents = nil
	e.MissThreshold = 1
	_, err = e.Sweep(context.Background())
	require.NoError(t, err)
}
