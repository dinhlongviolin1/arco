package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// --vm targets the repo-spawn path only; without --repo it's a usage error
// (the guard fires before any socket call, so no daemon is needed).
func TestDispatchCmd_VMRequiresRepo(t *testing.T) {
	root := newRoot()
	var errBuf bytes.Buffer
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&errBuf)
	root.SetArgs([]string{"dispatch", "--vm", "vm1", "do the thing"})
	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "--vm requires --repo")
}

func TestDispatchCmd_BaseRequiresRepo(t *testing.T) {
	root := newRoot()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"dispatch", "--base", "HEAD~1", "do the thing"})
	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "--base requires --repo")
}
