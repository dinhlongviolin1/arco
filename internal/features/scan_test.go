package features

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

type fakeScanner struct {
	agents []core.ScannedAgent
	err    error
}

func (f fakeScanner) ScanAgents(context.Context) ([]core.ScannedAgent, error) {
	return f.agents, f.err
}

func agent(vm, ref, kind, state, title, cwd, sid string, alive, tracked bool, wid string) core.ScannedAgent {
	return core.ScannedAgent{
		AgentObs: core.AgentObs{Ref: ref, Kind: kind, State: state, Title: title, Cwd: cwd, SessionID: sid, Alive: alive},
		VM:       vm, Tracked: tracked, WorkerID: wid,
	}
}

func TestScan_FeatureShape(t *testing.T) {
	f := Scan(fakeScanner{})
	require.Equal(t, "scan", f.Name)
	require.NotNil(t, f.Command, "exposes an operator command")
	require.NotNil(t, f.Tool, "exposes a brain tool")
	require.Equal(t, feature.BrainSafe, f.Tool.Access, "scan is read-only")
	require.Equal(t, "scan", f.Command.Name)
	require.Equal(t, "scan", f.Tool.Name)
}

func TestScan_RendersLiveAndDone(t *testing.T) {
	fs := fakeScanner{agents: []core.ScannedAgent{
		agent("", "w1:p1", "claude", "working", "review arco", "/home/op/arco", "3dca0eaf", true, true, "01AAAAAAAAAAAAAAAAAAAAAAAA"),
		agent("", "w5:p1", "claude", "idle", "pull latest", "/home/op/sysadmin", "0d64f4be", true, false, ""),
		agent("", "wQ:p1", "claude", "done", "", "/home/op/sysadmin", "", false, false, ""),
	}}
	f := Scan(fs)

	// Both surfaces render identically from the one implementation.
	cmdOut, err := f.Command.Run(context.Background(), feature.CmdInput{})
	require.NoError(t, err)
	toolOut, err := f.Tool.Call(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, cmdOut, toolOut, "command and tool share one implementation")

	require.Contains(t, cmdOut, "herdr agent sessions (3 — 2 live, 1 done)")
	require.Contains(t, cmdOut, "w5:p1")
	require.Contains(t, cmdOut, "✅ tracked")
	require.Contains(t, cmdOut, "🆓 untracked")
	require.Contains(t, cmdOut, "🏁 finished (pane lingering)")
	require.Contains(t, cmdOut, "cwd: /home/op/sysadmin")
	require.Contains(t, cmdOut, "session 3dca0eaf")
	require.Contains(t, cmdOut, "/adopt")
}

func TestScan_Empty(t *testing.T) {
	out, err := Scan(fakeScanner{}).Command.Run(context.Background(), feature.CmdInput{})
	require.NoError(t, err)
	require.Equal(t, "no herdr agent sessions found on the fleet", out)
}

func TestScan_NoAdoptHintWhenAllTracked(t *testing.T) {
	fs := fakeScanner{agents: []core.ScannedAgent{
		agent("", "w1:p1", "claude", "working", "", "", "", true, true, "01AAAAAAAAAAAAAAAAAAAAAAAA"),
	}}
	out, _ := Scan(fs).Command.Run(context.Background(), feature.CmdInput{})
	require.NotContains(t, out, "/adopt", "no adopt hint when nothing is adoptable")
}

func TestScan_LongTitleTruncated(t *testing.T) {
	long := "this is a very long terminal title that definitely exceeds the sixty byte display cap for a bullet"
	fs := fakeScanner{agents: []core.ScannedAgent{
		agent("", "w1:p1", "claude", "working", long, "", "", true, false, ""),
	}}
	out, _ := Scan(fs).Command.Run(context.Background(), feature.CmdInput{})
	require.Contains(t, out, "…", "an over-long title is elided")
	require.NotContains(t, out, long, "the full over-long title is not shown verbatim")
}

func TestScan_ErrorPropagates(t *testing.T) {
	boom := errors.New("herdr unreachable")
	_, cerr := Scan(fakeScanner{err: boom}).Command.Run(context.Background(), feature.CmdInput{})
	require.ErrorIs(t, cerr, boom, "command surfaces the scan error")
	_, terr := Scan(fakeScanner{err: boom}).Tool.Call(context.Background(), nil)
	require.ErrorIs(t, terr, boom, "tool surfaces the scan error")
}
