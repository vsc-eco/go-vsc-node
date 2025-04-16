package model

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

type Uint64 uint64

func (b Uint64) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, uint64(b))
}

func (b *Uint64) UnmarshalGQL(v any) (err error) {
	var u uint64
	switch v := v.(type) {
	case string:
		u, err = strconv.ParseUint(v, 10, 64)
	case uint64:
		u = v
	case int64:
		u = uint64(v)
	case json.Number:
		u, err = strconv.ParseUint(string(v), 10, 64)
	default:
		err = fmt.Errorf("%T is not a Uint64", v)
	}
	*b = Uint64(u)
	return
}
