package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

type Int64 int64

func (b Int64) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, int64(b))
}

// errInt64 is the one message a client gets for any bad Int64 input; see
// errUint64.
var errInt64 = errors.New("must be a signed 64-bit integer")

func (b *Int64) UnmarshalGQL(v any) error {
	var u int64
	switch v := v.(type) {
	case string:
		p, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return errInt64
		}
		u = p
	case int64:
		u = v
	case uint64:
		// Above MaxInt64 used to wrap silently to a negative value.
		if v > math.MaxInt64 {
			return errInt64
		}
		u = int64(v)
	case json.Number:
		p, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return errInt64
		}
		u = p
	default:
		return errInt64
	}
	*b = Int64(u)
	return nil
}
