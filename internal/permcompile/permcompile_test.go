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

	// capstone audit MED-7: fs.worktree grants an unscoped Read, so the hook must
	// contain READS too — an out-of-worktree Read is denied; an in-worktree Read is
	// allowed. And the settings matcher must include Read so the agent invokes it.
	require.Contains(t, run(`{"tool":"Read","file_path":"/etc/passwd"}`), `"deny"`)
	require.NotContains(t, run(`{"tool":"Read","file_path":"`+wt+`/a.go"}`), `"deny"`)
	b, _ := os.ReadFile(filepath.Join(cfg, "settings.json"))
	require.Contains(t, string(b), "Read", "PreToolUse matcher gates Read")
	require.Regexp(t, `Bash\|Edit\|Write\|Read`, string(b), "matcher includes Read")
}

// Regression (opus re-review HIGH-1): a `..` traversal that textually starts
// with $WORKTREE but escapes it must be denied (path normalized before check).
func TestCompile_HookBlocksDotDotTraversal(t *testing.T) {
	cfg, wt := t.TempDir(), t.TempDir()
	require.NoError(t, Compile(cfg, wt, map[string]bool{"fs.worktree": true}, catalog()))
	hook := filepath.Join(cfg, "hooks", "pretooluse.sh")
	run := func(input string) string {
		cmd := exec.Command("sh", hook)
		cmd.Stdin = strings.NewReader(input)
		out, _ := cmd.Output()
		return string(out)
	}
	require.Contains(t, run(`{"tool":"Write","file_path":"`+wt+`/../../etc/passwd"}`), `"deny"`,
		"a .. escape that shares the worktree prefix must still be denied")
}

// Regression (opus re-review HIGH-2): a decoy "file_path" planted in a later
// field (content) must not shadow the real, out-of-worktree file_path.
func TestCompile_HookIgnoresDecoyFilePath(t *testing.T) {
	cfg, wt := t.TempDir(), t.TempDir()
	require.NoError(t, Compile(cfg, wt, map[string]bool{"fs.worktree": true}, catalog()))
	hook := filepath.Join(cfg, "hooks", "pretooluse.sh")
	run := func(input string) string {
		cmd := exec.Command("sh", hook)
		cmd.Stdin = strings.NewReader(input)
		out, _ := cmd.Output()
		return string(out)
	}
	// real file_path first (out-of-worktree), decoy in-worktree path in content → deny
	in := `{"file_path":"/etc/passwd","content":"\"file_path\": \"` + wt + `/ok\""}`
	require.Contains(t, run(in), `"deny"`, "the FIRST file_path is authoritative, not a later decoy")
}

// Regression (opus re-review MED-3): colon/plus refspec pushes to a protected
// branch are also denied by the best-effort git-push globs.
func TestCompile_HookBlocksRefspecPush(t *testing.T) {
	cfg, wt := t.TempDir(), t.TempDir()
	require.NoError(t, Compile(cfg, wt, map[string]bool{"fs.worktree": true}, catalog()))
	hook := filepath.Join(cfg, "hooks", "pretooluse.sh")
	run := func(input string) string {
		cmd := exec.Command("sh", hook)
		cmd.Stdin = strings.NewReader(input)
		out, _ := cmd.Output()
		return string(out)
	}
	require.Contains(t, run(`{"tool":"Bash","command":"git push origin HEAD:main"}`), `"deny"`)
	require.Contains(t, run(`{"tool":"Bash","command":"git push origin +master"}`), `"deny"`)
	// Full-ref refspec form — the bypass the space/colon/plus globs missed (review
	// HIGH-1): `git push origin refs/heads/main` has no " main"/":main"/"+main".
	require.Contains(t, run(`{"tool":"Bash","command":"git push origin refs/heads/main"}`), `"deny"`)
	require.Contains(t, run(`{"tool":"Bash","command":"git push origin HEAD:refs/heads/master"}`), `"deny"`)
	// A legitimately-named feature branch must NOT be over-blocked.
	require.NotContains(t, run(`{"tool":"Bash","command":"git push origin feature/login"}`), `"deny"`)
}

// ManagedSettings is the deny-only, non-overridable layer (P3): it must contain
// ONLY denies (never allow/ask) and cover high-blast + static dangerous shapes.
func TestManagedSettings_DenyOnly(t *testing.T) {
	// full high-blast danger set (as the daemon passes the whole catalog)
	cat := []core.CatalogRow{
		{Capability: "external.deploy", ActionClass: core.ClassDanger, Tier: core.TierHighBlast, HighBlast: true},
		{Capability: "secrets.read", ActionClass: core.ClassDanger, Tier: core.TierHighBlast, HighBlast: true},
		{Capability: "fs.destructive", ActionClass: core.ClassDanger, Tier: core.TierHighBlast, HighBlast: true},
	}
	b, err := ManagedSettings(cat)
	require.NoError(t, err)
	var m struct {
		Permissions struct {
			Allow, Ask, Deny []string
		} `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal(b, &m))
	require.Empty(t, m.Permissions.Allow, "managed layer must never grant")
	require.Empty(t, m.Permissions.Ask, "managed layer must never ask")
	require.NotEmpty(t, m.Permissions.Deny)
	// high-blast dangerous SHAPES present (these toolPatterns are now non-empty —
	// the earlier assertions were vacuous because the map had no entries: opus F2)
	require.NotEmpty(t, toolPatterns["external.deploy"])
	require.Subset(t, m.Permissions.Deny, toolPatterns["external.deploy"])
	require.Subset(t, m.Permissions.Deny, toolPatterns["secrets.read"])
	require.Subset(t, m.Permissions.Deny, toolPatterns["fs.destructive"])
	require.Contains(t, m.Permissions.Deny, "Bash(kubectl:*)")
	require.Contains(t, m.Permissions.Deny, "Bash(rm -rf:*)")
}

// Guard (opus F1/F2): the worker-invokable high-blast danger caps must each have
// a non-empty deny mapping, so a new one can't be added without shapes and
// silently leave the managed layer vacuous for it. (fleet.*/external.spend are
// arco-orchestration ops with no worker tool shape — intentionally excluded.)
func TestToolPatterns_HighBlastDangerCovered(t *testing.T) {
	for _, cap := range []string{"git.push.main", "external.deploy", "external.send", "secrets.read", "fs.destructive"} {
		require.NotEmpty(t, toolPatterns[cap], "high-blast danger cap %s must have deny shapes", cap)
	}
}

// Guard (opus F3): no tool pattern may contain a comma — LaunchArgs comma-joins
// the allow/disallow lists, so a comma would split one pattern into two flags.
func TestToolPatterns_NoCommaInPatterns(t *testing.T) {
	for cap, pats := range toolPatterns {
		for _, p := range pats {
			require.NotContains(t, p, ",", "pattern for %s contains a comma", cap)
		}
	}
	for _, p := range staticDeny {
		require.NotContains(t, p, ",")
	}
}

// Compile emits the managed-settings.json artifact alongside settings.json.
func TestCompile_WritesManagedSettings(t *testing.T) {
	cfg, wt := t.TempDir(), t.TempDir()
	require.NoError(t, Compile(cfg, wt, map[string]bool{"git.commit": true}, catalog()))
	b, err := os.ReadFile(filepath.Join(cfg, "managed-settings.json"))
	require.NoError(t, err)
	require.Contains(t, string(b), "deny")
	require.NotContains(t, string(b), `"allow"`, "managed artifact is deny-only")
}

// LaunchArgs pins the spawn contract (P6): settings outside the worktree, a
// non-bypass permission mode, high-blast disallowed, granted routine allowed.
func TestLaunchArgs_PinnedContract(t *testing.T) {
	cfg := t.TempDir()
	// a high-blast cap that HAS tool patterns, so --disallowedTools is populated
	cat := []core.CatalogRow{
		{Capability: "git.commit", ActionClass: core.ClassRoutine, Tier: core.TierLow, DefaultAllowed: true, CompiledWorker: true},
		{Capability: "git.pr.merge", ActionClass: core.ClassDanger, Tier: core.TierHighBlast, HighBlast: true},
	}
	args := LaunchArgs(cfg, map[string]bool{"git.commit": true}, cat)
	joined := strings.Join(args, " ")
	require.Contains(t, joined, "--settings "+filepath.Join(cfg, "settings.json"))
	require.Contains(t, joined, "--permission-mode default")
	require.NotContains(t, joined, "bypassPermissions", "mode must not bypass the deny-rules/hook")
	require.Contains(t, joined, "--allowedTools")
	require.Contains(t, joined, "Bash(git commit:*)", "granted routine cap is allowed")
	require.Contains(t, joined, "--disallowedTools")
	require.Contains(t, joined, "Bash(gh pr merge:*)", "high-blast cap is disallowed")
}

func TestFlags_HighBlastDisallowedGrantedAllowed(t *testing.T) {
	allowed, disallowed := Flags(map[string]bool{"git.commit": true}, catalog())
	require.Contains(t, allowed, "Bash(git commit:*)")
	require.Subset(t, disallowed, toolPatterns["git.push.main"]) // high-blast always disallowed
	// an ungranted medium cap is in neither list (Claude default-denies it)
	require.NotContains(t, allowed, "Bash(gh pr merge:*)")
	require.NotContains(t, disallowed, "Bash(gh pr merge:*)")
}
