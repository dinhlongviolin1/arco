package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// ClavisCreds resolves a clavis profile's env (the scoped provider credentials a
// worker launches with) by shelling `clavis env <profile> --json --reveal-key`.
// clavis is "Claude Code, wrapped": the profile's ANTHROPIC_AUTH_TOKEN/BASE_URL +
// model vars are exactly what make the launched claude agent authenticate as that
// provider. arco injects these (post-scrub) so a worker uses its POOL's scoped
// profile, never arco's own inherited creds (P1). Implements core.AgentCredentials.
type ClavisCreds struct{ Bin string }

var _ core.AgentCredentials = (*ClavisCreds)(nil)

// NewClavisCreds builds a resolver using the given clavis binary ("" → "clavis").
func NewClavisCreds(bin string) *ClavisCreds {
	if bin == "" {
		bin = "clavis"
	}
	return &ClavisCreds{Bin: bin}
}

// EnvFor returns the profile's env as sorted "KEY=VALUE" lines. An empty profile
// yields nil (no injection). Errors surface so a misconfigured pool fails the
// spawn loudly rather than launching a credential-less (unauthenticated) worker.
func (c *ClavisCreds) EnvFor(ctx context.Context, profile string) ([]string, error) {
	if profile == "" {
		return nil, nil
	}
	out, err := runOutput(ctx, c.Bin, "env", profile, "--json", "--reveal-key")
	if err != nil {
		// Deliberately DROP clavis's raw stderr from the returned error: this runs
		// with --reveal-key, and the error propagates into a durable dispatch_done
		// event payload — a token echoed on stderr could slip past the write-time
		// redactor (which only matches known shapes). Keep just the profile name.
		return nil, fmt.Errorf("clavis env %s failed (exit error; see daemon output)", profile)
	}
	var m map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("clavis env %s: bad json: %w", profile, err)
	}
	kv := make([]string, 0, len(m))
	for k, v := range m {
		kv = append(kv, k+"="+v)
	}
	sort.Strings(kv) // deterministic order (stable launch argv / tests)
	return kv, nil
}
