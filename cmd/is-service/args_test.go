package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Round-9 audit R9-TEST-COV-01: validateOperatorURL is the
// startup-gate that rejects URL-flag misconfig BEFORE the value
// reaches the IS-service runtime or any log line. Pin every
// rejection branch + happy-path so a future relaxation can't
// silently re-open the R7-OP-01-logleak surface.
func TestValidateOperatorURL(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantErr  bool
		errMatch string // substring expected in err.Error
	}{
		// Happy path.
		{"empty-ok", "", false, ""},
		{"clean-https", "https://gql.example.org/graphql", false, ""},
		{"clean-https-port", "https://gql.example.org:8443/graphql", false, ""},
		{"clean-http", "http://gql.example.org/graphql", false, ""},
		// Scheme rejections.
		{"missing-scheme", "gql.example.org/graphql", true, "scheme"},
		{"opaque-userinfo", "user:pass@host:8080/api", true, "scheme"},
		{"ftp-scheme", "ftp://gql.example.org", true, "scheme"},
		{"javascript-scheme", "javascript://attacker", true, "scheme"},
		// Host rejection.
		{"empty-host", "https://", true, "host"},
		// Userinfo rejection.
		{"userinfo", "https://user:pass@gql.example.org/graphql", true, "credentials"},
		// Query / fragment rejection.
		{"with-query", "https://gql.example.org/?token=secret", true, "query"},
		{"with-fragment", "https://gql.example.org/#frag", true, "fragment"},
		// Unparseable.
		{"control-byte", "https://gql.example.org/\x00leak", true, "parseable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateOperatorURL("-testFlag", c.in)
			if c.wantErr {
				assert.Error(t, err, "input %q must be rejected", c.in)
				if err != nil && c.errMatch != "" {
					assert.True(t,
						strings.Contains(strings.ToLower(err.Error()), c.errMatch),
						"err %q must contain %q", err.Error(), c.errMatch)
				}
			} else {
				assert.NoError(t, err, "input %q must be accepted", c.in)
			}
		})
	}
}
