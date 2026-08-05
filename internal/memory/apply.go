package memory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Errors returned by ApplyMemoryDiff.
var (
	// ErrBrainAuthor rejects a brain-authored memory write (24a is manual-only;
	// build-guide B13 — no author=brain auto-apply, ever).
	ErrBrainAuthor = errors.New("memory: brain-authored writes are not allowed (manual memory)")
	// ErrNoHuman rejects a write with no human decider.
	ErrNoHuman = errors.New("memory: a human decided_by is required")
	// ErrBadOp rejects an unknown op.
	ErrBadOp = errors.New("memory: op must be add|update|expire")
)

// MemoryDiff is a proposed change to a topic file. Author must be user|external
// (never brain); DecidedBy must be a human. Content is ignored for expire.
type MemoryDiff struct {
	Op        string // add | update | expire
	Topic     string
	Content   string
	Author    string // user | external  (NEVER brain)
	DecidedBy string // human identity (required)
}

// revision is the audit record appended to revisions.jsonl on every write.
type revision struct {
	Op        string `json:"op"`
	Topic     string `json:"topic"`
	Author    string `json:"author"`
	DecidedBy string `json:"decided_by"`
	At        string `json:"at"`
}

// ApplyMemoryDiff applies a HUMAN-approved memory change. It refuses a
// brain-authored write and a write with no human decider, then writes/removes
// the topic file and appends an audit revision. `now` is injected for
// determinism in tests.
func (s *Store) ApplyMemoryDiff(d MemoryDiff, now func() time.Time) error {
	if d.Author == "brain" {
		return ErrBrainAuthor
	}
	if d.Author != "user" && d.Author != "external" {
		return ErrBrainAuthor // fail closed on any non-human author, incl. ""
	}
	if d.DecidedBy == "" {
		return ErrNoHuman
	}
	topic := cleanTopic(d.Topic)
	if topic == "" {
		return ErrBadOp
	}
	p := s.path(topic)
	switch d.Op {
	case "add", "update":
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(d.Content), 0o600); err != nil {
			return err
		}
	case "expire":
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	default:
		return ErrBadOp
	}
	return s.appendRevision(revision{
		Op: d.Op, Topic: topic, Author: d.Author, DecidedBy: d.DecidedBy,
		At: nowStr(now),
	})
}

func (s *Store) appendRevision(r revision) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, "revisions.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(r)
	_, err = f.Write(append(b, '\n'))
	return err
}

func nowStr(now func() time.Time) string {
	t := time.Unix(0, 0).UTC()
	if now != nil {
		t = now().UTC()
	}
	return t.Format(time.RFC3339Nano)
}
