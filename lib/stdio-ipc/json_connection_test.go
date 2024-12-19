package stdio_ipc_test

import (
	"bytes"
	"encoding/json"
	"testing"
	stdio_ipc "vsc-node/lib/stdio-ipc"
	"vsc-node/modules/wasm/ipc_requests"

	"github.com/JustinKnueppel/go-result"
	"github.com/moznion/go-optional"
	"github.com/stretchr/testify/assert"
)

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}

func assertResultOk[T any](t *testing.T, res result.Result[T]) T {
	return res.MapErr(func(err error) error {
		assert.Nil(t, err)
		return nil
	}).Unwrap()
}

type m1 struct{}

// Process implements ipc_requests.Message.
func (m *m1) UnmarshalJSON([]byte) error {
	return nil
}

// Process implements ipc_requests.Message.
func (m *m1) Process() result.Result[ipc_requests.ProcessedMessage[string]] {
	return result.Ok(ipc_requests.ProcessedMessage[string]{
		Result:   optional.Some("result from m1"),
		Response: optional.Some[ipc_requests.Message[string]](&m1{}),
	})
}

type m2 struct{}

// Process implements ipc_requests.Message.
func (m *m2) UnmarshalJSON([]byte) error {
	return nil
}

// Process implements ipc_requests.Message.
func (m *m2) Process() result.Result[ipc_requests.ProcessedMessage[string]] {
	return result.Ok(ipc_requests.ProcessedMessage[string]{
		Result:   optional.Some("result from m2"),
		Response: optional.Some[ipc_requests.Message[string]](&m1{}),
	})
}

var _ ipc_requests.Message[string] = &m1{}
var _ ipc_requests.Message[string] = &m2{}

func TestBasicJsonConnection(t *testing.T) {
	stdin := &bytes.Buffer{}
	out := resultWrap(json.Marshal([]ipc_requests.Message[string]{})).Unwrap()
	stdout := bytes.NewBuffer(out)
	cio := assertResultOk(t, stdio_ipc.NewJsonConnection(stdin, stdout, map[string]ipc_requests.Message[string]{
		"nil": nil,
	}))

	var m ipc_requests.Message[string]
	assert.Error(t, cio.Receive(&m), "EOF")

	assert.True(t, cio.Finished())

	assert.Nil(t, cio.Send(nil))

	assert.Nil(t, cio.Close())
	assert.Equal(t, stdin.String(), "[\n{\"Type\":\"nil\",\"Message\":null}\n]")
}

func TestDuplexJsonConnection(t *testing.T) {
	stdin := bytes.NewBuffer(make([]byte, 0))
	stdout := bytes.NewBuffer(make([]byte, 0))
	typeMap := map[string]ipc_requests.Message[string]{
		"m1": &m1{},
		"m2": &m2{},
	}
	cio1 := assertResultOk(t, stdio_ipc.NewJsonConnection(stdin, stdout, typeMap))
	cio2 := assertResultOk(t, stdio_ipc.NewJsonConnection(stdout, stdin, typeMap))

	var m ipc_requests.Message[string]
	assert.NoError(t, cio1.Send(&m2{}))
	assert.NoError(t, cio2.Receive(&m))
	assert.NotNil(t, m)
	assert.Equal(t, m, &m2{})
	res := assertResultOk(t, m.Process())
	resp, ok := res.Response.Unwrap().(*m1)
	assert.True(t, ok)
	resStr := res.Result.Unwrap()
	assert.Equal(t, resStr, "result from m2")
	assert.NoError(t, cio2.Send(resp))

	assert.NoError(t, cio1.Close())
	assert.NoError(t, cio2.Close())
}
