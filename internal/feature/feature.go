// Package feature defines the compile-time contract for arco's pluggable
// capabilities — the seam that makes arco a modular agent you customize by
// adding a feature, not by editing five hardcoded layers.
//
// A Feature is a struct of OPTIONAL declarations that a bundle constructs at
// daemon assembly (from injected deps) and hands to a [Registry]. The registry
// binds each declaration to every surface that consumes it: today the Telegram
// command switch + the generated command menu and /help; next the brain
// tool-loop and the MCP server (each just one more reader of the same
// declarations). A feature that offers a Command AND a BrainSafe Tool is a
// verb the operator can type and the brain can call — declared once, in one
// place.
//
// This is the stdlib-only contract leaf: it imports NOTHING from arco, so no
// bundle depends on another through it. Bundles import feature; feature imports
// nobody. That one rule is the whole isolation story.
package feature

import (
	"context"
	"encoding/json"
)

// Access is a Tool's trust level — the gate the host enforces before a
// non-operator caller (the brain, an MCP client) may invoke it. Commands are
// always operator-invoked, so Access governs Tools only.
type Access int

const (
	// Operator tools are human-only: never exposed to the brain or MCP.
	Operator Access = iota
	// BrainSafe tools are read-only / idempotent: always brain-callable.
	BrainSafe
	// BrainAct tools mutate fleet or ledger state. The brain may PROPOSE them, but
	// whether one runs is the operator's per-feature policy ([Gate.Mode], default
	// confirm): auto runs it, off refuses it, confirm posts a Telegram ✅/❌ card and
	// runs it only on the operator's approval. The read-only tool-loop never
	// executes a BrainAct tool without that gate.
	BrainAct
)

func (a Access) String() string {
	switch a {
	case BrainSafe:
		return "brain-safe"
	case BrainAct:
		return "brain-act"
	default:
		return "operator"
	}
}

// Mode is the operator-configured execution policy for a mutating (BrainAct)
// tool when the brain proposes it in chat. Read-only tools ignore it.
type Mode string

const (
	// ModeAuto executes the proposed action immediately.
	ModeAuto Mode = "auto"
	// ModeConfirm posts a Telegram card and executes only on the operator's ✅.
	ModeConfirm Mode = "confirm"
	// ModeOff refuses: the operator must run the slash command themselves.
	ModeOff Mode = "off"
)

// ParseMode maps a config string to a Mode, defaulting to def for empty/unknown
// input (so a typo fails safe toward the caller's default, not toward auto).
func ParseMode(s string, def Mode) Mode {
	switch Mode(s) {
	case ModeAuto, ModeConfirm, ModeOff:
		return Mode(s)
	default:
		return def
	}
}

// CmdInput is what a [Command].Run receives from the surface that invoked it
// (a Telegram command today, a CLI subcommand later). It carries the resolved
// context so the closure never reaches back into a specific surface.
type CmdInput struct {
	Arg       string // everything after the command word, trimmed
	ThreadID  int64  // Telegram message-thread id (0 = General / console)
	SessionID string // the session bound to ThreadID, if any ("" = none)
	Actor     string // who invoked it (operator id), for authz + audit
}

// Command is an operator-facing verb. One declaration the registry binds to the
// Telegram command switch, the generated command menu (setMyCommands), /help,
// and a CLI subcommand — instead of hand-threading each surface.
type Command struct {
	Name  string // the command word WITHOUT a leading slash, e.g. "scan"
	Help  string // one line for /help and the client command menu
	Usage string // argument hint, e.g. "<pane>" ("" = takes no argument)
	// Run executes the command and returns the reply text to post back. An error
	// is surfaced to the operator; it must not panic (the host recovers, but a
	// clean error reads better).
	Run func(ctx context.Context, in CmdInput) (reply string, err error)
}

// Tool is the LLM-callable surface a feature optionally exposes. The host runs
// every invocation through its chokepoint (authorize by Access, persist intent
// for mutating calls, execute, redact the result); a Tool never talks to the
// model or the ledger itself — it just does the work and returns a string.
type Tool struct {
	Name   string          // stable identifier the model names to call it
	Desc   string          // one line telling the model when to use it
	Schema json.RawMessage // JSON Schema for args (nil = takes no arguments)
	Access Access
	Call   func(ctx context.Context, args json.RawMessage) (result string, err error)
}

// Gate is the per-turn policy + confirmation channel for MUTATING (BrainAct)
// tools, supplied by the surface that starts a brain conversation (Telegram). The
// tool-loop consults Mode(name) for each proposed mutating tool: auto → run it,
// off → refuse, confirm → hand it to Confirm (which posts an approval card and
// returns the message to relay to the model; the action runs only when the
// operator approves). A zero Gate (nil funcs) means all mutating tools are off.
type Gate struct {
	Mode    func(toolName string) Mode
	Confirm func(ctx context.Context, t Tool, args json.RawMessage) (result string, err error)
}

// Feature is a struct of optional declarations — a bundle fills only the
// surfaces it offers. A read-only capability sets Command and a BrainSafe Tool;
// a chat-only capability sets just a Tool; a host service sets neither and is
// wired directly at assembly. Assembled by a [Registry] at boot.
type Feature struct {
	Name    string // bundle-unique label, for wiring errors and diagnostics
	Command *Command
	Tool    *Tool
}
