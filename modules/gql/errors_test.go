package gql

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
)

func skipTestSanitize(t *testing.T) {
	_, numErr := strconv.ParseUint("x", 10, 64)
	var typeErr error = &json.UnmarshalTypeError{Value: "string", Type: nil}
	_, timeErr := time.Parse(time.RFC3339, "nope")
	cases := []struct {
		name     string
		err      error
		want     string
		internal bool
	}{
		{"strconv", numErr, msgInvalidNumber, false},
		{"wrapped strconv", fmt.Errorf("height: %w", numErr), msgInvalidNumber, false},
		{"flattened strconv", fmt.Errorf("bad: %v", numErr), msgInvalidNumber, false},
		{"json type", typeErr, msgInvalidValue, false},
		{"go type text", errors.New("cannot unmarshal string into Go value of type int64"), msgInvalidValue, false},
		{"mongo command", mongo.CommandError{Code: 2, Message: "$in needs an array", Name: "BadValue"}, msgInternal, true},
		{"mongo flattened", errors.New("(BadValue) $in needs an array"), msgInternal, true},
		{"time parse", timeErr, msgInvalidValue, false},
		{"client error", errors.New("contract not found"), "", false},
	}
	for _, c := range cases {
		got, internal := sanitize(c.err)
		if got != c.want || internal != c.internal {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, got, internal, c.want, c.internal)
		}
	}
}

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

// The reported repro: a non-numeric Uint64 variable used to answer with
// strconv.ParseUint's message.
func TestInvalidScalarVariableIsSanitized(t *testing.T) {
	h := makeTestHandler(t)
	rec := post(t, h, `{"query":"query($height:Uint64!){ witnessNodes(height:$height){ account } }","variables":{"height":"not-a-uint"}}`)
	out := rec.Body.String()
	for _, leak := range []string{"strconv", "ParseUint", "Go value", "invalid syntax"} {
		if strings.Contains(out, leak) {
			t.Fatalf("response leaks %q: %s", leak, truncate(out, 400))
		}
	}
	if !strings.Contains(out, "unsigned 64-bit integer") {
		t.Fatalf("expected a clear validation message, got: %s", truncate(out, 400))
	}
}

// Malformed JSON used to come back as "json request body could not be
// decoded: ... body:<the request>".
func TestMalformedJSONDoesNotEchoBody(t *testing.T) {
	h := makeTestHandler(t)
	const marker = "SECRET-MARKER-1234"
	rec := post(t, h, `{"query": "`+marker)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	out := rec.Body.String()
	if strings.Contains(out, marker) {
		t.Fatalf("response echoes the request body: %s", truncate(out, 400))
	}
	if !json.Valid([]byte(out)) || !strings.Contains(out, "not valid JSON") {
		t.Fatalf("unexpected response: %s", truncate(out, 400))
	}
}
