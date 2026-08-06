// Package vm provides VMClient implementations. Fake is an in-memory client for
// tests and headless smoke runs; LocalVMClient (added later) shells out to
// clavis/herdr on the local machine.
package vm

import (
	"context"
	"sync"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// Prompted records one Prompt call.
type Prompted struct{ Workspace, Text string }

// Fake is an in-memory VMClient. Safe for concurrent use.
type Fake struct {
	mu        sync.Mutex
	prompts   []Prompted
	Agents    []core.AgentObs
	Heads     map[string]string
	PromptErr error
	// Statuses maps a target to the AgentStatus the Fake reports ("" = unknown).
	// Lets tests drive the busy-agent guard on redeliver.
	Statuses map[string]string
	// AliveOnPrompt models "the agent spawned but Prompt still returned an error"
	// (ambiguous launch): a prompted workspace is thereafter reported alive.
	AliveOnPrompt bool
	killed        []string
	launched      []core.LaunchSpec
	LaunchErr     error
	// LaunchAliveOnErr models a launch that SPAWNED the agent but still returned an
	// error (ref-capture timeout / transient post-spawn error): the agent shows
	// alive by workspace despite LaunchErr, so the caller must resolve by liveness.
	LaunchAliveOnErr bool
}

// Launched returns the LaunchSpecs seen (test inspection).
func (f *Fake) Launched() []core.LaunchSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.LaunchSpec(nil), f.launched...)
}

// Launch records the spec and returns a deterministic synthetic ref + stable
// identity (bootID), registering the new agent as alive (so a subsequent
// ListAgents/sweep correlates by ref AND matches identity).
func (f *Fake) Launch(_ context.Context, spec core.LaunchSpec) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launched = append(f.launched, spec)
	if f.LaunchErr != nil {
		if f.LaunchAliveOnErr { // spawned-but-errored: agent is alive despite the error
			f.Agents = append(f.Agents, core.AgentObs{Workspace: spec.Name, Alive: true})
		}
		return "", "", f.LaunchErr
	}
	ref := "pane:" + spec.Name
	bootID := "term:" + spec.Name
	f.Agents = append(f.Agents, core.AgentObs{Ref: ref, Workspace: spec.Name, BootID: bootID, Alive: true})
	return ref, bootID, nil
}

var _ core.VMClient = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake { return &Fake{Heads: map[string]string{}} }

func (f *Fake) Prompt(_ context.Context, workspace, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, Prompted{workspace, text})
	if f.AliveOnPrompt {
		f.Agents = append(f.Agents, core.AgentObs{Workspace: workspace, Alive: true})
	}
	return f.PromptErr
}

// PromptReady records like Prompt (the Fake has no readiness race to confirm).
func (f *Fake) PromptReady(ctx context.Context, workspace, text string) error {
	return f.Prompt(ctx, workspace, text)
}

// AgentStatus returns the configured status for a target ("" = unknown).
func (f *Fake) AgentStatus(_ context.Context, target string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Statuses[target], nil
}

func (f *Fake) Prompts() []Prompted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Prompted(nil), f.prompts...)
}

func (f *Fake) ListAgents(context.Context) ([]core.AgentObs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.AgentObs(nil), f.Agents...), nil
}

func (f *Fake) GitHeads(_ context.Context, worktrees []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, wt := range worktrees {
		if h, ok := f.Heads[wt]; ok {
			out[wt] = h
		}
	}
	return out, nil
}

func (f *Fake) Kill(_ context.Context, workspace string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, workspace)
	return nil
}

// Killed returns the targets passed to Kill (test inspection).
func (f *Fake) Killed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.killed...)
}

func (f *Fake) Diff(context.Context, string, string, string) (core.Diff, error) {
	return core.Diff{}, nil
}
