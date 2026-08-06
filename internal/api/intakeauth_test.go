package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dinhlongviolin1/arco/internal/ledger"
	"github.com/dinhlongviolin1/arco/internal/reconcile"
	"github.com/dinhlongviolin1/arco/internal/vm"
)

func TestVerifyIntakeSig(t *testing.T) {
	body := []byte(`{"worker_ref":"w"}`)
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	require.True(t, verifyIntakeSig("k", body, good))
	require.False(t, verifyIntakeSig("k", body, ""), "missing header")
	require.False(t, verifyIntakeSig("k", body, "sha256=deadbeef"), "wrong digest")
	require.False(t, verifyIntakeSig("k", body, "md5=abc"), "wrong algo prefix")
	require.False(t, verifyIntakeSig("k", body, good[:len(good)-2]+"zz"), "non-hex")
	require.False(t, verifyIntakeSig("WRONG", body, good), "wrong secret")
	require.False(t, verifyIntakeSig("k", append(body, '!'), good), "tampered body")
}

func signedAPI(t *testing.T, secret string) *httptest.Server {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "sig.db"))
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	srv := New(s, reconcile.New(s, vm.NewFake()))
	srv.SetIntakeSecret(secret)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func rawPost(t *testing.T, ts *httptest.Server, path string, body []byte, sig string) int {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Arco-Signature", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	return resp.StatusCode
}

func TestAPI_SignedIntakeRequired(t *testing.T) {
	ts := signedAPI(t, "topsecret")
	var d DispatchResp
	require.Equal(t, http.StatusOK, post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d))

	body, _ := json.Marshal(EventReq{WorkerRef: d.WorkerID, HerdrState: "idle", Alive: true})
	mac := hmac.New(sha256.New, []byte("topsecret"))
	mac.Write(body)
	goodSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	require.Equal(t, http.StatusUnauthorized, rawPost(t, ts, "/v1/events", body, ""), "unsigned rejected")
	require.Equal(t, http.StatusUnauthorized, rawPost(t, ts, "/v1/events", body, "sha256=00"), "bad sig rejected")
	require.Equal(t, http.StatusOK, rawPost(t, ts, "/v1/events", body, goodSig), "valid sig accepted")
}

func TestAPI_IntakeBodySizeLimited(t *testing.T) {
	ts := signedAPI(t, "") // no auth, but the size cap still applies
	huge := append([]byte(`{"worker_ref":"w","pad":"`), bytes.Repeat([]byte("A"), 2<<20)...)
	huge = append(huge, []byte(`"}`)...)
	require.Equal(t, http.StatusRequestEntityTooLarge, rawPost(t, ts, "/v1/events", huge, ""))
}

// Unsigned intake still works when no secret is configured (local unix-socket trust).
func TestAPI_UnsignedIntakeAllowedWithoutSecret(t *testing.T) {
	ts := signedAPI(t, "")
	var d DispatchResp
	post(t, ts, "/v1/dispatch", DispatchReq{Task: "x", New: true}, &d)
	body, _ := json.Marshal(EventReq{WorkerRef: d.WorkerID, HerdrState: "idle", Alive: true})
	require.Equal(t, http.StatusOK, rawPost(t, ts, "/v1/events", body, ""))
}
