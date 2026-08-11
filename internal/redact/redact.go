// Package redact scrubs secrets at WRITE time (build-guide-rev6 B4). The ledger
// is immutable, so a secret written verbatim can never be removed — scrubbing
// must happen BEFORE AppendEvent and BEFORE the brain prompt (the biggest exfil
// surface, going to a third-party LLM). Egress read-path scrubbing is a separate
// defense-in-depth layer.
//
// It is deterministic and versioned so byte-stable assembly + prompt-cache
// telemetry stay meaningful. Regex matching is best-effort; the at-rest
// guarantee is the OS-user + 0600 file perms (precondition 1).
package redact

import (
	"regexp"

	"github.com/dinhlongviolin1/arco/internal/core"
)

const version = "redact-v1"

// pattern is a named secret shape.
type pattern struct {
	name string
	re   *regexp.Regexp
}

// patterns are ordered; each match becomes [REDACTED:<name>]. Kept intentionally
// conservative (known token shapes + URL creds) to avoid mangling normal text.
var patterns = []pattern{
	{"github-pat", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`)},
	{"github-token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"anthropic-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`)},
	// OpenAI: legacy sk-… and current sk-proj-/sk-svcacct-/sk-admin-… (opus P2-2).
	{"openai-key", regexp.MustCompile(`sk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`)},
	{"aws-access-key", regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)},
	// URL embedded credentials: scheme://user:pass@host → keep host, drop creds.
	// userinfo classes allow ':' so a colon-bearing password is caught (opus P2-1);
	// the required ':' means git@host (SSH-style, no password) is NOT touched.
	{"url-credentials", regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^\s/@]+:[^\s/@]+@`)},
}

// Redactor implements core.Scrubber.
type Redactor struct{}

var _ core.Scrubber = Redactor{}

// New returns a Redactor.
func New() Redactor { return Redactor{} }

// Scrub replaces known secret shapes in s and returns the cleaned string plus
// the number of redactions made. Deterministic for a given s.
func (Redactor) Scrub(s string) (string, int) {
	n := 0
	out := s
	for _, p := range patterns {
		hits := len(p.re.FindAllStringIndex(out, -1))
		if hits == 0 {
			continue
		}
		n += hits
		if p.name == "url-credentials" {
			out = p.re.ReplaceAllString(out, "${1}[REDACTED:url-credentials]@") // keep scheme+host
		} else {
			out = p.re.ReplaceAllString(out, "[REDACTED:"+p.name+"]")
		}
	}
	return out, n
}

// Version identifies the pattern set (bump when patterns change).
func (Redactor) Version() string { return version }

// Noop is a Scrubber that changes nothing (for tests / when redaction is off).
type Noop struct{}

var _ core.Scrubber = Noop{}

func (Noop) Scrub(s string) (string, int) { return s, 0 }
func (Noop) Version() string              { return "noop" }
