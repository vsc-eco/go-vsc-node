package ipc_host

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	stdio_ipc "vsc-node/lib/stdio-ipc"
	ipc_common "vsc-node/lib/stdio-ipc/common"
	"vsc-node/lib/stdio-ipc/messages"
	"vsc-node/modules/wasm/ipc_requests"

	"github.com/JustinKnueppel/go-result"
	"github.com/moznion/go-optional"
)

func Run[Result any](executable string, args ...string) result.Result[Result] {
	return RunWithContext[Result](context.Background(), executable, args...)
}

func RunWithContext[Result any](ctx context.Context, executable string, args ...string) result.Result[Result] {
	return RunWithContextAndTypeMap(ctx, messages.MessageTypes[Result](), executable, args...)
}

func RunWithContextAndTypeMap[Result any](ctx context.Context, typeMap map[string]ipc_requests.Message[Result], executable string, args ...string) result.Result[Result] {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	return result.AndThen[stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]](
		result.AndThen(
			newCommandIO(cmd, typeMap),
			func(cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]) result.Result[stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]] {
				stderr, _ := cmd.StderrPipe()
				go io.Copy(os.Stderr, stderr)
				res := resultWrap(cio, cmd.Start())
				// time.Sleep(time.Second)
				// err, _ := io.ReadAll(stderr)
				fmt.Fprintf(os.Stderr, "err: ")
				// io.Copy(os.Stderr, stderr)
				return res
			},
		),
		func(cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]) result.Result[Result] {
			return ExecuteCommand(ctx, cio)
		},
	)
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

func newCommandIO[Result any](cmd *exec.Cmd, typeMap map[string]ipc_requests.Message[Result]) result.Result[stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]] {
	return result.AndThen(
		resultWrap(cmd.StdoutPipe()),
		func(stdout io.ReadCloser) result.Result[stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]] {
			return result.AndThen(
				resultWrap(cmd.StdinPipe()),
				func(stdin io.WriteCloser) result.Result[stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]] {
					// stdin.Write([]byte("[\n"))
					fmt.Fprintln(os.Stderr, "stdout:")
					// io.Copy(os.Stderr, stdout)
					res := stdio_ipc.NewJsonConnection(stdin, stdout, typeMap)
					fmt.Fprintln(os.Stderr, "connection")
					return res
				})
		})
}

type parseLoopInfo[Result any] struct {
	res  Result
	done bool
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

func ExecuteCommand[Result any](ctx context.Context, cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]) result.Result[Result] {
	return result.Map(
		parseMessages(ctx, cio),
		func(state parseLoopInfo[Result]) Result {
			return state.res
		},
	)
}

func processMessage[Result any](ctx context.Context, cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]) result.Result[optional.Pair[stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]], ipc_requests.ProcessedMessage[Result]]] {
	return result.Map(
		ipc_common.ProcessMessage(ctx, cio),
		func(pm ipc_requests.ProcessedMessage[Result]) optional.Pair[stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]], ipc_requests.ProcessedMessage[Result]] {
			return optionalPair(cio, pm)
		},
	)
}

func parseResult[Result any](pm ipc_requests.ProcessedMessage[Result]) result.Result[parseLoopInfo[Result]] {
	return result.Ok(
		optional.MapOr(
			pm.Result,
			parseLoopInfo[Result]{},
			func(value Result) parseLoopInfo[Result] {
				return parseLoopInfo[Result]{value, true}
			},
		),
	)
}

func handleProcessedMessage[Result any](pair optional.Pair[stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]], ipc_requests.ProcessedMessage[Result]]) result.Result[parseLoopInfo[Result]] {
	cio := pair.Value1
	pm := pair.Value2
	return result.AndThen(
		ipc_common.SendResponse(cio, pm),
		parseResult,
	)
}

func parseMessage[Result any](ctx context.Context, cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]) result.Result[parseLoopInfo[Result]] {
	return result.AndThen(
		processMessage(ctx, cio),
		handleProcessedMessage,
	)
}

func parseMessages[Result any](ctx context.Context, cio stdio_ipc.Connection[ipc_requests.Message[Result], ipc_requests.Message[Result]]) result.Result[parseLoopInfo[Result]] {
	state := result.Ok(parseLoopInfo[Result]{})
	for !state.UnwrapOr(parseLoopInfo[Result]{done: true}).done && !cio.Finished() {
		state = parseMessage(ctx, cio)
	}
	return state
}
