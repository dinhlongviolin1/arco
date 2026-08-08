// autonomy_disjoint_test.go closes the build-guide-rev6 §E "Autonomy
// disjointness (structural)" debt: brain output can NEVER reach a
// decision-shaped sink. The invariant is pinned STRUCTURALLY (reflection over
// the DTO field sets — see the comment on core.DraftAnswer in core/ports.go:
// "It has NO scope/yes-no/grant field: a brain-sourced value can never reach
// DecideConfirm/Grant") plus two behavioral pins on the ledger's decision
// writers, which stamp decided_by='human' unconditionally.
//
// These tests break ON PURPOSE when a field is added: the author must prove
// disjointness again before widening the whitelist.
package reconcile

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// adFieldNames returns the exported struct field names of typ.
func adFieldNames(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	require.Equal(t, reflect.Struct, typ.Kind())
	names := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		names = append(names, typ.Field(i).Name)
	}
	return names
}

// adAssertNoDecisionShapedField fails if any field name smells like a
// decision/authority channel — the shapes that would let brain output flow
// into a scope, grant, or yes/no sink.
func adAssertNoDecisionShapedField(t *testing.T, typeName string, fields []string) {
	t.Helper()
	for _, f := range fields {
		lf := strings.ToLower(f)
		for _, bad := range []string{"scope", "decision", "decide", "grant", "yes", "approve", "capability"} {
			require.NotContains(t, lf, bad,
				"%s.%s is decision-shaped: brain output must stay disjoint from authority (core/ports.go DraftAnswer doc)", typeName, f)
		}
	}
}

// DraftAnswer (the two-level resolver's output, stored only as a shadow draft
// on the escalation) must have EXACTLY today's advisory fields. A new field
// fails this test on purpose.
func TestAutonomyDisjoint_DraftAnswerFieldWhitelist(t *testing.T) {
	fields := adFieldNames(t, reflect.TypeOf(core.DraftAnswer{}))
	require.ElementsMatch(t, []string{"Text", "Confidence", "Rationale", "Tainted"}, fields,
		"core.DraftAnswer grew/lost a field — re-prove autonomy disjointness before updating this whitelist")
	adAssertNoDecisionShapedField(t, "core.DraftAnswer", fields)
}

// StepResult (the brain's typed decision applied by the reconciler) likewise:
// its fields select a reconciler BRANCH (kind/instruction/reason), never a
// scope, grant, or confirm answer.
func TestAutonomyDisjoint_StepResultFieldWhitelist(t *testing.T) {
	fields := adFieldNames(t, reflect.TypeOf(core.StepResult{}))
	require.ElementsMatch(t, []string{"Kind", "Worker", "Instruction", "Reason"}, fields,
		"core.StepResult grew/lost a field — re-prove autonomy disjointness before updating this whitelist")
	adAssertNoDecisionShapedField(t, "core.StepResult", fields)
}

// Behavioral pin: DecideConfirm's UPDATE (ledger/escalations.go decide) stamps
// decided_by='human' as a literal, regardless of caller input. Here the caller
// even supplies an event CLAIMING Actor "brain" — the stored row must still
// say a human decided, so a brain-sourced call path could never masquerade as
// the decision authority.
func TestAutonomyDisjoint_DecideConfirmStampsHuman(t *testing.T) {
	_, s, _ := newEngine(t)
	ctx := context.Background()

	sessID, wid := ulid.Make().String(), ulid.Make().String()
	var escID string
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sessID, Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{ID: wid, OwnerSession: sessID, State: core.WorkerWaitingConfirmation, Workspace: "arco_" + wid}); err != nil {
			return err
		}
		var err error
		escID, err = tx.OpenEscalation(core.Escalation{
			WorkerID: wid, SessionID: sessID, Kind: "confirm",
			ActionClass: core.ClassDanger, Tier: core.TierHighBlast, Action: "deploy to prod",
		})
		return err
	}))

	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.DecideConfirm(escID, true, core.ScopeOnce, core.Event{
			Kind: "escalation_decided", Actor: "brain", Payload: `{"decision":"approved"}`,
		})
	}))

	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "approved", esc.Status)
	require.Equal(t, "human", esc.DecidedBy, "decided_by is stamped 'human' unconditionally — caller input cannot override it")
	require.Equal(t, "human", esc.AnsweredBy)
}

// Behavioral pin: Tx.AnswerQuestion records the same — its UPDATE shares
// decide()'s literal decided_by='human'/answered_by='human' stamps, so the
// only path that resolves a question is, by construction, a human one.
func TestAutonomyDisjoint_AnswerQuestionStampsHuman(t *testing.T) {
	_, s, _ := newEngine(t)
	ctx := context.Background()

	sessID, wid := ulid.Make().String(), ulid.Make().String()
	var escID string
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		if err := tx.CreateSession(core.Session{ID: sessID, Status: core.SessionActive, Kind: core.SessionKindWork}); err != nil {
			return err
		}
		if err := tx.CreateWorker(core.Worker{ID: wid, OwnerSession: sessID, State: core.WorkerWaitingForUser, Workspace: "arco_" + wid}); err != nil {
			return err
		}
		var err error
		escID, err = tx.OpenEscalation(core.Escalation{
			WorkerID: wid, SessionID: sessID, Kind: "question",
			QuestionClass: "clarify", ActionClass: core.ClassAmbiguous, Tier: core.TierMedium,
			Action: "which framework?", DraftAnswer: "use the existing one", BrainRationale: "matches the repo",
		})
		return err
	}))

	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.AnswerQuestion(escID, "use the existing one", core.ScopeOnce, core.Event{
			Kind: "escalation_answered", Actor: "brain", Payload: `{"via":"test"}`,
		})
	}))

	esc, err := s.Reader().GetEscalation(escID)
	require.NoError(t, err)
	require.Equal(t, "answered", esc.Status)
	require.Equal(t, "use the existing one", esc.AnswerText)
	require.Equal(t, "human", esc.DecidedBy, "an answer is recorded as human-decided regardless of the caller's claimed actor")
	require.Equal(t, "human", esc.AnsweredBy,
		"the brain's draft stays in draft_answer; answered_by flips to human only on the human decision")
}
