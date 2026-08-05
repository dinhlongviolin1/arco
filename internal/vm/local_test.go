package vm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

// fakeHerdr installs a `herdr` script on PATH that answers `agent list --json`
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
	log := fakeHerdr(t, `[{"workspace":"arco_a","status":"working"},{"workspace":"arco_b","status":"gone"}]`)
	l := NewLocal("herdr")

	agents, err := l.ListAgents(context.Background())
	require.NoError(t, err)
	require.Len(t, agents, 2)
	require.Equal(t, "arco_a", agents[0].Workspace)
	require.True(t, agents[0].Alive)
	require.False(t, agents[1].Alive, "status=gone → not alive")

	require.NoError(t, l.Prompt(context.Background(), "arco_a", "do X"))
	b, err := os.ReadFile(log)
	require.NoError(t, err)
	require.Contains(t, string(b), "arco_a :: do X")

	require.NoError(t, l.Kill(context.Background(), "arco_a"))
}
