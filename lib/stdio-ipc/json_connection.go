package stdio_ipc

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sync"
	"vsc-node/modules/wasm/ipc_requests"

	"github.com/JustinKnueppel/go-result"
	"github.com/go-viper/mapstructure/v2"
	"github.com/moznion/go-optional"
)

type cmdIo[Result any] struct {
	dec   *jsonArrayDecoder
	enc   *jsonArrayEncoder
	types map[string]reflect.Type
}

// Close implements Connection.
func (c *cmdIo[Result]) Close() error {
	return resultGetErr(
		result.AndThen(
			result.Err[any](c.enc.Close()),
			func(any) result.Result[any] {
				return result.Err[any](c.dec.Close())
			},
		),
	)
}

// Finished implements Connection.
func (c *cmdIo[Result]) Finished() bool {
	return !c.dec.More()
}

// Receive implements Connection.
func (c *cmdIo[Result]) Receive(msg *ipc_requests.Message[Result]) (err error) {
	var m map[string]interface{}
	err = c.dec.Decode(&m)
	if err != nil {
		return
	}
	Type, ok := m["Type"]
	if !ok {
		err = fmt.Errorf("missing 'Type' field from incoming message")
		return
	}
	typeStr, ok := Type.(string)
	if !ok {
		err = fmt.Errorf("incoming message's 'Type' field is not a string")
		return
	}
	rt, ok := c.types[typeStr]
	if !ok {
		err = fmt.Errorf("type '%s' has not been registered as an incoming message type", typeStr)
		return
	}
	*msg = reflect.New(rt).Interface().(ipc_requests.Message[Result])
	err = mapstructure.Decode(m["Message"], msg)
	// fmt.Printf("debug vals:\n%+v\n\n%+v\n\n", m, *msg)
	return
}

// Send implements Connection.
func (c *cmdIo[Result]) Send(msg ipc_requests.Message[Result]) error {
	rt := reflect.TypeOf(msg)
	for rt != nil && rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	typeName := optional.None[string]()
	for k, v := range c.types {
		if rt == v {
			typeName = optional.Some(k)
		}
	}
	Type, err := typeName.Take()
	if err != nil {
		return fmt.Errorf("type name \"%s\" is not included in stdio_ipc.Connection's type map", rt.Name())
	}
	realMsg := Message[Result]{Type, msg}
	return c.enc.Encode(&realMsg)
}

var _ Connection[ipc_requests.Message[any], ipc_requests.Message[any]] = &cmdIo[any]{}

type Message[Result any] struct {
	Type    string
	Message ipc_requests.Message[Result]
}

func NewJsonConnection[Result any](stdin io.Writer, stdout io.Reader, typeMap map[string]ipc_requests.Message[Result]) result.Result[Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]] {
	types := make(map[string]reflect.Type, len(typeMap))
	for k, v := range typeMap {
		rt := reflect.TypeOf(v)
		for rt != nil && rt.Kind() == reflect.Pointer {
			rt = rt.Elem()
		}
		types[k] = rt
	}
	dec := NewJsonArrayDecoder(stdout)
	return result.Map(
		NewJsonArrayEncoder(stdin),
		func(enc *jsonArrayEncoder) Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]] {
			return &cmdIo[Result]{dec, enc, types}
		})
}

type jsonArrayDecoder struct {
	decoder *json.Decoder
	started bool
	lock    sync.Locker
}

var _ io.Closer = &jsonArrayDecoder{}

func NewJsonArrayDecoder(r io.Reader) *jsonArrayDecoder {
	return &jsonArrayDecoder{decoder: json.NewDecoder(r), started: false, lock: &sync.Mutex{}}
}

func (dec *jsonArrayDecoder) ensureStarted() result.Result[json.Token] {
	return result.AndThen(
		result.Ok(dec.started),
		func(started bool) result.Result[json.Token] {
			if !started {
				return result.Map(
					resultWrap(dec.decoder.Token()),
					func(tok json.Token) json.Token {
						dec.started = true
						return tok
					},
				)
			} else {
				return result.Err[json.Token](nil)
			}
		},
	)
}

func (dec *jsonArrayDecoder) More() bool {
	dec.lock.Lock()
	defer dec.lock.Unlock()
	if !dec.started {
		return true
	}
	buf, _ := io.ReadAll(dec.decoder.Buffered()) // This can't fail because the buffer is in memory
	return len(buf) != 0 || dec.decoder.More()
	// return dec.ensureStarted().IsOkAnd(func(t json.Token) bool {
	// 	return dec.decoder.More()
	// })
}

func (dec *jsonArrayDecoder) Decode(v any) error {
	dec.lock.Lock()
	defer dec.lock.Unlock()
	return resultGetErr(
		result.AndThen(
			dec.ensureStarted(),
			func(json.Token) result.Result[bool] {
				return resultWrap(true, dec.decoder.Decode(v))
			},
		),
	)
}

// Close implements io.Closer.
func (dec *jsonArrayDecoder) Close() error {
	dec.lock.Lock()
	defer dec.lock.Unlock()
	if !dec.started {
		return nil
	}
	tok, err := dec.decoder.Token()
	if err != nil {
		return fmt.Errorf("could not decode ending bracket: %v", err)
	}
	if del, ok := tok.(json.Delim); !ok || del != ']' {
		return fmt.Errorf("could not decode ending bracket: token %v", tok)
	}
	return nil
}

type jsonArrayEncoder struct {
	w       io.Writer
	encoder *json.Encoder
	started bool
	lock    sync.Locker
}

var _ io.Closer = &jsonArrayEncoder{}

func NewJsonArrayEncoder(w io.Writer) result.Result[*jsonArrayEncoder] {
	return result.Map(
		resultWrap(w.Write([]byte("[\n"))),
		func(int) *jsonArrayEncoder {
			return &jsonArrayEncoder{w: w, encoder: json.NewEncoder(w), started: false, lock: &sync.Mutex{}}
		},
	)
}

func (enc *jsonArrayEncoder) Encode(v any) error {
	enc.lock.Lock()
	defer enc.lock.Unlock()
	return resultGetErr(
		result.AndThen(
			result.AndThen(
				result.Ok(enc.started),
				func(started bool) result.Result[int] {
					if !started {
						enc.started = true
						return result.Ok(0)
					} else {
						return resultWrap(enc.w.Write([]byte(",\n"))) // TODO print a newline here?
					}
				},
			),
			func(written int) result.Result[bool] {
				return resultWrap(written != 0, enc.encoder.Encode(v))
			},
		),
	)
}

// Close implements io.Closer.
func (enc *jsonArrayEncoder) Close() (err error) {
	enc.lock.Lock()
	defer enc.lock.Unlock()
	_, err = enc.w.Write([]byte("]"))
	return
}

func resultGetErr[T any](r result.Result[T]) error {
	return result.MapOrElse(
		r,
		func(err error) error {
			return err
		},
		func(_ T) error {
			return nil
		})
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}
