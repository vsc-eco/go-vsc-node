package model

import (
	"errors"
	"io"
	"time"

	"github.com/99designs/gqlgen/graphql"
)

// MarshalDateTime converts time.Time to ISO 8601 string.
func MarshalDateTime(t time.Time) graphql.Marshaler {
	return graphql.WriterFunc(func(w io.Writer) {
		iso := t.Format(time.RFC3339)    // RFC3339 is ISO 8601 compliant
		w.Write([]byte(`"` + iso + `"`)) // Wrap in quotes for JSON
	})
}

var errDateTime = errors.New("must be an RFC 3339 date-time, e.g. 2026-01-02T15:04:05Z")

// UnmarshalDateTime parses ISO 8601 string to time.Time. time.Parse's own
// error spells out Go's reference layout, so it is not returned to clients.
func UnmarshalDateTime(v interface{}) (time.Time, error) {
	str, ok := v.(string)
	if !ok {
		return time.Time{}, errDateTime
	}
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return time.Time{}, errDateTime
	}
	return t, nil
}
