// Package admission is the pure, dependency-free lease-admission decision for a
// provider pool (build-guide-rev6 PASS-3): given a pool's config + the current
// active-lease and start-window counts, decide whether one more worker may be
// admitted. It caps rate-limit / concurrency ONLY — never cost (§A #1). Like
// fusion, it holds no state and touches no storage, so the ledger can call it
// inside the single-writer tx after reading the counts atomically.
package admission

import (
	"time"

	"github.com/dinhlongviolin1/arco/internal/core"
)

// Rejection reasons (machine-readable; surfaced via core.LeaseRejection.Reason).
const (
	ReasonDisabled   = "disabled"
	ReasonCooldown   = "cooldown"
	ReasonAtCapacity = "at_capacity"
	ReasonStartRate  = "start_rate"
)

// Decision is the outcome of an admission check. OK=true means admit; otherwise
// Reason is one of the Reason* constants above.
type Decision struct {
	OK     bool
	Reason string
}

// Admit decides whether one more lease may be granted against p, given the
// number of currently-active (un-released) leases and the number acquired within
// the last core.StartRateWindow. `now` is the injected clock reading; a cooldown
// whose CooldownUntil has passed is treated as expired (admit-eligible) so the
// ledger can lazily clear it. Order matters: disabled and an unexpired cooldown
// are hard stops checked before the numeric caps.
func Admit(p core.ProviderPool, activeCount, startsInWindow int, now time.Time) Decision {
	if p.State == core.PoolDisabled {
		return Decision{Reason: ReasonDisabled}
	}
	if p.State == core.PoolCooldown && !cooldownExpired(p.CooldownUntil, now) {
		return Decision{Reason: ReasonCooldown}
	}
	if activeCount >= p.MaxActive {
		return Decision{Reason: ReasonAtCapacity}
	}
	if startsInWindow >= p.MaxStartsPerMin {
		return Decision{Reason: ReasonStartRate}
	}
	return Decision{OK: true}
}

// cooldownExpired reports whether a cooldown deadline has passed. An empty or
// unparseable deadline is treated as EXPIRED (fail-open on cooldown only —
// disabled is still a hard stop; a malformed timestamp shouldn't wedge a pool).
func cooldownExpired(until string, now time.Time) bool {
	if until == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, until)
	if err != nil {
		return true
	}
	return !now.Before(t)
}
