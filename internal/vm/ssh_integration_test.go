package vm

// CROSS-HOST integration tests for the ssh command layer (the "distributed"
// slice). Skipped unless ARCO_TEST_SSH_HOST is set, e.g.:
//
//	ARCO_TEST_SSH_HOST=vm1 go test ./internal/vm/ -run Integration -v
//
// They exercise the REAL transport — a real ssh client, a real remote login
// shell, real argv passing — against a self-installed fake herdr on the remote
// host, and real remote git. No hostnames/credentials are hardcoded: the test
// target is operator-supplied, and all scaffolding lives in a /tmp dir that is
// removed on cleanup. The remote host is only ever asked to run the fake herdr,
// git, and its own cleanup.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Remote preconditions for these tests (all fail LOUDLY, none silently pass): a
// POSIX-compatible remote login shell + /bin/sh + coreutils, remote `git` (test 3),
// a non-noexec /tmp, and key-based auth (BatchMode). NOTE: sshOpts uses
// StrictHostKeyChecking=accept-new — the FIRST connection to a new host accepts
// its key TOFU; pin known hosts out-of-band for anything you care about.

// xhost is the operator-supplied remote test host ("" → skip all cross-host tests).
func xhost(t *testing.T) string {
	t.Helper()
	h := os.Getenv("ARCO_TEST_SSH_HOST")
	if h == "" {
		t.Skip("set ARCO_TEST_SSH_HOST to a reachable ssh host to run cross-host tests")
	}
	return h
}

// xhostClient builds a remote VMClient whose herdr is a fake we install in dir.
func xhostClient(t *testing.T, dir string) *LocalVMClient {
	t.Helper()
	c, err := NewRemote(xhost(t), dir+"/herdr")
	require.NoError(t, err)
	return c
}

// xhostSetup installs a fake herdr on the remote host under dir and registers a
// cleanup that removes the whole dir (via the same transport it tests). The fake
// implements `agent list` (fixed envelope), `agent prompt` (dumps its argv,
// NUL-separated, to dir/prompt-dump.bin), and exits 0 otherwise.
func xhostSetup(t *testing.T, dir string) *LocalVMClient {
	t.Helper()
	ctx := context.Background()
	c := xhostClient(t, dir)
	runRemote := func(name string, args ...string) string {
		out, err := c.cmd(ctx, name, args...).Output()
		require.NoError(t, err, "remote setup: %s %v", name, args)
		return string(out)
	}
	runRemote("mkdir", "-p", dir)
	// Register cleanup IMMEDIATELY after the dir exists, so a failure anywhere in
	// the remaining setup (stub install, chmod) still removes the remote dir.
	// Bounded + logged: a stalled remote must not hang the test binary, and a
	// leaked dir must leave a signal.
	t.Cleanup(func() {
		cc, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.cmd(cc, "rm", "-rf", dir).Output(); err != nil {
			t.Logf("cross-host cleanup of %s failed: %v", dir, err)
		}
	})
	stub := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"'agent list')\n" +
		"  printf '%s' '{\"result\":{\"agents\":[{\"agent\":\"claude\",\"agent_status\":\"working\",\"pane_id\":\"xh:p1\",\"workspace_id\":\"xh\",\"terminal_id\":\"term_XH\"}]}}'\n" +
		"  ;;\n" +
		"'agent prompt')\n" +
		"  printf '%s\\0' \"$@\" > " + shellQuote(dir+"/prompt-dump.bin") + "\n" +
		"  ;;\n" +
		"esac\nexit 0\n"
	writeCmd := c.cmd(ctx, "sh", "-c", "cat > "+shellQuote(dir+"/herdr"))
	writeCmd.Stdin = strings.NewReader(stub)
	require.NoError(t, writeCmd.Run(), "install fake herdr on remote")
	runRemote("chmod", "+x", dir+"/herdr")
	return c
}

// xhostDir returns a per-test remote scratch dir. Uniqueness must survive two
// concurrent `go test` runs (possibly on different machines) pointed at one shared
// host: same pid is possible, so a random suffix guards against one run's cleanup
// rm -rf'ing the other's dir mid-test.
func xhostDir(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("/tmp/arco-xhost-%d-%x-%s", os.Getpid(), rand.Uint32(),
		strings.ReplaceAll(t.Name(), "/", "-"))
}

// TestIntegration_RemoteListAgents runs the full herdr-chain parsing over a real
// ssh connection to a real second host: ListAgents → remote fake herdr envelope →
// parsed observations.
func TestIntegration_RemoteListAgents(t *testing.T) {
	c := xhostSetup(t, xhostDir(t))
	obs, err := c.ListAgents(context.Background())
	require.NoError(t, err)
	require.Len(t, obs, 1)
	require.Equal(t, "xh:p1", obs[0].Ref)
	require.Equal(t, "term_XH", obs[0].BootID)
	require.True(t, obs[0].Alive, "status 'working' is alive")
}

// TestIntegration_RemotePromptInjectionCrossHost is the cross-host injection
// proof: hostile task texts travel over a REAL ssh connection, are parsed by the
// REAL remote login shell, and the remote fake herdr must receive the intended
// argv BYTE-EXACT (read back from the remote dump file through the same
// transport). Any breakout or expansion anywhere in the chain fails this.
func TestIntegration_RemotePromptInjectionCrossHost(t *testing.T) {
	dir := xhostDir(t)
	c := xhostSetup(t, dir)
	ctx := context.Background()
	for _, evil := range []string{
		"$(rm -rf /)", "`rm -rf /`", "it's", "a;b|c&d", "a\nb",
		"--help", "-oProxyCommand=touch /tmp/pwned", "*", "$HOME", "日本'語",
	} {
		require.NoError(t, c.Prompt(ctx, "xh:p1", evil), "prompt with %q", evil)
		out, err := c.cmd(ctx, "cat", dir+"/prompt-dump.bin").Output()
		require.NoError(t, err, "read back remote argv dump for %q", evil)
		want := strings.Join([]string{"agent", "prompt", "xh:p1", evil}, "\x00") + "\x00"
		require.Equal(t, want, string(out),
			"cross-host: remote herdr must receive the intended argv for %q", evil)
	}
}

// TestIntegration_RemoteGitHeads exercises real remote git through the transport:
// create a repo on the remote host, commit, and read its HEAD via GitHeads.
func TestIntegration_RemoteGitHeads(t *testing.T) {
	dir := xhostDir(t)
	c := xhostSetup(t, dir)
	ctx := context.Background()
	repo := dir + "/repo"
	out, err := c.cmd(ctx, "sh", "-c",
		"mkdir -p "+shellQuote(repo)+" && cd "+shellQuote(repo)+
			" && git init -q && echo hi > f && git add f"+
			" && git -c user.email=arco@test -c user.name=arco commit -qm init"+
			" && git rev-parse HEAD").Output()
	require.NoError(t, err, "create remote test repo")
	sha := strings.TrimSpace(string(out))
	if len(sha) != 40 && len(sha) != 64 { // SHA-1 or SHA-256 repos
		t.Fatalf("remote rev-parse returned %q, not a commit sha", sha)
	}

	heads, err := c.GitHeads(ctx, []string{repo})
	require.NoError(t, err)
	require.Equal(t, sha, heads[repo], "GitHeads over ssh must read the remote repo's HEAD")
}
