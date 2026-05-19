package gql

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// End-to-end reproduction of pentest finding W-C1.
//
// Bug: countSelections walked fragment spreads recursively but
// did not count the spread itself, so a chain of N empty
// fragments (fragment F1 { ...F2 } ... fragment FN { __typename })
// produced a total count of 1 and bypassed the alias / field
// selection cap. gqlparser/gqlgen still walked every fragment at
// execute time, which took ~55s with 3000 fragments and crashed
// the testnet node.
//
// Post-fix: either the fragment-definition cap (50) or the per-spread
// contribution to the selection count must reject the query before
// any execution work happens.
func TestWC1_FragmentChainDoSRejected(t *testing.T) {
	clearHardeningEnv(t)
	h := makeTestHandler(t)

	const fragments = 3000
	var b strings.Builder
	b.WriteString(`{"query":"query Q { ...F0 } `)
	for i := 0; i < fragments-1; i++ {
		b.WriteString("fragment F")
		b.WriteString(itoa(i))
		b.WriteString(" on Query { ...F")
		b.WriteString(itoa(i + 1))
		b.WriteString(" } ")
	}
	b.WriteString("fragment F")
	b.WriteString(itoa(fragments - 1))
	b.WriteString(" on Query { __typename }")
	b.WriteString(`"}`)
	body := b.String()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("W-C1 leak: handler took >60s to reject a %d-fragment chain", fragments)
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("W-C1 leak: handler took %v to reject a %d-fragment chain (want <2s)", elapsed, fragments)
	}

	resp := strings.ToLower(rec.Body.String())
	if strings.Contains(resp, `"__typename":"query"`) {
		t.Fatalf("W-C1 leak: fragment chain executed instead of being rejected: %s", truncate(resp, 200))
	}
	if !strings.Contains(resp, "fragment") &&
		!strings.Contains(resp, "alias") &&
		!strings.Contains(resp, "field selection") &&
		!strings.Contains(resp, "limit") {
		t.Fatalf("W-C1 fix not in effect: expected fragment / alias / limit error, got: %s",
			truncate(resp, 200))
	}
}

// W-C1: a legitimate query with a small handful of fragments
// must still pass the pre-parse fragmentDefLimiter and reach the
// resolver. This guards against the regex being too aggressive.
func TestWC1_NormalFragmentsNotBlocked(t *testing.T) {
	clearHardeningEnv(t)
	h := makeTestHandler(t)

	body := `{"query":"query Q { ...A ...B } fragment A on Query { __typename } fragment B on Query { __typename }"}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	resp := rec.Body.String()
	if !strings.Contains(resp, `"__typename":"Query"`) {
		t.Fatalf("W-C1 false positive: 2-fragment query was rejected: %s", truncate(resp, 200))
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
