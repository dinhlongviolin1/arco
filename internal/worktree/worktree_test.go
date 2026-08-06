package worktree

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

// srcRepo builds a local source repo with two commits; returns dir, base, head.
func srcRepo(t *testing.T) (dir, base, head string) {
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

func TestProvision_ClonesAndChecksOutBase(t *testing.T) {
	src, base, head := srcRepo(t)
	dest := filepath.Join(t.TempDir(), "wt") // fresh, non-existent

	got, err := Provision(context.Background(), "git", src, base, dest)
	require.NoError(t, err)
	require.Equal(t, base, got, "HEAD is the detached base commit, not the source tip")
	require.NotEqual(t, head, got)

	// it's a real, independent clone: file at base content, own .git
	b, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "one\n", string(b), "checked out at base")
	require.DirExists(t, filepath.Join(dest, ".git"), "private per-worker gitdir (clone-per-worker)")
}

func TestProvision_DefaultsToTipWhenNoBase(t *testing.T) {
	src, _, head := srcRepo(t)
	dest := filepath.Join(t.TempDir(), "wt")
	got, err := Provision(context.Background(), "git", src, "", dest)
	require.NoError(t, err)
	require.Equal(t, head, got, "no base → clone's default HEAD (tip)")
}

func TestProvision_Guards(t *testing.T) {
	// option-shaped args rejected (no injection)
	_, err := Provision(context.Background(), "git", "--upload-pack=evil", "", filepath.Join(t.TempDir(), "wt"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "option-shaped")

	// dest must not pre-exist
	src, base, _ := srcRepo(t)
	existing := t.TempDir()
	_, err = Provision(context.Background(), "git", src, base, existing)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")

	// missing repo/dest
	_, err = Provision(context.Background(), "git", "", "", "")
	require.Error(t, err)
}

// The ext:: remote helper executes an arbitrary command on clone; provisioning
// must refuse it (protocol.ext.allow=never) rather than run it. Uses a sentinel
// file the helper would create if it ran.
func TestProvision_BlocksExtTransport(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "pwned")
	// ext::sh -c '... touch sentinel ...' — if the transport were allowed, git
	// would execute this and create the sentinel.
	repo := "ext::sh -c \"touch " + sentinel + "; false\""
	dest := filepath.Join(t.TempDir(), "wt")
	_, err := Provision(context.Background(), "git", repo, "", dest)
	require.Error(t, err, "ext:: clone must fail (transport blocked)")
	require.NoFileExists(t, sentinel, "the ext helper command must NOT have executed")
}

func TestRemove(t *testing.T) {
	d := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, os.MkdirAll(d, 0o700))
	require.NoError(t, Remove(d))
	require.NoDirExists(t, d)
	require.Error(t, Remove("/"), "refuses dangerous paths")
}
