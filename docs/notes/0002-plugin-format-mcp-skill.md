# 0002 — arco plugin format = MCP + SKILL.md, not DeepSeek Harness

## Status

accepted (2026-08-25). First capability shipped under it: the Telegram image
relay (`skills/arco-image/SKILL.md` + the `arco image` CLI core).

## Problem

arco needs a pluggable way to give worker agents new capabilities (first case: a
Telegram image relay). The question was whether to build that plugin system on
DeepSeek Harness (`dsh`) / its Cordis "everything is a plugin" runtime, learn
from it and build our own, or use open standards.

## Decision

**Package arco capabilities as an MCP server (the callable capability) + a
Claude-Code-style `SKILL.md` bundle (the instructions), injected per worker
kind.** For now the portable *core* is the `arco image`-style CLI over the
per-worker socket, which a `SKILL.md` wraps; an MCP server is the natural future
wrapper when a capability must be consumable outside arco. Do **not** rebuild
arco on dsh.

Backed by a 4-reviewer study (clavis-qwen, opus, fable, web-research) of the
cloned `deepseek-harness`; all four independently reached this conclusion.

## Alternatives considered

- **Build arco on dsh / Cordis.** Rejected. dsh is an *in-process TypeScript
  runtime that owns the agent loop* — a different layer than arco (a Go
  supervisor of external agent CLIs; "arco supervises agents, dsh *is* one").
  Adopting it means replacing arco's Go core, and its Cordis plugins are
  dsh-only (not loadable by claude/codex/gemini/qwen workers) — highest lock-in.
  It's also a `0.1.1-rc.2` developer preview on a vendored Cordis RC fork with
  self-declared breaking changes and no external PRs.
- **Learn from dsh, invent our own plugin runtime.** Partially adopted, not as
  the format: borrow dsh's *design* (provider registry + ranked discovery,
  per-scope layering, disposer-based registration, layered config patches) into
  arco's Go core where useful — but the interchange *format* stays MCP + SKILL.md.
- **MCP + SKILL.md (chosen).** dsh itself validates this: it's an MCP *client*
  and ships a Claude-Code-aligned SKILL.md loader. These are open, file-based,
  provider-neutral — consumable by every worker kind arco supervises today.

## Consequences

- Zero lock-in; a capability written once works across worker kinds and survives
  future harness choices.
- Biggest cost lands on arco: MCP gives tools but no lifecycle/policy hooks, so
  approval-gating, per-worker capability scoping, and MCP-server supervision
  (crash/restart, credential brokering) are arco's job. Mitigation: define
  arco's plugin *manifest* to carry both an MCP server spec and (future) hook
  declarations, so today's MCP-only plugins stay valid when a hook layer lands.
- Optional later: add dsh itself to arco's worker roster as one more
  MCP/ACP-speaking `agent_kind` — consume it, never become it.
