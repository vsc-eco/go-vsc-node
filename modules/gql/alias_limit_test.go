package gql

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// End-to-end reproduction of pentest finding F10.
//
// Bug: the GQL server only enforces gqlgen's FixedComplexityLimit.
// That gate looks at the per-field complexity calculation, which
// for a leaf field like `__typename` is 1 — so an attacker can
// pack 1000 aliases of a cheap field into one request, multiplying
// the work the server does without ever tripping the complexity
// limit. The pentest confirmed up to 1000 aliases of localNodeInfo
// in a single request.
//
// This test fires a 200-alias query of `__typename` (a meta-field
// that doesn't touch any resolver, so the all-nil Resolver from
// makeTestHandler is fine) at the production handler:
//
//   pre-fix: server returns data — the 200 aliases all execute.
//   post-fix: server returns an error mentioning the alias /
//             field-selection cap, with no data populated.

func TestF10_AliasAmplificationCapped(t *testing.T) {
	clearHardeningEnv(t)
	h := makeTestHandler(t)

	// Build a query with 200 aliased __typename selections.
	const aliases = 200
	var b strings.Builder
	b.WriteString(`{"query":"{`)
	for i := 0; i < aliases; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(" a")
		// numeric suffix
		b.WriteString(itoa(i))
		b.WriteString(": __typename")
	}
	b.WriteString(` }"}`)
	body := b.String()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	resp := rec.Body.String()
	// Pre-fix the response will have data:{"a0":"Query","a1":"Query",...}
	// Post-fix it will be an error mentioning the alias cap and have
	// no data field populated.
	if strings.Contains(resp, `"a199":"Query"`) ||
		strings.Contains(resp, `"a100":"Query"`) {
		t.Fatalf(
			"F10 leak: %d-alias query executed without rejection.\n"+
				"  response (truncated): %s",
			aliases, truncate(resp, 200))
	}
	if !strings.Contains(strings.ToLower(resp), "alias") &&
		!strings.Contains(strings.ToLower(resp), "field selection") &&
		!strings.Contains(strings.ToLower(resp), "limit") {
		t.Fatalf(
			"F10 fix not in effect: expected alias / field-selection / limit error, got: %s",
			truncate(resp, 200))
	}
}

func TestF10_NormalQueriesNotBlocked(t *testing.T) {
	clearHardeningEnv(t)
	h := makeTestHandler(t)

	// A plain 5-alias query is well under any reasonable cap and
	// must continue to work.
	body := `{"query":"{ a: __typename b: __typename c: __typename d: __typename e: __typename }"}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	resp := rec.Body.String()
	if !strings.Contains(resp, `"a":"Query"`) || !strings.Contains(resp, `"e":"Query"`) {
		t.Fatalf("normal 5-alias query was incorrectly rejected: %s", truncate(resp, 200))
	}
}

// itoa is a small inlined integer-to-string to avoid pulling in
// strconv just for the alias suffixes.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
