package schedule

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseInterval(t *testing.T) {
	base := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"30m": 30 * time.Minute, "every 30m": 30 * time.Minute,
		"2h": 2 * time.Hour, "1d": 24 * time.Hour, "1w": 7 * 24 * time.Hour,
	}
	for spec, want := range cases {
		sp, err := Parse(spec)
		require.NoError(t, err, spec)
		require.True(t, sp.IsInterval(), spec)
		require.Equal(t, base.Add(want), sp.Next(base), spec)
	}
}

func TestParseIntervalTooShort(t *testing.T) {
	_, err := Parse("30s")
	require.Error(t, err, "sub-minute intervals are rejected")
}

func TestParseCron(t *testing.T) {
	sp, err := Parse("0 8 * * *") // daily at 08:00
	require.NoError(t, err)
	require.False(t, sp.IsInterval())
	// from 07:30 → next is 08:00 same day
	from := time.Date(2026, 1, 2, 7, 30, 0, 0, time.UTC)
	require.Equal(t, time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC), sp.Next(from))
	// from 08:30 → next is 08:00 next day
	from = time.Date(2026, 1, 2, 8, 30, 0, 0, time.UTC)
	require.Equal(t, time.Date(2026, 1, 3, 8, 0, 0, 0, time.UTC), sp.Next(from))
}

func TestParseInvalid(t *testing.T) {
	for _, bad := range []string{"", "banana", "0 8 * *", "every day"} {
		_, err := Parse(bad)
		require.Error(t, err, bad)
	}
}

func TestCanonical(t *testing.T) {
	sp, _ := Parse("every 2h")
	require.Equal(t, "2h", sp.Canonical(), "the 'every ' prefix is normalized away")
}
