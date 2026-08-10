package reconcile

// rev7 T3.5 supplements to the guideline tests: the auto_answer audit event,
// the zero-knob fail-closed posture, and the EarnOutReport surface.

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func TestEarnOut_AutoAnswerAppendsAuditEventWithClassStats(t *testing.T) {
	e, s, _ := newEarnedOut(t)
	id := mkRunning(t, e, s, "/wt/eo-main", "base")
	setMode(t, s, id, core.ModeAuto)
	parkDrafted(t, s, id, "eoQ:p1", "question", "clarify", "use the blue pipeline")

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, res.AutoAnswered)
	e.Exec.Wait()

	// An operator must be able to reconstruct WHY arco answered by itself from
	// the ledger alone: one auto_answer event carrying the class stats.
	evs, err := s.Reader().EventsSince(0, 1000)
	require.NoError(t, err)
	var found bool
	for _, ev := range evs {
		if ev.Kind != "auto_answer" {
			continue
		}
		found = true
		require.Equal(t, "brain", ev.Actor)
		require.Equal(t, id, ev.WorkerID, "the audit event lands in the worker's stream")
		for _, want := range []string{`"decided_by":"brain"`, `"question_class":"clarify"`,
			`"agree":1`, `"total":1`, `"min_decisions":1`, `"min_agreement":1`} {
			require.True(t, strings.Contains(ev.Payload, want), "payload %s missing %s", ev.Payload, want)
		}
	}
	require.True(t, found, "the auto-answer must append an auto_answer audit event")
}

func TestEarnOut_ZeroKnobsNeverPromote(t *testing.T) {
	// "Promote instantly" must not be the meaning of an unset knob: with the
	// thresholds at zero every other gate passing still auto-answers nothing.
	e, s, fake := newEngine(t)
	e.MissThreshold = 100
	e.VerificationLive = true
	e.EarnOutMinDecisions = 0
	e.EarnOutMinAgreement = 0
	id := mkRunning(t, e, s, "/wt/eo-zero", "base")
	setMode(t, s, id, core.ModeAuto)
	escID := parkDrafted(t, s, id, "eoZ:p1", "question", "clarify", "use the blue pipeline")

	res, err := e.Sweep(context.Background())
	require.NoError(t, err)
	require.Zero(t, res.AutoAnswered)
	e.Exec.Wait()

	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "pending", esc.Status)
	_, ok := promptTo(fake.Prompts(), "eoZ:p1")
	require.False(t, ok)
}

func TestEarnOut_ReportListsEveryClassWithLiveGates(t *testing.T) {
	e, _, _ := newEarnedOut(t) // clarify seeded 1/1; gates 1 decision @ 1.0

	rep, err := e.EarnOutReport()
	require.NoError(t, err)
	require.Len(t, rep, 5, "the frozen question_class enum, tallied or not")
	byClass := map[string]EarnOutClassReport{}
	for _, r := range rep {
		byClass[r.Class] = r
	}
	require.Equal(t, 1, byClass["clarify"].Agree)
	require.Equal(t, 1, byClass["clarify"].Total)
	require.True(t, byClass["clarify"].Promotes)
	for _, c := range []string{"proceed-confirmation", "scope-change", "resource", "other"} {
		require.Zero(t, byClass[c].Total)
		require.False(t, byClass[c].Promotes, "%s has no earned history", c)
	}

	// The report reflects the LIVE gates: verification going dark stops every
	// class from promoting, history intact.
	e.VerificationLive = false
	rep, err = e.EarnOutReport()
	require.NoError(t, err)
	for _, r := range rep {
		require.False(t, r.Promotes)
	}
}
