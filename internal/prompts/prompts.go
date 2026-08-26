// Package prompts holds arco's model-facing prompt TEXT as editable template
// files, separate from code — so the wording (the brain's decision instruction,
// the operator chat persona, the rollup directive) can be tuned in the codebase
// without threading string literals through Go. The files are embedded at build
// time, so the production binary is self-contained: to change wording you edit
// the .tmpl and rebuild.
//
// Code keeps the LOGIC (what data to gather, budget/trim, ordering, redaction);
// these files keep the WORDING. A template may use {{.Field}} placeholders for
// data the caller supplies.
package prompts

import (
	"bytes"
	"embed"
	"fmt"
	"strings"
	"text/template"
)

//go:embed defaults/*.tmpl
var defaultsFS embed.FS

// tmpl is the parsed embedded set. Set once at init and read-only thereafter, so
// Render needs no lock.
var tmpl *template.Template

func init() {
	t, err := template.New("prompts").ParseFS(defaultsFS, "defaults/*.tmpl")
	if err != nil {
		panic("prompts: embedded defaults failed to parse: " + err.Error())
	}
	tmpl = t
}

// Render executes the named template (e.g. "chat.tmpl") with data and returns
// the text. Unknown name is an error.
func Render(name string, data any) (string, error) {
	if tmpl.Lookup(name) == nil {
		return "", fmt.Errorf("prompts: no template %q", name)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
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
