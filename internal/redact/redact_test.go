package redact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScrub_GoldenCorpus(t *testing.T) {
	r := New()
	cases := []struct {
		name, in, mustNotContain string
	}{
		{"github-pat", "token github_pat_11ABCDEFG0abcdefghijklmnop qrstuv end", "github_pat_11ABCDEFG0abcdefghijklmnop"},
		{"github-classic", "use ghp_abcdefghijklmnopqrstuvwxyz0123456789 now", "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"anthropic", "ANTHROPIC=sk-ant-api03-abcdefghijklmnop1234567890 x", "sk-ant-api03-abcdefghijklmnop1234567890"},
		{"openai", "key sk-abcdefghijklmnopqrstuvwxyz012345 done", "sk-abcdefghijklmnopqrstuvwxyz012345"},
		{"aws", "AKIAIOSFODNN7EXAMPLE creds", "AKIAIOSFODNN7EXAMPLE"},
		{"openai-proj", "key sk-proj-abcdefghijklmnopqrstuvwxyz1234567890 done", "sk-proj-abcdefghijklmnopqrstuvwxyz1234567890"},
		// Modern OpenAI key SHAPE: base64url body with `_` and `-` — the shapes
		// the old `[A-Za-z0-9]{20,}\b` pattern silently failed on (rev20 #19).
		// Bodies are obviously-synthetic (no real project marker) so this fixture
		// isn't itself flagged as a live key, while still exercising the class.
		{"openai-proj-underscore-dash", "OPENAI=sk-proj-AAAA1111_bbbb2222-CCCC3333dddd4444EEEE end", "sk-proj-AAAA1111_bbbb2222-CCCC3333dddd4444EEEE"},
		{"openai-svcacct-underscore-dash", "k sk-svcacct-ZZZZ0000_yyyy1111-XXXX2222wwww3333 x", "sk-svcacct-ZZZZ0000_yyyy1111-XXXX2222wwww3333"},
		{"url-creds", "clone https://alice:s3cr3tpw@github.com/x/y.git", "s3cr3tpw"},
		{"url-creds-colon-pass", "https://alice:s3:cr3t@github.com/x/y.git", "s3:cr3t"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, n := r.Scrub(c.in)
			require.Greater(t, n, 0, "expected a redaction")
			require.NotContains(t, out, c.mustNotContain, "secret survived scrubbing")
			require.Contains(t, out, "[REDACTED:", "a redaction marker is present")
		})
	}
}

func TestScrub_UrlCredsKeepsHost(t *testing.T) {
	out, n := New().Scrub("git remote add o https://u:p@example.com/r.git")
	require.Equal(t, 1, n)
	require.Contains(t, out, "example.com/r.git", "host is preserved")
	require.NotContains(t, out, "u:p@")
}

func TestScrub_LeavesNormalTextUntouched(t *testing.T) {
	in := "Refactored the parser; see PR #42. cost is not tracked. path=/home/x/a.go"
	out, n := New().Scrub(in)
	require.Equal(t, 0, n)
	require.Equal(t, in, out)
}

// No false positives on benign shapes: an epoch:hash control value (telegram
// pattern removed, P2-3) and an SSH-style git@host URL (no password).
func TestScrub_NoFalsePositives(t *testing.T) {
	for _, in := range []string{
		`{"deadline":"1700000000:abcdefghijklmnopqrstuvwxyz01234"}`,
		"clone ssh://git@github.com/x/y.git",
		"remote git@github.com:org/repo.git",
	} {
		out, n := New().Scrub(in)
		require.Equal(t, 0, n, "benign input redacted: %q", in)
		require.Equal(t, in, out)
	}
}

func TestScrub_Deterministic(t *testing.T) {
	r := New()
	in := "ghp_abcdefghijklmnopqrstuvwxyz0123456789 and sk-ant-api03-zzzzzzzzzzzzzzzzzzzz9"
	a, na := r.Scrub(in)
	b, nb := r.Scrub(in)
	require.Equal(t, a, b)
	require.Equal(t, na, nb)
}

func TestScrub_PrivateKeyBlock(t *testing.T) {
	in := "cfg\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAA\n-----END OPENSSH PRIVATE KEY-----\ntail"
	out, n := New().Scrub(in)
	require.Equal(t, 1, n)
	require.NotContains(t, out, "b3BlbnNzaC1rZXktdjEAAAA")
	require.True(t, strings.HasPrefix(out, "cfg\n"))
	require.True(t, strings.HasSuffix(out, "\ntail"))
}
