package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
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
	// startup. When set, currentToken() re-reads it on every
	// invocation so vault-agent-style short-lived tokens (15min
	// cadence) refresh without an IS-service restart. Sign() calls
	// currentToken() per request; fetchLatestPublicKey() also calls
	// it but only runs at startup, so the practical effect of the
	// re-read is on the Sign() hot path. Audit OPS-R15-01 (R15) +
	// R16-CONS-vault-token-docstring-fetch-claim-misleading.
	tokenFile string
	// tokenSrc is the non-secret label identifying which input
	// supplied the startup token. Surfaced in 401/403 Vault error
	// messages. Audit R17-CONS-tokensource-label-claim-overstates-risk.
	tokenSrc VaultTokenSource
	mount    string // default "transit"
	keyName  string
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
	// tokenFallbackLastWarnUnix throttles the slog.Warn emitted when
	// currentToken falls back to the cached startup token because
	// TokenFile became unreadable. Stores the Unix timestamp (seconds)
	// of the most recent warn. Audit R16-SEC-vault-currenttoken-silent-
	// fallback established the warn; R17-CORR-token-fallback-once-no-
	// recurrence-window replaced the original sync.Once with a 1-hour
	// throttle so a vault-agent that crashes → recovers → crashes again
	// emits a SECOND warn for the second incident (sync.Once would have
	// stayed silent past the first one for the whole process lifetime).
	tokenFallbackLastWarnUnix atomic.Int64
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
	// TokenSource is the non-secret label identifying which input
	// supplied the token at startup ("literal" | "file" | "env").
	// Surfaced in Vault 401/403 error messages so on-call knows which
	// input to investigate without cross-referencing startup logs.
	// Audit R17-CONS-tokensource-label-claim-overstates-risk.
	TokenSource VaultTokenSource
	// HTTPClient is optional; nil falls back to a client with NO
	// client-level Timeout — per-call context.WithTimeout drives the
	// deadline: startup probe = 5s (fail-fast), Sign() = 30s (audit
	// R15-CORR-vault-sign-5s-timeout-under-load bumped from 5s
	// because Vault under HSM-backed load routinely sees >5s p99).
	// Audit R16-CORR-vault-sign-30s-context-capped-by-10s-http-timeout
	// removed the prior 10s client Timeout because it silently capped
	// the context bump at 10s, making the 30s a dead value.
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
		// Per-call WithTimeout drives the deadline (5s for startup
		// probe; 30s for Sign per R15-CORR-vault-sign-5s-timeout-
		// under-load). Audit R16-CORR-vault-sign-30s-context-capped-
		// by-10s-http-timeout: a 10s client-level Timeout would silently
		// override the 30s context — the bump landed but did nothing.
		// Setting Timeout to 0 disables the client-level cap and lets
		// per-call WithTimeout drive end-to-end.
		httpClient = &http.Client{Timeout: 0}
	}
	s := &AddressSignerVaultTransit{
		addr:        strings.TrimRight(cfg.Addr, "/"),
		token:       cfg.Token,
		tokenFile:   cfg.TokenFile,
		tokenSrc:    cfg.TokenSource,
		mount:       mount,
		keyName:     cfg.KeyName,
		http:        httpClient,
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
//
// Audit OPS-R15-01 (R15) + R16-SEC-vault-currenttoken-silent-fallback
// (MED) + R17-CORR-token-fallback-once-no-recurrence-window (LOW):
// the fallback is intentional so a transient permission/disk-full
// glitch doesn't take Sign() down outright, but pre-R16 it was SILENT
// — an operator whose vault-agent crashed kept seeing successful
// signs (with a stale token, until that lapsed too) instead of
// getting an immediate signal. R16 added a sync.Once-gated warn;
// R17 replaced that with a 1-hour-throttled atomic so a recurring
// crash (recover → re-crash hours later) still surfaces. Audit R18-
// CONS-currenttoken-godoc-stale-first-time-vs-throttle caught this
// docstring lagging the R17 behaviour change.
func (s *AddressSignerVaultTransit) currentToken() string {
	if s.tokenFile == "" {
		return s.token
	}
	raw, err := os.ReadFile(s.tokenFile)
	if err != nil {
		// Token file lapsed (permissions, disk full, agent crashed).
		// Use the cached startup token as a fallback; Sign()'s caller
		// sees the Vault-level failure if both are invalid.
		s.warnTokenFallbackOnce("read failed", err)
		return s.token
	}
	t := strings.TrimSpace(string(raw))
	if t == "" {
		s.warnTokenFallbackOnce("file empty after trim", nil)
		return s.token
	}
	return t
}

// tokenSource returns a human-readable label naming where the Vault
// token came from at startup. Used in auth-failure error messages so
// the operator knows which input to investigate first.
//
// Audit R16-OPS-vault-sign-401-no-token-source-identity (initial
// version) + R17-CONS-tokensource-label-claim-overstates-risk (this
// version): the label is a NON-SECRET identifier — threading the
// source through VaultTransitConfig.TokenSource lets us distinguish
// "literal" vs "file" vs "env" precisely without leaking the secret
// itself.
func (s *AddressSignerVaultTransit) tokenSource() string {
	// Audit R18-OPS-vault-token-file-path-included-in-error-message-
	// discloses-fs-layout: prior version returned "file=" + s.tokenFile,
	// which leaked the operator's literal filesystem path into Vault
	// 401/403 HTTP response bodies. The path is "non-secret" per the
	// R17 audit framing but still fingerprints the host's layout; we
	// return only the category label here. Operators can still look
	// up the configured path in startup logs (logged once at
	// configuration time) when they need it for triage.
	if s.tokenFile != "" {
		return "file"
	}
	switch s.tokenSrc {
	case VaultTokenSourceLiteral:
		return "literal (-signerVaultToken)"
	case VaultTokenSourceEnv:
		return "env VAULT_TOKEN"
	case VaultTokenSourceFile:
		// Caller cleared tokenFile but kept the source label set to
		// File — unusual but technically valid. Return a generic
		// label rather than the (now-empty) path. Audit R18-CONS-
		// tokensource-switch-case-comment-wrong-fall-through fixed
		// the prior "fall through" wording (the case returns directly).
		return "file (path cleared)"
	}
	return "unknown (TokenSource unset)"
}

// warnTokenFallbackOnce logs at WARN that we fell back to the cached
// startup token because the configured TokenFile was unusable. The
// reason + (optional) error give the operator enough hint to triage.
//
// Audit R16-SEC-vault-currenttoken-silent-fallback established the
// warn; R17-CORR-token-fallback-once-no-recurrence-window replaced
// the original sync.Once gate with a 1-hour-throttled emission so a
// recurring vault-agent crash (crashes → recovers → crashes again
// hours later) emits a SECOND warn for the SECOND incident. sync.Once
// would have stayed silent past the first one for the whole process
// lifetime, hiding intermittent flapping on long-running deploys.
//
// Throttle = 1 hour means a sustained outage emits at most ~24 warns/day,
// bounding log noise while keeping fresh incidents visible.
func (s *AddressSignerVaultTransit) warnTokenFallbackOnce(reason string, err error) {
	const throttleSec = int64(3600) // 1 hour
	now := time.Now().Unix()
	last := s.tokenFallbackLastWarnUnix.Load()
	if last > 0 && now-last < throttleSec {
		return
	}
	// Compare-and-swap so two concurrent fallback calls don't both
	// emit. Only one wins the CAS; the other returns silently.
	if !s.tokenFallbackLastWarnUnix.CompareAndSwap(last, now) {
		return
	}
	args := []any{"reason", reason, "tokenFile", s.tokenFile}
	if err != nil {
		args = append(args, "err", err)
	}
	args = append(args,
		"behaviour", "using cached startup token; rotation is currently disabled until TokenFile is readable",
		"action", "check vault-agent / sink file permissions / disk")
	slog.Warn("vault TokenFile unreadable — falling back to cached startup token", args...)
}

// Sign produces an Ed25519 signature via Vault transit/sign.
// Returns base64-StdEncoded sig (matches AddressSignerEd25519's
// wire shape).
//
// Audit R17-CORR-vault-sign-ctx-from-background-not-request-ctx:
// the inbound ctx is honoured for cancellation — if the client
// disconnects mid-sign, the Vault round-trip is cancelled and
// Vault's signing slot is freed. The per-call WithTimeout(30s)
// derives from this caller ctx so it ALSO short-circuits on caller
// cancellation, not just on the 30s deadline.
func (s *AddressSignerVaultTransit) Sign(callerCtx context.Context, depositAddress, instruction string) (string, error) {
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
	// from 5s → 30s. Vault under HSM-backed load (high concurrency
	// on a single Transit key, or seal-wrapped operations) routinely
	// sees >5s p99 latencies; a tight 5s causes a 500 → user retries →
	// retry storm cascade.
	//
	// Audit R17-CONS-vault-sign-30s-context-comment-stale: the 30s
	// WithTimeout is now the sole deadline on the request — R16's
	// httpClient.Timeout=0 fix removed the conflicting 10s client cap
	// that previously silently overrode this context. Per-call layers
	// in effect today: startup probe = 5s (fail-fast), Sign() = 30s
	// (under-load tolerance).
	ctx, cancel := context.WithTimeout(callerCtx, 30*time.Second)
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
		// Audit OPS-R15-04 (R15) + R16-OPS-vault-sign-401-no-token-
		// source-identity (R16, MED): tag the status class so on-call
		// sees at-a-glance whether it's an auth issue (token rotation /
		// permission denied), a config issue (mount or key missing /
		// renamed), or a Vault-internal failure. For 401 specifically,
		// include the token source (file / env / literal) so the
		// operator knows whether to inspect the TokenFile or rotate
		// the env-supplied token. Pre-fix every code mapped to the
		// same wrapped error string; bisect required reading the raw
		// response body + cross-referencing with the binary's
		// startup logs to figure out which token path was in play.
		var kind string
		switch {
		case resp.StatusCode == 401:
			kind = "unauthenticated (token expired or invalid — check token rotation; source=" + s.tokenSource() + ")"
		case resp.StatusCode == 403:
			kind = "permission denied (policy missing update on transit/sign/<key>; source=" + s.tokenSource() + ")"
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

// VaultTokenSource is a non-secret label identifying WHICH input
// supplied the Vault token. Used in Vault 401/403 error messages so
// the operator immediately knows whether to inspect the
// -signerVaultTokenFile path, the VAULT_TOKEN env, or the
// -signerVaultToken CLI flag. Audit R17-CONS-tokensource-label-claim-
// overstates-risk: the label itself carries zero secret material.
type VaultTokenSource string

const (
	VaultTokenSourceLiteral VaultTokenSource = "literal"
	VaultTokenSourceFile    VaultTokenSource = "file"
	VaultTokenSourceEnv     VaultTokenSource = "env"
)

// ResolveVaultToken reads the Vault token from one of:
//   - `tokenLiteral` (highest priority, typically -signerVaultToken=...
//     — refused outside devnet per SEC-6)
//   - `tokenFile` (path; trimmed of trailing whitespace) — preferred
//     for production (supports vault-agent rotation)
//   - VAULT_TOKEN env var (fallback for ergonomics)
//
// Returns the token + a non-secret label identifying its source +
// an error if all three are empty.
func ResolveVaultToken(tokenLiteral, tokenFile string) (string, VaultTokenSource, error) {
	if tokenLiteral != "" {
		return tokenLiteral, VaultTokenSourceLiteral, nil
	}
	if tokenFile != "" {
		raw, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", "", fmt.Errorf("reading -signerVaultTokenFile: %w", err)
		}
		t := strings.TrimSpace(string(raw))
		if t == "" {
			return "", "", fmt.Errorf("token file %s is empty", tokenFile)
		}
		return t, VaultTokenSourceFile, nil
	}
	if t := os.Getenv("VAULT_TOKEN"); t != "" {
		return t, VaultTokenSourceEnv, nil
	}
	return "", "", fmt.Errorf("no vault token configured (set -signerVaultToken, -signerVaultTokenFile, or VAULT_TOKEN env)")
}
