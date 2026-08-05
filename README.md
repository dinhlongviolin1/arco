# arco

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-pre--alpha%20(design)-orange.svg)](docs/)
[![gitleaks](https://img.shields.io/badge/protected%20by-gitleaks-brightgreen.svg)](.gitleaks.toml)

**Command your worker agents the way a bow commands the strings** — a self-hosted daemon that
supervises a fleet of coding-agent workers across your machines, decides what to do when they
finish or block, and is driven from the CLI (with optional Telegram and Web dashboards).

> `clavis` launches workers · `herdr` herds them on each machine · **`arco` commands the whole ensemble.**
> (*arco* is the violin instruction to play *with the bow* — how a player draws sound and control from the strings; `clavis` is Latin for "key".)

---

## ⚠️ Status: pre-alpha (design phase)

This repository currently holds the **design and implementation plan**, not yet a running
daemon. The architecture has been through five independent review rounds (consolidated into the
rev-6 build guide); the build starts at **PASS-0** in [`docs/build-guide-rev6.md`](docs/build-guide-rev6.md).
**Do not point it at production credentials or repositories** — see [SECURITY.md](SECURITY.md).

## What it is

One durable Go daemon that owns the truth about every worker via a local SQLite ledger,
reacts to worker state changes, and invokes a short-lived LLM "brain" **only when a
decision is needed** — so nothing long-running is an LLM process (no leaks, bounded cost,
crash-safe).

- **Sessions** — the first-class unit of *work + conversation*; a session owns a group of
  workers (possibly across machines), holds its own context, and carries a capability tree.
- **Autonomy-first** — routine worker actions never gate; only a *question* (stuck) or a
  *confirm* (dangerous) interrupts, and questions are answered two-level (the daemon tries
  first, escalates to you only when uncertain or when the action is dangerous).
- **Least privilege** — each session has a capability tree; workers inherit a narrowed copy;
  you widen it with explicit, auditable grants.
- **Durable & crash-safe** — idempotent event intake, an authoritative reconcile sweep,
  and intent→execute→result for every side effect.

## Design docs

- [`docs/build-guide-rev6.md`](docs/build-guide-rev6.md) — **START HERE.** The authoritative,
  consolidated build guide: resolved decisions, the frozen `0001_init.sql` (schema + seed data),
  the frozen Go contracts, and the PASS-0→PASS-3 task list.
- [`docs/overview.md`](docs/overview.md) — readable overview + roadmap.
- [`docs/implementation-plan.md`](docs/implementation-plan.md) — the layered rev-1→rev-5 plan and
  per-task detail (**provenance**; superseded by the rev-6 guide where they differ).
- [`docs/hardening-report-rev5.md`](docs/hardening-report-rev5.md) ·
  [`docs/memory-links-rev5.md`](docs/memory-links-rev5.md) ·
  [`docs/design-blueprint.md`](docs/design-blueprint.md) — review findings, the memory decision, and
  the design rationale (provenance).

## Contributing & security

- [CONTRIBUTING.md](CONTRIBUTING.md) — how to build, test, and submit changes.
- [SECURITY.md](SECURITY.md) — threat model and how to report a vulnerability privately.
- Secret scanning is enforced via [gitleaks](https://github.com/gitleaks/gitleaks) in CI and
  as a pre-commit hook.

## License

[Apache License 2.0](LICENSE) © 2026 Long Nguyen and the Arco contributors.
