// Package quarantine neutralizes host-escalation vectors a checked-out repo can
// ship (build-guide-rev6 B5/B6, precondition 2): repo-committed .claude/,
// .mcp.json, settings.local.json, and git hooks / core.fsmonitor / .gitattributes
// filters that execute on checkout or per tool call. It runs on a worktree AFTER
// checkout and BEFORE the worker's own compiled config is staged, so only
// arco-authored config remains.
//
// Quarantine renames rather than deletes (preserves evidence + reversible) and
// reports what it neutralized so the daemon can record an audit event.
package quarantine

import (
	"os"
	"os/exec"
	"path/filepath"
)

// repoConfigFiles are the repo-shipped config/tool surfaces that must not be
// active in an arco worker.
var repoConfigFiles = []string{
	".claude",                     // repo settings.json / settings.local.json / hooks dir
	".mcp.json",                   // auto-registered MCP tools that escape Allowed()
	".claude/settings.local.json", // higher-precedence than project settings (belt if .claude survived)
}

// Report lists what a quarantine pass neutralized.
type Report struct {
	Renamed   []string // paths renamed to <path>.arco-quarantined
	HooksPath bool     // core.hooksPath was pointed away from repo hooks
	FSMonitor bool     // core.fsmonitor was unset
	GitAttrs  bool     // .gitattributes was neutralized (filter/smudge/clean)
}

// Run quarantines worktree. It is best-effort per item (a missing item is not an
// error) but returns the first hard error encountered.
func Run(worktree string, gitBin string) (Report, error) {
	if gitBin == "" {
		gitBin = "git"
	}
	var rep Report

	for _, rel := range repoConfigFiles {
		p := filepath.Join(worktree, filepath.FromSlash(rel))
		fi, err := os.Lstat(p)
		if err != nil {
			continue // absent → nothing to do
		}
		_ = fi
		dst := p + ".arco-quarantined"
		_ = os.RemoveAll(dst) // clear a stale prior quarantine
		if err := os.Rename(p, dst); err != nil {
			return rep, err
		}
		rep.Renamed = append(rep.Renamed, rel)
	}

	// Disable repo git hooks (checkout/commit hooks are arbitrary code) by
	// pointing hooksPath at an empty dir, and unset the fsmonitor daemon hook.
	if err := gitConfig(worktree, gitBin, "core.hooksPath", os.DevNull); err == nil {
		rep.HooksPath = true
	}
	if unsetGitConfig(worktree, gitBin, "core.fsmonitor") {
		rep.FSMonitor = true
	}

	// Neutralize a repo .gitattributes that could run filter/smudge/clean
	// programs on checkout.
	ga := filepath.Join(worktree, ".gitattributes")
	if _, err := os.Lstat(ga); err == nil {
		dst := ga + ".arco-quarantined"
		_ = os.RemoveAll(dst)
		if err := os.Rename(ga, dst); err == nil {
			rep.GitAttrs = true
		}
	}
	return rep, nil
}

func gitConfig(worktree, gitBin, key, val string) error {
	return exec.Command(gitBin, "-C", worktree, "config", key, val).Run()
}

func unsetGitConfig(worktree, gitBin, key string) bool {
	// --unset-all returns non-zero (5) if the key was absent; treat that as ok.
	err := exec.Command(gitBin, "-C", worktree, "config", "--unset-all", key).Run()
	return err == nil
}
