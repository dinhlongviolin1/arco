package features

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

type fakeDiffer struct {
	diff core.Diff
	err  error
	got  string
}

func (f *fakeDiffer) WorkerDiff(_ context.Context, id string) (core.Diff, error) {
	f.got = id
	return f.diff, f.err
}

func workerLedger(ids ...string) fakeLedger {
	var ws []core.Worker
	for _, id := range ids {
		ws = append(ws, core.Worker{ID: id, State: core.WorkerRunning})
	}
	return fakeLedger{workers: ws}
}

func TestDiff_FeatureShape(t *testing.T) {
	f := Diff(&fakeDiffer{}, fakeLedger{})
	require.NotNil(t, f.Command)
	require.NotNil(t, f.Tool)
	require.Equal(t, feature.BrainSafe, f.Tool.Access)
}

func TestDiff_CommandResolvesAndRenders(t *testing.T) {
	d := &fakeDiffer{diff: core.Diff{Patch: "--- a\n+++ b\n+added line"}}
	l := workerLedger("01ABCWORKERXYZ")
	out, err := Diff(d, l).Command.Run(context.Background(), feature.CmdInput{Arg: "WORKERXYZ"})
	require.NoError(t, err)
	require.Equal(t, "01ABCWORKERXYZ", d.got, "resolved the worker by fragment")
	require.Contains(t, out, "diff — ORKERXYZ") // short() = last 8 chars of the id
	require.Contains(t, out, "+added line")
}

func TestDiff_TruncatedMarker(t *testing.T) {
	d := &fakeDiffer{diff: core.Diff{Patch: "big patch", Truncated: true}}
	out, _ := Diff(d, workerLedger("01ABCWORKERXYZ")).Command.Run(context.Background(), feature.CmdInput{Arg: "WORKERXYZ"})
	require.Contains(t, out, "diff truncated by arco")
}

func TestDiff_EmptyPatch(t *testing.T) {
	d := &fakeDiffer{diff: core.Diff{Patch: "   "}}
	out, _ := Diff(d, workerLedger("01ABCWORKERXYZ")).Command.Run(context.Background(), feature.CmdInput{Arg: "WORKERXYZ"})
	require.Contains(t, out, "no diff — base == head")
}

func TestDiff_MissingArg(t *testing.T) {
	_, err := Diff(&fakeDiffer{}, fakeLedger{}).Command.Run(context.Background(), feature.CmdInput{Arg: ""})
	require.Error(t, err)
}

func TestDiff_AmbiguousFragment(t *testing.T) {
	l := workerLedger("01AAABBB", "01AAACCC")
	_, err := Diff(&fakeDiffer{}, l).Command.Run(context.Background(), feature.CmdInput{Arg: "01AAA"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "matches 2 workers")
}

func TestDiff_NoMatch(t *testing.T) {
	_, err := Diff(&fakeDiffer{}, workerLedger("01ABC")).Command.Run(context.Background(), feature.CmdInput{Arg: "ZZZ"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no worker matches")
}

// The TOOL feeds resolution/lookup guidance back to the model instead of erroring.
func TestDiff_ToolResolutionFedBack(t *testing.T) {
	out, err := Diff(&fakeDiffer{}, fakeLedger{}).Tool.Call(context.Background(), []byte(`{}`))
	require.NoError(t, err)
	require.Contains(t, out, "provide a worker id")

	out2, err2 := Diff(&fakeDiffer{}, workerLedger("01ABC")).Tool.Call(context.Background(), []byte(`{"worker":"ZZZ"}`))
	require.NoError(t, err2)
	require.Contains(t, out2, "no worker matches")
}

func TestDiff_ToolReturnsPatch(t *testing.T) {
	d := &fakeDiffer{diff: core.Diff{Patch: "the diff body"}}
	out, err := Diff(d, workerLedger("01ABCWORKERXYZ")).Tool.Call(context.Background(), []byte(`{"worker":"WORKERXYZ"}`))
	require.NoError(t, err)
	require.Contains(t, out, "the diff body")
}

func TestDiff_WorkerDiffError(t *testing.T) {
	d := &fakeDiffer{err: errors.New("git failed")}
	_, err := Diff(d, workerLedger("01ABC")).Command.Run(context.Background(), feature.CmdInput{Arg: "01ABC"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "diff error")
}
