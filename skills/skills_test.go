package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstall_WritesSkillAndGitExcludes(t *testing.T) {
	wt := t.TempDir()
	require.NoError(t, exec.Command("git", "-C", wt, "init").Run())

	require.NoError(t, Install("git", wt, []string{"arco-image"}))

	// the skill landed where Claude discovers it
	p := filepath.Join(wt, ".claude", "skills", "arco-image", "SKILL.md")
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Contains(t, string(data), "arco image send", "the embedded skill body was written")
	require.Contains(t, string(data), "name: arco-image", "frontmatter present")

	// .claude/ is git-excluded so the agent can't accidentally commit it
	excl, err := os.ReadFile(filepath.Join(wt, ".git", "info", "exclude"))
	require.NoError(t, err)
	require.Contains(t, string(excl), ".claude/")

	// idempotent: a second install doesn't double-append the exclude
	require.NoError(t, Install("git", wt, []string{"arco-image"}))
	excl2, _ := os.ReadFile(filepath.Join(wt, ".git", "info", "exclude"))
	require.Equal(t, 1, strings.Count(string(excl2), "# arco-injected agent config"))
}

func TestInstall_UnknownSkillErrors(t *testing.T) {
	wt := t.TempDir()
	require.Error(t, Install("git", wt, []string{"does-not-exist"}))
}
