# arco

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-alpha-orange.svg)](docs/rev7-review.md)
[![gitleaks](https://img.shields.io/badge/protected%20by-gitleaks-brightgreen.svg)](.gitleaks.toml)

**Command your worker agents the way a bow commands the strings** — a self-hosted daemon that
supervises a fleet of coding-agent workers across your machines, decides what to do when they
finish or block, and is driven from the CLI plus phone-first ntfy decision cards
(`/metrics` and a herdr status plugin as side doors — no web dashboard by design).

> `clavis` launches workers · `herdr` herds them on each machine · **`arco` commands the whole ensemble.**
> (*arco* is the violin instruction to play *with the bow* — how a player draws sound and control from the strings; `clavis` is Latin for "key".)

---

## ⚠️ Status: alpha (rev-7 complete)

The daemon, CLI, and supervision loop are implemented and test-covered (rev-7 close-out:
[`docs/rev7-review.md`](docs/rev7-review.md)); the single-VM spawn/supervise/kill loop is
live-verified ([`docs/deployment-hardening.md`](docs/deployment-hardening.md) §11). Known
open items — most notably the worker cred-consumption contract — are tracked in the rev-7
review package and hardening §12. **Do not point it at production credentials or
repositories yet** — see [SECURITY.md](SECURITY.md).

## Install

One static binary (pure-Go SQLite — no libc, no runtime deps), released the same way as
`clavis`: goreleaser on every `v*` tag, `checksums.txt` alongside.

### Quick install (Linux / macOS)

```sh
curl -fsSL https://raw.githubusercontent.com/dinhlongviolin1/arco/main/install.sh | sh
```

Or via Homebrew (macOS + Linuxbrew, once the tap is published):

```sh
brew install dinhlongviolin1/tap/arco
```

### Other ways

```sh
go install github.com/dinhlongviolin1/arco/cmd/arco@latest   # from source
```

Or grab `arco_<os>_<arch>.tar.gz` from the
[releases page](https://github.com/dinhlongviolin1/arco/releases) and verify it against
`checksums.txt`. Pin a version with `ARCO_VERSION=v0.1.0`, choose the target dir with
`ARCO_INSTALL_DIR=~/.local/bin`.

### Running it (incl. LXC / Proxmox)

arco is one process + one SQLite file — it runs anywhere a shell does. In a container it
wants: `git`, `herdr` (running, for real workers), `clavis` (for inference — below), and a
`0700` state dir (`~/.arco`). Follow [`docs/deployment-hardening.md`](docs/deployment-hardening.md)
§11 for the config + pool + dispatch procedure and §5 for the hardened systemd unit
(`LoadCredential=` for the intake/ntfy secrets). Fake mode (`use_local_vm = false`, the
default) needs nothing but the binary — useful for poking the API/CLI in a bare container.

### Inference

arco itself talks to **no** LLM API directly and needs no `ANTHROPIC_*` env. Both
inference surfaces go through `clavis` profiles (your provider keys live in `~/.clavis`,
one named profile per provider/model account):

- **Brain** (the short-lived decision LLM): set `brain_profile = "<clavis profile>"` +
  `brain_model` (default `haiku`, deliberately cheap) in `config.toml`. Empty profile =
  brain disabled; arco still supervises, escalating every question to you.
- **Workers** (spawned coding agents): `arco pool create <id> --profile <clavis profile>`;
  each worker leases from the pool and gets that profile's scoped creds as `0600` files
  under its private root, pointed to by `CREDENTIALS_DIRECTORY` (never argv/env — MED-5).
  NB: the agent-side consumption of those files is a documented contract (hardening §12) —
  see open question #0 in the rev-7 review before expecting autonomous completion.

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
- [`docs/deployment-hardening.md`](docs/deployment-hardening.md) — **operator runbook:** the
  out-of-process half of the PASS-3 security preconditions (OS-user separation, managed-settings
  placement, server-side branch protection, signed intake, the live-herdr spike) — do these before
  pointing arco at real repos/creds.
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
