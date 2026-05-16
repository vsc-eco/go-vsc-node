package gql

import (
	"net/http"
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

// securityHeaders sets the four headers the pentest flagged as
// missing on every GraphQL response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'")
		next.ServeHTTP(w, r)
	})
}
