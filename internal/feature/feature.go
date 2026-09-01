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
	// BrainAct tools mutate fleet or ledger state. The INTENDED contract is that a
	// host degrades a disallowed BrainAct call to an operator escalation (never
	// silently executed, never silently hidden). NOTE: the only host today is the
	// read-only text-protocol loop (internal/toolloop), which does NOT expose or
	// execute BrainAct — it is reserved for a native-tool-use host with a grant +
	// escalation path (a later phase). So declaring a tool BrainAct today makes it
	// invisible to chat, not escalatable. Port read-only features as BrainSafe; a
	// mutating feature (adopt/kill/dispatch) waits on that host.
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

// Feature is a struct of optional declarations — a bundle fills only the
// surfaces it offers. A read-only capability sets Command and a BrainSafe Tool;
// a chat-only capability sets just a Tool; a host service sets neither and is
// wired directly at assembly. Assembled by a [Registry] at boot.
type Feature struct {
	Name    string // bundle-unique label, for wiring errors and diagnostics
	Command *Command
	Tool    *Tool
}
