package ipc_common

import (
	"context"
	stdio_ipc "vsc-node/lib/stdio-ipc"
	"vsc-node/modules/wasm/ipc_requests"

	"github.com/JustinKnueppel/go-result"
	"github.com/moznion/go-optional"
)

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}

func ProcessMessage[Result any](ctx context.Context, cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]) result.Result[ipc_requests.ProcessedMessage[Result]] {
	var m ipc_requests.Message[Result]
	return result.AndThen(
		resultWrap(m, cio.Receive(&m)),
		func(m ipc_requests.Message[Result]) result.Result[ipc_requests.ProcessedMessage[Result]] {
			return m.Process(ctx)
		},
	)
}

func SendResponse[Result any](cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]], pm ipc_requests.ProcessedMessage[Result]) result.Result[ipc_requests.ProcessedMessage[Result]] {
	return result.Map(
		result.Err[any](
			optional.Map(
				pm.Response,
				func(m ipc_requests.Message[Result]) error {
					return cio.Send(m)
				},
			).TakeOr(nil),
		),
		func(any) ipc_requests.ProcessedMessage[Result] {
			return pm
		},
	)
}
