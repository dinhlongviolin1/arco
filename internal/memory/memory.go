// Package memory is arco's 24a manual memory (build-guide-rev6 "links, not a
// graph"): whole-file USER.md + MEMORY.md index always-hot, per-topic files
// loaded JIT, cross-project [[wikilinks]] with a DERIVED backlink index. md files
// are the source of truth; there is no graph engine. Writes are HUMAN-only
// (author=brain is rejected) — the brain never mutates persistent memory.
package memory

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// wikilink matches [[topic]] or [[topic|rel]] — topic is a slug (letters,
// digits, _/-, and '/' for namespacing like oss/graphiti).
var wikilink = regexp.MustCompile(`\[\[([A-Za-z0-9._/-]+)(?:\|[^\]]*)?\]\]`)

// Store is a file-backed memory store rooted at Dir (e.g. ~/.arco/memory).
type Store struct{ Dir string }

// New returns a Store rooted at dir.
func New(dir string) *Store { return &Store{Dir: dir} }

func (s *Store) path(topic string) string {
	// topic is a slug; join safely (no traversal — see cleanTopic).
	return filepath.Join(s.Dir, filepath.FromSlash(cleanTopic(topic))+".md")
}

// cleanTopic strips any path traversal, keeping a safe relative slug.
func cleanTopic(topic string) string {
	t := strings.TrimSuffix(strings.TrimSpace(topic), ".md")
	t = filepath.ToSlash(filepath.Clean("/" + t)) // anchor at root, resolve ..
	return strings.TrimPrefix(t, "/")
}

// LoadUserMemory returns the always-hot USER.md and MEMORY.md contents (missing
// files return "").
func (s *Store) LoadUserMemory() (userMD, indexMD string) {
	userMD = s.readFile("USER.md")
	indexMD = s.readFile("MEMORY.md")
	return
}

func (s *Store) readFile(rel string) string {
	b, err := os.ReadFile(filepath.Join(s.Dir, rel))
	if err != nil {
		return ""
	}
	return string(b)
}

// TopicView is a topic file plus its one-hop links (build-guide: depth-1 only;
// multi-hop = the brain issuing another Read).
type TopicView struct {
	Topic     string
	Content   string
	OutLinks  []string // [[...]] found in this file
	BackLinks []string // topics whose files link to this one
}

// Read returns a topic file with its out-links and backlinks. Missing topic →
// empty Content but still-computed backlinks.
func (s *Store) Read(topic string) (TopicView, error) {
	topic = cleanTopic(topic)
	v := TopicView{Topic: topic}
	if b, err := os.ReadFile(s.path(topic)); err == nil {
		v.Content = string(b)
		v.OutLinks = parseLinks(v.Content)
	}
	back, err := s.backlinksTo(topic)
	if err != nil {
		return v, err
	}
	v.BackLinks = back
	return v, nil
}

// parseLinks extracts unique, sorted [[wikilink]] targets from content.
func parseLinks(content string) []string {
	set := map[string]bool{}
	for _, m := range wikilink.FindAllStringSubmatch(content, -1) {
		set[cleanTopic(m[1])] = true
	}
	return sortedKeys(set)
}

// backlinksTo scans every topic file for a [[topic]] link. O(files) — fine at
// the <10² topic scale the design targets; rebuild-from-files, never a cache.
func (s *Store) backlinksTo(topic string) ([]string, error) {
	set := map[string]bool{}
	err := s.walkTopics(func(name, content string) {
		if name == topic {
			return
		}
		for _, l := range parseLinks(content) {
			if l == topic {
				set[name] = true
			}
		}
	})
	return sortedKeys(set), err
}

// walkTopics calls fn(topicSlug, content) for every .md under Dir except the
// always-hot USER.md/MEMORY.md.
func (s *Store) walkTopics(fn func(name, content string)) error {
	return filepath.Walk(s.Dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		rel, rerr := filepath.Rel(s.Dir, p)
		if rerr != nil {
			return nil
		}
		if rel == "USER.md" || rel == "MEMORY.md" {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		fn(cleanTopic(rel), string(b))
		return nil
	})
}

// Links returns the full derived link index: topic → its out-link targets. This
// is the "derived memory_links" rebuilt from files (no write API, no cache).
func (s *Store) Links() (map[string][]string, error) {
	out := map[string][]string{}
	err := s.walkTopics(func(name, content string) {
		if l := parseLinks(content); len(l) > 0 {
			out[name] = l
		}
	})
	return out, err
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
