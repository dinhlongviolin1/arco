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

// shCommand builds `sh -c script` inheriting the test process env.
func shCommand(script string) *exec.Cmd { return exec.Command("sh", "-c", script) }

// evilTokens is the shell-injection corpus: every class of metacharacter a hostile
// worker task/label/path could carry. If shellQuote survives all of these through
// a REAL shell, the remote command layer is injection-safe.
var evilTokens = []string{
	"plain",
	"hello world",          // space
	"it's",                 // single quote
	`a"b`,                  // double quote
	"a;b", "a|b", "a&b",   // separators
	"a&&b||c",              // lists
	"$(rm -rf /)",          // command substitution
	"`rm -rf /`",           // backtick substitution
	"a\nb",                 // newline
	"a\tb",                 // tab
	"*", "?", "[abc]", "~", // glob + tilde
	"$HOME", "${PATH}",     // variable expansion
	`\`, `\\`,              // backslashes
	"-rf", "--evil",        // flag-shaped
	"",                     // empty
	"'", "'''", `'\''`,     // quote gymnastics
	"a'b'c\"d\"e",          // mixed quotes
	"\x01\x02",             // control chars
	"$'x'",                 // bash ANSI-C quoting (literal under POSIX sh)
	"a>b", "a<b",           // redirection
	"  x  ",                // leading/trailing spaces
	"日本語",                // unicode
	"a!(b)",                // history-expansion char (non-interactive sh: literal)
}

// runThroughShell executes `printf '%s\0' <tokens...>` through a real /bin/sh with
// the given pre-quoted argument string and returns the output. The NUL separator
// makes the round-trip byte-exact for ANY token content.
func runThroughShell(t *testing.T, argStr string) []byte {
	t.Helper()
	out, err := shOutput(t, "printf '%s\\0' "+argStr)
	require.NoError(t, err)
	return out
}

// shOutput runs `sh -c script` with PATH limited to dir (plus a system floor so sh
// itself resolves) and returns stdout.
func shOutput(t *testing.T, script string, envExtra ...string) ([]byte, error) {
	t.Helper()
	cmd := shCommand(script)
	cmd.Env = append(os.Environ(), envExtra...)
	return cmd.Output()
}

// TestShellQuote_RoundTripViaRealShell proves shellQuote is injection-safe the only
// way that matters: a REAL POSIX shell parses the quoted form back into the exact
// original bytes — no breakout, no expansion, no loss.
func TestShellQuote_RoundTripViaRealShell(t *testing.T) {
	for _, tok := range evilTokens {
		out := runThroughShell(t, shellQuote(tok))
		require.Equal(t, tok+"\x00", string(out), "token %q must round-trip exactly", tok)
	}
}

// TestShellJoin_RoundTrip proves multi-token commands (the actual remote-command
// shape: program + args) survive a real shell as discrete, intact tokens.
func TestShellJoin_RoundTrip(t *testing.T) {
	cmd := append([]string{"herdr", "agent", "prompt"}, evilTokens...)
	out := runThroughShell(t, shellJoin(cmd))
	require.Equal(t, strings.Join(cmd, "\x00")+"\x00", string(out))
}

// TestSSHRunner_CommandShape pins the ssh argv layout: pinned non-interactive opts,
// a `--` end-of-options separator, then host, then the remote command as ONE argv
// element (ssh concatenates argv[1:] with spaces — our single-token construction is
// what makes quoting sound).
func TestSSHRunner_CommandShape(t *testing.T) {
	r := sshRunner{host: "vm1.example", opts: sshOpts}
	cmd := r.command(context.Background(), "herdr", "agent", "list")
	want := append([]string{"ssh"}, sshOpts...)
	want = append(want, "--", "vm1.example", shellJoin([]string{"herdr", "agent", "list"}))
	require.Equal(t, want, cmd.Args)
	require.Equal(t, shellJoin([]string{"herdr", "agent", "list"}), cmd.Args[len(cmd.Args)-1],
		"the remote command must be ONE argv element")
}

// TestNewRemote_RejectsFlagHosts guards the ssh OPTION-injection hole (CVE-2023-51385
// class): a host beginning '-' would be parsed by ssh as an option — e.g.
// `-oProxyCommand=...` executes a command locally on OpenSSH < 9.6. Fail fast,
// independent of the fleet's ssh patch level.
func TestNewRemote_RejectsFlagHosts(t *testing.T) {
	for _, host := range []string{"-oProxyCommand=touch /tmp/pwned", "-J", "-F/etc/passwd", "-", ""} {
		_, err := NewRemote(host, "herdr")
		require.Error(t, err, "host %q must be rejected", host)
	}
	ok, err := NewRemote("vm1.example", "")
	require.NoError(t, err)
	require.Equal(t, "herdr", ok.Herdr, "herdr defaults")
}

// TestRemote_ListAgentsOverFakeSSH proves the FULL herdr-chain logic (parse +
// liveness mapping) runs unchanged over the ssh transport: a fake `ssh` on PATH
// stands in for the remote host and serves the agent-list envelope.
func TestRemote_ListAgentsOverFakeSSH(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s' '{\"result\":{\"agents\":[{\"agent\":\"claude\",\"agent_status\":\"idle\",\"pane_id\":\"wR:p1\",\"workspace_id\":\"wR\",\"terminal_id\":\"term_R\"}]}}'\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	client, cerr := NewRemote("vm1.example", "herdr")
	require.NoError(t, cerr)
	obs, err := client.ListAgents(context.Background())
	require.NoError(t, err)
	require.Len(t, obs, 1)
	require.Equal(t, "wR:p1", obs[0].Ref)
	require.Equal(t, "term_R", obs[0].BootID)
	require.True(t, obs[0].Alive)
}

// TestRemote_PromptInjectionContained is the end-to-end injection proof for the
// highest-risk input (a worker TASK, delivered via Prompt): take the EXACT remote
// command string ssh would execute, run it through a REAL shell with a stub herdr
// on PATH, and require the stub to receive the intended argv byte-exact. If the
// quoting were breakable, the malicious text would alter the argv (or run a second
// command) and this would fail.
func TestRemote_PromptInjectionContained(t *testing.T) {
	for _, evil := range evilTokens {
		dir := t.TempDir()
		// stub herdr dumps its argv NUL-separated, exactly as delivered by the shell
		stub := "#!/bin/sh\nprintf '%s\\0' \"$@\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "herdr"), []byte(stub), 0o755))

		r := sshRunner{host: "vm1.example", opts: sshOpts}
		remoteCmd := r.command(context.Background(), "herdr", "agent", "prompt", "wX:p1", evil).Args
		got, err := shOutput(t, remoteCmd[len(remoteCmd)-1], "PATH="+dir)
		require.NoError(t, err, "evil token %q: the remote command must execute", evil)
		require.Equal(t, strings.Join([]string{"agent", "prompt", "wX:p1", evil}, "\x00")+"\x00", string(got),
			"evil token %q: herdr must receive the intended argv, unaltered", evil)
	}
}

// TestNewRemote_ScrubsSSHClientEnv proves the ssh CLIENT process itself is scrubbed
// of arco's own provider creds (P1 holds across the tunnel's local half).
func TestNewRemote_ScrubsSSHClientEnv(t *testing.T) {
	r := sshRunner{host: "h", opts: sshOpts}
	cmd := r.command(context.Background(), "true")
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "CLAVIS_") {
			t.Fatalf("ssh client env must be scrubbed, found %q", kv)
		}
	}
}
