package gqlgen

import (
	"encoding/base64"
	"fmt"
)

// decodeSubmittedB64 accepts any common base64 alphabet/padding (standard or
// URL-safe, padded or raw). Frontends using btoa() emit standard base64 with
// + and /, which base64.URLEncoding rejects; this superset accepts all of them
// without ambiguity (Std/URL only diverge on 2 symbols the other rejects).
//
// This helper deliberately lives outside schema.resolvers.go: gqlgen rewrites
// that file on every `make generate` and strips non-resolver functions from
// it. Files other than schema.resolvers.go are never touched by codegen.
func decodeSubmittedB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("illegal base64 data: not valid standard or url-safe base64")
}
