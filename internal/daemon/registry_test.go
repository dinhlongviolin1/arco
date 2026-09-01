package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
	"github.com/dinhlongviolin1/arco/internal/feature"
	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

// The console sentinel session is created idempotently, and the ContextStore
// adapter round-trips durable chat messages through the real ledger.
func TestConsoleSessionAndContextStore(t *testing.T) {
	store, err := ledger.Open(filepath.Join(t.TempDir(), "arco.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, store.Migrate(context.Background()))

	ctx := context.Background()
	require.NoError(t, ensureConsoleSession(ctx, store))
	s, err := store.Reader().GetSession(core.ConsoleSessionID)
	require.NoError(t, err)
	require.Equal(t, core.ConsoleSessionID, s.ID)
	require.NoError(t, ensureConsoleSession(ctx, store), "second call is an idempotent no-op")

	cs := contextStore{s: store}
	require.NoError(t, cs.AppendMessage(ctx, core.ConsoleSessionID, "operator", "hello there"))
	require.NoError(t, cs.AppendMessage(ctx, core.ConsoleSessionID, "arco", "hi — 0 workers running"))
	msgs, err := cs.RecentMessages(core.ConsoleSessionID, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "operator", msgs[0].Role)
	require.Equal(t, "hello there", msgs[0].Content)
	require.Equal(t, "arco", msgs[1].Role)
}

// buildRegistry is the composition root: this asserts a ported feature (/scan)
// is actually wired to both surfaces — the operator command AND the BrainSafe
// tool — from one registration, and that it runs end-to-end against a real
// engine (empty fleet → the empty-scan message).
func TestBuildRegistry_ServesScan(t *testing.T) {
	store, err := ledger.Open(filepath.Join(t.TempDir(), "arco.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, store.Migrate(context.Background()))
	eng := reconcile.New(store, vm.NewFake())

	reg := buildRegistry(eng, []string{"local (this box)"})

	// The command is registered and named without a slash.
	cmd, ok := reg.Command("scan")
	require.True(t, ok, "/scan is served by the registry")
	require.Equal(t, "scan", cmd.Name)

	// The same capability is exposed to the brain as a read-only tool.
	var scanTool *feature.Tool
	for _, tl := range reg.ForBrain() {
		if tl.Name == "scan" {
			t := tl
			scanTool = &t
		}
	}
	require.NotNil(t, scanTool, "scan is a brain tool")
	require.Equal(t, feature.BrainSafe, scanTool.Access)

	// It runs against the real engine — empty fleet yields the empty message.
	out, err := cmd.Run(context.Background(), feature.CmdInput{})
	require.NoError(t, err)
	require.Equal(t, "no herdr agent sessions found on the fleet", out)

	// Feature #2 (/peek) is also wired to both surfaces.
	if _, ok := reg.Command("peek"); !ok {
		t.Error("/peek is served by the registry")
	}
	var peekTool bool
	for _, tl := range reg.ForBrain() {
		if tl.Name == "peek" {
			peekTool = true
			require.Equal(t, feature.BrainSafe, tl.Access)
		}
	}
	require.True(t, peekTool, "peek is a brain tool")

	// The ledger read features (#3–5) are wired as commands + BrainSafe tools.
	brainTools := map[string]bool{}
	for _, tl := range reg.ForBrain() {
		brainTools[tl.Name] = true
	}
	// /kill and /adopt are Command-only mutating features: served as commands but
	// NEVER brain tools (the read-only loop must not terminate/adopt a worker).
	brainNames := map[string]bool{}
	for _, tl := range reg.ForBrain() {
		brainNames[tl.Name] = true
	}
	for _, name := range []string{"kill", "adopt"} {
		if _, ok := reg.Command(name); !ok {
			t.Errorf("/%s is served by the registry", name)
		}
		require.False(t, brainNames[name], "mutating /%s must NOT be a brain tool", name)
	}
	for _, name := range []string{"workers", "sessions", "status", "diff", "vms"} {
		if _, ok := reg.Command(name); !ok {
			t.Errorf("/%s is served by the registry", name)
		}
		require.True(t, brainTools[name], "%s is a brain tool", name)
	}
	// /status runs against the real engine (empty fleet → running, 0 workers).
	statusCmd, _ := reg.Command("status")
	sout, err := statusCmd.Run(context.Background(), feature.CmdInput{})
	require.NoError(t, err)
	require.Contains(t, sout, "active workers: 0")
}
