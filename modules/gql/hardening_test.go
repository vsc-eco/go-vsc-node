package gql

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vsc-node/modules/gql/gqlgen"
)

// End-to-end reproduction of pentest finding F15: the GraphQL HTTP
// layer was missing standard browser-security response headers
// (HSTS, X-Content-Type-Options, X-Frame-Options, Content-Security
// -Policy). The test wires the actual gqlManager.Init pipeline (no
// shortcuts), builds the same handler the production server uses,
// and drives a real HTTP request against it via httptest. Pre-fix
// (develop): responses lack every security header. Post-fix:
// security headers are present on every response unconditionally.
//
// F9 (CORS allowlist) and F12 (introspection gating) were originally
// in this commit but were dropped during review — those concerns
// are handled at the proxy layer instead.

func makeTestHandler(t *testing.T) http.Handler {
	t.Helper()
	schema := gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: &gqlgen.Resolver{}})
	g := New(schema, NewGqlConfig())
	if err := g.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if g.server == nil || g.server.Handler == nil {
		t.Fatal("gqlManager.Init did not produce a handler")
	}
	return g.server.Handler
}

// truncate is a tiny test helper used by hardening + alias-limit
// suites for capping verbose response bodies in error output.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// clearHardeningEnv used to scrub env vars that gated production-mode
// hardening (F9, F12, etc). Those flags were dropped during PR review;
// the helper is kept as a no-op so existing test files (e.g.
// alias_limit_test.go) that call it for hygiene continue to compile.
func clearHardeningEnv(t *testing.T) {
	t.Helper()
}

// F15: security headers must be present on every response.
func TestF15_SecurityHeadersAlwaysPresent(t *testing.T) {
	h := makeTestHandler(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/graphql",
		strings.NewReader(`{"query":"{ __schema { queryType { name } } }"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	missing := []string{}
	for _, hdr := range []string{
		"Strict-Transport-Security",
		"X-Content-Type-Options",
		"X-Frame-Options",
		"Content-Security-Policy",
	} {
		if rec.Header().Get(hdr) == "" {
			missing = append(missing, hdr)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("F15 leak: missing security headers: %v\nfull headers: %+v",
			missing, rec.Header())
	}
}
