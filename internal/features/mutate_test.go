package features

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
)

type recordingAdopter struct {
	adopted []string
	failOn  map[string]bool
}

func (r *recordingAdopter) adopt(_ context.Context, ref string) (string, string, error) {
	if r.failOn[ref] {
		return "", "", errors.New("already tracked")
	}
	r.adopted = append(r.adopted, ref)
	return "01WORKER" + ref, "01SESSION" + ref, nil
}

func TestAdopt_HasBrainActTool(t *testing.T) {
	f := Adopt(fakeScanner{}, (&recordingAdopter{}).adopt)
	require.NotNil(t, f.Command)
	require.NotNil(t, f.Tool)
	require.Equal(t, feature.BrainAct, f.Tool.Access)
}

func TestAdopt_ToolExecutes(t *testing.T) {
	ad := &recordingAdopter{}
	out, err := Adopt(fakeScanner{}, ad.adopt).Tool.Call(context.Background(), []byte(`{"ref":"w5:p1"}`))
	require.NoError(t, err)
	require.Equal(t, []string{"w5:p1"}, ad.adopted)
	require.Contains(t, out, "monitor-only")
}

func TestAdopt_AllOnlyLiveUntracked(t *testing.T) {
	scan := fakeScanner{agents: []core.ScannedAgent{
		{AgentObs: core.AgentObs{Ref: "w1:p1", Alive: true}, Tracked: true, WorkerID: "01AAAA"},
		{AgentObs: core.AgentObs{Ref: "w5:p1", Alive: true}},   // live + untracked → adopt
		{AgentObs: core.AgentObs{Ref: "wQ:p1", State: "done"}}, // finished → skip
	}}
	ad := &recordingAdopter{}
	out, err := Adopt(scan, ad.adopt).Command.Run(context.Background(), feature.CmdInput{Arg: ""})
	require.NoError(t, err)
	require.Equal(t, []string{"w5:p1"}, ad.adopted, "only live untracked agents are adopted")
	require.Contains(t, out, "adopted as worker")
}

func TestAdopt_NothingToAdopt(t *testing.T) {
	scan := fakeScanner{agents: []core.ScannedAgent{
		{AgentObs: core.AgentObs{Ref: "w1:p1", Alive: true}, Tracked: true, WorkerID: "01AAAA"},
	}}
	out, err := Adopt(scan, (&recordingAdopter{}).adopt).Command.Run(context.Background(), feature.CmdInput{Arg: "all"})
	require.NoError(t, err)
	require.Contains(t, out, "nothing to adopt")
}

func TestAdopt_ByRef(t *testing.T) {
	ad := &recordingAdopter{}
	out, err := Adopt(fakeScanner{}, ad.adopt).Command.Run(context.Background(), feature.CmdInput{Arg: "w5:p1"})
	require.NoError(t, err)
	require.Equal(t, []string{"w5:p1"}, ad.adopted)
	require.Contains(t, out, "monitor-only")
}

func TestAdopt_AllReportsPerRefFailures(t *testing.T) {
	scan := fakeScanner{agents: []core.ScannedAgent{
		{AgentObs: core.AgentObs{Ref: "ok:1", Alive: true}},
		{AgentObs: core.AgentObs{Ref: "bad:1", Alive: true}},
	}}
	ad := &recordingAdopter{failOn: map[string]bool{"bad:1": true}}
	out, err := Adopt(scan, ad.adopt).Command.Run(context.Background(), feature.CmdInput{Arg: ""})
	require.NoError(t, err, "a per-ref failure is reported, not fatal")
	require.Contains(t, out, "ok:1")
	require.Contains(t, out, "bad:1 — skipped")
	require.Equal(t, []string{"ok:1"}, ad.adopted)
}

func TestDispatch_ToolProposesAndExecutes(t *testing.T) {
	var got struct{ repo, task string }
	f := Dispatch(func(_ context.Context, repo, task string) (string, string, error) {
		got.repo, got.task = repo, task
		return "01WORKERAAA", "01SESSIONBBB", nil
	})
	require.Nil(t, f.Command, "dispatch is tool-only (the operator /dispatch keeps its own path)")
	require.NotNil(t, f.Tool)
	require.Equal(t, feature.BrainAct, f.Tool.Access)

	out, err := f.Tool.Call(context.Background(), []byte(`{"repo":"/srv/app.git","task":"add health endpoint"}`))
	require.NoError(t, err)
	require.Equal(t, "/srv/app.git", got.repo)
	require.Equal(t, "add health endpoint", got.task)
	require.Contains(t, out, "started worker")

	// missing args → guidance, no spawn
	got = struct{ repo, task string }{}
	out2, _ := f.Tool.Call(context.Background(), []byte(`{"repo":"/srv/app.git"}`))
	require.Contains(t, out2, "provide")
	require.Empty(t, got.repo)
}

type fakeKiller struct {
	killed []string
	err    error
}

func (f *fakeKiller) KillWorker(_ context.Context, id string) error {
	f.killed = append(f.killed, id)
	return f.err
}

func TestKill_HasBrainActTool(t *testing.T) {
	f := Kill(&fakeKiller{}, fakeLedger{})
	require.NotNil(t, f.Command)
	require.NotNil(t, f.Tool, "kill is brain-proposable (gated by the confirm/off policy)")
	require.Equal(t, feature.BrainAct, f.Tool.Access, "mutating → BrainAct, not BrainSafe")
}

func TestKill_ToolExecutes(t *testing.T) {
	k := &fakeKiller{}
	l := workerLedger("01ABCWORKERXYZ")
	out, err := Kill(k, l).Tool.Call(context.Background(), []byte(`{"worker":"WORKERXYZ"}`))
	require.NoError(t, err)
	require.Equal(t, []string{"01ABCWORKERXYZ"}, k.killed)
	require.Contains(t, out, "killed worker")
}

func TestKill_ResolvesByFragmentAndTerminates(t *testing.T) {
	k := &fakeKiller{}
	l := workerLedger("01ABCWORKERXYZ")
	out, err := Kill(k, l).Command.Run(context.Background(), feature.CmdInput{Arg: "WORKERXYZ"})
	require.NoError(t, err)
	require.Equal(t, []string{"01ABCWORKERXYZ"}, k.killed, "resolves a worker by any id fragment")
	require.Contains(t, out, "killed worker")
}

func TestKill_AmbiguousKillsNothing(t *testing.T) {
	k := &fakeKiller{}
	l := workerLedger("01AAABBB", "01AAACCC")
	_, err := Kill(k, l).Command.Run(context.Background(), feature.CmdInput{Arg: "01AAA"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "matches 2 workers")
	require.Empty(t, k.killed, "an ambiguous fragment kills nothing")
}

func TestKill_MissingArg(t *testing.T) {
	k := &fakeKiller{}
	_, err := Kill(k, fakeLedger{}).Command.Run(context.Background(), feature.CmdInput{Arg: ""})
	require.Error(t, err)
	require.Empty(t, k.killed)
}

func TestKill_NoMatch(t *testing.T) {
	k := &fakeKiller{}
	_, err := Kill(k, workerLedger("01ABC")).Command.Run(context.Background(), feature.CmdInput{Arg: "ZZZ"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no worker matches")
	require.Empty(t, k.killed)
}

func TestKill_EngineErrorSurfaced(t *testing.T) {
	k := &fakeKiller{err: errors.New("herdr refused")}
	_, err := Kill(k, workerLedger("01ABC")).Command.Run(context.Background(), feature.CmdInput{Arg: "01ABC"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "kill failed")
}

// A killed (terminal) worker is still resolvable by fragment (kill is idempotent
// from the operator's view; resolution matches on id, not state).
func TestKill_ResolvesRegardlessOfState(t *testing.T) {
	k := &fakeKiller{}
	l := fakeLedger{workers: []core.Worker{{ID: "01ABC", State: core.WorkerKilled}}}
	_, err := Kill(k, l).Command.Run(context.Background(), feature.CmdInput{Arg: "01ABC"})
	require.NoError(t, err)
	require.Equal(t, []string{"01ABC"}, k.killed)
}
