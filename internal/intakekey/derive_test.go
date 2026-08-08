package intakekey

// GUIDELINE TESTS — rev7 T3.4 (HKDF per-worker intake keys).
//
// Pinned derivation (exact, golden-vectored below):
//   Derive(master, workerID) = hex( HKDF-SHA256(secret=master, salt=nil,
//                                   info="arco/intake/v1|"+workerID, L=32) )
// Go 1.24+ stdlib crypto/hkdf — no new dependencies.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDerive_GoldenVectors(t *testing.T) {
	// Recomputing these requires the exact salt/info layout above — any drift in
	// the derivation breaks every deployed worker key, so it is pinned hard.
	require.Equal(t,
		"64a8c13413d0da187c79ea6827ecb94bd2d5209f5e114680739ca44afb6f41e1",
		Derive("m", "w1"))
	require.Equal(t,
		"dad0a53aa96b7fe0968845a939b33599f7ef2d38b64d604a0b8f1b5d26452011",
		Derive("topsecret", "wA"))
}

func TestDerive_Properties(t *testing.T) {
	k1 := Derive("master", "worker-1")
	require.Len(t, k1, 64, "32-byte key, lowercase hex")
	require.Equal(t, k1, Derive("master", "worker-1"), "deterministic")
	require.NotEqual(t, k1, Derive("master", "worker-2"), "distinct per worker")
	require.NotEqual(t, k1, Derive("master2", "worker-1"), "distinct per master (rotation)")
	require.NotEqual(t, k1, "master", "derived key never equals the master")
}

func TestDerive_EmptyInputsAreInert(t *testing.T) {
	// No master configured (unsigned local-socket mode) or no worker identity →
	// no key material. Callers treat "" as feature-off, never as a valid key.
	require.Empty(t, Derive("", "worker-1"))
	require.Empty(t, Derive("master", ""))
}
