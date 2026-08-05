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
