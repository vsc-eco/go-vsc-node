package gql

import (
	"net/http"
	"strings"
)

// Pentest finding F15: the GraphQL HTTP layer was missing standard
// browser-security response headers (HSTS, X-Content-Type-Options,
// X-Frame-Options, Content-Security-Policy). The middleware below
// is applied unconditionally — there is no operational reason for a
// JSON-only API to omit any of these.
//
// F9 (CORS allowlist) and F12 (introspection gating) were originally
// proposed alongside this in the same commit but were dropped during
// review; CORS / introspection policy is being handled at the proxy
// layer instead.

// sandboxCSP allows Apollo Sandbox to load its CDN bundle + inline
// scripts/styles. Scoped to /sandbox only so the JSON API keeps the
// strict default-src 'self' from F15.
const sandboxCSP = "default-src 'self'; " +
	"script-src 'self' https://embeddable-sandbox.cdn.apollographql.com 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' https://embeddable-sandbox.cdn.apollographql.com data:; " +
	"connect-src 'self' https://embeddable-sandbox.cdn.apollographql.com https:; " +
	"frame-src 'self' https://sandbox.embed.apollographql.com; " +
	"font-src 'self' data:"

// securityHeaders sets the four headers the pentest flagged as
// missing on every GraphQL response. /sandbox gets a CSP relaxed
// enough to let the Apollo embedded sandbox actually load; the
// JSON API path keeps the strict policy.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/sandbox") {
			h.Set("Content-Security-Policy", sandboxCSP)
		} else {
			h.Set("Content-Security-Policy", "default-src 'self'")
		}
		next.ServeHTTP(w, r)
	})
}
