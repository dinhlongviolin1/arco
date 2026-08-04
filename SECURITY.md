# Security Policy

## Status

Arco is **pre-alpha** and under active design. It is a supervisor for **autonomous
coding agents that execute code and run tools**. Treat every deployment as
security-sensitive.

**Do not run arco against production credentials, secrets, or repositories** until
the security preconditions in the implementation plan are met (OS-user separation,
scrubbed spawn environment, hardened git, no high-blast credentials on worker boxes,
server-side branch protection, authenticated Telegram/Web/intake, and secret redaction).
See the "Security preconditions" section of [`docs/implementation-plan.md`](docs/implementation-plan.md).

## Threat model (summary)

- Workers run untrusted repository content and may be prompt-injected; the capability
  tree and arco's own action boundary are the containment, with server-side git
  rules as the only non-advisory layer.
- Worker-side permission config is defense-in-depth, **not** a hard boundary.
- High-blast capabilities (push to default branch, deploy, spend, destructive deletes) are
  never handed to workers; arco performs them only after an explicit confirm.

## Reporting a vulnerability

Please report suspected vulnerabilities **privately** — do not open a public issue.

- Preferred: open a **GitHub private security advisory** via the repository's
  **Security → Report a vulnerability** tab.
- Or email the maintainer (see the GitHub profile for `dinhlongviolin1`).

Please include a description, reproduction steps, and impact. We'll acknowledge within a
few days. As a pre-1.0 project there are no formal SLAs yet, but security reports are
prioritized.

## Secret hygiene

This repo enforces [gitleaks](https://github.com/gitleaks/gitleaks) in CI and as a
pre-commit hook. Never commit tokens, keys, or `.env` files — see
[`.gitignore`](.gitignore) and [`.gitleaks.toml`](.gitleaks.toml).
