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
	killed    []string
}

var _ core.VMClient = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake { return &Fake{Heads: map[string]string{}} }

func (f *Fake) Prompt(_ context.Context, workspace, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, Prompted{workspace, text})
	return f.PromptErr
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

func (f *Fake) Diff(context.Context, string, string, string) (core.Diff, error) {
	return core.Diff{}, nil
}
