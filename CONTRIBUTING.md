# Contributing to castellan

Thanks for your interest! Castellan is early — the most useful contributions right now are
review of the design docs and, once PASS-1 lands, implementation against the plan.

## Ground rules

- Read [`docs/implementation-plan.md`](docs/implementation-plan.md) first. The build is
  organized into **PASS-0 (schema/contract freeze) → PASS-1 (spine) → PASS-2 (single-VM
  walking skeleton) → PASS-3 (hardening)**. Work with that order.
- **Test-driven**: each task in the plan is a failing test → minimal implementation →
  green → commit. Please follow it.
- **Go 1.22+**, pure-Go dependencies where possible (the ledger uses `modernc.org/sqlite`
  so the binary is cgo-free).
- Keep changes focused; one logical change per PR.

## Before you commit

1. Install the hooks: `pip install pre-commit && pre-commit install`
   (this runs **gitleaks** + basic hygiene checks on every commit).
2. Never commit secrets, `.env` files, or runtime state (`*.db`, logs, worktrees) — the
   `.gitignore` and gitleaks config guard these, but double-check.
3. Run `gitleaks detect --no-banner` locally if you have it installed.

## Commit & PR conventions

- Conventional-commit style is appreciated (`feat:`, `fix:`, `docs:`, `chore:` …).
- Explain *why*, not just *what*, in the PR description.
- CI must be green (gitleaks; Go build/test once code exists).

## Licensing

By contributing, you agree that your contributions are licensed under the project's
[Apache License 2.0](LICENSE).

## Conduct

Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).
