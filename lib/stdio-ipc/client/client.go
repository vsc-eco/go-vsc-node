package ipc_client

import (
	"context"
	"fmt"
	"io"
	"os"

	stdio_ipc "vsc-node/lib/stdio-ipc"
	ipc_common "vsc-node/lib/stdio-ipc/common"
	"vsc-node/lib/stdio-ipc/messages"
	"vsc-node/modules/wasm/ipc_requests"

	"github.com/JustinKnueppel/go-result"
	"github.com/moznion/go-optional"
)

func Run[Result any]() result.Result[Client[Result]] {
	return RunWithStdio[Result](os.Stdin, os.Stdout)
}

func RunWithStdio[Result any](stdin io.Reader, stdout io.Writer) result.Result[Client[Result]] {
	return RunWithStdioAndContext[Result](context.Background(), stdin, stdout)
}

func RunWithStdioAndContext[Result any](ctx context.Context, stdin io.Reader, stdout io.Writer) result.Result[Client[Result]] {
	return RunWithStdioAndContextAndTypeMap(ctx, messages.MessageTypes[Result](), stdin, stdout)
}

func RunWithStdioAndContextAndTypeMap[Result any](ctx context.Context, typeMap map[string]ipc_requests.Message[Result], stdin io.Reader, stdout io.Writer) result.Result[Client[Result]] {
	return result.AndThen[stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]](
		stdio_ipc.NewJsonConnection(stdout, stdin, typeMap),
		newClient,
	)
}

type Client[Result any] interface {
	// Sends a message request then waits for a response.
	// The response is processed.
	// The processed response automatically sends its response if it exists.
	// And the raw processed response is return (including the automatically sent response).
	Request(msg ipc_requests.Message[Result]) result.Result[ipc_requests.ProcessedMessage[Result]]

	Send(msg ipc_requests.Message[Result]) result.Result[any]

	io.Closer
}

type client[Result any] struct {
	cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]
}

// Request implements Client.
func (c *client[Result]) Request(msg ipc_requests.Message[Result]) result.Result[ipc_requests.ProcessedMessage[Result]] {
	return result.AndThen(
		result.AndThen(
			resultWrap(c.cio, c.cio.Send(msg)),
			ipc_common.ProcessMessage[Result],
		),
		func(pm ipc_requests.ProcessedMessage[Result]) result.Result[ipc_requests.ProcessedMessage[Result]] {
			return ipc_common.SendResponse(c.cio, pm)
		},
	)
}

// Request implements Client.
func (c *client[Result]) Send(msg ipc_requests.Message[Result]) result.Result[any] {
	return result.Err[any](c.cio.Send(msg))
}

// Request implements Client.
func (c *client[Result]) Close() error {
	return c.cio.Close()
}

func newClient[Result any](cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]) result.Result[Client[Result]] {
	return result.Ok[Client[Result]](&client[Result]{cio})
}

var ErrResultTypeCast = fmt.Errorf("type cast")

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}

func resultWrapTypeCast[T any](res T, ok bool) result.Result[T] {
	if !ok {
		return result.Err[T](ErrResultTypeCast)
	}
	return result.Ok(res)
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

func optionalPair[T any, U any](left T, right U) optional.Pair[T, U] {
	return optional.Pair[T, U]{
		Value1: left,
		Value2: right,
	}
}

func resultToOptional[T any](res result.Result[T]) optional.Option[T] {
	if res.IsOk() {
		return optional.Some(res.Unwrap())
	}
	return optional.None[T]()
}

func resultErrToOptional[T any](res result.Result[T]) optional.Option[error] {
	if res.IsErr() {
		return optional.Some(res.UnwrapErr())
	}
	return optional.None[error]()
}

func resultToEmptyResult[T any](res result.Result[T]) emptyResult {
	return emptyResult{
		result.Map(
			res,
			func(T) empty {
				return empty{}
			},
		),
	}

}

type empty struct{}
type emptyResult struct {
	result.Result[empty]
}

func emptyResultMap[T any](res emptyResult, mapper func() T) result.Result[T] {
	return result.Map(res.Result, func(empty) T {
		return mapper()
	})
}

func emptyResultAndThen[T any](res emptyResult, mapper func() result.Result[T]) result.Result[T] {
	return result.AndThen(res.Result, func(empty) result.Result[T] {
		return mapper()
	})
}
