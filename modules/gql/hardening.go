package gql

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"regexp"
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

// W-C1 pre-parse defense: gqlparser's fragment-cycle validator is
// O(F^2) in the number of fragment definitions per document. A
// ~109KB payload with 3000 fragments takes >25s to validate, well
// before any gqlgen extension (AliasLimit, FixedComplexityLimit)
// runs. The AliasLimit extension does also bound fragment cost
// once it gets to run, but we need a cheaper, earlier gate.
//
// fragmentDefRegex matches the GraphQL "fragment NAME on" token
// triple. The pattern is precise enough that legitimate field /
// variable names will not match, and we cap the scan at
// DefaultMaxFragmentDefinitions+1 matches so cost is bounded
// regardless of body size.
var fragmentDefRegex = regexp.MustCompile(`\bfragment\s+[A-Za-z_][A-Za-z0-9_]*\s+on\b`)

// fragmentDefLimiter rejects POST bodies that contain more than
// DefaultMaxFragmentDefinitions fragment definitions before
// gqlparser sees them. The body is buffered in memory so the
// downstream handler can still read it; MaxRequestBodyBytes
// upstream caps that buffer at 1 MiB.
func fragmentDefLimiter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// Most common cause here is the upstream MaxBytesHandler
			// tripping its cap; surface that as a 413, otherwise 400.
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		matches := fragmentDefRegex.FindAllIndex(body, DefaultMaxFragmentDefinitions+1)
		if len(matches) > DefaultMaxFragmentDefinitions {
			http.Error(w,
				`{"errors":[{"message":"operation has too many fragment definitions"}]}`,
				http.StatusBadRequest)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		next.ServeHTTP(w, r)
	})
}
