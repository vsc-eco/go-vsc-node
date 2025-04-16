package model

import (
	"fmt"
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

// UnmarshalDateTime parses ISO 8601 string to time.Time.
func UnmarshalDateTime(v interface{}) (time.Time, error) {
	str, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("expected string")
	}
	return time.Parse(time.RFC3339, str)
}
