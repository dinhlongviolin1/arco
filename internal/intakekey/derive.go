// Package intakekey derives per-worker intake signing keys from the intake
// master secret (rev7 T3.4). Each worker signs POST /v1/events with its OWN
// derived key — the master never rides the wire and never reaches a worker —
// so one compromised worker key cannot forge events for the rest of the fleet,
// and rotating the master rotates every derived key at once.
package intakekey

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
)

// infoPrefix domain-separates the derivation: a derived key is only ever valid
// for arco intake (versioned) and only for the worker bound into the info.
// Pinned by golden vectors in derive_test.go — changing it breaks every
// deployed worker key.
const infoPrefix = "arco/intake/v1|"

// Derive returns hex(HKDF-SHA256(secret=master, salt=nil,
// info="arco/intake/v1|"+workerID, L=32)), lowercase hex. Empty master
// (unsigned mode) or empty workerID (no identity) yields "" — callers treat
// "" as feature-off, never as a valid key.
func Derive(master, workerID string) string {
	if master == "" || workerID == "" {
		return ""
	}
	key, err := hkdf.Key(sha256.New, []byte(master), nil, infoPrefix+workerID, 32)
	if err != nil {
		return "" // unreachable: L=32 is far under the SHA-256 expand limit
	}
	return hex.EncodeToString(key)
}
