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
	// Reject file:// URLs: they force the git transport → source-repo `upload-pack`
	// hooks (uploadpack.packObjectsHook) execute at clone time (opus F3). A plain
	// local PATH uses git's copy optimization (no upload-pack) and is fine; remote
	// https/ssh sources are assumed operator-trusted.
	if strings.HasPrefix(strings.ToLower(repo), "file://") {
		return "", fmt.Errorf("worktree: refusing file:// source (use a plain path or a remote URL)")
	}
	if _, statErr := os.Lstat(dest); statErr == nil {
		return "", fmt.Errorf("worktree: dest %q already exists (clone-per-worker needs a fresh dir)", dest)
	}

	// Full clone into the fresh per-worker gitdir. Hardening (opus review):
	//   - Protocol ALLOWLIST (not an ext-only denylist): deny everything, allow
	//     only file (local paths) / https / ssh / git — closes ext::, fd::, and
	//     any future/third-party remote-helper in one rule.
	//   - `--` ends options so a repo path can't be parsed as a flag.
	//   - No --recurse-submodules (submodule hooks live in a gitdir the
	//     superproject's hooksPath doesn't cover).
	// GIT_CONFIG_GLOBAL/SYSTEM=/dev/null + GIT_TERMINAL_PROMPT=0 are set in run().
	proto := []string{
		"-c", "protocol.allow=never",
		"-c", "protocol.file.allow=always", "-c", "protocol.https.allow=always",
		"-c", "protocol.ssh.allow=always", "-c", "protocol.git.allow=always",
	}
	if out, e := run(ctx, gitBin, append(append([]string{}, proto...), "clone", "--", repo, dest)...); e != nil {
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
	// not hang) + NEUTRALIZE host global/system git config (opus F1): env-scrub
	// preserves HOME, so a host-global filter driver (e.g. git-lfs
	// filter.lfs.process) would otherwise run on an attacker-committed
	// .gitattributes/.lfsconfig DURING clone/checkout — before quarantine and
	// before any sandbox = RCE in the daemon context. A fresh clone has no
	// attacker .git/config, so nulling global+system config means no driver a
	// committed .gitattributes references is defined → checkout-time filter exec
	// is closed for ALL drivers at once.
	cmd.Env = append(spawnenv.Scrub(os.Environ()),
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_SYSTEM="+os.DevNull)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
