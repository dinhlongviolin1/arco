// Package worktree provisions a fresh per-worker checkout (build-guide-rev6
// PASS-3 / T14 spawn prerequisite). It is a CLONE-PER-WORKER: each worker gets
// its own private gitdir (a full clone into dest), which is the invariant
// quarantine.Run assumes — an attacker who merely commits content can't reach a
// shared .git/config, and disabling repo hooks affects only this worker's clone.
//
// After Provision, the caller runs quarantine.Run(dest) to neutralize
// repo-shipped config, then stages the compiled worker config OUTSIDE the
// worktree (B6). Provision does git only; it takes an injected git binary and
// runs with a scrubbed env so no arco/provider credential leaks to git.
package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/spawnenv"
)

// Provision clones repo into dest and checks out base (detached), returning the
// checked-out HEAD commit. dest MUST NOT already exist (a fresh per-worker dir).
// repo is a path or URL; base is a commit-ish (branch/tag/sha). Both are guarded
// against option-injection (a leading '-' is rejected) and passed after `--`.
func Provision(ctx context.Context, gitBin, repo, base, dest string) (head string, err error) {
	if gitBin == "" {
		gitBin = "git"
	}
	if repo == "" || dest == "" {
		return "", fmt.Errorf("worktree: repo and dest are required")
	}
	if strings.HasPrefix(repo, "-") || strings.HasPrefix(base, "-") || strings.HasPrefix(dest, "-") {
		return "", fmt.Errorf("worktree: refusing option-shaped arg (repo/base/dest may not start with '-')")
	}
	if _, statErr := os.Lstat(dest); statErr == nil {
		return "", fmt.Errorf("worktree: dest %q already exists (clone-per-worker needs a fresh dir)", dest)
	}

	// Full clone into the fresh per-worker gitdir. Hardening: `protocol.ext.allow=
	// never` blocks the `ext::<cmd>` remote-helper that would EXECUTE an arbitrary
	// command on clone (the leading-'-' guard alone doesn't catch `ext::…`);
	// GIT_TERMINAL_PROMPT=0 (in run) stops a bad URL hanging on a credential
	// prompt. `--` ends options so a repo path can't be parsed as a flag. No
	// --recurse-submodules (submodule hooks live in a gitdir the superproject's
	// hooksPath doesn't cover); quarantine.Run neutralizes the rest post-clone.
	if out, e := run(ctx, gitBin, "-c", "protocol.ext.allow=never", "clone", "--", repo, dest); e != nil {
		return "", fmt.Errorf("worktree: clone: %v: %s", e, out)
	}
	// Detached checkout at base (if given) so the worker starts from a known point
	// without moving a shared branch ref.
	if base != "" {
		if out, e := run(ctx, gitBin, "-C", dest, "checkout", "--detach", base); e != nil {
			return "", fmt.Errorf("worktree: checkout %q: %v: %s", base, e, out)
		}
	}
	out, e := run(ctx, gitBin, "-C", dest, "rev-parse", "HEAD")
	if e != nil {
		return "", fmt.Errorf("worktree: rev-parse: %v: %s", e, out)
	}
	return strings.TrimSpace(out), nil
}

// Remove deletes a provisioned worktree dir (best-effort cleanup on worker end).
func Remove(dest string) error {
	if dest == "" || dest == "/" {
		return fmt.Errorf("worktree: refusing to remove %q", dest)
	}
	return os.RemoveAll(dest)
}

// run executes a git command with a credential-scrubbed env (P1), capturing
// combined output for the error message.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// Scrubbed env (P1) + no interactive credential prompt (a bad URL must fail,
	// not hang). The dangerous ext:: command-exec transport is blocked per-command
	// via protocol.ext.allow=never; we do NOT set GIT_PROTOCOL_FROM_USER=0 because
	// it would also block the `file` transport legit local-path clones need.
	cmd.Env = append(spawnenv.Scrub(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
