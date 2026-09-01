package features

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

type fakePeeker struct {
	agents   []core.ScannedAgent
	scanErr  error
	tail     string
	peekErr  error
	peekedAt []string
}

func (f *fakePeeker) ScanAgents(context.Context) ([]core.ScannedAgent, error) {
	return f.agents, f.scanErr
}
func (f *fakePeeker) PeekAgent(_ context.Context, ref string, _ int) (string, error) {
	f.peekedAt = append(f.peekedAt, ref)
	return f.tail, f.peekErr
}

func live(ref string) core.ScannedAgent {
	return core.ScannedAgent{AgentObs: core.AgentObs{Ref: ref, Alive: true}}
}

func TestPeek_FeatureShape(t *testing.T) {
	f := Peek(&fakePeeker{}, nil)
	require.Equal(t, "peek", f.Name)
	require.NotNil(t, f.Command)
	require.NotNil(t, f.Tool)
	require.Equal(t, feature.BrainSafe, f.Tool.Access)
}

// The COMMAND returns a brain summary of the pane tail.
func TestPeek_CommandSummarizes(t *testing.T) {
	p := &fakePeeker{tail: "$ go test ./...\nok"}
	summarize := func(_ context.Context, prompt string) (string, error) {
		require.Contains(t, prompt, "go test ./...", "the tail is handed to the summarizer")
		return "It's running the test suite.", nil
	}
	out, err := Peek(p, summarize).Command.Run(context.Background(), feature.CmdInput{Arg: "w5:p1"})
	require.NoError(t, err)
	require.Contains(t, out, "peek w5:p1")
	require.Contains(t, out, "running the test suite")
	require.Equal(t, []string{"w5:p1"}, p.peekedAt)
}

// The COMMAND falls back to the raw tail when the summarizer fails.
func TestPeek_CommandRawFallback(t *testing.T) {
	p := &fakePeeker{tail: "raw terminal bytes"}
	summarize := func(context.Context, string) (string, error) { return "", errors.New("brain down") }
	out, err := Peek(p, summarize).Command.Run(context.Background(), feature.CmdInput{Arg: "w5:p1"})
	require.NoError(t, err)
	require.Contains(t, out, "raw tail")
	require.Contains(t, out, "raw terminal bytes")
}

// The TOOL returns the RAW tail (no nested brain call) for the chat brain to
// summarize itself.
func TestPeek_ToolReturnsRawTail(t *testing.T) {
	p := &fakePeeker{tail: "raw pane output"}
	// summarize must NOT be called by the tool path
	summarize := func(context.Context, string) (string, error) { t.Fatal("tool must not summarize"); return "", nil }
	out, err := Peek(p, summarize).Tool.Call(context.Background(), []byte(`{"pane":"w9:p1"}`))
	require.NoError(t, err)
	require.Equal(t, "raw pane output", out)
	require.Equal(t, []string{"w9:p1"}, p.peekedAt)
}

// A bare invocation resolves to the single live agent.
func TestPeek_ResolvesSingleLive(t *testing.T) {
	p := &fakePeeker{agents: []core.ScannedAgent{live("only:p1")}, tail: "x"}
	out, err := Peek(p, func(context.Context, string) (string, error) { return "sum", nil }).
		Command.Run(context.Background(), feature.CmdInput{Arg: ""})
	require.NoError(t, err)
	require.Contains(t, out, "peek only:p1")
	require.Equal(t, []string{"only:p1"}, p.peekedAt)
}

// Ambiguous (many live) → the command errors asking for a pane.
func TestPeek_AmbiguousRefused(t *testing.T) {
	p := &fakePeeker{agents: []core.ScannedAgent{live("a:1"), live("b:1")}}
	_, err := Peek(p, nil).Command.Run(context.Background(), feature.CmdInput{Arg: ""})
	require.Error(t, err)
	require.Empty(t, p.peekedAt, "nothing is peeked when the target is ambiguous")
}

// The TOOL feeds a resolution failure back to the model (not a fatal error).
func TestPeek_ToolAmbiguousFedBack(t *testing.T) {
	p := &fakePeeker{agents: []core.ScannedAgent{live("a:1"), live("b:1")}}
	out, err := Peek(p, nil).Tool.Call(context.Background(), []byte(`{}`))
	require.NoError(t, err, "an ambiguous tool call is guidance, not an error")
	require.Contains(t, out, "name a pane")
}

func TestPeek_EmptyPane(t *testing.T) {
	p := &fakePeeker{tail: "   "}
	out, err := Peek(p, nil).Command.Run(context.Background(), feature.CmdInput{Arg: "w1:p1"})
	require.NoError(t, err)
	require.Contains(t, out, "empty")
}
