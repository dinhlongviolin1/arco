package permcompile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// catalog mirrors the seeded capability_catalog rows this test needs.
func catalog() []core.CatalogRow {
	return []core.CatalogRow{
		{Capability: "git.commit", ActionClass: core.ClassRoutine, Tier: core.TierLow, DefaultAllowed: true, CompiledWorker: true},
		{Capability: "exec.tests", ActionClass: core.ClassRoutine, Tier: core.TierLow, DefaultAllowed: true, CompiledWorker: true},
		{Capability: "net.fetch", ActionClass: core.ClassAmbiguous, Tier: core.TierMedium},
		{Capability: "git.pr.merge", ActionClass: core.ClassAmbiguous, Tier: core.TierMedium},
		{Capability: "git.push.main", ActionClass: core.ClassDanger, Tier: core.TierHighBlast, HighBlast: true},
		{Capability: "external.deploy", ActionClass: core.ClassDanger, Tier: core.TierHighBlast, HighBlast: true},
	}
}

func readSettings(t *testing.T, dir string) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	require.NoError(t, err)
	var s struct {
		Permissions struct {
			Allow, Ask, Deny []string
		} `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal(b, &s))
	return map[string][]string{"allow": s.Permissions.Allow, "ask": s.Permissions.Ask, "deny": s.Permissions.Deny}
}

func TestCompile_StructuralInvariants(t *testing.T) {
	cfg := t.TempDir()
	wt := t.TempDir()
	// grant a routine cap + a medium cap; leave the high-blast ones ungranted.
	granted := map[string]bool{"git.commit": true, "exec.tests": true, "git.pr.merge": true}
	require.NoError(t, Compile(cfg, wt, granted, catalog()))

	p := readSettings(t, cfg)
	// granted routine → allow
	require.Contains(t, p["allow"], "Bash(git commit:*)")
	require.Contains(t, p["allow"], "Bash(go test:*)")
	// granted medium → ask (NOT allow)
	require.Contains(t, p["ask"], "Bash(gh pr merge:*)")
	require.NotContains(t, p["allow"], "Bash(gh pr merge:*)")

	// high-blast NEVER in allow OR ask, regardless of anything
	for _, hb := range []string{"git.push.main", "external.deploy"} {
		for _, pat := range toolPatterns[hb] {
			require.NotContains(t, p["allow"], pat, "high-blast %s must never be allowed", hb)
			require.NotContains(t, p["ask"], pat)
		}
	}
	// ungranted net.fetch is not allowed (default-off)
	require.NotContains(t, p["allow"], "WebFetch")

	// static dangerous denies present
	require.Contains(t, p["deny"], "Bash(rm -rf:*)")
	require.Contains(t, p["deny"], "Read(./.env)")
}

func TestCompile_WritesExecutableHookOutsideWorktree(t *testing.T) {
	cfg := t.TempDir()
	wt := t.TempDir()
	require.NoError(t, Compile(cfg, wt, map[string]bool{"git.commit": true}, catalog()))

	hook := filepath.Join(cfg, "hooks", "pretooluse.sh")
	fi, err := os.Stat(hook)
	require.NoError(t, err)
	require.NotZero(t, fi.Mode()&0o100, "hook must be executable")
	// config lives outside the worktree (B6) — cfg and wt are distinct temp dirs
	require.NotEqual(t, cfg, wt)
	b, _ := os.ReadFile(hook)
	require.Contains(t, string(b), "permissionDecision", "hook emits a deny decision")
}

// Regression (opus P1#2): a row with Tier=high_blast but HighBlast=false, even if
// granted, must NOT reach allow — the gate trusts tier OR flag, not one bool.
func TestCompile_MislabeledHighBlastTierExcluded(t *testing.T) {
	cfg, wt := t.TempDir(), t.TempDir()
	cat := []core.CatalogRow{{Capability: "git.pr.merge", ActionClass: core.ClassDanger, Tier: core.TierHighBlast, HighBlast: false}}
	require.NoError(t, Compile(cfg, wt, map[string]bool{"git.pr.merge": true}, cat))
	p := readSettings(t, cfg)
	require.NotContains(t, p["allow"], "Bash(gh pr merge:*)")
	require.NotContains(t, p["ask"], "Bash(gh pr merge:*)")
	require.Contains(t, p["deny"], "Bash(gh pr merge:*)")
}

// Regression (opus P1#1): the PreToolUse hook actually blocks an out-of-worktree
// write and allows an in-worktree one (run the generated script, as opus did).
func TestCompile_HookBlocksOutOfWorktreeWrite(t *testing.T) {
	cfg, wt := t.TempDir(), t.TempDir()
	require.NoError(t, Compile(cfg, wt, map[string]bool{"fs.worktree": true}, catalog()))
	hook := filepath.Join(cfg, "hooks", "pretooluse.sh")

	run := func(input string) string {
		cmd := exec.Command("sh", hook)
		cmd.Stdin = strings.NewReader(input)
		out, _ := cmd.Output()
		return string(out)
	}
	// write outside the worktree → denied
	require.Contains(t, run(`{"tool":"Write","file_path":"/etc/passwd"}`), `"deny"`)
	// write inside the worktree → allowed (no deny emitted)
	require.NotContains(t, run(`{"tool":"Write","file_path":"`+wt+`/a.go"}`), `"deny"`)
	// push to master → denied (parity with settings.json staticDeny)
	require.Contains(t, run(`{"tool":"Bash","command":"git push origin master"}`), `"deny"`)
}

func TestFlags_HighBlastDisallowedGrantedAllowed(t *testing.T) {
	allowed, disallowed := Flags(map[string]bool{"git.commit": true}, catalog())
	require.Contains(t, allowed, "Bash(git commit:*)")
	require.Subset(t, disallowed, toolPatterns["git.push.main"]) // high-blast always disallowed
	// an ungranted medium cap is in neither list (Claude default-denies it)
	require.NotContains(t, allowed, "Bash(gh pr merge:*)")
	require.NotContains(t, disallowed, "Bash(gh pr merge:*)")
}
