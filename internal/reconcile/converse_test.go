package reconcile

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

// needleScrubber redacts a fixed secret — enough to prove Converse scrubs the
// WHOLE rendered prompt (including tool output), not just the user message.
type needleScrubber struct{ needle string }

func (s needleScrubber) Scrub(in string) (string, int) {
	if !strings.Contains(in, s.needle) {
		return in, 0
	}
	return strings.ReplaceAll(in, s.needle, "[redacted]"), 1
}
func (needleScrubber) Version() string { return "test" }

func brainSafe(name, out string, calls *int) feature.Tool {
	return feature.Tool{Name: name, Desc: name + " (read-only)", Access: feature.BrainSafe,
		Call: func(context.Context, json.RawMessage) (string, error) { *calls++; return out, nil }}
}

// scriptRunner returns each queued reply per invocation and captures the prompt
// arg clavis was called with (the last CLI arg).
func scriptRunner(prompts *[]string, replies ...string) func(context.Context, string, ...string) ([]byte, error) {
	i := 0
	return func(_ context.Context, _ string, args ...string) ([]byte, error) {
		*prompts = append(*prompts, args[len(args)-1])
		r := `{"final":"(end)"}`
		if i < len(replies) {
			r = replies[i]
		}
		i++
		return []byte(r), nil
	}
}

func TestConverse_CallsToolThenAnswers(t *testing.T) {
	e, _, _ := newEngine(t)
	var prompts []string
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: scriptRunner(&prompts, `{"tool":"scan","args":{}}`, `{"final":"there are 2 running"}`)}
	var scanned int
	out, err := e.Converse(context.Background(), "SYS", "what's running?", "",
		[]feature.Tool{brainSafe("scan", "2 live agents", &scanned)})
	require.NoError(t, err)
	require.Contains(t, out, "2 running")
	require.Equal(t, 1, scanned, "the brain called the scan tool once")
	require.Contains(t, prompts[1], "2 live agents", "the tool result is fed into the next round")
}

func TestConverse_ScrubsWholeRenderedPrompt(t *testing.T) {
	e, _, _ := newEngine(t)
	e.Redact = needleScrubber{needle: "sk-SECRET-TOKEN"}
	var prompts []string
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: scriptRunner(&prompts, `{"tool":"scan","args":{}}`, `{"final":"done"}`)}
	var scanned int
	// The tool returns worker output containing a secret — it must be scrubbed
	// out of the SECOND prompt (which embeds that tool output) before it leaves.
	_, err := e.Converse(context.Background(), "SYS", "peek", "",
		[]feature.Tool{brainSafe("scan", "env: TOKEN=sk-SECRET-TOKEN", &scanned)})
	require.NoError(t, err)
	require.NotContains(t, prompts[1], "sk-SECRET-TOKEN", "tool output secrets are scrubbed before reaching the model")
	require.Contains(t, prompts[1], "[redacted]")
}

func TestConverse_SystemPreambleReachesModel(t *testing.T) {
	e, _, _ := newEngine(t)
	var prompts []string
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: scriptRunner(&prompts, `{"final":"ok"}`)}
	_, err := e.Converse(context.Background(), "MANDATE: call scan for fleet questions", "hi", "", nil)
	require.NoError(t, err)
	require.Contains(t, prompts[0], "MANDATE: call scan", "the caller's system preamble is in the model prompt")
}

func TestConverse_ScrubsAnswerBeforeReturn(t *testing.T) {
	e, _, _ := newEngine(t)
	e.Redact = needleScrubber{needle: "sk-SECRET-TOKEN"}
	var prompts []string
	// The model's FINAL answer echoes a secret (e.g. lifted from tool output). It
	// must be scrubbed before it leaves for Telegram, not just on the way in.
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: scriptRunner(&prompts, `{"final":"the token is sk-SECRET-TOKEN"}`)}
	out, err := e.Converse(context.Background(), "SYS", "what's the token?", "", nil)
	require.NoError(t, err)
	require.NotContains(t, out, "sk-SECRET-TOKEN", "the answer is scrubbed on the way out")
	require.Contains(t, out, "[redacted]")
}

// With a sessionID, Converse injects the session-bound history tool: the brain
// can search this conversation's durable past, and the store's matches feed back.
func TestConverse_InjectsSessionHistoryTool(t *testing.T) {
	e, store, _ := newEngine(t)
	ctx := context.Background()
	require.NoError(t, store.WithTx(ctx, func(tx core.Tx) error {
		return tx.CreateSession(core.Session{ID: "S1", Status: core.SessionOpen, Kind: core.SessionKindWork})
	}))
	require.NoError(t, store.WithTx(ctx, func(tx core.Tx) error {
		_, err := tx.AppendMessage(core.Message{SessionID: "S1", Role: "operator", Content: "we hit the auth bug in login"})
		return err
	}))
	var prompts []string
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: scriptRunner(&prompts, `{"tool":"history","args":{"query":"auth"}}`, `{"final":"you mentioned the auth bug"}`)}

	out, err := e.Converse(ctx, "SYS", "what did we discuss earlier?", "S1", nil)
	require.NoError(t, err)
	require.Contains(t, out, "auth bug")
	require.Contains(t, prompts[0], "history", "the history tool is advertised in the catalog")
	require.Contains(t, prompts[1], "auth bug in login", "the durable match is fed back to the model")
}

// No sessionID → no history tool injected (nothing to bind it to).
func TestConverse_NoHistoryToolWithoutSession(t *testing.T) {
	e, _, _ := newEngine(t)
	var prompts []string
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: scriptRunner(&prompts, `{"final":"hi"}`)}
	_, err := e.Converse(context.Background(), "SYS", "hi", "", nil)
	require.NoError(t, err)
	require.NotContains(t, prompts[0], "history", "no session → no history tool")
}

func TestConverse_DisabledBrain(t *testing.T) {
	e, _, _ := newEngine(t)
	e.Brain = BrainCfg{Enabled: false}
	_, err := e.Converse(context.Background(), "SYS", "hi", "", nil)
	require.Error(t, err)
}

func TestConverse_ProseReplyPassthrough(t *testing.T) {
	e, _, _ := newEngine(t)
	var prompts []string
	e.Brain = BrainCfg{Enabled: true, Profile: "p", Model: "m",
		Runner: scriptRunner(&prompts, "You have 0 workers running.")}
	out, err := e.Converse(context.Background(), "SYS", "status?", "", nil)
	require.NoError(t, err)
	require.Contains(t, out, "0 workers running")
}
