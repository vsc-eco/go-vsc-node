package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round-9 audit R9-TEST-COV-01: validateOperatorURL is the
// startup-gate that rejects URL-flag misconfig BEFORE the value
// reaches the IS-service runtime or any log line. Pin every
// rejection branch + happy-path so a future relaxation can't
// silently re-open the R7-OP-01-logleak surface.
func TestValidateOperatorURL(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantErr  bool
		errMatch string // substring expected in err.Error
	}{
		// Happy path.
		{"empty-ok", "", false, ""},
		{"clean-https", "https://gql.example.org/graphql", false, ""},
		{"clean-https-port", "https://gql.example.org:8443/graphql", false, ""},
		{"clean-http", "http://gql.example.org/graphql", false, ""},
		// Round-10 audit R10-TEST-COV-BOUNDARY-01: production-realistic
		// operator inputs that the validator must accept.
		{"localhost-port", "https://localhost:3030", false, ""},
		{"ipv4", "https://192.0.2.1:8443/graphql", false, ""},
		{"ipv6-bracketed", "https://[::1]:8443", false, ""},
		// Scheme rejections.
		{"missing-scheme", "gql.example.org/graphql", true, "scheme"},
		// Round-10 audit R10-DRIFT-ARGSTEST-OPAQUE-USERINFO-WRONG-GATE:
		// renamed from "opaque-userinfo" — this triggers the missing-
		// scheme gate (the dedicated userinfo case below covers the
		// credentials gate).
		{"opaque-form-missing-scheme", "user:pass@host:8080/api", true, "scheme"},
		{"ftp-scheme", "ftp://gql.example.org", true, "scheme"},
		{"javascript-scheme", "javascript://attacker", true, "scheme"},
		// Host rejection.
		{"empty-host", "https://", true, "host"},
		// Userinfo rejection.
		{"userinfo", "https://user:pass@gql.example.org/graphql", true, "credentials"},
		// Query / fragment rejection.
		{"with-query", "https://gql.example.org/?token=secret", true, "query"},
		{"with-fragment", "https://gql.example.org/#frag", true, "fragment"},
		// Round-10 audit R10-SEC-PATH-SMUGGLE-01: percent-encoded
		// '?' / '#' / NUL in the path must be rejected too so a
		// future sanitiser regression that emits Path can't leak
		// smuggled secrets.
		{"encoded-question", "https://gql.example.org/path%3Ftoken=secret", true, "path"},
		{"encoded-hash", "https://gql.example.org/path%23frag", true, "path"},
		// Round-11 audit R11-INFO-PATH-SMUGGLE-DEFENSE-NARROW: the
		// reject set is C0 control bytes + DEL + ?/# so any
		// percent-encoded escape decoded by url.Parse fails too.
		{"encoded-tab", "https://gql.example.org/p%09ath", true, "path"},
		{"encoded-cr", "https://gql.example.org/p%0Dath", true, "path"},
		{"encoded-del", "https://gql.example.org/p%7Fath", true, "path"},
		// Round-12 audit R12-DRIFT-ARGS-COMMENT-SEMICOLON: matrix-
		// style ';' in path was the one delimiter the R11 comment
		// promised but the code skipped. Closed in R12.
		{"encoded-semicolon", "https://gql.example.org/path%3Btoken=secret", true, "path"},
		// Round-11 audit R11-INFO-PATH-SMUGGLE-TEST-COVERAGE-DOUBLE-ENCODED:
		// pin the Go-url-decodes-once contract — a double-encoded
		// '?' (%253F) decodes to literal '%3F' in u.Path, which is
		// not in the reject set and not interpreted as a delimiter
		// by net/http on the way out. Acceptable: the smuggled byte
		// never reaches the network as a real '?'.
		{"double-encoded-question-ok", "https://gql.example.org/path%253Ftoken", false, ""},
		// Unparseable.
		{"control-byte", "https://gql.example.org/\x00leak", true, "parseable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateOperatorURL("-testFlag", c.in)
			if c.wantErr {
				assert.Error(t, err, "input %q must be rejected", c.in)
				if err != nil && c.errMatch != "" {
					assert.True(t,
						strings.Contains(strings.ToLower(err.Error()), c.errMatch),
						"err %q must contain %q", err.Error(), c.errMatch)
				}
			} else {
				assert.NoError(t, err, "input %q must be accepted", c.in)
			}
		})
	}
}

// withArgs runs fn with os.Args overridden, restoring after.
func withArgs(t *testing.T, args []string, fn func()) {
	t.Helper()
	orig := os.Args
	t.Cleanup(func() { os.Args = orig })
	os.Args = args
	fn()
}

// TestParseArgs_RejectsLiteralVaultToken covers audit SEC-6 (R15,
// MEDIUM, security) + R16-SEC-sec6-testnet-not-gated (R16, widened
// the gate). A literal -signerVaultToken leaks via
// /proc/<pid>/cmdline, ps, systemd journal, and container-
// orchestrator inspect surfaces. Devnet is the ONLY mode that keeps
// the literal path (local-dev ergonomics; no production infra to
// leak to). Mainnet + real testnet both refuse.
//
// Audit R17-CONS-test-fn-name-mainnet-only-after-r16-widened: the
// function used to be named TestParseArgs_RejectsLiteralVaultToken-
// OnMainnet but R16 widened the gate beyond mainnet — renamed to
// drop the "OnMainnet" suffix.
func TestParseArgs_RejectsLiteralVaultToken(t *testing.T) {
	baseArgs := []string{
		"is-service",
		"-primaryPubkey=02" + strings.Repeat("aa", 32),
		"-backupPubkey=03" + strings.Repeat("bb", 32),
		"-signerVaultAddr=https://vault.internal:8200",
		"-signerVaultKeyName=is-service-signer",
	}

	t.Run("mainnet refuses literal token", func(t *testing.T) {
		args := append([]string(nil), baseArgs...)
		args = append(args,
			"-network=mainnet",
			"-chainID=vsc-mainnet",
			"-signerVaultToken=hvs.SECRETSECRETSECRET",
		)
		withArgs(t, args, func() {
			_, err := parseArgs()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "signerVaultToken")
			assert.Contains(t, err.Error(), "mainnet")
		})
	})

	t.Run("testnet refuses literal token (R16 widened gate)", func(t *testing.T) {
		// R16-SEC-sec6-testnet-not-gated: testnet now also refuses
		// because real testnet runs on operator infra with the same
		// leak surface (ps, journal, kubectl describe, etc.) as mainnet.
		args := append([]string(nil), baseArgs...)
		args = append(args,
			"-network=testnet",
			"-chainID=vsc-testnet",
			"-signerVaultToken=hvs.SECRETSECRETSECRET",
		)
		withArgs(t, args, func() {
			_, err := parseArgs()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "signerVaultToken")
			assert.Contains(t, err.Error(), "testnet")
		})
	})

	t.Run("devnet still allows literal token", func(t *testing.T) {
		args := append([]string(nil), baseArgs...)
		args = append(args,
			"-network=devnet",
			"-chainID=vsc-devnet",
			"-signerVaultToken=hvs.SECRETSECRETSECRET",
		)
		withArgs(t, args, func() {
			_, err := parseArgs()
			// Devnet is the only mode that keeps the literal path
			// (local-dev ergonomics; no production infra to leak to).
			if err != nil {
				assert.NotContains(t, strings.ToLower(err.Error()), "signervaulttoken",
					"devnet must NOT reject on the SEC-6 gate; got %v", err)
			}
		})
	})

	t.Run("mainnet allows token file", func(t *testing.T) {
		args := append([]string(nil), baseArgs...)
		args = append(args,
			"-network=mainnet",
			"-chainID=vsc-mainnet",
			"-signerVaultTokenFile=/etc/is-service/vault.token",
		)
		withArgs(t, args, func() {
			_, err := parseArgs()
			// May still error on other fields, but NOT on signerVaultToken.
			if err != nil {
				assert.NotContains(t, strings.ToLower(err.Error()), "signervaulttoken",
					"mainnet must accept -signerVaultTokenFile (the safe path); got %v", err)
			}
		})
	})
}
