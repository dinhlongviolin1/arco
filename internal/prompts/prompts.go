// Package prompts holds arco's model-facing prompt TEXT as editable template
// files, separate from code — so the wording (the brain's decision instruction,
// the operator chat persona, the rollup directive) can be tuned without touching
// or recompiling Go. Defaults are embedded (the binary is self-contained); an
// operator can override any of them by dropping a same-named file in the prompts
// override dir (see Load), picked up on the next daemon start.
//
// Code keeps the LOGIC (what data to gather, budget/trim, ordering, redaction);
// these files keep the WORDING. A template may use {{.Field}} placeholders for
// data the caller supplies.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
)

//go:embed defaults/*.tmpl
var defaultsFS embed.FS

var (
	mu     sync.RWMutex
	active *template.Template // parsed set: embedded defaults, optionally overlaid by Load
)

func init() {
	t, err := parseEmbedded()
	if err != nil {
		panic("prompts: embedded defaults failed to parse: " + err.Error())
	}
	active = t
}

func parseEmbedded() (*template.Template, error) {
	return template.New("prompts").ParseFS(defaultsFS, "defaults/*.tmpl")
}

// Load re-parses the embedded defaults and overlays any *.tmpl in dir (each
// overriding the embedded default of the same base name), making prompt wording
// editable at runtime without a recompile. An empty dir, or a dir that doesn't
// exist, is fine — defaults stand. A malformed override file is an error (fail
// loud rather than silently ignore a broken edit).
func Load(dir string) error {
	t, err := parseEmbedded()
	if err != nil {
		return err
	}
	if dir != "" {
		entries, derr := os.ReadDir(dir)
		if derr != nil && !os.IsNotExist(derr) {
			return fmt.Errorf("prompts: read override dir %q: %w", dir, derr)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmpl") {
				continue
			}
			b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				return fmt.Errorf("prompts: read override %q: %w", e.Name(), rerr)
			}
			if _, perr := t.New(e.Name()).Parse(string(b)); perr != nil {
				return fmt.Errorf("prompts: parse override %q: %w", e.Name(), perr)
			}
		}
	}
	mu.Lock()
	active = t
	mu.Unlock()
	return nil
}

// Render executes the named template (e.g. "chat.tmpl") with data and returns
// the text. Unknown name is an error.
func Render(name string, data any) (string, error) {
	mu.RLock()
	t := active
	mu.RUnlock()
	if t.Lookup(name) == nil {
		return "", fmt.Errorf("prompts: no template %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return "", fmt.Errorf("prompts: render %q: %w", name, err)
	}
	return buf.String(), nil
}

// MustText renders a static (no-placeholder) template with no data. It panics on
// a missing/broken template — used for prompt trailers captured into budget math,
// where a missing embedded default is a build-time bug, not a runtime condition.
func MustText(name string) string {
	s, err := Render(name, nil)
	if err != nil {
		panic("prompts: " + err.Error())
	}
	return strings.TrimRight(s, "\n") // trailer templates carry a trailing newline for file hygiene; drop it
}
