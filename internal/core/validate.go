package core

// LooksLikeRev reports whether s is a plausible git commit id (hex, 7..64
// chars) — enough to reject option-injection (`--upload-pack=…`), path
// traversal (`../`), and shell-metacharacter shapes without a full
// git check-ref-format. It is the single gate for any commit id that reaches a
// git or `gh` command line, whether that id came from intake (a worker's
// observed_head, untrusted) or an operator. Callers reject or drop a value that
// fails; they never pass an unvalidated worker-supplied ref to a subprocess.
func LooksLikeRev(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
