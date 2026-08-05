package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/core"
)

func pool(max, starts int, state core.PoolState, cooldown string) core.ProviderPool {
	return core.ProviderPool{MaxActive: max, MaxStartsPerMin: starts, State: state, CooldownUntil: cooldown}
}

func TestAdmit_Table(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute).Format(time.RFC3339Nano)
	future := now.Add(time.Minute).Format(time.RFC3339Nano)

	cases := []struct {
		name           string
		p              core.ProviderPool
		active, starts int
		wantOK         bool
		wantReason     string
	}{
		{"room to spare", pool(35, 10, core.PoolOK, ""), 10, 3, true, ""},
		{"at capacity", pool(35, 10, core.PoolOK, ""), 35, 0, false, ReasonAtCapacity},
		{"over capacity", pool(35, 10, core.PoolOK, ""), 36, 0, false, ReasonAtCapacity},
		{"start rate hit", pool(35, 10, core.PoolOK, ""), 1, 10, false, ReasonStartRate},
		{"disabled beats capacity room", pool(35, 10, core.PoolDisabled, ""), 0, 0, false, ReasonDisabled},
		{"unexpired cooldown blocks", pool(35, 10, core.PoolCooldown, future), 0, 0, false, ReasonCooldown},
		{"expired cooldown admits", pool(35, 10, core.PoolCooldown, past), 0, 0, true, ""},
		{"empty cooldown ts treated expired", pool(35, 10, core.PoolCooldown, ""), 0, 0, true, ""},
		{"malformed cooldown ts treated expired", pool(35, 10, core.PoolCooldown, "not-a-time"), 0, 0, true, ""},
		{"disabled outranks unexpired cooldown", pool(35, 10, core.PoolDisabled, future), 0, 0, false, ReasonDisabled},
		{"capacity checked before start rate", pool(1, 1, core.PoolOK, ""), 1, 5, false, ReasonAtCapacity},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := Admit(c.p, c.active, c.starts, now)
			require.Equal(t, c.wantOK, d.OK)
			require.Equal(t, c.wantReason, d.Reason)
		})
	}
}

// The cooldown boundary is inclusive of "now" (a deadline exactly at now is
// expired), so a pool never gets wedged one tick past its cooldown.
func TestAdmit_CooldownBoundaryInclusive(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	atNow := pool(35, 10, core.PoolCooldown, now.Format(time.RFC3339Nano))
	require.True(t, Admit(atNow, 0, 0, now).OK, "deadline == now must be treated as expired")

	justAfter := pool(35, 10, core.PoolCooldown, now.Add(time.Nanosecond).Format(time.RFC3339Nano))
	require.False(t, Admit(justAfter, 0, 0, now).OK, "deadline just after now still cools down")
}
