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

func TestCmd_KillResolvesByFragment(t *testing.T) {
	b, _, st, act := newTestBot(t)
	st.workers = []core.Worker{{ID: "01ABCWORKERXYZ", State: core.WorkerRunning}}
	b.handleMessage(context.Background(), &Message{Text: "/kill WORKERXYZ", From: &User{ID: allowedUID}})
	require.Equal(t, []string{"01ABCWORKERXYZ"}, act.kills, "resolves a worker by any id fragment")
}

func TestCmd_KillAmbiguousIsRefused(t *testing.T) {
	b, api, st, act := newTestBot(t)
	st.workers = []core.Worker{{ID: "01AAABBB", State: core.WorkerRunning}, {ID: "01AAACCC", State: core.WorkerRunning}}
	b.handleMessage(context.Background(), &Message{Text: "/kill 01AAA", From: &User{ID: allowedUID}})
	require.Empty(t, act.kills, "an ambiguous fragment kills nothing")
	require.Contains(t, lastSent(api), "matches 2 workers")
}

func TestCmd_WorkersAndSessionsRender(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	seedSession(st, "S1", "fix-auth")
	st.workers = []core.Worker{{ID: "01AAAA1111", OwnerSession: "S1", State: core.WorkerRunning, Task: "do the thing"}}
	b.handleMessage(context.Background(), &Message{Text: "/workers", From: &User{ID: allowedUID}})
	require.Contains(t, lastSent(api), "do the thing")
	b.handleMessage(context.Background(), &Message{Text: "/sessions", From: &User{ID: allowedUID}})
	require.Contains(t, lastSent(api), "fix-auth")
}

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

func TestCmd_VMsListsFleet(t *testing.T) {
	api := &fakeAPIRec{}
	b := New(Config{
		API: api, Store: newFakeStore(), GroupID: -1, MinLevel: notify.LevelInfo,
		Actions: &fakeActions{}, Allowed: []int64{allowedUID},
		VMs: []string{"local (default · this box)", "vm1 (host vm1)"},
	})
	b.handleMessage(context.Background(), &Message{Text: "/vms", From: &User{ID: allowedUID}})
	last := api.sent[len(api.sent)-1].text
	require.Contains(t, last, "attached VMs (2)")
	require.Contains(t, last, "vm1 (host vm1)")
}
