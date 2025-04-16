package model

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
)

type Int64 int64

func (b Int64) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, int64(b))
}

func (b *Int64) UnmarshalGQL(v any) (err error) {
	var u int64
	switch v := v.(type) {
	case string:
		u, err = strconv.ParseInt(v, 10, 64)
	case int64:
		u = v
	case uint64:
		u = int64(v)
	case json.Number:
		u, err = strconv.ParseInt(string(v), 10, 64)
	default:
		err = fmt.Errorf("%T is not an Int64", v)
	}
	*b = Int64(u)
	return
}
