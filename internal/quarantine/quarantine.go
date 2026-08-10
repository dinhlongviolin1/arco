// Package quarantine neutralizes host-escalation vectors a checked-out repo can
// ship (build-guide-rev6 B5/B6, precondition 2): repo-committed .claude/,
// .mcp.json, settings.local.json, .gitmodules, and .gitattributes. It runs on a
// worktree AFTER checkout and BEFORE the worker's own compiled config is staged.
//
// THREAT MODEL / SCOPING (opus review): git reads config only from $GIT_DIR/config,
// never from the worktree tree, so an attacker who merely COMMITS content cannot
// set core.pager/alias/filter.*/diff.* etc. — those are inert and intentionally
// not touched. This package assumes arco owns a FRESH gitdir (clone-per-worker);
// ADOPTING a pre-existing on-disk repo is OUT OF SCOPE (then .git/config is
// attacker-controlled and the caller must audit/re-clone instead). The
// attacker-committed surfaces we neutralize: .claude/, .mcp.json, .gitattributes
// (all dirs), .gitmodules. .gitattributes only executes with a configured driver
// (which the attacker can't set) — kept as defense-in-depth against arco's own /
// global config defining one.
//
// Quarantine renames aside (reversible, evidence-preserving) and reports what it
// neutralized. Failing to disable repo hooks is a HARD error (hooks armed is the
// worst outcome), unlike a missing optional file.
package quarantine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dinhlongviolin1/arco/internal/spawnenv"
)

// repoConfigFiles are the repo-shipped config/tool surfaces at the worktree root.
var repoConfigFiles = []string{
	".claude",    // repo settings.json / settings.local.json / hooks dir
	".mcp.json",  // auto-registered MCP tools that escape Allowed()
	".lfsconfig", // git-lfs: lfs.url exfil + lfs.customtransfer.<id>.path = arbitrary binary
}

// Report lists what a quarantine pass neutralized.
type Report struct {
	Renamed    []string // paths renamed to <path>.arco-quarantined
	HooksPath  bool     // core.hooksPath pointed at /dev/null (repo hooks off)
	FSMonitor  bool     // core.fsmonitor unset
	Submodules bool     // protocol.file/submodule.active locked down
	GitAttrs   int      // .gitattributes files neutralized (any depth)
	GitModules int      // .gitmodules files neutralized (any depth)
}

// Run quarantines worktree. Disabling repo hooks is a hard error; other items are
// best-effort but a real (non-not-exist) filesystem error is surfaced.
func Run(worktree string, gitBin string) (Report, error) {
	if gitBin == "" {
		gitBin = "git"
	}
	var rep Report

	for _, rel := range repoConfigFiles {
		p := filepath.Join(worktree, rel)
		if _, err := os.Lstat(p); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return rep, fmt.Errorf("quarantine: stat %s: %w", rel, err) // don't silently skip a real error
		}
		if err := renameAside(p); err != nil {
			return rep, err
		}
		rep.Renamed = append(rep.Renamed, rel)
	}

	// Disable repo git hooks — the single most important control. A failure here
	// means hooks may be ARMED, so it is fatal (was silently swallowed: opus P1).
	// NB: on a linked worktree `git config` writes the SHARED common config; the
	// caller must ensure worktree owns its gitdir (clone-per-worker), else scope
	// with `git config --worktree` (opus P2).
	if err := gitConfig(worktree, gitBin, "core.hooksPath", os.DevNull); err != nil {
		return rep, fmt.Errorf("quarantine: could not disable repo hooks (they may be armed): %w", err)
	}
	rep.HooksPath = true

	rep.FSMonitor = disableFSMonitor(worktree, gitBin)

	// Submodule hook-injection defense: even in a fresh clone a malicious
	// .gitmodules + protocol.file reopens the local-submodule vector, and the
	// superproject's hooksPath does NOT propagate to submodule gitdirs.
	okA := gitConfig(worktree, gitBin, "protocol.file.allow", "never") == nil
	okB := gitConfig(worktree, gitBin, "submodule.active", "false") == nil
	rep.Submodules = okA && okB

	// Recursively rename every .gitattributes / .gitmodules (git reads them at
	// every directory level), skipping the .git dir. A real walk error is fatal.
	if err := filepath.WalkDir(worktree, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		switch d.Name() {
		case ".gitattributes":
			if e := renameAside(p); e != nil {
				return e
			}
			rep.GitAttrs++
		case ".gitmodules":
			if e := renameAside(p); e != nil {
				return e
			}
			rep.GitModules++
		}
		return nil
	}); err != nil {
		return rep, fmt.Errorf("quarantine: walk: %w", err)
	}

	// Keep the .arco-quarantined artifacts from being staged/committed by a later
	// `git add -A` (which would propagate hostile content + destroy evidence).
	addExclude(worktree, gitBin)
	return rep, nil
}

// qgitTimeout bounds each quarantine git op — these are fast local $GIT_DIR
// config/rev-parse calls on a fresh clone, but a wedged git must not stall the
// spawn path indefinitely.
const qgitTimeout = 15 * time.Second

// qgit builds a `git -C <worktree> <args…>` command with arco's own credentials
// SCRUBBED from the environment (precondition P1 — no arco/provider secret ever
// reaches a git subprocess operating on a just-checked-out attacker tree) and a
// bounded context. Matches vm.newCmd / worktree.run; the caller runs it.
func qgit(worktree, gitBin string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), qgitTimeout)
	cmd := exec.CommandContext(ctx, gitBin, append([]string{"-C", worktree}, args...)...)
	cmd.Env = spawnenv.Scrub(os.Environ())
	return cmd, cancel
}

// disableFSMonitor unsets a repo-local core.fsmonitor, then — if the value is
// still EFFECTIVE (set in host global/system config, which --unset-all can't
// reach) — shadows it to false at repo scope so the daemon hook can't run.
func disableFSMonitor(worktree, gitBin string) bool {
	c1, cancel1 := qgit(worktree, gitBin, "config", "--unset-all", "core.fsmonitor")
	_ = c1.Run()
	cancel1()
	c2, cancel2 := qgit(worktree, gitBin, "config", "--get", "core.fsmonitor")
	out, _ := c2.Output()
	cancel2()
	if strings.TrimSpace(string(out)) == "" {
		return true
	}
	c3, cancel3 := qgit(worktree, gitBin, "config", "core.fsmonitor", "false")
	defer cancel3()
	return c3.Run() == nil
}

// addExclude appends the quarantine glob to the repo's info/exclude (best-effort).
func addExclude(worktree, gitBin string) {
	cmd, cancel := qgit(worktree, gitBin, "rev-parse", "--git-path", "info/exclude")
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return
	}
	p := strings.TrimSpace(string(out))
	if !filepath.IsAbs(p) {
		p = filepath.Join(worktree, p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n*.arco-quarantined\n")
}

// renameAside moves p to p.arco-quarantined (clearing any stale prior copy).
func renameAside(p string) error {
	dst := p + ".arco-quarantined"
	_ = os.RemoveAll(dst)
	return os.Rename(p, dst)
}

func gitConfig(worktree, gitBin, key, val string) error {
	cmd, cancel := qgit(worktree, gitBin, "config", key, val)
	defer cancel()
	return cmd.Run()
}
