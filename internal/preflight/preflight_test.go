package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func base(euid int) Probe {
	return Probe{
		Euid: euid, GitPath: "/usr/bin/git",
		StateDir: "/x", StateDirMode: 0o700, StateDirOK: true,
		SocketDir: "/run/arco", SocketDirMode: 0o700, SocketDirOK: true,
	}
}

func TestEvaluate_AllPassForSafePosture(t *testing.T) {
	r := Evaluate(base(1000))
	require.True(t, r.OK(), "safe posture passes: %v", r.Failures())
}

func TestEvaluate_CriticalFailures(t *testing.T) {
	// root
	require.False(t, Evaluate(base(0)).OK())
	// no git
	p := base(1000)
	p.GitPath = ""
	require.False(t, Evaluate(p).OK())
	// TCP intake without a secret
	p = base(1000)
	p.TCPAddr = "0.0.0.0:9000"
	require.False(t, Evaluate(p).OK())
	// TCP intake WITH a secret is fine
	p.IntakeSecret = "0123456789abcdef01"
	require.True(t, Evaluate(p).OK())
}

// A group/world-accessible state dir is a WARNING, not a startup blocker: OK()
// stays true (arco won't refuse or chmod a dir it may not own) but it surfaces
// in Failures() so the operator can tighten it.
func TestEvaluate_WideStateDirIsWarningNotFatal(t *testing.T) {
	p := base(1000)
	p.StateDirMode = 0o755
	r := Evaluate(p)
	require.True(t, r.OK(), "wide state dir must NOT block startup")
	require.NotEmpty(t, r.Failures(), "but it is surfaced as a warning")
	found := false
	for _, f := range r.Failures() {
		if strings.Contains(f, "state_dir_private") && strings.Contains(f, "warn") {
			found = true
		}
	}
	require.True(t, found, "state_dir_private appears as a warn")
}

// Gather over a dir created the way the daemon creates its state dir (MkdirAll
// 0700) must report a private, existing dir — i.e. the daemon's own preflight
// passes under a normal (non-root) uid.
func TestGather_DaemonStyleStateDirIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	p := Gather(dir, dir, "", "")
	require.True(t, p.StateDirOK)
	require.Equal(t, os.FileMode(0o700), p.StateDirMode.Perm(),
		"MkdirAll(0700) state dir mode (if this isn't 0700, umask widened it)")
}
