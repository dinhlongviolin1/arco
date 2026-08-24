# 0001 — Per-worker confinement tier + enforcement fact

## Status

proposed (2026-08-25) — documented from the DeepSeek Harness architecture review; not yet implemented.

## Problem

arco supervises a fleet of coding-agent workers, but today confinement is
all-or-nothing and invisible per worker:

- Workers run as the **daemon's own UID** (sandbox off by default); the `[sandbox]`
  `srt` wrapper is a single global boolean, made a *boot* requirement via the
  `sandbox_srt_present` preflight.
- There is no per-worker record of *how* confined a given worker actually was.
  A worker that launched with srt present but no policy (partial containment) is
  indistinguishable in the ledger from one that launched fully confined.

For a supervisor whose entire value is bounding blast radius across a fleet,
"how confined is *this* worker" must be a first-class, audited fact — otherwise,
when an agent escapes, the operator can't tell what it could reach.

DeepSeek Harness (`docs/subsystems/sandbox.md`) models this well: a 3-value mode
(`read-only` | `workspace-write` | `danger-full-access`) resolved **per call**,
an explicit `enforcement: full | partial` the backend must report, and the hard
rule "**silent unconfined passthrough is never legal for a confined policy**"
(`confine` returns enforcing argv or throws `SANDBOX_UNAVAILABLE`).

## Decision

Adopt, for arco's spawn path:

1. A typed per-worker confinement **tier** resolved from the session/pool (not a
   single global flag), landing in `internal/config` (`Sandbox`) +
   `internal/reconcile/spawn.go` (`provisionAndLaunch`), with a small
   `internal/sandbox` type.
2. A recorded **enforcement fact** (`full` | `partial`) on the worker row /
   `dispatch_done` event, so a degraded launch is visible, not silent.
3. **Fail-closed at launch:** a worker whose configured tier can't be enforced
   does not launch unconfined — it fails (or is recorded `partial` only when the
   tier explicitly permits best-effort). Keep the existing boot gate as-is.

Effort: **M**. Not started; implement after the current Telegram/plugin work.

## Alternatives considered

- **Keep the global boolean.** Rejected: it can't express per-worker intent and
  records nothing, so the operator can't reason about a specific worker's reach —
  the exact question that matters after an escape.
- **Per-worker separate UID / full OS isolation now.** Deferred, not rejected:
  it's the real fix for the shared-UID gap, but it's a deployment/OS change
  (user namespaces, cred brokering) far larger than this. The tier + enforcement
  fact is the cheap step that makes the gap *visible and bounded* today and sets
  up the UID work later.
- **Adopt DeepSeek Harness's sandbox package directly.** Rejected per
  [0002](0002-plugin-format-mcp-skill.md): it's an in-process TS runtime; arco is
  a Go supervisor. Borrow the *vocabulary* (tiers + enforcement fact + fail-closed
  rule), reimplement in Go.

## Consequences

- The ledger gains an auditable per-worker confinement record — an operator can
  answer "what could worker w1 reach?" from the log.
- A misconfigured/degraded sandbox becomes a loud, recorded `partial`, not a
  silent full-access passthrough.
- Adds a small config surface and a spawn-path branch; does not fix the
  shared-UID gap itself (that's the deferred per-worker-UID work).
