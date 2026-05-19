package gql

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// strconvQuote wraps strconv.Quote so the test bodies stay readable.
func strconvQuote(s string) string { return strconv.Quote(s) }

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

// W-C1 follow-up: the colleague's payload that crashed two
// testnet nodes — 50 chained fragments on __Schema, each
// containing only a spread to the next, leaf resolving
// `types { name }`. The crash is a 2^N allocation in
// gqlgen's collectFields (executable_schema.go:81-89), see
// the file header in alias_limit.go for the full diagnosis.
//
// We never let the harness construct the unmitigated depth-50
// payload — that's an 8 PB allocation that would kill the test
// runner. We only construct depth 50 *after* confirming the
// limiter rejects it; if the limiter is regressed and forwards
// the payload, the test machine OOMs and the test author sees
// the same failure mode as the testnet node.
func TestWC1_ChainedFragmentsRejectedFast(t *testing.T) {
	clearHardeningEnv(t)
	h := makeTestHandler(t)

	const depth = 50
	var q strings.Builder
	q.WriteString(`{ __schema { ...f1 } } `)
	for i := depth; i >= 1; i-- {
		if i == depth {
			q.WriteString("fragment f")
			q.WriteString(itoa(i))
			q.WriteString(" on __Schema { types { name } } ")
		} else {
			q.WriteString("fragment f")
			q.WriteString(itoa(i))
			q.WriteString(" on __Schema { ...f")
			q.WriteString(itoa(i + 1))
			q.WriteString(" } ")
		}
	}
	body := `{"query":` + strconvQuote(q.String()) + `}`

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
	case <-time.After(2 * time.Second):
		// If we hit this branch the limiter is broken; fail fast
		// before gqlgen's collectFields can OOM the test runner.
		t.Fatalf("W-C1 LEAK: handler took >2s on a %d-fragment chain — gqlgen would OOM. Test aborted before that.", depth)
	}
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Fatalf("W-C1: handler took %v to reject a %d-fragment chain (want <100ms)", elapsed, depth)
	}
	resp := strings.ToLower(rec.Body.String())
	if strings.Contains(resp, `"types"`) || strings.Contains(resp, `"data":{"`) {
		t.Fatalf("W-C1 LEAK: chained-fragment payload reached the resolver: %s", truncate(resp, 200))
	}
	if !strings.Contains(resp, "fragment") && !strings.Contains(resp, "limit") && !strings.Contains(resp, "depth") {
		t.Fatalf("W-C1: rejection message looks wrong: %s", truncate(resp, 200))
	}
}

// W-C1 also blocks inline-fragment chains since they trigger
// the same gqlgen collectFields doubling bug. With a non-empty
// leaf (`types { name }`), this would also OOM at depth ~30.
func TestWC1_InlineFragmentChainRejected(t *testing.T) {
	clearHardeningEnv(t)
	h := makeTestHandler(t)

	const depth = 30
	var q strings.Builder
	q.WriteString(`{ __schema { `)
	for i := 0; i < depth; i++ {
		q.WriteString("... on __Schema { ")
	}
	q.WriteString("types { name } ")
	for i := 0; i < depth; i++ {
		q.WriteString("} ")
	}
	q.WriteString(`} }`)
	body := `{"query":` + strconvQuote(q.String()) + `}`

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
	case <-time.After(2 * time.Second):
		t.Fatalf("W-C1 LEAK: inline-fragment chain depth=%d not rejected in 2s — gqlgen would OOM. Test aborted.", depth)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("W-C1: inline-fragment chain took %v to reject (want <200ms)", elapsed)
	}
	resp := strings.ToLower(rec.Body.String())
	if strings.Contains(resp, `"data":{"__schema"`) {
		t.Fatalf("W-C1 LEAK: inline-fragment chain reached the resolver: %s", truncate(resp, 200))
	}
	if !strings.Contains(resp, "depth") && !strings.Contains(resp, "limit") {
		t.Fatalf("W-C1: rejection message looks wrong: %s", truncate(resp, 200))
	}
}

// W-C1: an Apollo Sandbox-style introspection query (3 named
// fragments, chain depth 2) must continue to work. This is the
// shape clients actually send when they open /sandbox, so it
// sits closest to our limits and is the most likely regression
// surface if we ever tighten the caps further.
func TestWC1_IntrospectionQueryStillWorks(t *testing.T) {
	clearHardeningEnv(t)
	h := makeTestHandler(t)

	q := `query Introspection { __schema {
		queryType { name }
		types { ...FullType }
	} }
	fragment FullType on __Type { kind name description fields { ...F } }
	fragment F on __Field { name description type { ...TR } }
	fragment TR on __Type { kind name ofType { kind name } }`
	body := `{"query":` + strconvQuote(q) + `}`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	resp := rec.Body.String()
	if !strings.Contains(resp, `"queryType"`) {
		t.Fatalf("introspection query was rejected by W-C1 caps: %s", truncate(resp, 200))
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
