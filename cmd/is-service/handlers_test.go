package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
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
	assert.True(t, strings.HasPrefix(resp.DepositAddress, "tdash1"),
		"testnet deposit address should be tdash1...; got %q", resp.DepositAddress)
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
	assert.True(t, strings.HasPrefix(resp.DepositAddress, "tdash1"))
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

// ===== /healthz =====

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	w := doRequest(t, srv, "GET", "/healthz", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"ok":true`)
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
	a, err := s.Sign("addr1", "instr1")
	require.NoError(t, err)
	b, err := s.Sign("addr1", "instr1")
	require.NoError(t, err)
	assert.Equal(t, a, b, "HMAC over same inputs must be deterministic")
}

func TestAddressSignerHMAC_DifferentInputs(t *testing.T) {
	s := NewAddressSignerHMAC([]byte("secret"))
	a, _ := s.Sign("addr1", "instr1")
	b, _ := s.Sign("addr2", "instr1")
	c, _ := s.Sign("addr1", "instr2")
	assert.NotEqual(t, a, b, "different address must produce different signature")
	assert.NotEqual(t, a, c, "different instruction must produce different signature")
}

func TestAddressSignerHMAC_RefusesEmptySecret(t *testing.T) {
	s := NewAddressSignerHMAC(nil)
	_, err := s.Sign("addr", "instr")
	assert.Error(t, err, "empty secret must refuse to sign (would otherwise produce attacker-knowable signatures)")
}
