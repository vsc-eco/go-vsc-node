package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// AddressSignerVaultTransit signs (depositAddress || \0 || instruction)
// via HashiCorp Vault's transit/sign API. Spec §5.7's "private key
// MUST live in an HSM/KMS" mandate is satisfied here: the Vault
// process holds the key (optionally backed by an HSM seal in Vault
// Enterprise), the IS service holds only a short-lived Vault token,
// and signing happens via a single HTTP round-trip per
// /session/start.
//
// The IS service NEVER sees the private key bytes — Vault returns
// only the signature. If the IS-service host is compromised the
// attacker gains the (revocable, scoped) Vault token; rotating it
// invalidates the attacker's signing surface without losing the key.
//
// Key choice: Vault Transit's Ed25519 keys (`key_type=ed25519`)
// produce 64-byte signatures over the raw input — matches the
// AddressSignerEd25519 wire format exactly so Altera's
// signer-kind-agnostic verification path keeps working with no
// changes. Other key types (ECDSA P-256, RSA-PSS) would change
// the on-wire signature shape and require parallel verifier code in
// Altera; Ed25519 is the preferred choice.
//
// Auth: token-based. The operator passes either VAULT_TOKEN (env)
// or VAULT_TOKEN_FILE (path to a file containing the token). The
// token MUST be scoped to a Vault policy granting only
// `update` on `transit/sign/<key>` and `read` on
// `transit/keys/<key>` (for pubkey export). Wider grants are a
// production smell.
type AddressSignerVaultTransit struct {
	addr     string // e.g. https://vault.internal:8200
	token    string
	mount    string // default "transit"
	keyName  string
	http     *http.Client
	pubKeyB64 string // cached at startup; Ed25519 pubkey, base64 std
	pubKeyHex string // 64-char hex for Altera pinning
}

// NewAddressSignerVaultTransit constructs a signer + verifies it
// can reach Vault + caches the public key. Returns the signer + the
// pubkey hex (operators pin this in PUBLIC_IS_SERVICE_SIGNER_PUBKEY).
//
// `cfg` is sourced from CLI flags (see args.go); token resolution is
// handled here so the constructor stays callable from tests with a
// literal token.
type VaultTransitConfig struct {
	Addr    string // VAULT_ADDR equivalent
	Token   string // resolved at call site
	Mount   string // default "transit"
	KeyName string // required
	// HTTPClient is optional; nil falls back to a 10s-timeout client.
	HTTPClient *http.Client
}

func NewAddressSignerVaultTransit(cfg VaultTransitConfig) (*AddressSignerVaultTransit, string, error) {
	if cfg.Addr == "" {
		return nil, "", fmt.Errorf("vault addr required")
	}
	if _, err := url.Parse(cfg.Addr); err != nil {
		return nil, "", fmt.Errorf("invalid vault addr: %w", err)
	}
	if cfg.Token == "" {
		return nil, "", fmt.Errorf("vault token required (set VAULT_TOKEN or -signerVaultTokenFile)")
	}
	if cfg.KeyName == "" {
		return nil, "", fmt.Errorf("vault transit key name required")
	}
	mount := cfg.Mount
	if mount == "" {
		mount = "transit"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	s := &AddressSignerVaultTransit{
		addr:    strings.TrimRight(cfg.Addr, "/"),
		token:   cfg.Token,
		mount:   mount,
		keyName: cfg.KeyName,
		http:    httpClient,
	}
	// Fetch the pubkey at startup — fail-fast if Vault is
	// unreachable, the token is invalid, or the key doesn't exist.
	// Better to crash at boot than silently issue unverifiable
	// signatures to every /session/start.
	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pubB64, err := s.fetchLatestPublicKey(probeCtx)
	if err != nil {
		return nil, "", fmt.Errorf("vault transit startup probe: %w", err)
	}
	s.pubKeyB64 = pubB64
	pubRaw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		return nil, "", fmt.Errorf("decoding vault pubkey: %w", err)
	}
	if len(pubRaw) != 32 {
		return nil, "", fmt.Errorf("vault returned %d-byte pubkey; expected Ed25519 (32 bytes) — check key_type", len(pubRaw))
	}
	s.pubKeyHex = hex.EncodeToString(pubRaw)
	return s, s.pubKeyHex, nil
}

// Sign produces an Ed25519 signature via Vault transit/sign.
// Returns base64-StdEncoded sig (matches AddressSignerEd25519's
// wire shape).
func (s *AddressSignerVaultTransit) Sign(depositAddress, instruction string) (string, error) {
	buf := make([]byte, 0, len(depositAddress)+1+len(instruction))
	buf = append(buf, []byte(depositAddress)...)
	buf = append(buf, 0)
	buf = append(buf, []byte(instruction)...)
	inputB64 := base64.StdEncoding.EncodeToString(buf)

	body, _ := json.Marshal(map[string]any{"input": inputB64})
	endpoint := fmt.Sprintf("%s/v1/%s/sign/%s", s.addr, s.mount, s.keyName)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building vault sign request: %w", err)
	}
	req.Header.Set("X-Vault-Token", s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling vault transit/sign: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("vault transit/sign returned %d: %s", resp.StatusCode, string(rawBody))
	}
	var out struct {
		Data struct {
			Signature string `json:"signature"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return "", fmt.Errorf("decoding vault response: %w (raw=%s)", err, string(rawBody))
	}
	// Vault wraps signatures with "vault:v1:" so callers can
	// auto-detect the key version that signed. We only sign with
	// the latest version (Vault's default), so strip the prefix to
	// hand Altera the raw base64-encoded Ed25519 sig — same shape
	// AddressSignerEd25519 produces.
	const prefix = "vault:v1:"
	if !strings.HasPrefix(out.Data.Signature, prefix) {
		return "", fmt.Errorf("unexpected vault signature format: %q", out.Data.Signature)
	}
	sigB64 := strings.TrimPrefix(out.Data.Signature, prefix)
	// Sanity-check: it should be valid base64 + 64 bytes (Ed25519
	// sig size) once decoded.
	sigRaw, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return "", fmt.Errorf("vault signature not base64: %w", err)
	}
	if len(sigRaw) != 64 {
		return "", fmt.Errorf("vault returned %d-byte signature; expected 64 (Ed25519)", len(sigRaw))
	}
	return sigB64, nil
}

// PubkeyHex returns the cached Ed25519 public key as 64-char hex
// (operator pins this in PUBLIC_IS_SERVICE_SIGNER_PUBKEY).
func (s *AddressSignerVaultTransit) PubkeyHex() string {
	return s.pubKeyHex
}

// fetchLatestPublicKey calls GET /v1/<mount>/keys/<key> and returns
// the public_key for the key's latest_version. Vault returns the
// public key as base64-StdEncoded raw Ed25519 pubkey (32 bytes).
func (s *AddressSignerVaultTransit) fetchLatestPublicKey(ctx context.Context) (string, error) {
	endpoint := fmt.Sprintf("%s/v1/%s/keys/%s", s.addr, s.mount, s.keyName)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("building vault keys request: %w", err)
	}
	req.Header.Set("X-Vault-Token", s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling vault transit/keys: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("vault transit/keys returned %d: %s", resp.StatusCode, string(rawBody))
	}
	var out struct {
		Data struct {
			Type          string                  `json:"type"`
			LatestVersion int                     `json:"latest_version"`
			Keys          map[string]versionEntry `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return "", fmt.Errorf("decoding vault keys response: %w (raw=%s)", err, string(rawBody))
	}
	if out.Data.Type != "ed25519" {
		return "", fmt.Errorf("vault key type is %q; expected ed25519 (recreate with -type=ed25519)", out.Data.Type)
	}
	if out.Data.LatestVersion <= 0 {
		return "", fmt.Errorf("vault key has no version yet (latest_version=%d)", out.Data.LatestVersion)
	}
	versionKey := fmt.Sprintf("%d", out.Data.LatestVersion)
	entry, ok := out.Data.Keys[versionKey]
	if !ok || entry.PublicKey == "" {
		return "", fmt.Errorf("vault did not return a public_key for version %s", versionKey)
	}
	return entry.PublicKey, nil
}

type versionEntry struct {
	PublicKey string `json:"public_key"`
}

// ResolveVaultToken reads the Vault token from one of:
//   - `tokenLiteral` (highest priority, typically -signerVaultToken=...
//     — discouraged because tokens land in process tables / logs)
//   - `tokenFile` (path; trimmed of trailing whitespace) — preferred
//     for production
//   - VAULT_TOKEN env var (fallback for ergonomics)
//
// Returns an error if all three are empty.
func ResolveVaultToken(tokenLiteral, tokenFile string) (string, error) {
	if tokenLiteral != "" {
		return tokenLiteral, nil
	}
	if tokenFile != "" {
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("reading -signerVaultTokenFile: %w", err)
		}
		t := strings.TrimSpace(string(raw))
		if t == "" {
			return "", fmt.Errorf("token file %s is empty", tokenFile)
		}
		return t, nil
	}
	if t := os.Getenv("VAULT_TOKEN"); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("no vault token configured (set -signerVaultToken, -signerVaultTokenFile, or VAULT_TOKEN env)")
}
