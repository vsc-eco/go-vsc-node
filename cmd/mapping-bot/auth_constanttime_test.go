package main

import (
	"os"
	"regexp"
	"testing"
)

// TestAuthorizedBearer_Behavior is the GO-22 GREEN behavioral test: the bearer
// auth helper must accept the exact valid key, reject any wrong/missing key,
// and treat an empty configured key as "deny all".
func TestAuthorizedBearer_Behavior(t *testing.T) {
	const key = "s3cr3t-api-key"
	cases := []struct {
		name       string
		authHeader string
		apiKey     string
		want       bool
	}{
		{"valid bearer", "Bearer " + key, key, true},
		{"wrong key", "Bearer wrong", key, false},
		{"missing header", "", key, false},
		{"no bearer prefix", key, key, false},
		{"trailing space", "Bearer " + key + " ", key, false},
		{"empty configured key denies all", "Bearer ", "", false},
		{"empty configured key with header", "Bearer anything", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := authorizedBearer(c.authHeader, c.apiKey); got != c.want {
				t.Fatalf("authorizedBearer(%q, %q) = %v, want %v", c.authHeader, c.apiKey, got, c.want)
			}
		})
	}
}

// TestNoPlaintextBearerComparison is the GO-22 RED gate: it fails if any
// plaintext `== "Bearer "+apiKey` / `!= "Bearer "+apiKey` comparison remains in
// http_server.go. Pre-fix this test is RED; post-fix (constant-time
// subtle.ConstantTimeCompare via authorizedBearer) it is GREEN.
func TestNoPlaintextBearerComparison(t *testing.T) {
	src, err := os.ReadFile("http_server.go")
	if err != nil {
		t.Fatalf("read http_server.go: %v", err)
	}
	bad := regexp.MustCompile(`[!=]=\s*"Bearer "\s*\+\s*apiKey`)
	if loc := bad.FindIndex(src); loc != nil {
		t.Fatalf("plaintext bearer comparison still present in http_server.go at byte offset %d: %q",
			loc[0], string(src[loc[0]:loc[1]]))
	}
}
