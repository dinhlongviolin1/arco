package telegram

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

func lastSent(api *fakeAPIRec) string {
	if len(api.sent) == 0 {
		return ""
	}
	return api.sent[len(api.sent)-1].text
}

func TestCmd_DispatchSpawnsWorker(t *testing.T) {
	b, api, _, act := newTestBot(t)
	b.handleMessage(context.Background(), &Message{Text: "/dispatch /srv/app.git add a health endpoint", From: &User{ID: allowedUID}})
	require.Equal(t, []string{"/srv/app.git | add a health endpoint"}, act.dispatches)
	require.Contains(t, lastSent(api), "started issue")
}

func TestCmd_DispatchWithVM(t *testing.T) {
	b, _, _, act := newTestBot(t)
	b.handleMessage(context.Background(), &Message{Text: "/dispatch --vm vm1 /srv/app.git write docs", From: &User{ID: allowedUID}})
	require.Equal(t, []string{"/srv/app.git | write docs | vm=vm1"}, act.dispatches, "--vm picks the target VM; repo+task follow")
}

func TestCmd_DispatchIntoIssueTopic(t *testing.T) {
	b, _, st, act := newTestBot(t)
	topic := int64(7)
	st.sessions["ISSUE1"] = core.Session{ID: "ISSUE1", Status: core.SessionActive, TGTopicID: &topic}
	b.handleMessage(context.Background(), &Message{Text: "/dispatch /srv/app.git write docs", From: &User{ID: allowedUID}, MessageThreadID: topic})
	require.Equal(t, []string{"/srv/app.git | write docs | into=ISSUE1"}, act.dispatches,
		"dispatch inside an issue topic adds the aspect to that session")
}

func TestCmd_DispatchUsageOnMissingArgs(t *testing.T) {
	b, api, _, act := newTestBot(t)
	b.handleMessage(context.Background(), &Message{Text: "/dispatch onlyrepo", From: &User{ID: allowedUID}})
	require.Empty(t, act.dispatches, "no spawn without a task")
	require.Contains(t, lastSent(api), "usage:")
}

// NOTE: /kill is now a registered feature (internal/features.Kill) — worker
// resolution + termination are covered in internal/features/mutate_test.go.

// NOTE: /workers, /sessions, /status are now registered ledger features
// (internal/features.Workers/Sessions/Status) — their rendering is covered in
// internal/features/fleet_test.go, and the registry wiring in internal/daemon.

func TestChat_InTopicWithPendingQuestionIsTheAnswer(t *testing.T) {
	b, _, st, act := newTestBot(t)
	topic := int64(5)
	st.sessions["S1"] = core.Session{ID: "S1", Status: core.SessionActive, TGTopicID: &topic}
	st.escs["E1"] = core.Escalation{ID: "E1", SessionID: "S1", Kind: "question", Status: "pending"}

	b.handleMessage(context.Background(), &Message{Text: "use postgres, not sqlite", From: &User{ID: allowedUID}, MessageThreadID: topic})
	require.Len(t, act.answers, 1, "free text in a topic with a pending question is the answer")
	require.Equal(t, "E1", act.answers[0].escID)
	require.Equal(t, "use postgres, not sqlite", act.answers[0].text)
	require.Empty(t, act.chatPrompts, "did not fall through to brain chat")
}

func TestChat_SwipeReplyRoutesToThatCard(t *testing.T) {
	b, _, st, act := newTestBot(t)
	topic := int64(5)
	st.sessions["S1"] = core.Session{ID: "S1", Status: core.SessionActive, TGTopicID: &topic}
	st.escs["E1"] = core.Escalation{ID: "E1", SessionID: "S1", WorkerID: "WA", Kind: "question", Status: "pending"}
	st.escs["E2"] = core.Escalation{ID: "E2", SessionID: "S1", WorkerID: "WB", Kind: "question", Status: "pending"}
	// post E1's card so its message id is tracked
	require.NoError(t, b.Send(context.Background(), notify.FormatEscalation(notify.EscalationCard{
		EscalationID: "E1", Kind: "question", SessionID: "S1", WorkerID: "WA", Question: "?"})))
	cardMsgID := b.escMsg["E1"]
	require.NotZero(t, cardMsgID)

	// swipe-reply to E1's card, even though E2 is also pending (and older/newer)
	b.handleMessage(context.Background(), &Message{
		Text: "use the v2 api", From: &User{ID: allowedUID}, MessageThreadID: topic,
		ReplyToMessage: &Message{MessageID: cardMsgID},
	})
	require.Len(t, act.answers, 1)
	require.Equal(t, "E1", act.answers[0].escID, "swipe-reply routes to the replied card, not another pending one")
	require.Equal(t, "use the v2 api", act.answers[0].text)
}

func TestChat_MultiplePendingRefusesToGuess(t *testing.T) {
	b, api, st, act := newTestBot(t)
	topic := int64(5)
	st.sessions["S1"] = core.Session{ID: "S1", Status: core.SessionActive, TGTopicID: &topic}
	st.escs["E1"] = core.Escalation{ID: "E1", SessionID: "S1", WorkerID: "WA", Kind: "question", Status: "pending"}
	st.escs["E2"] = core.Escalation{ID: "E2", SessionID: "S1", WorkerID: "WB", Kind: "question", Status: "pending"}
	b.handleMessage(context.Background(), &Message{Text: "yes", From: &User{ID: allowedUID}, MessageThreadID: topic})
	require.Empty(t, act.answers, "a bare answer with 2 agents waiting must NOT be guessed onto one")
	require.Empty(t, act.chatPrompts, "and it's not sent to the brain")
	require.Contains(t, lastSent(api), "several agents are waiting")
}

func TestChat_FallsThroughToBrain(t *testing.T) {
	b, api, _, act := newTestBot(t)
	act.chatReply = "You have 0 workers running. Use /dispatch <repo> <task> to start one."
	b.handleMessage(context.Background(), &Message{Text: "what's running?", From: &User{ID: allowedUID}, MessageThreadID: 0})
	require.Len(t, act.chatPrompts, 1, "free text with no pending question goes to the brain")
	require.Contains(t, act.chatPrompts[0], "what's running?")
	require.Equal(t, act.chatReply, lastSent(api))
}

func TestChat_UnauthorizedDropped(t *testing.T) {
	b, api, _, act := newTestBot(t)
	b.handleMessage(context.Background(), &Message{Text: "/dispatch /r task", From: &User{ID: 999}})
	b.handleMessage(context.Background(), &Message{Text: "hello", From: &User{ID: 999}})
	require.Empty(t, act.dispatches)
	require.Empty(t, act.chatPrompts)
	require.Empty(t, api.sent, "a stranger gets nothing")
}

func TestCmd_HelpAndUnknown(t *testing.T) {
	b, api, _, _ := newTestBot(t)
	b.handleMessage(context.Background(), &Message{Text: "/help", From: &User{ID: allowedUID}})
	require.Contains(t, lastSent(api), "/dispatch")
	b.handleMessage(context.Background(), &Message{Text: "/frobnicate", From: &User{ID: allowedUID}})
	require.Contains(t, strings.ToLower(lastSent(api)), "unknown command")
}

// A new-issue /dispatch from General opens the issue's topic immediately (not
// lazily on the first escalation) and posts the starter card INTO that topic.
func TestCmd_DispatchNewIssueOpensTopic(t *testing.T) {
	b, api, _, _ := newTestBot(t)
	b.handleMessage(context.Background(), &Message{Text: "/dispatch /srv/app.git add health endpoint", From: &User{ID: allowedUID}})

	require.NotEmpty(t, api.created, "a forum topic is created for the new issue")
	// the origin-channel reply points to the opened topic
	require.Contains(t, lastSent(api), "opened its topic")
	// a starter card was posted into a topic thread (thread != 0)
	var starterInTopic bool
	for _, m := range api.sent {
		if m.thread != 0 && strings.Contains(m.text, "issue started") {
			starterInTopic = true
		}
	}
	require.True(t, starterInTopic, "the 🚀 starter card lands in the issue's topic")
}

// A natural-language chat question gets the LIVE herdr sessions in its context
// (not just arco's own ledger), so "how many claude sessions are running?" can
// be answered even when arco launched 0 workers.
func TestChat_IncludesLiveHerdrSessions(t *testing.T) {
	b, _, _, act := newTestBot(t)
	act.scanOut = []core.ScannedAgent{
		{AgentObs: core.AgentObs{Ref: "w1:p1", Kind: "claude", State: "working", Cwd: "/home/op/arco", Alive: true}},
		{AgentObs: core.AgentObs{Ref: "w5:p1", Kind: "claude", State: "idle", Cwd: "/home/op/sysadmin", Alive: true}},
	}
	b.handleMessage(context.Background(), &Message{Text: "how many claude sessions are running?", From: &User{ID: allowedUID}})
	require.NotEmpty(t, act.chatPrompts, "chat routed to the brain")
	p := act.chatPrompts[0]
	require.Contains(t, p, "Live herdr agent sessions")
	require.Contains(t, p, "2 total (2 live")
	require.Contains(t, p, "/home/op/sysadmin", "the real herdr session cwds are in the brain context")
}

// NOTE: /peek is now a registered feature (internal/features.Peek) — its command
// (brain summary) + tool (raw tail) are covered in internal/features/peek_test.go.

// Chat carries short per-thread memory: the second turn's brain prompt includes
// the first exchange, so a follow-up ("peek into it") can resolve.
func TestChat_RemembersRecentTurns(t *testing.T) {
	b, _, _, act := newTestBot(t)
	act.chatReply = "There are 2 sessions in ~/arco and ~/sysadmin."
	b.handleMessage(context.Background(), &Message{Text: "what sessions are running?", From: &User{ID: allowedUID}})
	b.handleMessage(context.Background(), &Message{Text: "peek into it", From: &User{ID: allowedUID}})
	require.Len(t, act.chatPrompts, 2)
	require.Contains(t, act.chatPrompts[1], "what sessions are running?", "prior operator turn is in context")
	require.Contains(t, act.chatPrompts[1], "~/sysadmin", "prior arco reply is in context")
}

// NOTE: /scan is now a registered feature (internal/features.Scan) — its
// rendering is covered in internal/features/scan_test.go, and the registry
// wiring in internal/daemon. The telegram package only owns the coexistence
// seam (feature_test.go), not the scan command itself.

// NOTE: /adopt is now a registered feature (internal/features.Adopt) — the
// adopt-all/untracked-only logic and adopt-by-ref are covered in
// internal/features/mutate_test.go.

// NOTE: /vms is now a registered feature (internal/features.VMs) — its rendering
// is covered in internal/features/fleet_test.go.
