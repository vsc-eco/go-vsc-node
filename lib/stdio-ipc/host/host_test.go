package ipc_host_test

import (
	"bytes"
	"context"
	"testing"
	stdio_ipc "vsc-node/lib/stdio-ipc"
	ipc_host "vsc-node/lib/stdio-ipc/host"
	"vsc-node/modules/wasm/ipc_requests"

	"github.com/JustinKnueppel/go-result"
	"github.com/moznion/go-optional"
	"github.com/stretchr/testify/assert"
)

func assertResultOk[T any](t *testing.T, res result.Result[T]) T {
	return res.MapErr(func(err error) error {
		assert.Nil(t, err)
		return nil
	}).Unwrap()
}

type m1 struct{}

// Process implements ipc_requests.Message.
func (m *m1) Process(context.Context) result.Result[ipc_requests.ProcessedMessage[string]] {
	return result.Ok(ipc_requests.ProcessedMessage[string]{
		Result:   optional.Some("result from m1"),
		Response: optional.Some[ipc_requests.Message[string]](&m1{}),
	})
}

type m2 struct{}

// Process implements ipc_requests.Message.
func (m *m2) Process(context.Context) result.Result[ipc_requests.ProcessedMessage[string]] {
	return result.Ok(ipc_requests.ProcessedMessage[string]{
		Result:   optional.Some("result from m2"),
		Response: optional.Some[ipc_requests.Message[string]](&m1{}),
	})
}

var _ ipc_requests.Message[string] = &m1{}
var _ ipc_requests.Message[string] = &m2{}

func TestBasicHost(t *testing.T) {
	stdin := bytes.NewBuffer(make([]byte, 0))
	stdout := bytes.NewBuffer(make([]byte, 0))
	typeMap := map[string]ipc_requests.Message[string]{
		"m1": &m1{},
		"m2": &m2{},
	}
	cio1 := assertResultOk(t, stdio_ipc.NewJsonConnection(stdin, stdout, typeMap))
	cio2 := assertResultOk(t, stdio_ipc.NewJsonConnection(stdout, stdin, typeMap))

	assert.NoError(t, cio1.Send(&m2{}))

	resStr := assertResultOk(t, ipc_host.ExecuteCommand(context.Background(), cio2))
	assert.Equal(t, resStr, "result from m2")

	assert.NoError(t, cio1.Close())
	assert.NoError(t, cio2.Close())

	assert.Equal(t, stdout.String(), "[\n{\"Type\":\"m1\",\"Message\":{}}\n]")
}
