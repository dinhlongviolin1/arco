// Package spawnenv implements the scrubbed spawn environment (build-guide-rev6
// PASS-3, security precondition P1): arco must never hand its own high-blast
// credentials — provider API keys, GitHub/GitLab tokens, Telegram/Slack tokens,
// cloud creds, its own ARCO_*/CLAVIS_* config — down to a child process. A
// worker (or any helper subprocess) that inherited arco's ANTHROPIC_API_KEY or
// GITHUB_TOKEN could act with far more authority than its capability tree grants.
//
// Scrub is a DENYLIST over the environment: everything is preserved (PATH, HOME,
// USER, LANG, GIT_* config, …) EXCEPT variables whose NAME matches a known
// secret-bearing prefix or a generic secret suffix. Preserving-by-default keeps
// git/herdr/etc. working; the named + suffix denylist catches the credentials.
package spawnenv

import (
	"strings"
)

// secretPrefixes: any env var whose name starts with one of these (case-
// insensitive) is a credential/config surface and is stripped.
var secretPrefixes = []string{
	"ANTHROPIC_", "OPENAI_", "CLAUDE_",
	"GITHUB_", "GH_", "GITLAB_", "GL_",
	"TELEGRAM", "TG_", "SLACK_", "DISCORD_",
	"AWS_", "AZURE_", "GCP_", "GOOGLE_",
	"ARCO_", "CLAVIS_", // arco's own config (socket/db paths, tokens, profiles)
	"NPM_TOKEN", "PYPI_", "DOCKER_",
}

// secretSuffixes: any env var whose name ends with one of these (case-
// insensitive) is treated as a secret regardless of prefix.
var secretSuffixes = []string{
	"_TOKEN", "_SECRET", "_PASSWORD", "_PASSWD",
	"_APIKEY", "_API_KEY", "_ACCESS_KEY", "_SECRET_KEY",
	"_PRIVATE_KEY", "_CREDENTIALS",
}

// IsSecretVar reports whether an environment variable NAME should be stripped.
func IsSecretVar(name string) bool {
	up := strings.ToUpper(name)
	for _, p := range secretPrefixes {
		if strings.HasPrefix(up, p) {
			return true
		}
	}
	for _, s := range secretSuffixes {
		if strings.HasSuffix(up, s) {
			return true
		}
	}
	return false
}

// Scrub returns environ (each entry "KEY=VALUE") with every secret-bearing
// variable removed, preserving the order and values of the rest. Deterministic;
// a malformed entry without '=' is treated as a bare name and checked too.
func Scrub(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if IsSecretVar(name) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
