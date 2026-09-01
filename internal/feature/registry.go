package feature

import (
	"fmt"
	"sort"
	"strings"
)

// Registry assembles Features at daemon boot and binds each declaration to every
// surface that consumes it. It is built once, on one goroutine, BEFORE serving
// begins, and is read-only thereafter — so lookups need no lock.
type Registry struct {
	features []Feature
	commands map[string]*Command
	tools    map[string]*Tool
}

// NewRegistry returns an empty registry. An empty registry is a valid, inert
// registry: every lookup misses, so a surface that consults it behaves exactly
// as it did before any feature was registered (the coexistence seam).
func NewRegistry() *Registry {
	return &Registry{commands: map[string]*Command{}, tools: map[string]*Tool{}}
}

// Register adds a feature and indexes its Command/Tool. It rejects a duplicate
// command or tool name so a wiring mistake fails LOUD at assembly, not silently
// at runtime. Command names are stored normalized (lower-case, no slash).
func (r *Registry) Register(f Feature) error {
	if f.Command != nil {
		name := NormalizeCmd(f.Command.Name)
		if name == "" {
			return fmt.Errorf("feature %q: empty command name", f.Name)
		}
		if _, dup := r.commands[name]; dup {
			return fmt.Errorf("feature %q: duplicate command %q", f.Name, name)
		}
		// Store the CANONICAL (normalized) name so every downstream consumer — the
		// switch shadow-panic, the menu/help dedup, the client command menu — sees
		// one form. Otherwise a variant like "Status" or "/scan" would slip past the
		// lower-cased built-in guards yet be shadowed at runtime by the case-folding
		// switch. Copy so the caller's Command value is not mutated.
		cmd := *f.Command
		cmd.Name = name
		r.commands[name] = &cmd
		f.Command = &cmd
	}
	if f.Tool != nil {
		if strings.TrimSpace(f.Tool.Name) == "" {
			return fmt.Errorf("feature %q: empty tool name", f.Name)
		}
		if _, dup := r.tools[f.Tool.Name]; dup {
			return fmt.Errorf("feature %q: duplicate tool %q", f.Name, f.Tool.Name)
		}
		r.tools[f.Tool.Name] = f.Tool
	}
	r.features = append(r.features, f)
	return nil
}

// MustRegister registers each feature and panics on the first error — for
// assembly-time wiring where a duplicate is a programming bug that should stop
// the daemon at boot.
func (r *Registry) MustRegister(fs ...Feature) {
	for _, f := range fs {
		if err := r.Register(f); err != nil {
			panic("feature: " + err.Error())
		}
	}
}

// Command looks up a command by name (case-insensitive; a leading slash is
// optional, so "/scan" and "scan" both resolve).
func (r *Registry) Command(name string) (*Command, bool) {
	c, ok := r.commands[NormalizeCmd(name)]
	return c, ok
}

// Commands returns every registered command, sorted by name. The command menu
// (setMyCommands) and /help are GENERATED from this — declare a feature and both
// update themselves.
func (r *Registry) Commands() []Command {
	out := make([]Command, 0, len(r.commands))
	for _, c := range r.commands {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ForBrain returns the tools a non-operator caller (the brain, MCP) may see:
// BrainSafe and BrainAct, never Operator. Whether a BrainAct tool is actually
// exposed/executed is the HOST's decision: the read-only toolloop host refuses
// them today (see the BrainAct note in feature.go); a future native-tool-use
// host will expose them behind a grant + escalation. ForBrain is the superset;
// the host filters.
func (r *Registry) ForBrain() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		if t.Access != Operator {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Tools returns every registered tool, sorted by name — for the MCP server, one
// more consumer of the same declarations.
func (r *Registry) Tools() []Tool {
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NormalizeCmd lowercases a command name and strips a leading slash and spaces,
// so lookups and registration agree on one canonical form.
func NormalizeCmd(name string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "/"))
}
