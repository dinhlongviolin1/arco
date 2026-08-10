//go:build !linux

package api

import (
	"context"
	"net"
)

// Non-Linux stub: SO_PEERCRED is a Linux ucred API, so peer-UID intake
// binding is NOT enforced here (peerUIDFrom never reports an identity and
// server.go skips the UID check). The daemon targets Linux (§5) — non-Linux
// binaries exist for the CLI and dev use; do not run a production daemon on
// them.
func PeerCredConnContext(ctx context.Context, _ net.Conn) context.Context { return ctx }

func peerUIDFrom(context.Context) (int, bool) { return 0, false }
