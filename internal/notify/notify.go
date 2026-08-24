// Package notify pushes arco decision cards (escalations opened/answered/
// expired, worker lost/failed/verified) to the operator via shoutrrr service
// URLs (ntfy and friends). Unconfigured (no URLs) it degrades to a no-op
// sender; sends are best-effort and never panic.
package notify

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/containrrr/shoutrrr"
	"github.com/containrrr/shoutrrr/pkg/types"
)

// Level is the severity of a card. Ordered: info < warn < urgent.
type Level int

const (
	LevelInfo Level = iota
	LevelWarn
	LevelUrgent
)

// ParseLevel parses "info"|"warn"|"urgent"; anything else is an error.
func ParseLevel(s string) (Level, error) {
	switch s {
	case "info":
		return LevelInfo, nil
	case "warn":
		return LevelWarn, nil
	case "urgent":
		return LevelUrgent, nil
	}
	return LevelInfo, fmt.Errorf("notify: unknown level %q (want info|warn|urgent)", s)
}

// String renders the level as its config name.
func (l Level) String() string {
	switch l {
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelUrgent:
		return "urgent"
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// Card is one push notification.
//
// The first three fields are the whole message for the generic shoutrrr path
// (ntfy et al.). The trailing fields are OPTIONAL routing metadata the Telegram
// forum sender uses (topic per session, inline answer buttons); the generic
// sender ignores them entirely, so a Card is backend-agnostic.
type Card struct {
	Level Level
	Title string
	Body  string

	// SessionID routes the card to that session's forum topic (Telegram). "" =
	// the General topic / a flat chat.
	SessionID string
	// EscalationID, when set, marks this as an escalation card: an OPEN one (the
	// Telegram sender attaches answer buttons keyed to it), or — with Resolved —
	// a card that edits the open card in place, stripping its buttons.
	// EscalationKind ("question" | "confirm") picks which keyboard.
	EscalationID   string
	EscalationKind string
	// Resolved marks the "escalation answered/expired" card: the Telegram sender
	// edits the original escalation message (removing its now-stale buttons)
	// instead of posting a new one, so a second tap can't hit a dead card.
	Resolved bool
	// WorkerID is the worker the card concerns (used by the diff button / status).
	WorkerID string
}

// Sender delivers cards.
type Sender interface {
	Send(ctx context.Context, c Card) error
}

// New builds a Sender for the given shoutrrr URLs, filtered to min. No URLs
// yields a no-op sender (never errors, sends nothing). A URL parse failure is
// deferred to Send (New has no error return) so a misconfigured URL surfaces
// as a send error instead of a panic.
func New(urls []string, min Level) Sender {
	if len(urls) == 0 {
		return noopSender{}
	}
	r, err := shoutrrr.CreateSender(urls...)
	if err != nil {
		// Construction failure is deferred to Send (New has no error return); the
		// filter still wraps it so muted severities never surface the misconfig.
		return Filtered(errSender{err: fmt.Errorf("notify: invalid shoutrrr url: %w", err)}, min)
	}
	return Filtered(routerSender{send: r.Send}, min)
}

// Filtered wraps inner, dropping cards with Level < min and forwarding the
// rest unchanged.
func Filtered(inner Sender, min Level) Sender {
	return filteredSender{inner: inner, min: min}
}

type filteredSender struct {
	inner Sender
	min   Level
}

func (f filteredSender) Send(ctx context.Context, c Card) error {
	if c.Level < f.min {
		return nil
	}
	return f.inner.Send(ctx, c)
}

// noopSender accepts and drops every card.
type noopSender struct{}

func (noopSender) Send(context.Context, Card) error { return nil }

// errSender reports a construction failure on every send.
type errSender struct{ err error }

func (s errSender) Send(context.Context, Card) error { return s.err }

// routerSender adapts a shoutrrr router to Sender: the card body is the
// message and the card title rides in the "title" param (ntfy etc. honor it).
type routerSender struct {
	send func(message string, params *types.Params) []error
}

func (s routerSender) Send(ctx context.Context, c Card) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	params := types.Params{"title": c.Title}
	var failed []string
	for _, err := range s.send(c.Body, &params) {
		if err != nil {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("notify: send failed: %s", strings.Join(failed, "; "))
	}
	return nil
}

// Recorder is a race-safe fake Sender for tests: Send appends, Cards returns
// a copy. The zero value is usable (&Recorder{}).
type Recorder struct {
	mu    sync.Mutex
	cards []Card
}

// Send records the card, never errors.
func (r *Recorder) Send(_ context.Context, c Card) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cards = append(r.cards, c)
	return nil
}

// Cards returns a copy of the recorded cards.
func (r *Recorder) Cards() []Card {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Card(nil), r.cards...)
}

// EscalationCard is the input for rendering an escalation decision card.
type EscalationCard struct {
	EscalationID string // routing: attaches Telegram answer buttons keyed to it
	Kind         string // "question" | "confirm" — picks the button set
	SessionID    string // routing: the session's forum topic
	WorkerID     string
	TaskTail     string
	Question     string
	Draft        string
	Confidence   float64
	Rationale    string
}

// FormatEscalation renders an escalation as an urgent decision card, carrying
// the routing metadata the Telegram sender needs (escalation id/kind + session).
func FormatEscalation(c EscalationCard) Card {
	lines := make([]string, 0, 4)
	if c.TaskTail != "" {
		lines = append(lines, "task: "+c.TaskTail)
	}
	lines = append(lines, "question: "+c.Question)
	if c.Draft != "" {
		lines = append(lines, fmt.Sprintf("draft: %s (confidence %.2f)", c.Draft, c.Confidence))
	}
	if c.Rationale != "" {
		lines = append(lines, "rationale: "+c.Rationale)
	}
	return Card{
		Level:          LevelUrgent,
		Title:          "arco: decision needed — " + c.WorkerID,
		Body:           strings.Join(lines, "\n"),
		SessionID:      c.SessionID,
		EscalationID:   c.EscalationID,
		EscalationKind: c.Kind,
		WorkerID:       c.WorkerID,
	}
}
