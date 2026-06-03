package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVault is a tiny stand-in for the subset of Vault Transit API
// the AddressSignerVaultTransit calls: GET /v1/<mount>/keys/<key>
// for pubkey + POST /v1/<mount>/sign/<key> for signing.
//
// Holds a real Ed25519 keypair so verification round-trips through
// the production code path (encoding, prefix-stripping, etc.)
// without any mocks in the adapter itself.
type fakeVault struct {
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	mount  string
	key    string
	token  string
	// Hook fields for failure-injection tests; nil = use defaults.
	signRespOverride func() (status int, body string)
}

func newFakeVault(t *testing.T, mount, key, token string) *fakeVault {
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return &fakeVault{pub: pub, priv: priv, mount: mount, key: key, token: token}
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Vault-Token") != f.token {
		http.Error(w, `{"errors":["forbidden"]}`, http.StatusForbidden)
		return
	}
	prefix := "/v1/" + f.mount
	switch {
	case r.Method == http.MethodGet && r.URL.Path == prefix+"/keys/"+f.key:
		resp := map[string]any{
			"data": map[string]any{
				"type":           "ed25519",
				"latest_version": 1,
				"keys": map[string]any{
					"1": map[string]any{
						"public_key": base64.StdEncoding.EncodeToString(f.pub),
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodPost && r.URL.Path == prefix+"/sign/"+f.key:
		if f.signRespOverride != nil {
			status, body := f.signRespOverride()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		var req struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		input, err := base64.StdEncoding.DecodeString(req.Input)
		if err != nil {
			http.Error(w, `{"errors":["bad base64"]}`, http.StatusBadRequest)
			return
		}
		sig := ed25519.Sign(f.priv, input)
		resp := map[string]any{
			"data": map[string]any{
				"signature": "vault:v1:" + base64.StdEncoding.EncodeToString(sig),
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	default:
		http.NotFound(w, r)
	}
}

func TestAddressSignerVaultTransit_RoundTrip(t *testing.T) {
	fv := newFakeVault(t, "transit", "is-signer", "fake-token-123")
	srv := httptest.NewServer(fv)
	t.Cleanup(srv.Close)

	signer, pubHex, err := NewAddressSignerVaultTransit(VaultTransitConfig{
		Addr:    srv.URL,
		Token:   "fake-token-123",
		Mount:   "transit",
		KeyName: "is-signer",
	})
	require.NoError(t, err)
	require.Len(t, pubHex, 64, "Ed25519 pubkey hex must be 64 chars")
	assert.Equal(t, hex.EncodeToString(fv.pub), pubHex,
		"PubkeyHex must equal hex(raw pubkey) — Altera pins this")

	depositAddr := "8jU46MxM2TpUsFFpm7V4fjJ4F9uroyTir1"
	instruction := "op=auth;sid=vault-roundtrip"
	sigB64, err := signer.Sign(depositAddr, instruction)
	require.NoError(t, err)
	require.NotEmpty(t, sigB64)
	assert.False(t, strings.HasPrefix(sigB64, "vault:v1:"),
		"Sign must strip Vault's `vault:v1:` prefix before returning")

	// Verify the signature against the public key + the same
	// canonical message AddressSignerEd25519 uses (addr || 0x00 ||
	// instruction). This proves the adapter produces wire-format-
	// equivalent signatures so Altera's verification path is
	// agnostic to the signer kind.
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	require.NoError(t, err)
	require.Len(t, sig, ed25519.SignatureSize)

	msg := append([]byte(depositAddr), 0)
	msg = append(msg, []byte(instruction)...)
	assert.True(t, ed25519.Verify(fv.pub, msg, sig),
		"Vault-signed Ed25519 signature must verify with the cached pubkey")
}

func TestAddressSignerVaultTransit_RejectsWrongKeyType(t *testing.T) {
	// Build a custom fake that returns key_type=ecdsa-p256 — should
	// be rejected at startup.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"type":           "ecdsa-p256",
				"latest_version": 1,
				"keys": map[string]any{
					"1": map[string]any{"public_key": "AAAA"},
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	_, _, err := NewAddressSignerVaultTransit(VaultTransitConfig{
		Addr:    srv.URL,
		Token:   "tok",
		KeyName: "ecdsa-key",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ed25519",
		"adapter must refuse a non-Ed25519 key at startup")
}

func TestAddressSignerVaultTransit_RejectsBadTokenAtBoot(t *testing.T) {
	fv := newFakeVault(t, "transit", "is-signer", "right-token")
	srv := httptest.NewServer(fv)
	t.Cleanup(srv.Close)

	_, _, err := NewAddressSignerVaultTransit(VaultTransitConfig{
		Addr:    srv.URL,
		Token:   "WRONG-token",
		KeyName: "is-signer",
	})
	require.Error(t, err, "bad token must surface at startup probe, not at first sign")
	assert.Contains(t, err.Error(), "403")
}

func TestAddressSignerVaultTransit_SurfaceSignFailures(t *testing.T) {
	fv := newFakeVault(t, "transit", "is-signer", "tok")
	srv := httptest.NewServer(fv)
	t.Cleanup(srv.Close)

	signer, _, err := NewAddressSignerVaultTransit(VaultTransitConfig{
		Addr: srv.URL, Token: "tok", KeyName: "is-signer",
	})
	require.NoError(t, err)

	// Inject a 500 on the next sign call.
	fv.signRespOverride = func() (int, string) {
		return http.StatusInternalServerError, `{"errors":["vault-down"]}`
	}
	_, err = signer.Sign("addr", "instr")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500",
		"sign-time vault errors must propagate, not silently use a stale signature")
}

func TestAddressSignerVaultTransit_MissingVaultPrefix(t *testing.T) {
	// Custom fake that returns a signature WITHOUT the vault:v1:
	// prefix — defensive check against an upstream API change.
	fv := newFakeVault(t, "transit", "is-signer", "tok")
	fv.signRespOverride = func() (int, string) {
		return http.StatusOK, fmt.Sprintf(`{"data":{"signature":%q}}`,
			"no-prefix-just-a-string")
	}
	srv := httptest.NewServer(fv)
	t.Cleanup(srv.Close)

	signer, _, err := NewAddressSignerVaultTransit(VaultTransitConfig{
		Addr: srv.URL, Token: "tok", KeyName: "is-signer",
	})
	require.NoError(t, err)
	_, err = signer.Sign("addr", "instr")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected vault signature format")
}

func TestResolveVaultToken_PrecedenceAndErrors(t *testing.T) {
	t.Setenv("VAULT_TOKEN", "")

	// All three empty → error.
	_, err := ResolveVaultToken("", "")
	require.Error(t, err)

	// Env-only.
	t.Setenv("VAULT_TOKEN", "env-token")
	got, err := ResolveVaultToken("", "")
	require.NoError(t, err)
	assert.Equal(t, "env-token", got)

	// Token-file wins over env.
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "vt")
	require.NoError(t, os.WriteFile(tokenPath, []byte("file-token\n"), 0o600))
	got, err = ResolveVaultToken("", tokenPath)
	require.NoError(t, err)
	assert.Equal(t, "file-token", got, "trailing whitespace must be trimmed")

	// Literal wins over both.
	got, err = ResolveVaultToken("literal", tokenPath)
	require.NoError(t, err)
	assert.Equal(t, "literal", got)

	// Empty file → error.
	emptyPath := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(emptyPath, []byte("\n  \n"), 0o600))
	_, err = ResolveVaultToken("", emptyPath)
	require.Error(t, err)
}
