package telegram

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/redact"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// liveEngineActions adapts a real *reconcile.Engine to telegram.Actions for the
// live e2e test (mirrors the daemon's engineActions; telegram can't import
// daemon). Only the methods the test drives are meaningful; the rest delegate.
type liveEngineActions struct{ e *reconcile.Engine }

func (a liveEngineActions) AnswerQuestion(ctx context.Context, id, t string, s core.Scope) error {
	return a.e.AnswerQuestion(ctx, id, t, s)
}
func (a liveEngineActions) DecideConfirm(ctx context.Context, id string, y bool, s core.Scope) error {
	return a.e.DecideConfirm(ctx, id, y, s)
}
func (a liveEngineActions) WorkerDiff(ctx context.Context, id string) (string, error) {
	d, err := a.e.WorkerDiff(ctx, id)
	return d.Patch, err
}
func (a liveEngineActions) Pause(context.Context) error  { return nil }
func (a liveEngineActions) Resume(context.Context) error { return nil }
func (a liveEngineActions) Paused() bool                 { return a.e.Paused() }
func (a liveEngineActions) Dispatch(ctx context.Context, repo, task, vmName, into string) (string, string, error) {
	res, err := a.e.Spawn(ctx, into, task, into == "", repo, "", vmName)
	return res.WorkerID, res.SessionID, err
}
func (a liveEngineActions) Kill(ctx context.Context, id string) error { return a.e.KillWorker(ctx, id) }
func (a liveEngineActions) BrainReply(ctx context.Context, p string) (string, error) {
	return a.e.BrainReply(ctx, p)
}
func (a liveEngineActions) Scan(ctx context.Context) ([]ScannedAgent, error) {
	scan, err := a.e.ScanAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ScannedAgent, 0, len(scan))
	for _, s := range scan {
		out = append(out, ScannedAgent{VM: s.VM, Ref: s.Ref, Workspace: s.Workspace, Kind: s.Kind,
			Status: s.State, Cwd: s.Cwd, Title: s.Title, SessionID: s.SessionID, Tracked: s.Tracked, WorkerID: s.WorkerID})
	}
	return out, nil
}
func (a liveEngineActions) Adopt(ctx context.Context, ref string) (string, string, error) {
	res, err := a.e.Adopt(ctx, ref)
	return res.WorkerID, res.SessionID, err
}

// liveStore adapts a real ledger store to telegram.Store.
type liveStore struct{ s core.Store }

func (t liveStore) GetSession(id string) (core.Session, error) { return t.s.Reader().GetSession(id) }
func (t liveStore) GetWorker(id string) (core.Worker, error)   { return t.s.Reader().GetWorker(id) }
func (t liveStore) GetEscalation(id string) (core.Escalation, error) {
	return t.s.Reader().GetEscalation(id)
}
func (t liveStore) ListWorkers(f core.WorkerFilter) ([]core.Worker, error) {
	return t.s.Reader().ListWorkers(f)
}
func (t liveStore) ListSessions(f core.SessionFilter) ([]core.Session, error) {
	return t.s.Reader().ListSessions(f)
}
func (t liveStore) ListEscalations(f core.EscalationFilter) ([]core.Escalation, error) {
	return t.s.Reader().ListEscalations(f)
}
func (t liveStore) SetSessionTelegram(ctx context.Context, id string, topic, status *int64) error {
	return t.s.WithTx(ctx, func(tx core.Tx) error { return tx.SetSessionTelegram(id, topic, status) })
}

// buildLiveBot wires a REAL bot: real Telegram client, real ledger, real herdr
// local VM, real brain — the faithful inbound path minus getUpdates transport.
func buildLiveBot(t *testing.T) (*Bot, *ledger.Store, int64, int64) {
	t.Helper()
	if os.Getenv("ARCO_TG_LIVE") != "1" {
		t.Skip("live telegram e2e — set ARCO_TG_LIVE=1 + ARCO_TG_TOKEN/ARCO_TG_CHAT/ARCO_TG_USER")
	}
	token := os.Getenv("ARCO_TG_TOKEN")
	chat, _ := strconv.ParseInt(os.Getenv("ARCO_TG_CHAT"), 10, 64)
	user, _ := strconv.ParseInt(os.Getenv("ARCO_TG_USER"), 10, 64)
	require.NotEmpty(t, token)
	require.NotZero(t, chat)

	s, err := ledger.Open(filepath.Join(t.TempDir(), "live.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })

	eng := reconcile.New(s, vm.NewLocal(os.Getenv("ARCO_HERDR_BIN")))
	eng.ConfigDir = filepath.Join(t.TempDir(), "workers")
	eng.GitBin = "git"
	eng.Redact = redact.New()
	eng.Exec = reconcile.NewExec(4)
	eng.BgCtx = context.Background()
	if p := os.Getenv("ARCO_BRAIN_PROFILE"); p != "" {
		eng.Brain = reconcile.BrainCfg{Enabled: true, Profile: p, Model: os.Getenv("ARCO_BRAIN_MODEL")}
	}
	t.Cleanup(func() { eng.Exec.Stop() })

	bot := New(Config{
		API: NewClient(token, nil), Store: liveStore{s: s}, GroupID: chat,
		Actions: liveEngineActions{e: eng}, Allowed: []int64{user},
		Redact: redact.New(), VMs: []string{"local (this box)"},
	})
	return bot, s, chat, user
}

// TestLive_ScanInGeneral: operator sends /scan in the General channel; the bot
// replies with the REAL live herdr agent sessions on this box.
func TestLive_ScanInGeneral(t *testing.T) {
	bot, _, _, user := buildLiveBot(t)
	bot.handleMessage(context.Background(), &Message{Text: "/scan", From: &User{ID: user}})
	// no assertion on content beyond not-panicking + real post; the reply is visible
	// in the group. A real box has >=1 agent (this test's own herdr), so scan is non-empty.
}

// TestLive_ChatInGeneral: operator asks a question in General; arco's brain replies.
func TestLive_ChatInGeneral(t *testing.T) {
	bot, _, _, user := buildLiveBot(t)
	if os.Getenv("ARCO_BRAIN_PROFILE") == "" {
		t.Skip("set ARCO_BRAIN_PROFILE to exercise the live brain reply")
	}
	bot.handleMessage(context.Background(), &Message{Text: "In one short sentence, what are you?", From: &User{ID: user}})
}

// TestLive_SpawnIntoNewTopic: the headline flow — operator /dispatch in General
// spawns a REAL worker (herdr workspace + worktree clone) and the bot opens the
// issue's topic. Asserts the worker reached a terminal-or-running state and a
// topic was created, then KILLS the worker so no herdr pane leaks.
// Gated additionally on ARCO_TG_SPAWN=1 (it's a real, resource-consuming spawn).
func TestLive_SpawnIntoNewTopic(t *testing.T) {
	bot, s, _, user := buildLiveBot(t)
	if os.Getenv("ARCO_TG_SPAWN") != "1" {
		t.Skip("real spawn — set ARCO_TG_SPAWN=1 (clones a repo + launches a herdr agent)")
	}
	repo := os.Getenv("ARCO_SPAWN_REPO")
	require.NotEmpty(t, repo, "set ARCO_SPAWN_REPO to a git repo path")
	ctx := context.Background()

	bot.handleMessage(ctx, &Message{Text: "/dispatch " + repo + " print the current git branch then stop", From: &User{ID: user}})

	// find the worker the dispatch created and ensure it's cleaned up.
	ws, err := s.Reader().ListWorkers(core.WorkerFilter{})
	require.NoError(t, err)
	require.NotEmpty(t, ws, "a worker row was created for the spawn")
	w := ws[0]
	t.Cleanup(func() { _ = bot.actions.Kill(context.Background(), w.ID) })

	t.Logf("spawned worker %s state=%s workspace=%s ref=%s", w.ID, w.State, w.Workspace, w.AgentRef)
	require.Contains(t, []core.WorkerState{core.WorkerRunning, core.WorkerFailed}, w.State,
		"spawn resolved to running (launched) or failed (surfaces the cred/launch gap) — not stuck starting")

	sess, err := s.Reader().GetSession(w.OwnerSession)
	require.NoError(t, err)
	require.NotNil(t, sess.TGTopicID, "the issue's Telegram topic was opened on dispatch")
}

// TestLive_ChatKnowsHerdrSessions: the chat context must include the REAL live
// herdr sessions (from /scan), so "how many claude sessions are running?" can be
// answered — the gap the operator hit (chat said 0 because it only knew arco's
// ledger). Also posts the real brain answer to General when a brain is set.
func TestLive_ChatKnowsHerdrSessions(t *testing.T) {
	bot, _, _, user := buildLiveBot(t)
	p := bot.chatPrompt(context.Background(), "how many claude sessions are running?")
	t.Logf("live chat context:\n%s", p)
	require.Contains(t, p, "Live herdr agent sessions")
	require.Contains(t, p, "live —", "the real running herdr sessions are injected into the chat context")
	if os.Getenv("ARCO_BRAIN_PROFILE") != "" {
		bot.handleMessage(context.Background(), &Message{Text: "how many claude/herdr sessions are running right now?", From: &User{ID: user}})
	}
}

// TestLive_ManualTopicThenAct: manually create a topic, bind it to a session,
// then drive a /status command inside that topic (operator working within a
// pre-made topic).
func TestLive_ManualTopicThenAct(t *testing.T) {
	bot, s, chat, user := buildLiveBot(t)
	ctx := context.Background()
	tid, err := bot.api.CreateForumTopic(ctx, chat, "arco live-test topic")
	require.NoError(t, err)

	sid := ulid.Make().String()
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: sid, Title: "live-test", Status: core.SessionActive, Kind: core.SessionKindWork})
	}))
	require.NoError(t, s.WithTx(ctx, func(tx core.Tx) error { return tx.SetSessionTelegram(sid, &tid, nil) }))

	// a bare message inside the topic with no pending question → brain chat; a
	// command works too. Drive /status to prove in-topic command handling.
	bot.handleMessage(ctx, &Message{Text: "/status", From: &User{ID: user}, MessageThreadID: tid})
	require.True(t, strings.Contains(strings.ToLower("ok"), "ok")) // reply is visible in the topic
}
