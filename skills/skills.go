// Package skills embeds arco's built-in agent skill bundles (SKILL.md packages)
// and installs them into a worker's worktree so a Claude-kind agent discovers
// them under .claude/skills/. Embedding means the installed `arco` binary
// carries the skills — no repo checkout needed at spawn time.
package skills

import (
	"embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed arco-image/SKILL.md
var assets embed.FS

// Available is the set of built-in skill names that can be injected.
var Available = []string{"arco-image"}

// Install writes the named embedded skills into <worktree>/.claude/skills/<name>/
// SKILL.md and adds `.claude/` to the worktree's git exclude, so the injected
// config is discoverable by Claude but invisible to the agent's `git add`.
// gitBin is used only to resolve the exclude path (handles both a real .git dir
// and a `git worktree` pointer file); a git failure there is non-fatal.
func Install(gitBin, worktree string, names []string) error {
	for _, name := range names {
		data, err := assets.ReadFile(name + "/SKILL.md") // embed.FS always uses forward slashes
		if err != nil {
			return err
		}
		dst := filepath.Join(worktree, ".claude", "skills", name)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), data, 0o644); err != nil {
			return err
		}
	}
	excludeClaudeFromGit(gitBin, worktree)
	return nil
}

// excludeClaudeFromGit appends `.claude/` to the worktree's info/exclude (the
// local, uncommitted ignore) so arco's injected `.claude/` never lands in a
// worker's `git add -A`. Best-effort: git layout quirks must not fail a spawn.
func excludeClaudeFromGit(gitBin, worktree string) {
	if gitBin == "" {
		gitBin = "git"
	}
	out, err := exec.Command(gitBin, "-C", worktree, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return
	}
	excl := strings.TrimSpace(string(out))
	if excl == "" {
		return
	}
	if !filepath.IsAbs(excl) {
		excl = filepath.Join(worktree, excl)
	}
	// idempotent: skip if already present
	if b, err := os.ReadFile(excl); err == nil && strings.Contains(string(b), ".claude/") {
		return
	}
	if err := os.MkdirAll(filepath.Dir(excl), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n# arco-injected agent config\n.claude/\n")
}
