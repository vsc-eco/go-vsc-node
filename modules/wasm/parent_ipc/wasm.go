package wasm_parent_ipc

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	ipc_host "vsc-node/lib/stdio-ipc/host"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"
	wasm_context "vsc-node/modules/wasm/context"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
)

var (
	ErrDone error = fmt.Errorf("Done")
)

const DefaultExecPath = "TODO"

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
	err := setup()
	if err != nil {
		return err
	}
	return nil
}

func (w *Wasm) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

func (w *Wasm) Stop() error {
	w.cancel()
	return nil
}

const timePer15_000GasUnits = 5 * time.Millisecond
const startupTime = 3000 * time.Millisecond // TODO investigate large startup time

func (w *Wasm) Execute(ctxValue wasm_context.ExecContextValue, byteCode []byte, gas uint, entrypoint string, args string) result.Result[string] {
	ctx, cancel := context.WithTimeout(context.WithValue(w.ctx, wasm_context.WasmExecCtxKey, ctxValue), (timePer15_000GasUnits*time.Duration(gas)/15_000)+startupTime)
	defer cancel()
	return ipc_host.RunWithContext[string](ctx,
		w.execPath[0], append(w.execPath[1:],
			// TODO move this to an IPC request since the cmd line arg length can be as little as 32KB
			// see http://stackoverflow.com/questions/19354870/ddg#19355351
			"-bytecode", hex.EncodeToString(byteCode),
			"-gas", fmt.Sprint(gas),
			"-entrypoint", entrypoint,
			"-args", args,
		)...)

}
