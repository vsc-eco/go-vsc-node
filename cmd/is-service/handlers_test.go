package main

import (
	"context"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	signer := NewAddressSignerHMAC([]byte("test-only-secret"))
	srv, err := NewServer(ServerConfig{
		PrimaryPubKeyHex: parityPrimaryPubKey,
		BackupPubKeyHex:  parityBackupPubKey,
		Network:          "testnet",
		ChainID:          "vsc-testnet",
		SessionTTL:       30 * time.Minute,
		Signer:           signer,
	})
	require.NoError(t, err)
	return srv
}

func doRequest(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithHeader(t, srv, method, path, body)
}

// doRequestWithHeader builds a request like doRequest but sets pairs of
// header/value extras (variadic; expects even count). Used by tests that
// need to provide the X-Cancel-Token header.
func doRequestWithHeader(t *testing.T, srv *Server, method, path string, body any, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	if len(headers)%2 != 0 {
		t.Fatalf("doRequestWithHeader: headers must come in name+value pairs (got %d)", len(headers))
	}
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		require.NoError(t, err)
		reqBody = bytes.NewReader(buf)
	}
	r := httptest.NewRequest(method, path, reqBody)
	for i := 0; i < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	return w
}

// ===== /session/start =====

func TestSessionStart_Auth_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	w := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth"})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	var resp SessionStartResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Sid, 32, "sid must be 32 hex chars")
	// Dash testnet P2SH addresses start with '8' or '9' (PubKeyHashAddrID
	// 0x13 → base58 prefix range). Was 'tdash1' (bech32 P2WSH) before
	// the Dash-compat fix; dashd v23 rejects bech32 since Dash never
	// activated SegWit.
	assert.True(t,
		strings.HasPrefix(resp.DepositAddress, "8") || strings.HasPrefix(resp.DepositAddress, "9"),
		"testnet deposit address should be Dash testnet P2SH (8.../9...); got %q", resp.DepositAddress)
	assert.NotEmpty(t, resp.AddressSignature)
	assert.Equal(t, MinDustDuffs, resp.RequiredAmountDuffs)
	assert.NotEmpty(t, resp.ExpiresAt)
	assert.Equal(t, "/session/"+resp.Sid+"/status", resp.StatusURL)
}

func TestSessionStart_Auth_ClientSuppliedSid(t *testing.T) {
	srv := newTestServer(t)
	w := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{
		Op:  "auth",
		Sid: "client-supplied-sid-abc123",
	})
	require.Equal(t, http.StatusCreated, w.Code)
	var resp SessionStartResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "client-supplied-sid-abc123", resp.Sid)
}

func TestSessionStart_Auth_DuplicateSidRejected(t *testing.T) {
	srv := newTestServer(t)
	first := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth", Sid: "same"})
	require.Equal(t, http.StatusCreated, first.Code)
	second := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth", Sid: "same"})
	assert.Equal(t, http.StatusConflict, second.Code,
		"second session with same sid must conflict; body=%s", second.Body.String())
}

func TestSessionStart_Call_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	w := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{
		Op: "call",
		Args: &CallArgs{
			Contract:    "vsc1Forwarder",
			Method:      "swap",
			ArgsB64:     base64.StdEncoding.EncodeToString([]byte(`{"in":"DASH","out":"HBD"}`)),
			AmountDuffs: 100_000_000, // 1 DASH
		},
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())

	var resp SessionStartResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, int64(100_000_000), resp.RequiredAmountDuffs,
		"value-bearing op=call requires the declared amount")
	assert.True(t,
		strings.HasPrefix(resp.DepositAddress, "8") || strings.HasPrefix(resp.DepositAddress, "9"),
		"testnet deposit address should be Dash P2SH (8.../9...); got %q", resp.DepositAddress)
}

func TestSessionStart_Call_AmountZeroIsDust(t *testing.T) {
	// op=call with amount=0 is the value-less case (NFT transfer etc.).
	// Should be allowed with only dust minimum.
	srv := newTestServer(t)
	w := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{
		Op: "call",
		Args: &CallArgs{
			Contract:    "vsc1NftContract",
			Method:      "transfer",
			ArgsB64:     base64.StdEncoding.EncodeToString([]byte(`{"to":"x","tokenId":"1"}`)),
			AmountDuffs: 0,
		},
	})
	require.Equal(t, http.StatusCreated, w.Code, "body=%s", w.Body.String())
	var resp SessionStartResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, MinDustDuffs, resp.RequiredAmountDuffs)
}

func TestSessionStart_Call_BelowFloorRejected(t *testing.T) {
	// op=call with declared amount > 0 but below 0.01 DASH should reject.
	srv := newTestServer(t)
	w := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{
		Op: "call",
		Args: &CallArgs{
			Contract:    "vsc1NftContract",
			Method:      "swap",
			ArgsB64:     "",
			AmountDuffs: 100_000, // 0.001 DASH — below 0.01 DASH floor
		},
	})
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "0.01 DASH")
}

// TestSessionStart_Call_RejectsReservedDelims — audit D2-DESIGN-08.
// User-supplied contract/method/sid must not contain ';' or '='; args
// must not contain ';' (base64 padding '=' is allowed there).
func TestSessionStart_Call_RejectsReservedDelims(t *testing.T) {
	srv := newTestServer(t)
	cases := []struct {
		name string
		args CallArgs
		sid  string
	}{
		{"contract has ;", CallArgs{Contract: "vsc1foo;contract=evil", Method: "m", ArgsB64: ""}, ""},
		{"contract has =", CallArgs{Contract: "vsc1=foo", Method: "m", ArgsB64: ""}, ""},
		{"method has ;", CallArgs{Contract: "vsc1foo", Method: "swap;contract=evil", ArgsB64: ""}, ""},
		{"method has =", CallArgs{Contract: "vsc1foo", Method: "sw=ap", ArgsB64: ""}, ""},
		{"args has ;", CallArgs{Contract: "vsc1foo", Method: "swap", ArgsB64: "abc;contract=evil"}, ""},
		{"sid has ;", CallArgs{Contract: "vsc1foo", Method: "swap", ArgsB64: ""}, "sid;contract=evil"},
		{"sid has =", CallArgs{Contract: "vsc1foo", Method: "swap", ArgsB64: ""}, "sid=evil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{
				Op:   "call",
				Sid:  tc.sid,
				Args: &tc.args,
			})
			assert.Equal(t, http.StatusBadRequest, w.Code,
				"reserved delimiter must be rejected; body=%s", w.Body.String())
		})
	}
}

// TestSessionStart_Call_AllowsBase64PaddingInArgs — '=' is the kv
// delimiter BUT base64 uses it for padding. The contract's
// ParseInstructionV2 splits on the FIRST '=' per field so trailing
// '=' in the args value is parsed correctly. Verify the IS service
// doesn't gratuitously reject base64-padded args.
func TestSessionStart_Call_AllowsBase64PaddingInArgs(t *testing.T) {
	srv := newTestServer(t)
	w := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{
		Op: "call",
		Args: &CallArgs{
			Contract: "vsc1foo",
			Method:   "swap",
			// Real base64 with '==' padding.
			ArgsB64: base64.StdEncoding.EncodeToString([]byte("hello")),
		},
	})
	assert.Equal(t, http.StatusCreated, w.Code,
		"base64-padded args must NOT be rejected (only ';' is reserved in args); body=%s",
		w.Body.String())
}

func TestSessionStart_Call_MissingArgsRejected(t *testing.T) {
	srv := newTestServer(t)
	w := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "call"})
	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
}

func TestSessionStart_RejectsInvalidOp(t *testing.T) {
	srv := newTestServer(t)
	w := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "nonsense"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSessionStart_RejectsMalformedJSON(t *testing.T) {
	srv := newTestServer(t)
	r := httptest.NewRequest("POST", "/session/start", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ===== /session/{sid}/status =====

func TestSessionStatus_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	startW := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth"})
	require.Equal(t, http.StatusCreated, startW.Code)
	var start SessionStartResponse
	require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &start))

	statusW := doRequest(t, srv, "GET", "/session/"+start.Sid+"/status", nil)
	require.Equal(t, http.StatusOK, statusW.Code)

	var status SessionStatusResponse
	require.NoError(t, json.Unmarshal(statusW.Body.Bytes(), &status))
	assert.Equal(t, start.Sid, status.Sid)
	assert.Equal(t, string(StateWaitingForIS), status.State)
	assert.Empty(t, status.SessionToken, "session token must NOT be issued until ON_CHAIN")
}

func TestSessionStatus_NotFound(t *testing.T) {
	srv := newTestServer(t)
	w := doRequest(t, srv, "GET", "/session/nonexistent-sid/status", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ===== /session/{sid}/cancel =====

func TestSessionCancel_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	startW := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth"})
	require.Equal(t, http.StatusCreated, startW.Code)
	var start SessionStartResponse
	require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &start))

	cancelW := doRequestWithHeader(t, srv, "POST", "/session/"+start.Sid+"/cancel", nil,
		"X-Cancel-Token", start.AddressSignature)
	assert.Equal(t, http.StatusNoContent, cancelW.Code)

	// After cancel, status reports EXPIRED.
	statusW := doRequest(t, srv, "GET", "/session/"+start.Sid+"/status", nil)
	require.Equal(t, http.StatusOK, statusW.Code)
	var status SessionStatusResponse
	require.NoError(t, json.Unmarshal(statusW.Body.Bytes(), &status))
	assert.Equal(t, string(StateExpired), status.State)
}

func TestSessionCancel_NotFound(t *testing.T) {
	srv := newTestServer(t)
	// Audit L1 fixes the cancel endpoint to require the token; nonexistent
	// session + any token returns 401 (not 404 — avoid the
	// session-existence oracle).
	w := doRequestWithHeader(t, srv, "POST", "/session/nonexistent/cancel", nil,
		"X-Cancel-Token", "any-fake-token")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestSessionCancel_WithoutTokenIs401(t *testing.T) {
	srv := newTestServer(t)
	startW := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth"})
	require.Equal(t, http.StatusCreated, startW.Code)
	var start SessionStartResponse
	require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &start))
	// No X-Cancel-Token header.
	w := doRequest(t, srv, "POST", "/session/"+start.Sid+"/cancel", nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestSessionCancel_WrongTokenIs401(t *testing.T) {
	srv := newTestServer(t)
	startW := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth"})
	require.Equal(t, http.StatusCreated, startW.Code)
	var start SessionStartResponse
	require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &start))
	w := doRequestWithHeader(t, srv, "POST", "/session/"+start.Sid+"/cancel", nil,
		"X-Cancel-Token", "wrong-token")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// Round-4 audit R4-003 regression: cancel during ATTESTING /
// L2_SUBMITTED returns 409 Conflict instead of clobbering state.
// Without this guard, Prune's R3-CSM-02 carve-out is bypassed via
// the cancel handler (state is no longer in-flight → next Prune
// deletes it → reconcileL2 sees missing entry → L2-credited-but-
// session-lost divergence).
func TestSessionCancel_RejectsDuringInFlight(t *testing.T) {
	for _, s := range []SessionState{StateAttesting, StateL2Submitted} {
		t.Run(string(s), func(t *testing.T) {
			srv := newTestServer(t)
			startW := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth"})
			require.Equal(t, http.StatusCreated, startW.Code)
			var start SessionStartResponse
			require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &start))

			// Manually nudge the session into the in-flight state.
			srv.sessions.MutateState(start.Sid, func(sess *Session) {
				sess.State = s
			})

			cancelW := doRequestWithHeader(t, srv, "POST", "/session/"+start.Sid+"/cancel", nil,
				"X-Cancel-Token", start.AddressSignature)
			assert.Equal(t, http.StatusConflict, cancelW.Code,
				"cancel must refuse during %s", s)

			// And state stays in-flight.
			statusW := doRequest(t, srv, "GET", "/session/"+start.Sid+"/status", nil)
			require.Equal(t, http.StatusOK, statusW.Code)
			var status SessionStatusResponse
			require.NoError(t, json.Unmarshal(statusW.Body.Bytes(), &status))
			assert.Equal(t, string(s), status.State)
		})
	}
}

// Round-4 audit R4-006: /session/{sid}/status must surface L2TxId so
// operators and frontends can recover an L2-credited-but-session-lost
// payment from the status endpoint alone (no log scraping).
func TestSessionStatus_SurfacesL2TxId(t *testing.T) {
	srv := newTestServer(t)
	startW := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth"})
	require.Equal(t, http.StatusCreated, startW.Code)
	var start SessionStartResponse
	require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &start))

	srv.sessions.MutateState(start.Sid, func(sess *Session) {
		sess.State = StateL2Submitted
		sess.L2TxId = "bafy-l2-test-cid"
	})

	w := doRequest(t, srv, "GET", "/session/"+start.Sid+"/status", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var resp SessionStatusResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "bafy-l2-test-cid", resp.L2TxId)
}

// ===== /healthz =====

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	w := doRequest(t, srv, "GET", "/healthz", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"ok":true`)
}

// Round-5 audit R5-COV-05 / round-7 R7-DRIFT-01 / round-8 R8-OP-01:
// assert /healthz JSON includes the round-3 + round-4 + round-5 +
// round-6 fields. Locks down the shape so frontend dashboards have
// a stable contract.
func TestHealthz_EmitsExpectedKeys(t *testing.T) {
	before := time.Now().Unix()
	srv := newTestServer(t)
	w := doRequest(t, srv, "GET", "/healthz", nil)
	after := time.Now().Unix()
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	for _, key := range []string{"ok", "sessions", "sessionsByState",
		"processStartedAt", "processStartedAtUnix"} {
		_, present := body[key]
		assert.True(t, present, "/healthz missing key %q", key)
	}
	// Round-8 audit R8-OP-01: assert by bracket [before, after]
	// rather than InDelta(5s) — gives zero false-positives on slow
	// CI without bloating the tolerance window.
	if v, ok := body["processStartedAtUnix"].(float64); ok {
		assert.GreaterOrEqual(t, int64(v), before,
			"processStartedAtUnix must be >= before-snapshot")
		assert.LessOrEqual(t, int64(v), after,
			"processStartedAtUnix must be <= after-snapshot")
	} else {
		t.Errorf("processStartedAtUnix must be numeric, got %T", body["processStartedAtUnix"])
	}
}

// Round-7 audit R7-TEST-01 / round-8 R8-DRIFT-WARMUP-FIELDS: the
// SubmitterHealth warmup sentinel and the wrapped-error case must
// produce different /healthz behaviour. Warmup → ok=false (503),
// submitterWarmup=true, balance/RC/probeErr omitted. Real probe
// failure → degraded only after >= submitterDegradedFailThreshold
// consecutive fails.
func TestHealthz_WarmupSentinelNotProbeFailure(t *testing.T) {
	srv := newTestServer(t)
	srv.submitterHealth = func() (string, int64, int64, int, error) {
		return "did:test", 0, 0, 0, errSubmitterWarmup
	}
	w := doRequest(t, srv, "GET", "/healthz", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"/healthz must return 503 during submitter warmup so readiness probes wait")
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, true, body["submitterWarmup"])
	assert.Equal(t, false, body["ok"])
	// Round-7 R7-CORR-01-gvn + round-8 R8-DRIFT-WARMUP-FIELDS: balance/RC/probeErr
	// keys are omitted during warmup. The warmup sentinel is NOT a
	// probe failure so submitterProbeErr must not appear either.
	for _, omitKey := range []string{
		"submitterBalanceHbdCents", "submitterRcRemaining", "submitterProbeErr",
	} {
		_, has := body[omitKey]
		assert.False(t, has, "%q must be omitted during warmup", omitKey)
	}
}

func TestHealthz_SubmitterHysteresisGate(t *testing.T) {
	srv := newTestServer(t)
	// Round-8 audit R8-TEST-01: use the package-local threshold
	// constant rather than literal 2/3 so a future bump to the
	// hysteresis threshold automatically updates this test.
	belowThreshold := submitterDegradedFailThreshold - 1
	atThreshold := submitterDegradedFailThreshold

	srv.submitterHealth = func() (string, int64, int64, int, error) {
		return "did:test", 100, 200, belowThreshold, errors.New("transient flap")
	}
	w := doRequest(t, srv, "GET", "/healthz", nil)
	assert.Equal(t, http.StatusOK, w.Code,
		"%d consecutive fails must NOT trip degraded", belowThreshold)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	_, degraded := body["submitterDegraded"]
	assert.False(t, degraded, "submitterDegraded must stay off below threshold")
	assert.Equal(t, float64(belowThreshold), body["submitterConsecutiveFails"])

	srv.submitterHealth = func() (string, int64, int64, int, error) {
		return "did:test", 100, 200, atThreshold, errors.New("transient flap")
	}
	w = doRequest(t, srv, "GET", "/healthz", nil)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code,
		"%d consecutive fails must flip /healthz to 503", atThreshold)
}

// Round-5 audit R5-COV-03: isTrustedProxy must normalize IPv6 host
// casing through canonicalHostMatch so an operator typo (FE80::1) is
// treated equivalent to its lowercase canonical form.
func TestIsTrustedProxy_IPv6Canonical(t *testing.T) {
	srv := newTestServer(t)
	srv.trustedProxies = []string{"FE80::1", "2001:DB8::ABCD"}
	cases := []struct {
		host    string
		trusted bool
	}{
		{"127.0.0.1", true},      // loopback default
		{"::1", true},            // loopback default
		{"LocalHost", true},      // case-insensitive name
		{"fe80::1", true},        // lowercase IPv6 matches uppercase operator config
		{"FE80::1", true},        // same value
		{"2001:db8::abcd", true}, // mixed-case operator config matches canonical
		{"::2", false},           // unrelated IPv6
		{"10.0.0.1", false},      // unrelated IPv4
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			assert.Equal(t, c.trusted, srv.isTrustedProxy(c.host))
		})
	}
}

// ===== NewServer config validation =====

func TestNewServer_RejectsEmptyPubkeys(t *testing.T) {
	_, err := NewServer(ServerConfig{
		PrimaryPubKeyHex: "",
		BackupPubKeyHex:  parityBackupPubKey,
		Network:          "testnet",
		ChainID:          "vsc-testnet",
		Signer:           NewAddressSignerHMAC([]byte("x")),
	})
	assert.Error(t, err)
}

func TestNewServer_RejectsMissingSigner(t *testing.T) {
	_, err := NewServer(ServerConfig{
		PrimaryPubKeyHex: parityPrimaryPubKey,
		BackupPubKeyHex:  parityBackupPubKey,
		Network:          "testnet",
		ChainID:          "vsc-testnet",
		Signer:           nil,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "HSM")
}

func TestNewServer_RejectsBadNetwork(t *testing.T) {
	_, err := NewServer(ServerConfig{
		PrimaryPubKeyHex: parityPrimaryPubKey,
		BackupPubKeyHex:  parityBackupPubKey,
		Network:          "fakenet",
		ChainID:          "vsc-fake",
		Signer:           NewAddressSignerHMAC([]byte("x")),
	})
	assert.Error(t, err)
}

// ===== AddressSignerHMAC =====

func TestAddressSignerHMAC_Deterministic(t *testing.T) {
	s := NewAddressSignerHMAC([]byte("secret"))
	a, err := s.Sign(context.Background(), "addr1", "instr1")
	require.NoError(t, err)
	b, err := s.Sign(context.Background(), "addr1", "instr1")
	require.NoError(t, err)
	assert.Equal(t, a, b, "HMAC over same inputs must be deterministic")
}

func TestAddressSignerHMAC_DifferentInputs(t *testing.T) {
	s := NewAddressSignerHMAC([]byte("secret"))
	a, _ := s.Sign(context.Background(), "addr1", "instr1")
	b, _ := s.Sign(context.Background(), "addr2", "instr1")
	c, _ := s.Sign(context.Background(), "addr1", "instr2")
	assert.NotEqual(t, a, b, "different address must produce different signature")
	assert.NotEqual(t, a, c, "different instruction must produce different signature")
}

func TestAddressSignerHMAC_RefusesEmptySecret(t *testing.T) {
	s := NewAddressSignerHMAC(nil)
	_, err := s.Sign(context.Background(), "addr", "instr")
	assert.Error(t, err, "empty secret must refuse to sign (would otherwise produce attacker-knowable signatures)")
}

// ===== clientIP — X-Forwarded-For parsing =====

// TestClientIP_XFF_RightmostBehindTrustedProxy covers audit SEC-4 (R15).
// Trusted proxies (nginx, traefik, etc.) APPEND the connecting IP to
// the existing X-Forwarded-For header. An attacker connecting through
// such a proxy can inject `X-Forwarded-For: bypass-0, bypass-1` and
// the proxy turns it into `bypass-0, bypass-1, <attacker-real-ip>`.
// The leftmost value is attacker-controlled and would let the attacker
// rotate the per-IP rate-limit bucket per request. We MUST pick the
// rightmost entry (the trusted proxy's just-appended connecting IP).
func TestClientIP_XFF_RightmostBehindTrustedProxy(t *testing.T) {
	srv := newTestServer(t)
	// "127.0.0.1" is hardcoded as a trusted proxy by default.
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2, 3.3.3.3")
	got := srv.clientIP(r)
	assert.Equal(t, "3.3.3.3", got,
		"clientIP must return the RIGHTMOST X-Forwarded-For entry "+
			"(trusted proxy appended IP), not the leftmost "+
			"(attacker-controlled value). Leftmost-parsing would let "+
			"an attacker rotate the per-IP rate-limit bucket per "+
			"request — audit SEC-4.")
}

// TestClientIP_XFF_SingleValue verifies the no-comma branch still
// returns the single header value.
func TestClientIP_XFF_SingleValue(t *testing.T) {
	srv := newTestServer(t)
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "5.5.5.5")
	assert.Equal(t, "5.5.5.5", srv.clientIP(r))
}

// TestClientIP_XFF_IgnoredFromUntrustedProxy verifies the trusted-
// proxy gate: if RemoteAddr isn't in the trusted set the XFF header
// is ignored entirely (caller-spoofable otherwise).
func TestClientIP_XFF_IgnoredFromUntrustedProxy(t *testing.T) {
	srv := newTestServer(t)
	r := httptest.NewRequest("GET", "/healthz", nil)
	r.RemoteAddr = "9.9.9.9:54321"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2")
	assert.Equal(t, "9.9.9.9", srv.clientIP(r),
		"untrusted RemoteAddr must ignore XFF entirely")
}

// ===== rate limit buckets =====

// TestRateLimits_StatusAndCancelAreIndependent covers audit
// R16-SEC-status-cancel-no-rate-limit (LOW) +
// R17-CORR-status-cancel-shared-bucket-multi-tab-cancel-fails (LOW).
//
// Pre-R17: /status + /cancel shared a 60/min bucket. A user polling
// /status at 2s cadence from two tabs exhausted the bucket and the
// subsequent /cancel click hit 429.
//
// Post-R17: separate buckets — /status 60/min, /cancel 10/min. The
// test exhausts /status (61 calls) and verifies /cancel is still
// accepted.
func TestRateLimits_StatusAndCancelAreIndependent(t *testing.T) {
	srv := newTestServer(t)

	// Create a session so /cancel has something to cancel + we have a
	// real sid + cancel token.
	startResp := doRequest(t, srv, "POST", "/session/start", SessionStartRequest{Op: "auth"})
	require.Equal(t, http.StatusCreated, startResp.Code, "body=%s", startResp.Body.String())
	var start SessionStartResponse
	require.NoError(t, json.Unmarshal(startResp.Body.Bytes(), &start))

	// Burn /status well past the 60/min cap. 70 calls > 60/min so the
	// last few should 429 within the bucket — but /cancel must still
	// work because it has its OWN bucket.
	for i := 0; i < 65; i++ {
		w := doRequest(t, srv, "GET", "/session/"+start.Sid+"/status", nil)
		// Don't assert per-call status (depends on rate-limit window
		// drift); we only care that the bucket DOES cap eventually.
		_ = w
	}

	// Now hit /cancel. With separate buckets this MUST be 200 (cancel
	// accepted) or 409 (session not in cancellable state). With the
	// pre-R17 shared-bucket bug this would have been 429.
	w := doRequestWithHeader(t, srv, "POST", "/session/"+start.Sid+"/cancel", nil,
		"X-Cancel-Token", start.AddressSignature)
	assert.NotEqual(t, http.StatusTooManyRequests, w.Code,
		"/cancel must have its own rate-limit bucket so a hot /status "+
			"flow cannot lock the user out of cancelling (audit R17)")
}
