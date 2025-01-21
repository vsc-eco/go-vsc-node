package execute

import (
	"context"
	"fmt"
	wasm_context "vsc-node/modules/wasm/context"
	"vsc-node/modules/wasm/ipc_requests"
	"vsc-node/modules/wasm/sdk"

	result "github.com/JustinKnueppel/go-result"
	"github.com/moznion/go-optional"
)

type SdkCallRequest[Result any] struct {
	Function string
	Argument string
}

var _ ipc_requests.Message[any] = &SdkCallRequest[any]{}

// Process implements ipc_requests.Message.
func (s *SdkCallRequest[Result]) Process(ctx context.Context) result.Result[ipc_requests.ProcessedMessage[Result]] {
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

var ErrEmptySdkCallResponse = fmt.Errorf("empty sdk call response")

type SdkCallResponse[Result any] struct {
	Result *string
	Error  *string
}

var _ ipc_requests.Message[any] = &SdkCallResponse[any]{}

// Process implements ipc_requests.Message.
func (res *SdkCallResponse[Result]) Process(context.Context) result.Result[ipc_requests.ProcessedMessage[Result]] {
	return result.Map(
		resultWrap(optional.FromNillable(res.Result).Take()).MapErr(func(err error) error {
			if res.Error == nil {
				return ErrEmptySdkCallResponse
			}
			return fmt.Errorf("%s", *res.Error)
		}),
		func(res string) ipc_requests.ProcessedMessage[Result] {
			return any(ipc_requests.ProcessedMessage[string]{Result: optional.Some(res)}).(ipc_requests.ProcessedMessage[Result])
		},
	)
}

type ExecutionFinish[Result any] struct {
	Result *string
	Error  *string
}

var _ ipc_requests.Message[any] = &ExecutionFinish[any]{}

// Process implements ipc_requests.Message.
func (res *ExecutionFinish[Result]) Process(context.Context) result.Result[ipc_requests.ProcessedMessage[Result]] {
	return result.Map(
		resultWrap(optional.FromNillable(res.Result).Take()).MapErr(func(err error) error {
			if res.Error == nil {
				return ErrEmptySdkCallResponse
			}
			return fmt.Errorf("%s", *res.Error)
		}),
		func(res string) ipc_requests.ProcessedMessage[Result] {
			return any(ipc_requests.ProcessedMessage[string]{Result: optional.Some(res)}).(ipc_requests.ProcessedMessage[Result])
		},
	)
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}
