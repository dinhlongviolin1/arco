package permcompile

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestFlags_HighBlastDisallowedGrantedAllowed(t *testing.T) {
	allowed, disallowed := Flags(map[string]bool{"git.commit": true}, catalog())
	require.Contains(t, allowed, "Bash(git commit:*)")
	require.Subset(t, disallowed, toolPatterns["git.push.main"]) // high-blast always disallowed
	// an ungranted medium cap is in neither list (Claude default-denies it)
	require.NotContains(t, allowed, "Bash(gh pr merge:*)")
	require.NotContains(t, disallowed, "Bash(gh pr merge:*)")
}
