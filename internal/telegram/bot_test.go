package telegram

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// --- fakes ---

type sentMsg struct {
	thread   int64
	text     string
	keyboard *InlineKeyboardMarkup
}

type fakeAPIRec struct {
	mu       sync.Mutex
	sent     []sentMsg
	edits    []EditMessageTextReq
	created  []string // topic names
	closed   []int64
	pinned   []int64
	toasts   []string
	photos   []SendPhotoReq
	commands []BotCommand
	nextMsg  int64
	nextTop  int64
}

func (f *fakeAPIRec) SendMessage(_ context.Context, req SendMessageReq) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{thread: req.MessageThreadID, text: req.Text, keyboard: req.ReplyMarkup})
	f.nextMsg++
	return Message{MessageID: f.nextMsg, MessageThreadID: req.MessageThreadID}, nil
}
func (f *fakeAPIRec) EditMessageText(_ context.Context, req EditMessageTextReq) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, req)
	return nil
}
func (f *fakeAPIRec) CreateForumTopic(_ context.Context, _ int64, name string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, name)
	f.nextTop++
	return f.nextTop, nil
}
func (f *fakeAPIRec) CloseForumTopic(_ context.Context, _, threadID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, threadID)
	return nil
}
func (f *fakeAPIRec) PinChatMessage(_ context.Context, _, messageID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pinned = append(f.pinned, messageID)
	return nil
}
func (f *fakeAPIRec) AnswerCallbackQuery(_ context.Context, _, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toasts = append(f.toasts, text)
	return nil
}
func (f *fakeAPIRec) GetUpdates(_ context.Context, _, _ int) ([]Update, error) { return nil, nil }
func (f *fakeAPIRec) SendPhoto(_ context.Context, req SendPhotoReq) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.photos = append(f.photos, req)
	f.nextMsg++
	return Message{MessageID: f.nextMsg, MessageThreadID: req.MessageThreadID}, nil
}
func (f *fakeAPIRec) GetFile(_ context.Context, fileID string) (File, error) {
	return File{FileID: fileID, FilePath: "photos/" + fileID + ".jpg"}, nil
}
func (f *fakeAPIRec) DownloadFile(_ context.Context, _ string) ([]byte, error) {
	return []byte("IMAGEBYTES"), nil
}
func (f *fakeAPIRec) SetMyCommands(_ context.Context, cmds []BotCommand) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = cmds
	return nil
}

type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]core.Session
	escs     map[string]core.Escalation
	workers  []core.Worker
}

func newFakeStore() *fakeStore {
	return &fakeStore{sessions: map[string]core.Session{}, escs: map[string]core.Escalation{}}
}
func (s *fakeStore) GetSession(id string) (core.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id], nil
}
func (s *fakeStore) GetWorker(id string) (core.Worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.workers {
		if w.ID == id {
			return w, nil
		}
	}
	return core.Worker{}, errNotFound
}
func (s *fakeStore) GetEscalation(id string) (core.Escalation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.escs[id]
	if !ok {
		return core.Escalation{}, errNotFound
	}
	return e, nil
}
func (s *fakeStore) ListWorkers(f core.WorkerFilter) ([]core.Worker, error) {
	var out []core.Worker
	for _, w := range s.workers {
		if f.OwnerSession == "" || w.OwnerSession == f.OwnerSession {
			out = append(out, w)
		}
	}
	return out, nil
}
func (s *fakeStore) ListSessions(core.SessionFilter) ([]core.Session, error) {
	out := make([]core.Session, 0, len(s.sessions))
	for _, v := range s.sessions {
		out = append(out, v)
	}
	return out, nil
}
func (s *fakeStore) ListEscalations(f core.EscalationFilter) ([]core.Escalation, error) {
	var out []core.Escalation
	for _, e := range s.escs {
		if (f.SessionID == "" || e.SessionID == f.SessionID) && (f.Status == "" || e.Status == f.Status) {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *fakeStore) SetSessionTelegram(_ context.Context, id string, topicID, statusMsgID *int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if topicID != nil {
		sess.TGTopicID = topicID
	}
	if statusMsgID != nil {
		sess.TGStatusMsgID = statusMsgID
	}
	s.sessions[id] = sess
	return nil
}

var errNotFound = &notFound{}

type notFound struct{}

func (*notFound) Error() string { return "not found" }

type answerCall struct {
	escID string
	text  string
	scope core.Scope
}
type confirmCall struct {
	escID string
	yes   bool
}
type fakeActions struct {
	mu          sync.Mutex
	answers     []answerCall
	confirms    []confirmCall
	diffOut     string
	paused      bool
	pauseHit    int
	resHit      int
	dispatches  []string
	kills       []string
	chatPrompts []string
	chatReply   string
	scanOut     []ScannedAgent
	adopts      []string
}

func (a *fakeActions) AnswerQuestion(_ context.Context, escID, text string, scope core.Scope) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answers = append(a.answers, answerCall{escID, text, scope})
	return nil
}
func (a *fakeActions) DecideConfirm(_ context.Context, escID string, yes bool, _ core.Scope) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.confirms = append(a.confirms, confirmCall{escID, yes})
	return nil
}
func (a *fakeActions) WorkerDiff(context.Context, string) (string, error) { return a.diffOut, nil }
func (a *fakeActions) Pause(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pauseHit++
	a.paused = true
	return nil
}
func (a *fakeActions) Resume(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resHit++
	a.paused = false
	return nil
}
func (a *fakeActions) Paused() bool { return a.paused }
func (a *fakeActions) Dispatch(_ context.Context, repo, task, vm, into string) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	rec := repo + " | " + task
	if vm != "" {
		rec += " | vm=" + vm
	}
	if into != "" {
		rec += " | into=" + into
	}
	a.dispatches = append(a.dispatches, rec)
	return "01WORKERID000000000000000", "01SESSIONID00000000000000", nil
}
func (a *fakeActions) Kill(_ context.Context, workerID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.kills = append(a.kills, workerID)
	return nil
}
func (a *fakeActions) BrainReply(_ context.Context, prompt string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.chatPrompts = append(a.chatPrompts, prompt)
	if a.chatReply == "" {
		return "ok", nil
	}
	return a.chatReply, nil
}
func (a *fakeActions) Scan(context.Context) ([]ScannedAgent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.scanOut, nil
}
func (a *fakeActions) Adopt(_ context.Context, ref string) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.adopts = append(a.adopts, ref)
	return "01WORKERID000000000000000", "01SESSIONID00000000000000", nil
}

type fakeScrubber struct{}

func (fakeScrubber) Scrub(s string) (string, int) {
	return strings.ReplaceAll(s, "SECRET", "[REDACTED]"), 0
}
func (fakeScrubber) Version() string { return "test" }

const allowedUID = int64(573409113)

func newTestBot(t *testing.T) (*Bot, *fakeAPIRec, *fakeStore, *fakeActions) {
	t.Helper()
	api := &fakeAPIRec{}
	st := newFakeStore()
	act := &fakeActions{}
	b := New(Config{
		API: api, Store: st, GroupID: -100999, MinLevel: notify.LevelInfo,
		Actions: act, Allowed: []int64{allowedUID}, Redact: fakeScrubber{},
	})
	return b, api, st, act
}

func seedSession(st *fakeStore, id, slug string) {
	st.sessions[id] = core.Session{ID: id, Slug: slug, Status: core.SessionActive, SupervisionMode: core.ModeAssist}
}

// --- T2/T3: outbound ---

func TestSend_CreatesTopicStatusCardAndEscalationKeyboard(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	seedSession(st, "S1", "fix-auth")
	st.escs["E1"] = core.Escalation{ID: "E1", SessionID: "S1", WorkerID: "W1", Kind: "question", Status: "pending", DraftAnswer: "do X"}

	card := notify.FormatEscalation(notify.EscalationCard{
		EscalationID: "E1", Kind: "question", SessionID: "S1", WorkerID: "W1", Question: "proceed?",
	})
	require.NoError(t, b.Send(context.Background(), card))

	require.Equal(t, []string{"session: fix-auth"}, api.created, "topic created once with a session name")
	require.NotNil(t, st.sessions["S1"].TGTopicID, "topic id persisted")
	require.NotEmpty(t, api.pinned, "status card pinned")
	require.NotNil(t, st.sessions["S1"].TGStatusMsgID, "status card msg id persisted")

	// the escalation card carries the question keyboard, routed to the topic.
	var esc *sentMsg
	for i := range api.sent {
		if api.sent[i].keyboard != nil {
			esc = &api.sent[i]
		}
	}
	require.NotNil(t, esc, "an escalation message with a keyboard was sent")
	require.Equal(t, EncodeCallback(ActQuestionOnce, "E1"), esc.keyboard.InlineKeyboard[0][0].CallbackData)
	require.NotZero(t, esc.thread, "escalation routed into the session topic, not General")
}

func TestSend_ReusesExistingTopic(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	seedSession(st, "S1", "s1")
	require.NoError(t, b.Send(context.Background(), notify.Card{Level: notify.LevelWarn, Title: "a", SessionID: "S1"}))
	require.NoError(t, b.Send(context.Background(), notify.Card{Level: notify.LevelWarn, Title: "b", SessionID: "S1"}))
	require.Len(t, api.created, 1, "topic created exactly once across two cards")
}

func TestSend_MutedAndBelowMinDropped(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	seedSession(st, "S1", "s1")
	s := st.sessions["S1"]
	s.NotifyLevel = "silent"
	st.sessions["S1"] = s
	require.NoError(t, b.Send(context.Background(), notify.Card{Level: notify.LevelUrgent, Title: "x", SessionID: "S1"}))
	require.Empty(t, api.sent, "silent session mutes everything")
	require.Empty(t, api.created, "muted card must not even create a topic")

	// below global min
	b.min = notify.LevelWarn
	s.NotifyLevel = "all"
	st.sessions["S1"] = s
	require.NoError(t, b.Send(context.Background(), notify.Card{Level: notify.LevelInfo, Title: "y", SessionID: "S1"}))
	require.Empty(t, api.sent, "below-min card dropped")
}

func TestSend_MirrorsEscalationToGeneral(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	seedSession(st, "S1", "s1")
	st.escs["E1"] = core.Escalation{ID: "E1", SessionID: "S1", Kind: "question", Status: "pending"}
	require.NoError(t, b.Send(context.Background(), notify.FormatEscalation(notify.EscalationCard{
		EscalationID: "E1", Kind: "question", SessionID: "S1", WorkerID: "W1", Question: "?",
	})))
	var mirrored bool
	for _, m := range api.sent {
		if m.thread == 0 && strings.Contains(m.text, "decision needed in topic") {
			mirrored = true
		}
	}
	require.True(t, mirrored, "an open escalation is mirrored to General (thread 0)")
}

func TestSend_ResolvedEditsOriginalAndStripsButtons(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	seedSession(st, "S1", "s1")
	st.escs["E1"] = core.Escalation{ID: "E1", SessionID: "S1", WorkerID: "W1", Kind: "question", Status: "pending", DraftAnswer: "do X"}

	// open escalation card → stores its message id
	require.NoError(t, b.Send(context.Background(), notify.FormatEscalation(notify.EscalationCard{
		EscalationID: "E1", Kind: "question", SessionID: "S1", WorkerID: "W1", Question: "?",
	})))
	sentBefore := len(api.sent)
	require.NotZero(t, b.escMsg["E1"], "open escalation card message id remembered")

	// resolution card → edits the SAME message, strips the keyboard, posts nothing new
	require.NoError(t, b.Send(context.Background(), notify.Card{
		Level: notify.LevelInfo, Title: "arco: escalation answered — W1", Body: "answer: do X",
		SessionID: "S1", EscalationID: "E1", Resolved: true,
	}))
	require.Len(t, api.edits, 1, "resolved card edits in place")
	require.NotNil(t, api.edits[0].ReplyMarkup)
	require.Empty(t, api.edits[0].ReplyMarkup.InlineKeyboard, "buttons stripped on resolve")
	require.Contains(t, api.edits[0].Text, "answered")
	require.Equal(t, sentBefore, len(api.sent), "no NEW message posted for the resolution")
	require.Zero(t, b.escMsg["E1"], "card id forgotten after resolve")
}

func TestSend_ResolvedFallsBackWhenCardUnknown(t *testing.T) {
	b, api, st, _ := newTestBot(t)
	seedSession(st, "S1", "s1")
	// no prior open card for E9 (e.g. daemon restarted) → resolution posts a normal message
	require.NoError(t, b.Send(context.Background(), notify.Card{
		Level: notify.LevelInfo, Title: "answered — W1", SessionID: "S1", EscalationID: "E9", Resolved: true,
	}))
	require.Empty(t, api.edits, "nothing to edit")
	require.NotEmpty(t, api.sent, "falls back to posting a message")
}

// --- T4: inbound auth + actions ---

func TestCallback_UnauthorizedUserIsDropped(t *testing.T) {
	b, api, st, act := newTestBot(t)
	st.escs["E1"] = core.Escalation{ID: "E1", Kind: "question", Status: "pending", DraftAnswer: "x"}
	b.handleCallback(context.Background(), &CallbackQuery{
		ID: "cbq", From: User{ID: 999}, Data: EncodeCallback(ActQuestionOnce, "E1"),
	})
	require.Empty(t, act.answers, "a stranger's tap must NOT reach the engine")
	require.Equal(t, []string{"not authorized"}, api.toasts)
}

func TestCallback_QuestionOnceUsesDraftAndScopeOnce(t *testing.T) {
	b, _, st, act := newTestBot(t)
	st.escs["E1"] = core.Escalation{ID: "E1", Kind: "question", Status: "pending", DraftAnswer: "apply the migration"}
	b.handleCallback(context.Background(), &CallbackQuery{ID: "c", From: User{ID: allowedUID}, Data: EncodeCallback(ActQuestionOnce, "E1")})
	require.Len(t, act.answers, 1)
	require.Equal(t, "apply the migration", act.answers[0].text, "tap accepts the brain DRAFT")
	require.Equal(t, core.ScopeOnce, act.answers[0].scope)
}

func TestCallback_QuestionAlwaysUsesSessionScope(t *testing.T) {
	b, _, st, act := newTestBot(t)
	st.escs["E1"] = core.Escalation{ID: "E1", Kind: "question", Status: "pending", DraftAnswer: "yes"}
	b.handleCallback(context.Background(), &CallbackQuery{ID: "c", From: User{ID: allowedUID}, Data: EncodeCallback(ActQuestionSession, "E1")})
	require.Len(t, act.answers, 1)
	require.Equal(t, core.ScopeSession, act.answers[0].scope, "'always' promotes a standing session grant")
}

func TestCallback_ConfirmApproveAndReject(t *testing.T) {
	b, _, st, act := newTestBot(t)
	st.escs["C1"] = core.Escalation{ID: "C1", Kind: "confirm", Status: "pending"}
	b.handleCallback(context.Background(), &CallbackQuery{ID: "c", From: User{ID: allowedUID}, Data: EncodeCallback(ActConfirmYes, "C1")})
	b.handleCallback(context.Background(), &CallbackQuery{ID: "c", From: User{ID: allowedUID}, Data: EncodeCallback(ActConfirmNo, "C1")})
	require.Equal(t, []confirmCall{{"C1", true}, {"C1", false}}, act.confirms)
}

func TestCallback_DiffIsRedactedAndPosted(t *testing.T) {
	b, api, st, act := newTestBot(t)
	seedSession(st, "S1", "s1")
	st.escs["E1"] = core.Escalation{ID: "E1", SessionID: "S1", WorkerID: "W1", Kind: "question", Status: "pending"}
	act.diffOut = "line with SECRET token"
	b.handleCallback(context.Background(), &CallbackQuery{ID: "c", From: User{ID: allowedUID}, Data: EncodeCallback(ActDiff, "E1")})
	var got string
	for _, m := range api.sent {
		if strings.Contains(m.text, "diff — W1") {
			got = m.text
		}
	}
	require.NotEmpty(t, got, "diff was posted")
	require.Contains(t, got, "[REDACTED]", "diff is scrubbed before leaving for Telegram")
	require.NotContains(t, got, "SECRET")
}

func TestCallback_MalformedDataDropped(t *testing.T) {
	b, api, _, act := newTestBot(t)
	b.handleCallback(context.Background(), &CallbackQuery{ID: "c", From: User{ID: allowedUID}, Data: "rm -rf:whatever"})
	require.Empty(t, act.answers)
	require.Empty(t, act.confirms)
	require.Equal(t, []string{"unknown action"}, api.toasts)
}

// --- T5: console ---

func TestMessage_ConsolePauseResumeAuth(t *testing.T) {
	b, _, _, act := newTestBot(t)
	// unauthorized command dropped
	b.handleMessage(context.Background(), &Message{Text: "/pause", From: &User{ID: 999}})
	require.Zero(t, act.pauseHit, "a stranger cannot pause the fleet")
	// authorized
	b.handleMessage(context.Background(), &Message{Text: "/pause", From: &User{ID: allowedUID}})
	b.handleMessage(context.Background(), &Message{Text: "/resume", From: &User{ID: allowedUID}})
	require.Equal(t, 1, act.pauseHit)
	require.Equal(t, 1, act.resHit)
}

func TestMessage_StatusReportsEstopAndCounts(t *testing.T) {
	b, api, st, act := newTestBot(t)
	st.workers = []core.Worker{{ID: "W1", State: core.WorkerRunning}, {ID: "W2", State: core.WorkerRunning}}
	act.paused = true
	b.handleMessage(context.Background(), &Message{Text: "/status", From: &User{ID: allowedUID}})
	require.NotEmpty(t, api.sent)
	last := api.sent[len(api.sent)-1].text
	require.Contains(t, last, "ESTOP ENGAGED")
	require.Contains(t, last, "active workers: 2")
}
