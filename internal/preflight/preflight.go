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

// Probe is the observed system/config state the checks decide over (injected so
// Check is deterministic and testable).
type Probe struct {
	Euid         int         // effective uid; 0 = root
	GitPath      string      // resolved path to git, "" if not found
	StateDir     string      // arco state dir (holds the ledger + worker configs)
	StateDirMode fs.FileMode // permission bits of StateDir (0 if it doesn't exist)
	StateDirOK   bool        // whether StateDir was stat-able
	TCPAddr      string      // configured network intake address ("" = socket only)
	IntakeSecret string      // configured HMAC intake secret
}

// Gather collects the real Probe from the OS + the given config values.
func Gather(stateDir, tcpAddr, intakeSecret string) Probe {
	p := Probe{Euid: os.Geteuid(), StateDir: stateDir, TCPAddr: tcpAddr, IntakeSecret: intakeSecret}
	if path, err := exec.LookPath("git"); err == nil {
		p.GitPath = path
	}
	if fi, err := os.Stat(stateDir); err == nil {
		p.StateDirMode, p.StateDirOK = fi.Mode(), true
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
	// escape defeats every filesystem sandbox — refuse.
	r.Checks = append(r.Checks, Check{
		Name: "not_root", Critical: true, Pass: p.Euid != 0,
		Detail: "arco must not run as root (uid 0) — a worker escape would be unconfined",
	})

	// git_present (CRITICAL): quarantine + git hardening + diff-gate all shell out
	// to git; without it those controls silently no-op.
	r.Checks = append(r.Checks, Check{
		Name: "git_present", Critical: true, Pass: p.GitPath != "",
		Detail: "git binary not found on PATH — quarantine / git-hardening / diff-gate require it",
	})

	// state_dir_private (WARNING, not fatal): the ledger + compiled worker configs
	// live here; world/group access exposes the event log and lets others tamper
	// with a worker's staged config, so 0700 is recommended. It is NOT critical
	// because arco must not refuse to run — nor chmod — a state dir it may not own
	// (the operator can point db_path at a shared dir); arco's authoritative
	// controls (Allowed(), redaction) don't depend on these bits. Surfaced so the
	// operator tightens it.
	privatePerm := p.StateDirOK && p.StateDirMode.Perm()&0o077 == 0
	r.Checks = append(r.Checks, Check{
		Name: "state_dir_private", Critical: false, Pass: privatePerm,
		Detail: fmt.Sprintf("state dir %s should be 0700 (no group/other access); got %v (exists=%v)", p.StateDir, p.StateDirMode.Perm(), p.StateDirOK),
	})

	// tcp_intake_signed (CRITICAL): a network-exposed intake without a shared
	// secret is an unauthenticated event-injection surface (P4).
	r.Checks = append(r.Checks, Check{
		Name: "tcp_intake_signed", Critical: true, Pass: p.TCPAddr == "" || p.IntakeSecret != "",
		Detail: "tcp_addr is set but intake_secret is empty — network intake must be HMAC-signed",
	})

	return r
}
