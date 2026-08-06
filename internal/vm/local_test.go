package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

// realGitRepo builds a temp repo with two commits; returns dir, base, head.
func realGitRepo(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	base = strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0o644))
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "head")
	head = strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
	return
}

func TestLocal_GitHeadsAndDiff(t *testing.T) {
	dir, base, head := realGitRepo(t)
	l := NewLocal("herdr")

	heads, err := l.GitHeads(context.Background(), []string{dir, filepath.Join(dir, "nope")})
	require.NoError(t, err)
	require.Equal(t, head, heads[dir])
	require.NotContains(t, heads, filepath.Join(dir, "nope"), "a bad worktree is omitted, not fatal")

	d, err := l.Diff(context.Background(), dir, base, head)
	require.NoError(t, err)
	require.Equal(t, 1, d.Files)
	require.Equal(t, 1, d.Insertions)
	require.Contains(t, d.Patch, "two")

	// empty base/head → empty diff, no git call
	d0, err := l.Diff(context.Background(), dir, "", "")
	require.NoError(t, err)
	require.Equal(t, 0, d0.Files)
}

func TestLocal_DiffRejectsNonCommitRev(t *testing.T) {
	dir, _, head := realGitRepo(t)
	l := NewLocal("herdr")
	// a base that starts with '-' must never reach git as an option
	_, err := l.Diff(context.Background(), dir, "--output=/tmp/pwn", head)
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-commit-shaped")
}

func TestLocal_GitHeadsCtxCancelReturnsError(t *testing.T) {
	dir, _, _ := realGitRepo(t)
	l := NewLocal("herdr")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := l.GitHeads(ctx, []string{dir})
	require.Error(t, err, "a cancelled sweep must not look like 0 heads")
}

// fakeHerdr installs a `herdr` script on PATH that answers `agent list`
// and logs `agent prompt`, so we exercise the real exec path without herdr.
func fakeHerdr(t *testing.T, listJSON string) (promptLog string) {
	t.Helper()
	bin := t.TempDir()
	promptLog = filepath.Join(bin, "prompt.log")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = agent ] && [ \"$2\" = list ]; then printf '%s' '" + listJSON + "'; exit 0; fi\n" +
		"if [ \"$1\" = agent ] && [ \"$2\" = prompt ]; then echo \"$3 :: $4\" >> " + promptLog + "; exit 0; fi\n" +
		"if [ \"$1\" = agent ] && [ \"$2\" = send-keys ]; then exit 0; fi\n" +
		"exit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "herdr"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return promptLog
}

func TestLocal_ListAgentsAndPrompt(t *testing.T) {
	// REAL herdr 0.7.5 `agent list` envelope (Task-S spike): wrapped in
	// result.agents, fields agent_status/workspace_id/terminal_id, states
	// idle|working|blocked|done|unknown.
	realList := `{"id":"cli:agent:list","result":{"type":"agent_list","agents":[` +
		`{"agent":"claude","agent_status":"working","pane_id":"wB:p1","workspace_id":"wB","terminal_id":"term_aaa"},` +
		`{"agent":"claude","agent_status":"done","pane_id":"wC:p1","workspace_id":"wC","terminal_id":"term_bbb"}` +
		`]}}`
	log := fakeHerdr(t, realList)
	l := NewLocal("herdr")

	agents, err := l.ListAgents(context.Background())
	require.NoError(t, err)
	require.Len(t, agents, 2)
	require.Equal(t, "wB", agents[0].Workspace, "workspace_id maps to Workspace")
	require.Equal(t, "term_aaa", agents[0].BootID, "terminal_id carries the identity/PID-reuse guard")
	require.True(t, agents[0].Alive, "working → alive")
	require.False(t, agents[1].Alive, "agent_status=done → not alive")

	require.NoError(t, l.Prompt(context.Background(), "wB:p1", "do X"))
	b, err := os.ReadFile(log)
	require.NoError(t, err)
	require.Contains(t, string(b), "wB:p1 :: do X")

	require.NoError(t, l.Kill(context.Background(), "wB:p1"))
}

func TestFake_Launch(t *testing.T) {
	f := NewFake()
	ref, err := f.Launch(context.Background(), core.LaunchSpec{Name: "arco_x", Kind: "claude", Args: []string{"--settings", "/cfg"}})
	require.NoError(t, err)
	require.Equal(t, "pane:arco_x", ref)
	// the launched spec is recorded, and the new agent shows alive by ref
	require.Len(t, f.Launched(), 1)
	require.Equal(t, "claude", f.Launched()[0].Kind)
	agents, _ := f.ListAgents(context.Background())
	require.Len(t, agents, 1)
	require.Equal(t, ref, agents[0].Ref)
	require.True(t, agents[0].Alive)
}
