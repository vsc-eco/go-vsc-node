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
	addr    string // e.g. https://vault.internal:8200
	token   string
	// tokenFile is the optional path the token was sourced from at
	// startup. When set, Sign() and fetchLatestPublicKey() re-read
	// it on every call so vault-agent-style short-lived tokens
	// (15min cadence) refresh without an IS-service restart.
	// Audit OPS-R15-01 (R15).
	tokenFile string
	mount     string // default "transit"
	keyName   string
	// keyVersion is the Vault transit key version chosen at startup
	// (from fetchLatestPublicKey's latest_version response). Sign()
	// pins this value in the request body so a Vault key rotation
	// while the IS service is running keeps producing v<startup>
	// signatures — the cached pubKeyHex stays consistent, Altera's
	// PUBLIC_IS_SERVICE_SIGNER_PUBKEY pin doesn't break, and the
	// fixed prefix-strip below stays correct.
	// Audit SEC-1 (R15).
	keyVersion int
	// sigPrefix is "vault:v<keyVersion>:" — derived once at startup
	// so Sign()'s prefix-strip doesn't have to re-scan or parse
	// per call.
	sigPrefix string
	http      *http.Client
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
	// TokenFile is the optional path the token was loaded from. When
	// set, Sign() re-reads the file on each call so vault-agent
	// rotation works without IS-service restart. Pass empty when
	// the token is a static literal (env or CLI flag); the cached
	// token is then used for the process lifetime.
	// Audit OPS-R15-01 (R15).
	TokenFile string
	// HTTPClient is optional; nil falls back to a 10s-timeout client.
	// Per-call context.WithTimeout layers: startup probe = 5s
	// (fail-fast), Sign() = 30s (audit R15-CORR-vault-sign-5s-timeout-
	// under-load bumped from 5s because Vault under HSM-backed load
	// routinely sees >5s p99).
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
		addr:      strings.TrimRight(cfg.Addr, "/"),
		token:     cfg.Token,
		tokenFile: cfg.TokenFile,
		mount:     mount,
		keyName:   cfg.KeyName,
		http:      httpClient,
	}
	// Fetch the pubkey at startup — fail-fast if Vault is
	// unreachable, the token is invalid, or the key doesn't exist.
	// Better to crash at boot than silently issue unverifiable
	// signatures to every /session/start.
	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pubB64, version, err := s.fetchLatestPublicKey(probeCtx)
	if err != nil {
		return nil, "", fmt.Errorf("vault transit startup probe: %w", err)
	}
	s.pubKeyB64 = pubB64
	s.keyVersion = version
	s.sigPrefix = fmt.Sprintf("vault:v%d:", version)
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

// currentToken returns the active Vault token, re-reading tokenFile if
// configured so vault-agent rotation picks up automatically. Falls back
// to the cached startup token when tokenFile is empty or unreadable —
// the cached token may have lapsed but is better than no token at all
// (Sign() will surface the 403 to the caller).
// Audit OPS-R15-01 (R15).
func (s *AddressSignerVaultTransit) currentToken() string {
	if s.tokenFile == "" {
		return s.token
	}
	raw, err := os.ReadFile(s.tokenFile)
	if err != nil {
		// Token file lapsed (permissions, disk full, agent crashed).
		// Use the cached startup token as a fallback; Sign()'s caller
		// sees the Vault-level failure if both are invalid.
		return s.token
	}
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return s.token
	}
	return t
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

	// Audit SEC-1 (R15): pin key_version to the version chosen at
	// startup. Vault's default is "latest", so without this pin a
	// rotation while the IS service is running would start producing
	// vault:v<N+1>: signatures + a v<N+1> pubkey, breaking both the
	// hardcoded prefix-strip below and Altera's
	// PUBLIC_IS_SERVICE_SIGNER_PUBKEY pin. By pinning we keep the
	// version stable across the process lifetime; rotation requires
	// a coordinated IS-service restart + Altera pubkey update.
	body, _ := json.Marshal(map[string]any{
		"input":       inputB64,
		"key_version": s.keyVersion,
	})
	endpoint := fmt.Sprintf("%s/v1/%s/sign/%s", s.addr, s.mount, s.keyName)
	// Audit R15-CORR-vault-sign-5s-timeout-under-load (LOW): bumped
	// from 5s → 30s. Vault under load (HSM-backed key, high concurrency
	// on a single Transit key, or seal-wrapped operations) routinely
	// sees >5s p99 latencies; a tight 5s causes a 500 → user retries →
	// retry storm cascade. The HTTP client default Timeout is 10s, so
	// the WithTimeout(30s) only kicks in when the HTTP roundtrip
	// itself somehow exceeds 10s (a misbehaving proxy or a TCP stall
	// that survives the HTTP timeout); 30s gives that case a clear
	// upper bound rather than indefinite waiting.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building vault sign request: %w", err)
	}
	req.Header.Set("X-Vault-Token", s.currentToken())
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling vault transit/sign: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Audit OPS-R15-04 (R15): tag the status class so on-call sees
		// at-a-glance whether it's an auth issue (token rotation /
		// permission denied), a config issue (mount or key missing /
		// renamed), or a Vault-internal failure. Pre-fix every code
		// mapped to the same wrapped error string; bisect required
		// reading the raw response body.
		var kind string
		switch {
		case resp.StatusCode == 401:
			kind = "unauthenticated (token expired or invalid — check token rotation)"
		case resp.StatusCode == 403:
			kind = "permission denied (policy missing update on transit/sign/<key>)"
		case resp.StatusCode == 404:
			kind = "not found (check -signerVaultMount + -signerVaultKeyName)"
		case resp.StatusCode == 429:
			kind = "rate limited by Vault (back off + retry)"
		case resp.StatusCode >= 500:
			kind = "vault server error (5xx — investigate vault logs)"
		default:
			kind = "unexpected status"
		}
		return "", fmt.Errorf("vault transit/sign %d [%s]: %s",
			resp.StatusCode, kind, string(rawBody))
	}
	var out struct {
		Data struct {
			Signature string `json:"signature"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return "", fmt.Errorf("decoding vault response: %w (raw=%s)", err, string(rawBody))
	}
	// Vault returns signatures prefixed with "vault:v<keyVersion>:".
	// Audit R15-CONS-09: we explicitly do NOT auto-detect from the
	// prefix at sign time — we capture latest_version at startup and
	// pin it into both the request key_version field AND s.sigPrefix.
	// A mismatched prefix here means Vault produced a different
	// version than the pinned one (e.g. someone wrote
	// `min_decryption_version` mid-flight or hacked the response),
	// and we error out rather than silently accept. Strip the fixed
	// prefix to hand Altera the raw base64-encoded Ed25519 sig (same
	// shape AddressSignerEd25519 produces).
	if !strings.HasPrefix(out.Data.Signature, s.sigPrefix) {
		return "", fmt.Errorf("unexpected vault signature format: want prefix %q, got %q",
			s.sigPrefix, out.Data.Signature)
	}
	sigB64 := strings.TrimPrefix(out.Data.Signature, s.sigPrefix)
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
// the public_key + version-number for the key's latest_version. Vault
// returns the public key as base64-StdEncoded raw Ed25519 pubkey
// (32 bytes). The version-number is captured so Sign() can pin
// key_version per audit SEC-1 (R15).
func (s *AddressSignerVaultTransit) fetchLatestPublicKey(ctx context.Context) (string, int, error) {
	endpoint := fmt.Sprintf("%s/v1/%s/keys/%s", s.addr, s.mount, s.keyName)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return "", 0, fmt.Errorf("building vault keys request: %w", err)
	}
	req.Header.Set("X-Vault-Token", s.currentToken())
	resp, err := s.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("calling vault transit/keys: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("vault transit/keys returned %d: %s", resp.StatusCode, string(rawBody))
	}
	var out struct {
		Data struct {
			Type          string                  `json:"type"`
			LatestVersion int                     `json:"latest_version"`
			Keys          map[string]versionEntry `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rawBody, &out); err != nil {
		return "", 0, fmt.Errorf("decoding vault keys response: %w (raw=%s)", err, string(rawBody))
	}
	if out.Data.Type != "ed25519" {
		return "", 0, fmt.Errorf("vault key type is %q; expected ed25519 (recreate with -type=ed25519)", out.Data.Type)
	}
	if out.Data.LatestVersion <= 0 {
		return "", 0, fmt.Errorf("vault key has no version yet (latest_version=%d)", out.Data.LatestVersion)
	}
	versionKey := fmt.Sprintf("%d", out.Data.LatestVersion)
	entry, ok := out.Data.Keys[versionKey]
	if !ok || entry.PublicKey == "" {
		return "", 0, fmt.Errorf("vault did not return a public_key for version %s", versionKey)
	}
	return entry.PublicKey, out.Data.LatestVersion, nil
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
