//go:build linux

package api

import (
	"context"
	"net"

	"golang.org/x/sys/unix"
)

// peerCredKey is an unexported context key so only this package can read the
// peer UID the kernel vouched for (a request-scoped, unforgable identity on a
// unix socket — unlike the HMAC secret, which any same-box process may hold).
type peerCredKey struct{}

// PeerCredConnContext is the http.Server ConnContext hook: on a unix conn it
// resolves the connecting peer's UID via SO_PEERCRED and stashes it in the
// request context, so the intake can bind worker events to the worker's
// spawn-time UID (rev7/T1.6). Non-unix conns (TCP/httptest) and resolution
// errors return ctx unchanged → those transports keep today's behavior.
func PeerCredConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	uid, ok := peerUIDOfConn(uc)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, peerCredKey{}, uid)
}

// peerUIDOfConn reads the peer's ucred from the conn's fd. Errors fail open
// (no UID recorded → ungated), matching the transport's pre-peercred behavior
// — a getsockopt failure must not take down intake.
func peerUIDOfConn(uc *net.UnixConn) (int, bool) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var uid int
	var got bool
	if err := raw.Control(func(fd uintptr) {
		ucred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			return
		}
		uid, got = int(ucred.Uid), true
	}); err != nil {
		return 0, false
	}
	return uid, got
}

// peerUIDFrom reports the peer UID PeerCredConnContext recorded on the conn
// behind this request, if any (false for TCP/httptest or a resolution error).
func peerUIDFrom(ctx context.Context) (int, bool) {
	uid, ok := ctx.Value(peerCredKey{}).(int)
	return uid, ok
}
