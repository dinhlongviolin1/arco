package ledger

import "time"

// nowRFC is a read-time timestamp (used only for grant-expiry comparison, which
// does not affect byte-stable assembly). Stored timestamps come from the
// Store's injected clock, not this.
func nowRFC() string { return time.Now().UTC().Format(time.RFC3339Nano) }
