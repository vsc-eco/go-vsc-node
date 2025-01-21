package sdk

import (
	"context"
	wasm_context "vsc-node/modules/wasm/context"

	"github.com/JustinKnueppel/go-result"
)

var SdkModule = map[string]func(context.Context, string) result.Result[string]{
	"db.getObject": func(ctx context.Context, a string) result.Result[string] {
		/*execValue :*/ _ = ctx.Value(wasm_context.WasmExecCtxKey).(wasm_context.ExecContextValue)
		return result.Ok("test")
	},
}
