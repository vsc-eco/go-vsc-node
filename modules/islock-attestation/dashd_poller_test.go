package islock_attestation

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSanitizeRPCURLForLog pins every redaction branch of the
// SanitizeRPCURLForLog helper. Audit R17-SEC-sanitizeRPCURL-leaks-on-
// parse-edge-cases (HIGH) found that the R16 implementation leaked
// credentials in three concrete ways:
//
//  1. Query-string secrets preserved verbatim
//  2. url.Parse-error fallback returned the raw input including
//     embedded userinfo
//  3. Opaque "user:pass@host" forms (empty scheme) rendered
//     userinfo via u.String()
//
// Each case below maps to one of those bypasses (plus the happy paths).
// The case grid mirrors orchestrator_test.go:TestSanitizeURLForLog so
// the two RPC vs GraphQL sanitisers can't drift apart.
func TestSanitizeRPCURLForLog(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// Happy paths.
		{"empty", "", ""},
		{"clean-http", "http://dash-rpc.internal:9998", "http://dash-rpc.internal:9998"},
		{"clean-https-with-path", "https://dash-rpc.example.com/rpc", "https://dash-rpc.example.com"},

		// Bypass-1: query-string secret. Used by Cloudflare /
		// HashiCorp HCP / service-mesh RPC fronts.
		{"strip-query-token", "https://dash.rpc.internal/?token=hvs.SECRET", "https://dash.rpc.internal"},
		{"strip-query-apikey", "http://host:9998/rpc?api_key=secret&user=admin", "http://host:9998"},

		// Bypass-2: malformed inputs that pre-R17 hit the `return raw`
		// branch and emitted credentials verbatim. With the new
		// redact-on-parse-error contract, all of these must NOT
		// echo the input.
		{"unparseable-control-byte", "http://user:pa\x00ss@host", "<redacted: unparseable URL>"},
		{"unparseable-mixed-ipv6", "http://user:pass@[bad-ipv6/path", "<redacted: unparseable URL>"},

		// Bypass-3: parses but no scheme/host → opaque form. Pre-R17
		// rendered "user:pass@host" via u.String().
		{"opaque-userinfo-missing-scheme", "user:pass@host:8080", "<redacted: missing scheme>"},
		{"plain-host-port-no-scheme", "host:9998", "<redacted: missing scheme>"},
		{"junk-no-url", "junk_no_url", "<redacted: missing scheme>"},

		// Userinfo (basic auth) strip — original R16 use case.
		{"strip-userinfo", "http://user:pass@dash-rpc:9998", "http://dash-rpc:9998"},
		{"strip-userinfo-https", "https://operator:s3cret@vault.internal:8200/v1/transit", "https://vault.internal:8200"},

		// Fragments are dropped too.
		{"strip-fragment", "http://dash-rpc:9998/rpc#secret", "http://dash-rpc:9998"},

		// IPv6 host preservation (bracket form must survive).
		{"ipv6-host", "http://[2001:db8::1]:9998", "http://[2001:db8::1]:9998"},
		{"ipv6-host-with-userinfo", "http://user:pass@[2001:db8::1]:9998", "http://[2001:db8::1]:9998"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizeRPCURLForLog(c.in)
			assert.Equal(t, c.want, got,
				"input %q must sanitise to %q (audit R17-SEC-sanitizeRPCURL-leaks-on-parse-edge-cases)",
				c.in, c.want)
			// Also: output MUST NOT contain the substring "pass" — if
			// my expected-value table is wrong, this catches a credential
			// leak that the table didn't account for.
			if c.in != c.want { // skip pass-through happy paths
				assert.NotContains(t, got, "pass",
					"output must never echo userinfo (input=%q)", c.in)
				assert.NotContains(t, got, "SECRET",
					"output must never echo query-string secrets (input=%q)", c.in)
				assert.NotContains(t, got, "hvs.",
					"output must never echo Vault tokens (input=%q)", c.in)
			}
		})
	}
}
