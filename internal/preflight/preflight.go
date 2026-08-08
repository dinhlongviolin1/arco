// Package preflight is arco's boot-time enforcement of the local security
// preconditions (build-guide-rev6 PASS-3): before accepting traffic the daemon
// verifies the invariants it CAN check locally and refuses to start when a
// CRITICAL one fails, rather than running in an unsafe posture. The operator
// still owns the parts arco can't verify (OS-user account setup, server-side
// branch protection); this closes arco's half — "don't run if the box is unsafe".
//
// Check is a PURE function over an injected Probe so the decision logic is fully
// testable; Run gathers the real Probe from the OS + config and calls it.
package preflight

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
)

// minIntakeSecretLen is the shortest HMAC intake secret we accept for network
// intake — the sole guard on the network event-injection surface, so a trivial
// (brute-forceable) secret is rejected.
const minIntakeSecretLen = 16

// Probe is the observed system/config state the checks decide over (injected so
// Check is deterministic and testable).
type Probe struct {
	Euid          int         // effective uid; 0 = root (euid, so setuid-root is caught)
	GitPath       string      // resolved path to git, "" if not found
	StateDir      string      // arco state dir (holds the ledger + worker configs)
	StateDirMode  fs.FileMode // permission bits of StateDir (0 if it doesn't exist)
	StateDirOK    bool        // whether StateDir was stat-able
	SocketDir     string      // dir holding the unix control socket (local-auth trust root)
	SocketDirMode fs.FileMode // permission bits of SocketDir
	SocketDirOK   bool        // whether SocketDir was stat-able
	TCPAddr       string      // configured network intake address ("" = socket only)
	IntakeSecret  string      // configured HMAC intake secret

	SandboxEnabled bool   // whether [sandbox] enabled is set (opt-in srt wrapping)
	SrtPath        string // resolved path to the srt sandbox runtime, "" if not found
}

// Gather collects the real Probe from the OS + the given config values.
func Gather(stateDir, socketDir, tcpAddr, intakeSecret string, sandboxEnabled bool) Probe {
	p := Probe{
		Euid: os.Geteuid(), StateDir: stateDir, SocketDir: socketDir,
		TCPAddr: tcpAddr, IntakeSecret: intakeSecret, SandboxEnabled: sandboxEnabled,
	}
	if path, err := exec.LookPath("git"); err == nil {
		p.GitPath = path
	}
	// Resolved unconditionally (exactly like git): the check below decides whether
	// its absence matters, so the report always shows what the box actually has.
	if path, err := exec.LookPath("srt"); err == nil {
		p.SrtPath = path
	}
	if fi, err := os.Stat(stateDir); err == nil {
		p.StateDirMode, p.StateDirOK = fi.Mode(), true
	}
	if fi, err := os.Stat(socketDir); err == nil {
		p.SocketDirMode, p.SocketDirOK = fi.Mode(), true
	}
	return p
}

// Check is one precondition result. Critical failures block startup.
type Check struct {
	Name     string
	Critical bool
	Pass     bool
	Detail   string
}

// Report is the full preflight outcome.
type Report struct{ Checks []Check }

// OK reports whether every CRITICAL check passed (non-critical failures are
// warnings the daemon may log but not block on).
func (r Report) OK() bool {
	for _, c := range r.Checks {
		if c.Critical && !c.Pass {
			return false
		}
	}
	return true
}

// Failures returns the human-readable "name: detail" of every failed check.
func (r Report) Failures() []string {
	var out []string
	for _, c := range r.Checks {
		if !c.Pass {
			sev := "warn"
			if c.Critical {
				sev = "CRITICAL"
			}
			out = append(out, fmt.Sprintf("[%s] %s: %s", sev, c.Name, c.Detail))
		}
	}
	return out
}

// Evaluate runs the precondition checks over p (pure).
func Evaluate(p Probe) Report {
	var r Report

	// not_root (CRITICAL): a supervisor running as root means a worker/subprocess
	// escape defeats every filesystem sandbox — refuse. NB: checks euid only; it
	// does NOT catch gid 0 or elevated capabilities (CAP_DAC_OVERRIDE/…) — those
	// stay the operator's responsibility (OS-user separation).
	r.Checks = append(r.Checks, Check{
		Name: "not_root", Critical: true, Pass: p.Euid != 0,
		Detail: "arco must not run with effective uid 0 — a worker escape would be unconfined",
	})

	// git_present (CRITICAL): quarantine + git hardening + diff-gate all shell out
	// to git; without it those controls silently no-op.
	r.Checks = append(r.Checks, Check{
		Name: "git_present", Critical: true, Pass: p.GitPath != "",
		Detail: "git binary not found on PATH — quarantine / git-hardening / diff-gate require it",
	})

	// {state,socket}_dir_private (WARNING, not fatal): the ledger + compiled worker
	// configs live in the state dir; the unix control socket's dir is the trust
	// root for the local (unauthenticated) mutating routes. 0700 is recommended
	// for both, but NEITHER is critical: arco must not refuse to run — nor chmod —
	// a dir it may not own (the operator can point db_path/socket at a shared
	// dir), and its authoritative controls don't depend on these bits. Surfaced so
	// the operator tightens them.
	r.Checks = append(r.Checks, dirPrivate("state_dir_private", p.StateDir, p.StateDirMode, p.StateDirOK))
	r.Checks = append(r.Checks, dirPrivate("socket_dir_private", p.SocketDir, p.SocketDirMode, p.SocketDirOK))

	// tcp_intake_signed (CRITICAL): a network-exposed intake without a
	// sufficiently-long shared secret is an unauthenticated (or brute-forceable)
	// event-injection surface (P4).
	signed := p.TCPAddr == "" || len(p.IntakeSecret) >= minIntakeSecretLen
	r.Checks = append(r.Checks, Check{
		Name: "tcp_intake_signed", Critical: true, Pass: signed,
		Detail: fmt.Sprintf("tcp_addr is set but intake_secret is missing or shorter than %d bytes — network intake must be HMAC-signed with a strong secret", minIntakeSecretLen),
	})

	// sandbox_srt_present (CRITICAL, only when opted in): the sandbox is off by
	// default, so an operator who never enables it owes nothing. But once
	// [sandbox] enabled is set, arco has PROMISED confinement — booting without
	// the srt binary would run every worker unsandboxed while the config says
	// otherwise, which is strictly worse than refusing to start. Disabled ⇒ passes
	// regardless of srt (zero install burden for the default posture).
	r.Checks = append(r.Checks, Check{
		Name: "sandbox_srt_present", Critical: true, Pass: !p.SandboxEnabled || p.SrtPath != "",
		Detail: "sandbox.enabled is set but the srt binary was not found on PATH — refusing to boot workers that would be silently unsandboxed",
	})

	return r
}

// dirPrivate is a WARNING check that a directory is 0700 (no group/other bits).
func dirPrivate(name, dir string, mode fs.FileMode, ok bool) Check {
	return Check{
		Name: name, Critical: false, Pass: ok && mode.Perm()&0o077 == 0,
		Detail: fmt.Sprintf("dir %s should be 0700 (no group/other access); got %v (exists=%v)", dir, mode.Perm(), ok),
	}
}
