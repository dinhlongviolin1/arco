package spawnenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsSecretVar(t *testing.T) {
	secret := []string{
		"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_TOKEN",
		"GITHUB_TOKEN", "GH_TOKEN", "GITLAB_TOKEN",
		"TELEGRAM_BOT_TOKEN", "SLACK_TOKEN", "DISCORD_TOKEN",
		"AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_SESSION_TOKEN",
		"GOOGLE_APPLICATION_CREDENTIALS", "GCP_SA_KEY",
		"ARCO_SOCKET", "ARCO_DB_PATH", "CLAVIS_PROFILE",
		"NPM_TOKEN", "DOCKER_PASSWORD",
		"SOME_SERVICE_TOKEN", "FOO_SECRET", "DB_PASSWORD", "X_PRIVATE_KEY", "Y_API_KEY",
		// DB passwords + credential-bearing URLs (opus review)
		"PGPASSWORD", "MYSQL_PWD", "SOME_PWD", "DATABASE_URL", "REDIS_URL",
		"MONGODB_URI", "SENTRY_DSN", "MY_DSN", "KUBECONFIG",
		// case-insensitive
		"github_token", "anthropic_api_key", "pgpassword",
	}
	for _, k := range secret {
		require.True(t, IsSecretVar(k), "%s should be treated as secret", k)
	}

	safe := []string{
		"PATH", "HOME", "USER", "SHELL", "LANG", "LC_ALL", "TERM", "TMPDIR", "PWD",
		"GIT_AUTHOR_NAME", "GIT_COMMITTER_EMAIL", "EDITOR", "XDG_RUNTIME_DIR",
		"SSH_AUTH_SOCK", "GOPATH", "FOO", "MY_CONFIG", "KEYBOARD_LAYOUT",
	}
	for _, k := range safe {
		require.False(t, IsSecretVar(k), "%s should be preserved", k)
	}
}

func TestScrub_StripsSecretsPreservesRestAndOrder(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-xxx",
		"HOME=/home/arco",
		"GITHUB_TOKEN=ghp_xxx",
		"GIT_AUTHOR_NAME=arco",
		"ARCO_SOCKET=/run/arco.sock",
		"LANG=en_US.UTF-8",
		"MY_SERVICE_TOKEN=abc",
	}
	got := Scrub(in)
	require.Equal(t, []string{
		"PATH=/usr/bin",
		"HOME=/home/arco",
		"GIT_AUTHOR_NAME=arco",
		"LANG=en_US.UTF-8",
	}, got, "secrets removed, benign vars preserved in original order")
}

// ScrubWorker additionally strips the operator's SSH agent / GPG keyring
// pointers so an untrusted worker can't push/sign/ssh as the operator — while
// plain Scrub (arco's own git subprocesses) keeps them for ssh:// clones.
func TestScrubWorker_StripsSSHAgentAndGPG(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"SSH_AUTH_SOCK=/tmp/ssh-XXXX/agent.123",
		"SSH_AGENT_PID=123",
		"GNUPGHOME=/home/op/.gnupg",
		"HOME=/home/arco",
	}
	// arco's own subprocesses keep the agent (can clone ssh:// repos).
	require.Contains(t, Scrub(in), "SSH_AUTH_SOCK=/tmp/ssh-XXXX/agent.123",
		"plain Scrub must keep SSH_AUTH_SOCK for arco's own ssh clones")
	// the worker launch env does not.
	got := ScrubWorker(in)
	require.Equal(t, []string{"PATH=/usr/bin", "HOME=/home/arco"}, got,
		"worker env drops SSH_AUTH_SOCK/SSH_AGENT_PID/GNUPGHOME, keeps benign vars in order")
}

func TestScrub_HandlesBareNameAndEmpty(t *testing.T) {
	require.Empty(t, Scrub(nil))
	// a malformed entry with no '=' is treated as a bare name and still filtered
	require.Equal(t, []string{"PATH=/x"}, Scrub([]string{"GITHUB_TOKEN", "PATH=/x"}))
}
