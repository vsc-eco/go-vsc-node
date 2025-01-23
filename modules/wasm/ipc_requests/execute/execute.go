package execute

import (
	"context"
	"fmt"
	"os"
	wasm_context "vsc-node/modules/wasm/context"
	"vsc-node/modules/wasm/ipc_requests"
	"vsc-node/modules/wasm/sdk"

	result "github.com/JustinKnueppel/go-result"
	"github.com/moznion/go-optional"
)

type SdkCallRequest[Result any] struct {
	Function string
	Argument any
}

var _ ipc_requests.Message[any] = &SdkCallRequest[any]{}

// Process implements ipc_requests.Message.
func (s *SdkCallRequest[Result]) Process(ctx context.Context) result.Result[ipc_requests.ProcessedMessage[Result]] {
	fmt.Fprintln(os.Stderr, "sdk call request", s)
	fn, ok := sdk.SdkModule[s.Function]
	if !ok {
		return result.Err[ipc_requests.ProcessedMessage[Result]](fmt.Errorf("vm requested non-existing function: %s", s.Function))
	}
	res := result.MapOrElse(
		fn(ctx, s.Argument),
		func(err error) *SdkCallResponse[Result] {
			str := err.Error()
			return &SdkCallResponse[Result]{
				Error: &str,
			}
		},
		func(res string) *SdkCallResponse[Result] {
			return &SdkCallResponse[Result]{
				Result: &res,
			}
		},
	)
	return result.Ok(ipc_requests.ProcessedMessage[Result]{
		Response: optional.Some[ipc_requests.Message[Result]](res),
	})
}

type BasicErrorResult[Result any] struct {
	Result *string
	Error  *string
}

func (res BasicErrorResult[Result]) process(emptyErr error) result.Result[ipc_requests.ProcessedMessage[Result]] {
	return result.Map(
		resultWrap(optional.FromNillable(res.Result).Take()).MapErr(func(err error) error {
			if res.Error == nil {
				return emptyErr
			}
			return fmt.Errorf("%s", *res.Error)
		}),
		func(res string) ipc_requests.ProcessedMessage[Result] {
			return any(ipc_requests.ProcessedMessage[string]{Result: optional.Some(res)}).(ipc_requests.ProcessedMessage[Result])
		},
	)
}

var ErrEmptySdkCallResponse = fmt.Errorf("empty sdk call response")

type SdkCallResponse[Result any] BasicErrorResult[Result]

var _ ipc_requests.Message[any] = &SdkCallResponse[any]{}

// Process implements ipc_requests.Message.
func (res *SdkCallResponse[Result]) Process(context.Context) result.Result[ipc_requests.ProcessedMessage[Result]] {
	return BasicErrorResult[Result](*res).process(ErrEmptySdkCallResponse)
}

var ErrEmptyExecutionFinish = fmt.Errorf("empty execution finish response")

type ExecutionFinish[Result any] BasicErrorResult[Result]

var _ ipc_requests.Message[any] = &ExecutionFinish[any]{}

// Process implements ipc_requests.Message.
func (res *ExecutionFinish[Result]) Process(context.Context) result.Result[ipc_requests.ProcessedMessage[Result]] {
	return BasicErrorResult[Result](*res).process(ErrEmptyExecutionFinish)
}

type ExecutionReady[Result any] struct{}

var _ ipc_requests.Message[any] = &ExecutionReady[any]{}

// Process implements ipc_requests.Message.
func (res *ExecutionReady[Result]) Process(ctx context.Context) result.Result[ipc_requests.ProcessedMessage[Result]] {
	return result.Ok(ipc_requests.ProcessedMessage[Result]{
		Response: optional.Some(ipc_requests.Message[Result](&ExecutionCode[Result]{
			Code: ctx.Value(wasm_context.WasmExecCodeCtxKey).(string),
		})),
	})
}

type ExecutionCode[Result any] struct {
	Code string
}

var _ ipc_requests.Message[any] = &ExecutionCode[any]{}

// Process implements ipc_requests.Message.
func (res *ExecutionCode[Result]) Process(context.Context) result.Result[ipc_requests.ProcessedMessage[Result]] {
	return result.Ok(ipc_requests.ProcessedMessage[Result]{
		Result: any(optional.Some(res.Code)).(optional.Option[Result]),
	})
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}
