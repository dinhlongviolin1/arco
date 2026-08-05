// Package brain is the short-lived LLM adapter: it assembles a prompt, invokes a
// model via clavis for ONE bounded decision, and parses a typed StepResult. It
// holds no long-lived state (build-guide Global Constraints).
package brain

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// ValidKinds is the frozen StepResult.Kind set; the reconciler has a branch for
// each and maps anything else to an error event (never a silent drop).
var ValidKinds = map[string]bool{
	"run_again": true, "dispatch": true, "handoff": true,
	"final_output": true, "question": true, "confirm": true,
}

// ErrMalformed indicates the model output could not be parsed into a valid StepResult.
var ErrMalformed = errors.New("brain: malformed StepResult")

// ParseStep extracts a StepResult from model output. It accepts a ```json fenced
// block or a bare JSON object (taking the first balanced {...}); an unknown Kind
// or unparseable body yields ErrMalformed.
func ParseStep(raw string) (core.StepResult, error) {
	body, ok := extractJSON(raw)
	if !ok {
		return core.StepResult{}, ErrMalformed
	}
	var s core.StepResult
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		return core.StepResult{}, ErrMalformed
	}
	if !ValidKinds[s.Kind] {
		return core.StepResult{}, ErrMalformed
	}
	return s, nil
}

// extractJSON returns the JSON object from a fenced block or the first balanced
// top-level {...} in raw.
func extractJSON(raw string) (string, bool) {
	if i := strings.Index(raw, "```json"); i >= 0 {
		rest := raw[i+len("```json"):]
		if j := strings.Index(rest, "```"); j >= 0 {
			return strings.TrimSpace(rest[:j]), true
		}
	}
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(raw); i++ {
		c := raw[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// skip
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return raw[start : i+1], true
			}
		}
	}
	return "", false
}
