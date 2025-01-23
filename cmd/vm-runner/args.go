package main

import (
	"flag"
	"fmt"
	wasm_runtime "vsc-node/modules/wasm/runtime"

	"github.com/JustinKnueppel/go-result"
)

type args struct {
	gas        uint
	entrypoint string
	args       string
	runtime    wasm_runtime.Runtime
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}

var (
	ErrGasRequired        = fmt.Errorf("gas required")
	ErrEntrypointRequired = fmt.Errorf("entrypoint required")
)

func parseArgs() result.Result[args] {
	gas := flag.Uint("gas", 0, "the amount of gas allocated to the wasm execution context")
	entrypoint := flag.String("entrypoint", "", "the function exported from the wasm instance that should be called")
	argss := flag.String("args", "", "the arguments that are passed into the entrypoint")
	runtime := flag.String("runtime", "", "the runtime the contract was compiled with [assembly-script, go]")
	flag.Parse()
	if *gas == 0 {
		return result.Err[args](ErrGasRequired)
	}
	if *entrypoint == "" {
		return result.Err[args](ErrEntrypointRequired)
	}
	return result.Map(
		wasm_runtime.NewFromString(*runtime),
		func(runtime wasm_runtime.Runtime) args {
			return args{
				gas:        *gas,
				entrypoint: *entrypoint,
				args:       *argss,
				runtime:    runtime,
			}
		},
	)
}
