package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

func TestLoadUserMemory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "USER.md", "I am Long")
	s := New(dir)
	u, idx := s.LoadUserMemory()
	require.Equal(t, "I am Long", u)
	require.Equal(t, "", idx) // missing MEMORY.md → ""
}

func TestRead_OutLinksAndBackLinks(t *testing.T) {
	dir := t.TempDir()
	// the motivating case: an external OSS project informs my project
	write(t, dir, "projects/arco.md", "adapts [[oss/graphiti]] and [[concepts/prompt-caching]]")
	write(t, dir, "oss/graphiti.md", "bi-temporal graph memory")
	write(t, dir, "projects/clavis.md", "sibling of [[projects/arco]]")
	s := New(dir)

	v, err := s.Read("projects/arco")
	require.NoError(t, err)
	require.Equal(t, []string{"concepts/prompt-caching", "oss/graphiti"}, v.OutLinks)
	require.Equal(t, []string{"projects/clavis"}, v.BackLinks, "clavis links to arco")

	// the OSS node sees arco as a backlink (cross-project retrieval)
	g, err := s.Read("oss/graphiti")
	require.NoError(t, err)
	require.Equal(t, []string{"projects/arco"}, g.BackLinks)
}

func TestRead_MissingTopicStillComputesBacklinks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "projects/arco.md", "see [[ghost]]")
	v, err := New(dir).Read("ghost")
	require.NoError(t, err)
	require.Equal(t, "", v.Content)
	require.Equal(t, []string{"projects/arco"}, v.BackLinks)
}

func TestLinks_DerivedIndex(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", "[[b]] [[c]]")
	write(t, dir, "MEMORY.md", "[[a]] index — must be excluded")
	links, err := New(dir).Links()
	require.NoError(t, err)
	require.Equal(t, []string{"b", "c"}, links["a"])
	require.NotContains(t, links, "MEMORY", "the always-hot index is not a topic node")
}

func TestCleanTopic_NoTraversal(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	// a traversal attempt is anchored under Dir, never escaping it
	require.NoError(t, s.ApplyMemoryDiff(MemoryDiff{
		Op: "add", Topic: "../../etc/evil", Content: "x", Author: "user", DecidedBy: "long",
	}, time.Now))
	// nothing was written outside Dir
	_, err := os.Stat(filepath.Join(dir, "..", "..", "etc", "evil.md"))
	require.Error(t, err)
	// it landed as a safe slug under Dir
	require.FileExists(t, filepath.Join(dir, "etc", "evil.md"))
}

func TestApplyMemoryDiff_HumanOnly(t *testing.T) {
	s := New(t.TempDir())

	// brain author rejected
	require.ErrorIs(t, s.ApplyMemoryDiff(MemoryDiff{Op: "add", Topic: "x", Content: "c", Author: "brain", DecidedBy: "long"}, time.Now), ErrBrainAuthor)
	// empty/other author rejected (fail closed)
	require.ErrorIs(t, s.ApplyMemoryDiff(MemoryDiff{Op: "add", Topic: "x", Content: "c", Author: "", DecidedBy: "long"}, time.Now), ErrBrainAuthor)
	// no human decider rejected
	require.ErrorIs(t, s.ApplyMemoryDiff(MemoryDiff{Op: "add", Topic: "x", Content: "c", Author: "user"}, time.Now), ErrNoHuman)
	// bad op rejected
	require.ErrorIs(t, s.ApplyMemoryDiff(MemoryDiff{Op: "delete", Topic: "x", Content: "c", Author: "user", DecidedBy: "long"}, time.Now), ErrBadOp)
}

// Regression (opus P1): a symlinked component must not let a write escape Dir.
func TestApplyMemoryDiff_SymlinkEscapeBlocked(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "oss")))
	s := New(dir)
	err := s.ApplyMemoryDiff(MemoryDiff{Op: "add", Topic: "oss/x", Content: "escaped", Author: "user", DecidedBy: "long"}, time.Now)
	require.ErrorIs(t, err, ErrEscape)
	_, statErr := os.Stat(filepath.Join(outside, "x.md"))
	require.Error(t, statErr, "nothing may be written outside Dir via a symlink")
}

// Regression (opus P2): [[links]] inside code fences are documentation, not links.
func TestParseLinks_IgnoresCodeSpans(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "guide.md", "How to link:\n```\nuse [[b]] like this\n```\nand inline `[[c]]` too.\nReal: [[d]]")
	s := New(dir)
	v, _ := s.Read("guide")
	require.Equal(t, []string{"d"}, v.OutLinks, "fenced/inline [[..]] are not links")

	// so b/c get no phantom backlink from guide
	vb, _ := s.Read("b")
	require.Empty(t, vb.BackLinks)
}

// Regression (qwen #1 read-side): a symlinked *.md must not be followed by Read.
func TestRead_SymlinkFileNotFollowed(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("TOPSECRET"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "leak.md")))
	v, err := New(dir).Read("leak")
	require.NoError(t, err)
	require.Equal(t, "", v.Content, "a symlinked topic file must not be read into content")
}

// Regression (qwen #4): the always-hot identity files can't be clobbered via topics.
func TestApplyMemoryDiff_ReservedNames(t *testing.T) {
	s := New(t.TempDir())
	for _, name := range []string{"USER", "user", "MEMORY", "memory"} {
		require.ErrorIs(t, s.ApplyMemoryDiff(MemoryDiff{Op: "add", Topic: name, Content: "x", Author: "user", DecidedBy: "long"}, time.Now), ErrBadOp)
	}
}

func TestApplyMemoryDiff_RejectsFakeDecider(t *testing.T) {
	s := New(t.TempDir())
	require.ErrorIs(t, s.ApplyMemoryDiff(MemoryDiff{Op: "add", Topic: "x", Content: "c", Author: "user", DecidedBy: "brain"}, time.Now), ErrNoHuman)
	require.ErrorIs(t, s.ApplyMemoryDiff(MemoryDiff{Op: "add", Topic: "x", Content: "c", Author: "user", DecidedBy: "  "}, time.Now), ErrNoHuman)
}

func TestApplyMemoryDiff_AddUpdateExpire(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	require.NoError(t, s.ApplyMemoryDiff(MemoryDiff{Op: "add", Topic: "oss/x", Content: "v1", Author: "user", DecidedBy: "long"}, time.Now))
	v, _ := s.Read("oss/x")
	require.Equal(t, "v1", v.Content)

	require.NoError(t, s.ApplyMemoryDiff(MemoryDiff{Op: "update", Topic: "oss/x", Content: "v2", Author: "user", DecidedBy: "long"}, time.Now))
	v, _ = s.Read("oss/x")
	require.Equal(t, "v2", v.Content)

	require.NoError(t, s.ApplyMemoryDiff(MemoryDiff{Op: "expire", Topic: "oss/x", Author: "user", DecidedBy: "long"}, time.Now))
	v, _ = s.Read("oss/x")
	require.Equal(t, "", v.Content, "expired topic file is gone")

	// audit trail exists
	b, err := os.ReadFile(filepath.Join(dir, "revisions.jsonl"))
	require.NoError(t, err)
	require.Contains(t, string(b), `"op":"add"`)
	require.Contains(t, string(b), `"decided_by":"long"`)
}
