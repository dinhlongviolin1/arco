# Decision notes (ADRs)

Lightweight architecture decision records. One file per decision that was
non-obvious or that a future review keeps re-litigating. Borrowed from DeepSeek
Harness's `.agents/notes` practice — but deliberately *without* its enforcement
machinery (no format-gate scripts, no i18n counterparts, no sidecar hashes):
this repo has one maintainer, so the value is the record, not the ceremony.

## Format

Each note is `NNNN-kebab-title.md` with:

- `## Status` — `proposed` | `accepted` | `implemented` | `rejected` | `superseded by NNNN`
- `## Problem` — what forced the decision
- `## Decision` — what we chose
- `## Alternatives considered` — **mandatory.** What we rejected and *why*. A
  decision without its alternatives invites re-litigation.
- `## Consequences` — what this commits us to (good and bad)

Keep them short. Update `## Status` in place as a decision moves along; don't
delete a rejected note — the rejection is the value.

## Index

- [0001](0001-per-worker-confinement-tier.md) — per-worker confinement tier + enforcement fact (proposed)
- [0002](0002-plugin-format-mcp-skill.md) — arco plugin format = MCP + SKILL.md, not DeepSeek Harness (accepted)
