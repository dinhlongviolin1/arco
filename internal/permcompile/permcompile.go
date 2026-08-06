// Package permcompile compiles a worker's effective capability tree into its
// Claude Code config (build-guide-rev6 Task 23): a settings.json with
// permissions.{allow,ask,deny}, a PreToolUse hook, and --allowedTools/
// --disallowedTools flags. This is LAYER 1 (prevention) — best-effort worker
// self-limiting; arco's own Allowed() boundary is the authoritative layer for
// arco-executed actions, and high-blast capabilities are NEVER compiled onto a
// worker at all.
//
// The config is staged OUTSIDE the worker-writable worktree (build-guide B6) and
// pointed at via --settings, so the worker can't rewrite its own hook.
//
// SPIKE NOTE: the capability→tool-pattern strings below are a reasonable mapping;
// the exact tokens Claude Code honors must be confirmed against a real version in
// the permcompile spike. The STRUCTURAL guarantees this package enforces
// (high-blast never in allow; granted→allow; medium→ask; out-of-worktree denied)
// are version-independent.
package permcompile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// toolPatterns maps a capability to the Claude tool-permission strings it grants.
var toolPatterns = map[string][]string{
	"git.branch.create": {"Bash(git branch:*)", "Bash(git checkout:*)", "Bash(git switch:*)"},
	"git.commit":        {"Bash(git add:*)", "Bash(git commit:*)"},
	"git.push.feature":  {"Bash(git push:*)"},
	"git.pr.open":       {"Bash(gh pr create:*)"},
	"git.pr.update":     {"Bash(gh pr edit:*)", "Bash(gh pr comment:*)"},
	"fs.worktree":       {"Edit", "Write", "Read"},
	"exec.tests":        {"Bash(go test:*)", "Bash(npm test:*)", "Bash(pytest:*)", "Bash(make test:*)"},
	"exec.build":        {"Bash(go build:*)", "Bash(npm run build:*)", "Bash(make:*)"},
	"exec.lint":         {"Bash(go vet:*)", "Bash(golangci-lint:*)", "Bash(npm run lint:*)"},
	"exec.install":      {"Bash(go mod:*)", "Bash(npm install:*)", "Bash(pip install:*)"},
	"net.fetch":         {"WebFetch", "WebSearch"},
	"git.pr.merge":      {"Bash(gh pr merge:*)"},
	"git.push.shared":   {"Bash(git push:* --force:*)"},
}

// staticDeny is always denied regardless of the tree — the highest-blast tool
// shapes that must never be reachable from a worker.
var staticDeny = []string{
	"Bash(git push:* origin main:*)", "Bash(git push:* origin master:*)",
	"Bash(rm -rf:*)", "Bash(sudo:*)", "Bash(curl:* | sh:*)",
	"Read(./.env)", "Read(~/.ssh/**)", "Read(~/.config/**)",
}

// managedDenyList is the always-deny set: the static dangerous shapes plus every
// high-blast capability's tool patterns. This is the NON-ADVISORY deny layer —
// it belongs in managed settings (highest precedence) so a repo
// settings.local.json or --dangerously-skip-permissions cannot override it.
func managedDenyList(cat []core.CatalogRow) []string {
	deny := map[string]bool{}
	for _, s := range staticDeny {
		deny[s] = true
	}
	for _, row := range cat {
		if row.HighBlast || row.Tier == core.TierHighBlast {
			for _, p := range toolPatterns[row.Capability] {
				deny[p] = true
			}
		}
	}
	return keys(deny)
}

// ManagedSettings returns a DENY-ONLY managed-settings.json (security precond
// P3): the highest-precedence Claude Code policy a worker cannot override. It
// carries ONLY denies (never allow/ask) so it can only ever REMOVE authority —
// even if a worker ships a permissive repo settings.local.json or is launched
// with --dangerously-skip-permissions, these denies still bind. The operator
// deploys it to Claude's managed-settings path (root-owned); arco produces the
// content (arco is non-root, so it can't write that path itself).
func ManagedSettings(cat []core.CatalogRow) ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"permissions": map[string]any{"deny": managedDenyList(cat)},
	}, "", "  ")
}

// LaunchArgs returns the pinned worker-launch flags (security precond P6: pinned
// spawn contract). --settings points at the daemon-owned config OUTSIDE the
// worktree; the permission mode is pinned to "default" (NOT bypassPermissions)
// so the deny-rules + PreToolUse hook survive and a headless -p run aborts on an
// unlisted `ask` tool instead of silently proceeding. SPIKE NOTE: the exact flag
// tokens are confirmed against a real Claude Code version in the permcompile
// spike; the STRUCTURAL contract (settings-outside-worktree, high-blast
// disallowed, mode pinned non-bypass) is version-independent.
func LaunchArgs(configDir string, granted map[string]bool, cat []core.CatalogRow) []string {
	allowed, disallowed := Flags(granted, cat)
	args := []string{
		"--settings", filepath.Join(configDir, "settings.json"),
		"--permission-mode", "default",
	}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	if len(disallowed) > 0 {
		args = append(args, "--disallowedTools", strings.Join(disallowed, ","))
	}
	return args
}

// Compile writes settings.json + managed-settings.json + a PreToolUse hook into
// configDir (which MUST be outside the worktree). `granted` is the worker's
// effective capability set; `cat` MUST be the FULL capability_catalog (NOT
// DefaultTree(), which omits the high-blast rows) so the high-blast deny sees
// them. Placement rules:
//   - high_blast → never in allow (belt: also denied); routine/low granted →
//     allow; medium granted → ask; ungranted non-high-blast → neither (implicit
//     deny by Claude's default-deny).
func Compile(configDir, worktree string, granted map[string]bool, cat []core.CatalogRow) error {
	allow, ask := map[string]bool{}, map[string]bool{}
	deny := map[string]bool{}
	for _, s := range staticDeny {
		deny[s] = true
	}
	for _, row := range cat {
		pats := toolPatterns[row.Capability]
		switch {
		case row.HighBlast || row.Tier == core.TierHighBlast: // trust EITHER, not one bool (opus P1)
			// never on a worker — deny any pattern we know for it (defense-in-depth)
			for _, p := range pats {
				deny[p] = true
			}
		case !granted[row.Capability]:
			// not granted → not allowed (Claude default-denies unlisted tools)
		case row.Tier == core.TierMedium:
			for _, p := range pats {
				ask[p] = true
			}
		default: // granted low/routine
			for _, p := range pats {
				allow[p] = true
			}
		}
	}

	settings := map[string]any{
		"permissions": map[string]any{
			"allow": keys(allow),
			"ask":   keys(ask),
			"deny":  keys(deny),
		},
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "Bash|Edit|Write",
				"hooks":   []any{map[string]any{"type": "command", "command": filepath.Join(configDir, "hooks", "pretooluse.sh")}},
			}},
		},
	}
	if err := os.MkdirAll(filepath.Join(configDir, "hooks"), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), b, 0o600); err != nil {
		return err
	}
	// Deny-only managed-settings artifact (P3): the operator deploys it to Claude's
	// root-owned managed path so its denies can't be overridden by the worker.
	managed, err := ManagedSettings(cat)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(configDir, "managed-settings.json"), managed, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(configDir, "hooks", "pretooluse.sh"), []byte(hookScript(worktree)), 0o700)
}

// Flags returns the --allowedTools / --disallowedTools values for the launch
// command (a coarse belt over the settings.json allow/deny).
func Flags(granted map[string]bool, cat []core.CatalogRow) (allowed, disallowed []string) {
	as, ds := map[string]bool{}, map[string]bool{}
	for _, row := range cat {
		for _, p := range toolPatterns[row.Capability] {
			if row.HighBlast || row.Tier == core.TierHighBlast {
				ds[p] = true
			} else if granted[row.Capability] && row.Tier != core.TierMedium {
				as[p] = true
			}
		}
	}
	return keys(as), keys(ds)
}

// hookScript denies out-of-worktree file writes and blocked git subcommands.
// Command-string matching is best-effort — arco's boundary is the real gate.
func hookScript(worktree string) string {
	return `#!/bin/sh
# arco PreToolUse hook (layer 1, best-effort). Reads the tool-use JSON on stdin.
# NOTE: string matching is defeatable; arco's Allowed() boundary is authoritative.
WORKTREE=` + shellQuote(worktree) + `
deny() { printf '{"permissionDecision":"deny","reason":"%s"}\n' "$1"; exit 0; }
input=$(cat)

# Extract the FIRST file_path only (grep -o is left-to-right, head -1), so a decoy
# "file_path" planted in a later field (e.g. content) can't shadow the real one.
fp=$(printf '%s' "$input" | grep -o '"file_path"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n1 | sed 's/.*"\([^"]*\)"$/\1/')
if [ -n "$fp" ]; then
  # Normalize '..' and symlinks BEFORE the containment check — a textual prefix
  # match alone is defeated by "$WORKTREE/../../etc/passwd". readlink -m resolves
  # without requiring the path to exist; relative paths resolve against $PWD.
  real=$(readlink -m "$fp")
  wt=$(readlink -m "$WORKTREE")
  case "$real" in
    "$wt"/* | "$wt" ) ;;                    # inside the worktree — ok
    * ) deny "write outside worktree" ;;    # anywhere else (incl. .. escapes) — deny
  esac
fi

# Blocked git ops + secret/danger command shapes. Push-to-protected matching is
# best-effort (high_blast caps are never compiled + arco's Allowed() is the gate);
# cover space- and colon/plus-refspec forms of main/master.
case "$input" in
  *'push'*' main'* | *'push'*' master'* | *'push'*':main'* | *'push'*':master'* | *'push'*'+main'* | *'push'*'+master'* | *'--force'* | *'push'*' -f'* ) deny "blocked git push" ;;
  *'.env'* | *'.ssh/'* | *'sudo '* | *'rm -rf'* ) deny "blocked" ;;
esac
exit 0
`
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func keys(m map[string]bool) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
