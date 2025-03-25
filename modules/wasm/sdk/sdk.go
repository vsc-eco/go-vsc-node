package sdk

import (
	"context"
	"fmt"
	wasm_context "vsc-node/modules/wasm/context"

	"github.com/JustinKnueppel/go-result"
)

var SdkModule = map[string]func(context.Context, any) result.Result[string]{
	"db.getObject": func(ctx context.Context, a any) result.Result[string] {
		/*execValue :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		s, ok := a.(string)
		if !ok {
			return result.Err[string](fmt.Errorf("invalid argument"))
		}
		return result.Ok(s)
	},
}
