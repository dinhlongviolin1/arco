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

// Compile writes settings.json + a PreToolUse hook into configDir (which MUST be
// outside the worktree). `granted` is the worker's effective capability set;
// `cat` is the capability_catalog. Placement rules:
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
		case row.HighBlast:
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
	return os.WriteFile(filepath.Join(configDir, "hooks", "pretooluse.sh"), []byte(hookScript(worktree)), 0o700)
}

// Flags returns the --allowedTools / --disallowedTools values for the launch
// command (a coarse belt over the settings.json allow/deny).
func Flags(granted map[string]bool, cat []core.CatalogRow) (allowed, disallowed []string) {
	as, ds := map[string]bool{}, map[string]bool{}
	for _, row := range cat {
		for _, p := range toolPatterns[row.Capability] {
			if row.HighBlast {
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
# Deny writes/reads that resolve outside the worktree, and blocked git subcommands.
# NOTE: string matching is defeatable; arco's Allowed() boundary is authoritative.
WORKTREE=` + shellQuote(worktree) + `
input=$(cat)
case "$input" in
  *'"git push"'*'main'* | *'push origin main'* | *'--force'* ) echo '{"permissionDecision":"deny","reason":"blocked git op"}'; exit 0 ;;
  *'/.env'* | *'.ssh/'* | *'sudo '* | *'rm -rf'* ) echo '{"permissionDecision":"deny","reason":"blocked"}'; exit 0 ;;
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
