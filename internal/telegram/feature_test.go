package telegram

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
	"github.com/dinhlongviolin1/arco/internal/features"
	"github.com/dinhlongviolin1/arco/internal/notify"
)

// fakeScanner is a minimal Scanner for driving the real features.Scan feature
// end-to-end through the bot (auth → slash routing → reply).
type fakeScanner struct{ agents []core.ScannedAgent }

func (f fakeScanner) ScanAgents(context.Context) ([]core.ScannedAgent, error) {
	return f.agents, nil
}

// newTestBotRegActions is newTestBotReg but returns the fakeActions too, so a
// test can assert which chat path (Converse vs BrainReply) was taken.
func newTestBotRegActions(t *testing.T, reg *feature.Registry) (*Bot, *fakeAPIRec, *fakeActions) {
	t.Helper()
	api := &fakeAPIRec{}
	act := &fakeActions{}
	b := New(Config{
		API: api, Store: newFakeStore(), GroupID: -100999, MinLevel: notify.LevelInfo,
		Actions: act, Allowed: []int64{allowedUID}, Redact: fakeScrubber{}, Registry: reg,
	})
	return b, api, act
}

// With brain tools registered, a free-text chat message routes to the agentic
// Converse path (not the one-shot BrainReply), carrying the brain tools.
func TestChat_AgenticWhenToolsRegistered(t *testing.T) {
	var scanCalls int
	reg := feature.NewRegistry()
	reg.MustRegister(feature.Feature{Name: "scan", Tool: &feature.Tool{
		Name: "scan", Desc: "list", Access: feature.BrainSafe,
		Call: func(context.Context, json.RawMessage) (string, error) { scanCalls++; return "", nil },
	}})
	b, api, act := newTestBotRegActions(t, reg)
	act.chatReply = "there are 2 running"
	b.handleMessage(context.Background(), &Message{Text: "what's running?", From: &User{ID: allowedUID}})

	require.Len(t, act.conversePrompts, 1, "chat routed to the agentic Converse path")
	require.Empty(t, act.chatPrompts, "the one-shot BrainReply path was NOT used")
	require.Len(t, act.converseTools, 1, "the brain tools were passed to Converse")
	require.Equal(t, "scan", act.converseTools[0].Name)
	require.Contains(t, act.converseSystems[0], "CALL the scan tool", "the system preamble mandates scan for fleet questions")
	require.Contains(t, lastSent(api), "there are 2 running")
}

// With no registry, chat falls back to the legacy one-shot BrainReply — existing
// behavior is preserved for a bare bot.
func TestChat_FallsBackToBrainReplyWithoutTools(t *testing.T) {
	b, _, act := newTestBotRegActions(t, nil)
	act.chatReply = "hello"
	b.handleMessage(context.Background(), &Message{Text: "hi there", From: &User{ID: allowedUID}})
	require.Len(t, act.chatPrompts, 1, "the one-shot path is used when no tools are registered")
	require.Empty(t, act.conversePrompts)
}

// The REAL /scan feature, registered and driven through handleMessage — proving
// the whole path (not just the fake echo seam) works after the port.
func TestFeatureCommand_RealScanThroughHandleMessage(t *testing.T) {
	reg := feature.NewRegistry()
	reg.MustRegister(features.Scan(fakeScanner{agents: []core.ScannedAgent{
		{AgentObs: core.AgentObs{Ref: "w1:p1", Kind: "claude", State: "working", Alive: true}, Tracked: true, WorkerID: "01AAAAAAAA"},
		{AgentObs: core.AgentObs{Ref: "w5:p1", Kind: "claude", State: "idle", Alive: true}},
	}}))
	b, api, _ := newTestBotReg(t, reg)
	b.handleMessage(context.Background(), &Message{Text: "/scan", From: &User{ID: allowedUID}})
	out := lastSent(api)
	require.Contains(t, out, "herdr agent sessions (2 — 2 live, 0 done)")
	require.Contains(t, out, "✅ tracked")
	require.Contains(t, out, "🆓 untracked")
	require.Contains(t, out, "/adopt")
}

// newTestBotReg is newTestBot with a feature registry wired in.
func newTestBotReg(t *testing.T, reg *feature.Registry) (*Bot, *fakeAPIRec, *fakeStore) {
	t.Helper()
	api := &fakeAPIRec{}
	st := newFakeStore()
	b := New(Config{
		API: api, Store: st, GroupID: -100999, MinLevel: notify.LevelInfo,
		Actions: &fakeActions{}, Allowed: []int64{allowedUID}, Redact: fakeScrubber{},
		Registry: reg,
	})
	return b, api, st
}

// A registered feature command is dispatched through the default fall-through,
// and its closure receives the resolved arg + thread + session.
func TestFeatureCommand_DispatchedWithResolvedInput(t *testing.T) {
	var got feature.CmdInput
	reg := feature.NewRegistry()
	reg.MustRegister(feature.Feature{Name: "echo", Command: &feature.Command{
		Name: "echo", Help: "echo back",
		Run: func(_ context.Context, in feature.CmdInput) (string, error) {
			got = in
			return "echoed: " + in.Arg, nil
		},
	}})
	b, api, st := newTestBotReg(t, reg)
	topic := int64(42)
	st.sessions["S9"] = core.Session{ID: "S9", Status: core.SessionActive, TGTopicID: &topic}

	b.handleMessage(context.Background(), &Message{
		Text: "/echo hello world", From: &User{ID: allowedUID}, MessageThreadID: topic,
	})

	require.Contains(t, lastSent(api), "echoed: hello world")
	require.Equal(t, "hello world", got.Arg)
	require.Equal(t, topic, got.ThreadID)
	require.Equal(t, "S9", got.SessionID, "the thread's session is resolved for the feature")
}

// A feature command that returns an error surfaces it (with usage) to the operator.
func TestFeatureCommand_ErrorSurfaced(t *testing.T) {
	reg := feature.NewRegistry()
	reg.MustRegister(feature.Feature{Name: "boom", Command: &feature.Command{
		Name: "boom", Usage: "<x>",
		Run: func(context.Context, feature.CmdInput) (string, error) {
			return "", context.DeadlineExceeded
		},
	}})
	b, api, _ := newTestBotReg(t, reg)
	b.handleMessage(context.Background(), &Message{Text: "/boom", From: &User{ID: allowedUID}})
	require.Contains(t, lastSent(api), "/boom:")
	require.Contains(t, lastSent(api), "usage: /boom <x>")
}

// A feature that registers a name the switch already owns is a wiring bug that
// must fail loud at assembly, not silently produce a dead menu entry.
func TestFeatureCommand_ShadowingBuiltinPanics(t *testing.T) {
	reg := feature.NewRegistry()
	reg.MustRegister(feature.Feature{Name: "x", Command: &feature.Command{
		Name: "dispatch",
		Run:  func(context.Context, feature.CmdInput) (string, error) { return "FEATURE", nil },
	}})
	require.PanicsWithValue(t,
		"telegram: feature command /dispatch shadows a built-in — rename it or port the built-in",
		func() { newTestBotReg(t, reg) },
		"registering a built-in name must panic at New()")
}

// A case/slash VARIANT of a built-in name must also panic — the registry
// canonicalizes the name, so the lower-cased built-in guard still catches it
// (otherwise it would double-list yet be shadowed by the case-folding switch).
func TestFeatureCommand_ShadowingBuiltinVariantPanics(t *testing.T) {
	reg := feature.NewRegistry()
	reg.MustRegister(feature.Feature{Name: "x", Command: &feature.Command{
		Name: "/Dispatch", // capitalized + slashed variant of the built-in
		Run:  func(context.Context, feature.CmdInput) (string, error) { return "", nil },
	}})
	require.Panics(t, func() { newTestBotReg(t, reg) },
		"a case/slash variant of a built-in must still panic at assembly")
}

// /help generates a features section from the registry so a registered command
// is documented without editing the hardcoded help text.
func TestHelp_ListsFeatureCommands(t *testing.T) {
	reg := feature.NewRegistry()
	reg.MustRegister(feature.Feature{Name: "notes", Command: &feature.Command{
		Name: "notes", Usage: "<query>", Help: "search your notes",
		Run: func(context.Context, feature.CmdInput) (string, error) { return "", nil },
	}})
	b, api, _ := newTestBotReg(t, reg)
	b.handleMessage(context.Background(), &Message{Text: "/help", From: &User{ID: allowedUID}})
	out := lastSent(api)
	require.Contains(t, out, "features:")
	require.Contains(t, out, "/notes <query>")
	require.Contains(t, out, "search your notes")
}

// The command chokepoint scrubs a feature's reply before posting — a feature that
// surfaces raw worker output (e.g. /peek's tail) can't leak a secret to Telegram.
func TestFeatureCommand_ReplyIsScrubbed(t *testing.T) {
	reg := feature.NewRegistry()
	reg.MustRegister(feature.Feature{Name: "leak", Command: &feature.Command{
		Name: "leak",
		Run:  func(context.Context, feature.CmdInput) (string, error) { return "token is SECRET here", nil },
	}})
	b, api, _ := newTestBotReg(t, reg)
	b.handleMessage(context.Background(), &Message{Text: "/leak", From: &User{ID: allowedUID}})
	out := lastSent(api)
	require.NotContains(t, out, "SECRET", "the feature reply is scrubbed at the command chokepoint")
	require.Contains(t, out, "[REDACTED]")
}

// An unknown command with no matching feature still reports "unknown".
func TestFeatureCommand_UnknownStillUnknown(t *testing.T) {
	b, api, _ := newTestBotReg(t, feature.NewRegistry())
	b.handleMessage(context.Background(), &Message{Text: "/nope", From: &User{ID: allowedUID}})
	require.Contains(t, lastSent(api), "unknown command")
}

// The generated command menu includes registered feature commands, so "/"
// autocomplete stays in sync with the registry, and a built-in still appears
// exactly once (the dedup guard against the canonical built-in set).
func TestMenu_IncludesFeatureCommands(t *testing.T) {
	reg := feature.NewRegistry()
	reg.MustRegister(feature.Feature{Name: "notes", Command: &feature.Command{Name: "notes", Help: "list notes"}})
	b, _, _ := newTestBotReg(t, reg)
	menu := b.menu()
	var haveNotes, dispatchCount int
	for _, c := range menu {
		if c.Command == "notes" {
			haveNotes++
		}
		if c.Command == "dispatch" {
			dispatchCount++
		}
	}
	require.Equal(t, 1, haveNotes, "the feature command appears in the menu")
	require.Equal(t, 1, dispatchCount, "the built-in /dispatch appears exactly once")
}
