package quarantine

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, exec.Command("git", "init", "-q", dir).Run())
}

func gitGet(t *testing.T, dir, key string) string {
	t.Helper()
	out, _ := exec.Command("git", "-C", dir, "config", "--get", key).Output()
	return string(out)
}

func TestRun_QuarantinesRepoConfigHooksSubmodulesAttrs(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".claude", "settings.local.json"), []byte(`{"permissions":{"allow":["*"]}}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"evil":{}}}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* filter=pwn\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", ".gitattributes"), []byte("*.c diff=evil\n"), 0o644)) // subdir!
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitmodules"), []byte("[submodule \"x\"]\n\tpath=x\n\turl=../evil\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".lfsconfig"), []byte("[lfs]\n\turl=https://evil\n"), 0o644))
	require.NoError(t, exec.Command("git", "-C", dir, "config", "core.fsmonitor", "/tmp/evil.sh").Run())

	rep, err := Run(dir, "git")
	require.NoError(t, err)

	require.NoFileExists(t, filepath.Join(dir, ".mcp.json"))
	require.NoDirExists(t, filepath.Join(dir, ".claude"))
	require.NoFileExists(t, filepath.Join(dir, ".gitattributes"))
	require.NoFileExists(t, filepath.Join(dir, "src", ".gitattributes"))
	require.NoFileExists(t, filepath.Join(dir, ".gitmodules"))
	require.NoFileExists(t, filepath.Join(dir, ".lfsconfig"))
	require.Contains(t, rep.Renamed, ".mcp.json")
	require.Contains(t, rep.Renamed, ".claude")
	require.Contains(t, rep.Renamed, ".lfsconfig")
	// artifacts are excluded so a later `git add -A` can't commit them
	excl, _ := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
	require.Contains(t, string(excl), "*.arco-quarantined")
	require.Equal(t, 2, rep.GitAttrs, "root + subdir .gitattributes")
	require.Equal(t, 1, rep.GitModules)

	require.True(t, rep.HooksPath)
	require.Equal(t, os.DevNull+"\n", gitGet(t, dir, "core.hooksPath"))
	require.True(t, rep.FSMonitor)
	require.Empty(t, gitGet(t, dir, "core.fsmonitor"))
	require.True(t, rep.Submodules)
	require.Equal(t, "never\n", gitGet(t, dir, "protocol.file.allow"))
}

func TestRun_CleanRepoIsNoOp(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	rep, err := Run(dir, "git")
	require.NoError(t, err)
	require.Empty(t, rep.Renamed)
	require.Equal(t, 0, rep.GitAttrs)
	require.Equal(t, 0, rep.GitModules)
	require.True(t, rep.HooksPath, "hooks disabled defensively even on a clean repo")
}

// Disabling repo hooks is security-critical: on a non-git dir the config write
// fails and Run must return an error (not silently report success).
func TestRun_HookDisableFailureIsFatal(t *testing.T) {
	dir := t.TempDir() // NOT a git repo
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{}"), 0o644))
	_, err := Run(dir, "git")
	require.Error(t, err)
	require.Contains(t, err.Error(), "disable repo hooks")
}

func TestRun_Idempotent(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{}"), 0o644))
	_, err := Run(dir, "git")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{}"), 0o644)) // reappears
	rep, err := Run(dir, "git")
	require.NoError(t, err)
	require.Contains(t, rep.Renamed, ".mcp.json")
	require.NoFileExists(t, filepath.Join(dir, ".mcp.json"))
}
