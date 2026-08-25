package reconcile

// Dynamic agent kind: the spawn launch seam is configurable (herdr supervises
// any kind it knows — its list/prompt/wait/kill API is kind-agnostic), but the
// compiled permission args are claude-shaped and must never leak onto another
// kind's argv.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSpawn_DefaultKindClaudeWithCompiledArgs(t *testing.T) {
	e, _, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.AgentArgs = []string{"--extra-flag"}
	repo, _ := localRepo(t)

	_, err := e.Spawn(context.Background(), "", "task", true, repo, "", "")
	require.NoError(t, err)

	launched := fake.Launched()
	require.Len(t, launched, 1)
	require.Equal(t, "claude", launched[0].Kind, "unset AgentKind stays claude")
	require.NotEmpty(t, launched[0].Args, "claude launches carry the compiled permission args")
	require.Equal(t, "--extra-flag", launched[0].Args[len(launched[0].Args)-1],
		"operator AgentArgs append after the compiled args")
}

func TestSpawn_NonClaudeKindLaunchesWithAgentArgsOnly(t *testing.T) {
	e, _, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.AgentKind = "codex"
	e.AgentArgs = []string{"--profile", "work"}
	repo, _ := localRepo(t)

	_, err := e.Spawn(context.Background(), "", "task", true, repo, "", "")
	require.NoError(t, err)

	launched := fake.Launched()
	require.Len(t, launched, 1)
	require.Equal(t, "codex", launched[0].Kind)
	require.Equal(t, []string{"--profile", "work"}, launched[0].Args,
		"a non-claude kind must not inherit claude's compiled permission argv")
}

func TestSpawn_KindRecordedOnRowMatchesLaunch(t *testing.T) {
	e, s, fake := newEngine(t)
	e.ConfigDir = t.TempDir()
	e.AgentKind = "codex"
	repo, _ := localRepo(t)

	res, err := e.Spawn(context.Background(), "", "task", true, repo, "", "")
	require.NoError(t, err)

	w, err := s.Reader().GetWorker(res.WorkerID)
	require.NoError(t, err)
	require.Equal(t, "codex", w.AgentKind, "the persisted row must record the ACTUAL launched kind, not hardcoded claude")
	require.Equal(t, "codex", fake.Launched()[0].Kind)
}
