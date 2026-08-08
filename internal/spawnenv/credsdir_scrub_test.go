// Guideline test for T2.3's scrub addendum: arco itself may run under systemd
// with $CREDENTIALS_DIRECTORY pointing at ITS OWN LoadCredential= files (intake
// secret, telegram token). That pointer must never leak into a worker's
// inherited environment — the spawn path injects the worker's OWN
// CREDENTIALS_DIRECTORY afterwards, and two pointers (or the wrong one) would
// hand the agent a path to arco's credentials.
package spawnenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScrub_DropsArcoOwnCredentialsDirectory(t *testing.T) {
	require.True(t, IsSecretVar("CREDENTIALS_DIRECTORY"))
	require.True(t, IsSecretVar("credentials_directory"), "case-insensitive like the rest of the denylist")

	out := Scrub([]string{
		"PATH=/usr/bin",
		"CREDENTIALS_DIRECTORY=/run/credentials/arco.service",
		"HOME=/home/op",
	})
	require.Equal(t, []string{"PATH=/usr/bin", "HOME=/home/op"}, out)
}
