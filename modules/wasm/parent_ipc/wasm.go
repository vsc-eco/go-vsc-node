package wasm_parent_ipc

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	ipc_host "vsc-node/lib/stdio-ipc/host"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_runtime "vsc-node/modules/wasm/runtime"
	wasm_types "vsc-node/modules/wasm/types"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
)

var (
	ErrDone error = fmt.Errorf("Done")
)

const DefaultExecPath = "./vm-runner"

type Wasm struct {
	ctx      context.Context
	cancel   context.CancelFunc
	execPath []string
}

var _ a.Plugin = &Wasm{}

func New(execPath ...string) *Wasm {
	ctx, cancel := context.WithCancel(context.Background())
	if len(execPath) == 0 {
		execPath = []string{DefaultExecPath}
	}
	return &Wasm{
		ctx,
		cancel,
		execPath,
	}
}

func (w *Wasm) Init() error {
	if uint64(math.MaxUint) != uint64(math.MaxUint64) {
		return fmt.Errorf("gas calculations require `uint` to 64-bit. This isn't supported on your machine")
	}

	// err := setup()
	// if err != nil {
	// 	return err
	// }
	return nil
}

func (w *Wasm) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

func (w *Wasm) Stop() error {
	w.cancel()
	return nil
}

// tera  1_000_000_000_000 per second
// TODO consider: https://cosmwasm.cosmos.network/core/architecture/gas
const timePer15_000GasUnits = 5 * time.Millisecond
const startupTime = 3000 * time.Millisecond // TODO investigate large startup time

func (w *Wasm) Execute(ctxValue wasm_context.ExecContextValue, byteCode []byte, gas uint, entrypoint string, args string, runtime wasm_runtime.Runtime) (res result.Result[wasm_types.WasmResultStruct]) {
	ctx, cancel := context.WithTimeout(context.WithValue(context.WithValue(w.ctx, wasm_context.WasmExecCtxKey, ctxValue), wasm_context.WasmExecCodeCtxKey, hex.EncodeToString(byteCode)), (timePer15_000GasUnits*time.Duration(gas)/15_000)+startupTime)
	defer cancel()
	defer func() {
		rec := recover()
		if rec != nil {
			err, ok := rec.(error)
			if !ok {
				err = fmt.Errorf("%v", rec)
			}
			res = result.Err[wasm_types.WasmResultStruct](err)
		}
	}()
	return ipc_host.RunWithContext[wasm_types.WasmResultStruct](ctx,
		w.execPath[0], append(w.execPath[1:],
			"-gas", fmt.Sprint(gas),
			"-entrypoint", entrypoint,
			// TODO move this to an IPC request since the cmd line arg length can be as little as 32KB
			// see http://stackoverflow.com/questions/19354870/ddg#19355351
			"-args", args,
			"-runtime", runtime.String(),
		)...)
}
