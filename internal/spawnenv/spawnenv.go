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
	"_TOKEN", "_SECRET", "_PASSWORD", "_PASSWD", "_PWD",
	"_APIKEY", "_API_KEY", "_ACCESS_KEY", "_SECRET_KEY",
	"_PRIVATE_KEY", "_CREDENTIALS", "_DSN",
	// Shapes a compromised worker's argv exposure would otherwise leak (the
	// launch passes the scrubbed env to herdr as world-readable `--env` argv):
	// a bare `_KEY`, plus session/cookie/short-secret conventions.
	"_KEY", "_SESSION", "_COOKIE", "_SK",
}

// secretNames: exact names (no prefix/suffix shape) that carry credentials —
// notably DB passwords and connection URLs that embed user:pass@host (opus
// review). Matched case-insensitively.
var secretNames = map[string]bool{
	// Bare (un-suffixed) secret names the "_TOKEN"/"_SECRET"/… suffix shapes miss —
	// a worker inheriting a plain `TOKEN=…`/`PASSWORD=…` would otherwise get it
	// (review LOW-4). Not bare "KEY" (too generic → over-strips benign vars).
	"TOKEN": true, "PASSWORD": true, "PASSWD": true, "SECRET": true,
	"APIKEY": true, "API_KEY": true, "ACCESS_KEY": true, "SECRET_KEY": true, "PRIVATE_KEY": true,
	"PGPASSWORD": true, "MYSQL_PWD": true,
	"DATABASE_URL": true, "REDIS_URL": true, "MONGODB_URI": true, "MONGO_URL": true,
	"AMQP_URL": true, "CELERY_BROKER_URL": true, "SENTRY_DSN": true, "KUBECONFIG": true,
	// arco's OWN systemd-creds pointer (LoadCredential= files: intake secret,
	// telegram token). A worker must never inherit a path to arco's credentials;
	// the spawn path injects the worker's own CREDENTIALS_DIRECTORY after the scrub.
	"CREDENTIALS_DIRECTORY": true,
}

// IsSecretVar reports whether an environment variable NAME should be stripped.
func IsSecretVar(name string) bool {
	up := strings.ToUpper(name)
	if secretNames[up] {
		return true
	}
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
func Scrub(environ []string) []string { return scrub(environ, false) }

// workerOnlyStrip are variables kept for arco's OWN subprocesses (so git can
// clone an ssh:// repo / a gpg-signed op works) but stripped from a WORKER's
// launch env: an untrusted worker inheriting the operator's SSH agent or GPG
// keyring could push/sign/ssh anywhere AS the operator, bypassing the whole
// per-worker scoped-credential model (rev20 review #18/#3).
var workerOnlyStrip = map[string]bool{
	"SSH_AUTH_SOCK": true, "SSH_AGENT_PID": true,
	"GNUPGHOME": true, "GPG_AGENT_INFO": true,
}

// ScrubWorker is Scrub plus the worker-only strips above. Use it for the
// environment handed to a launched worker agent (spawn), NOT for arco's own
// git/ssh subprocesses (which legitimately need the agent to reach ssh:// repos).
func ScrubWorker(environ []string) []string { return scrub(environ, true) }

func scrub(environ []string, worker bool) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if IsSecretVar(name) || (worker && workerOnlyStrip[strings.ToUpper(name)]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
