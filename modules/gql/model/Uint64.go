package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

type Uint64 uint64

func (b Uint64) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, uint64(b))
}

// errUint64 is the one message a client gets for any bad Uint64 input. The
// parser's own error ("strconv.ParseUint: parsing ...") and Go type names
// are implementation detail and stay out of the response.
var errUint64 = errors.New("must be an unsigned 64-bit integer (0 to 18446744073709551615)")

func (b *Uint64) UnmarshalGQL(v any) error {
	var u uint64
	switch v := v.(type) {
	case string:
		p, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return errUint64
		}
		u = p
	case uint64:
		u = v
	case int64:
		// A negative value used to wrap silently to a huge unsigned one.
		if v < 0 {
			return errUint64
		}
		u = uint64(v)
	case json.Number:
		p, err := strconv.ParseUint(string(v), 10, 64)
		if err != nil {
			return errUint64
		}
		u = p
	default:
		return errUint64
	}
	*b = Uint64(u)
	return nil
}
