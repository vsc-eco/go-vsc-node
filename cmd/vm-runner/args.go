package main

import (
	"flag"
	"fmt"

	"github.com/JustinKnueppel/go-result"
)

type args struct {
	gas        uint
	entrypoint string
	args       string
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}

var (
	ErrByteCodeRequired   = fmt.Errorf("byte code required")
	ErrGasRequired        = fmt.Errorf("gas required")
	ErrEntrypointRequired = fmt.Errorf("entrypoint required")
)

func parseArgs() result.Result[args] {
	gas := flag.Uint("gas", 0, "the amount of gas allocated to the wasm execution context")
	entrypoint := flag.String("entrypoint", "", "the function exported from the wasm instance that should be called")
	argss := flag.String("args", "", "the arguments that are passed into the entrypoint")
	flag.Parse()
	if *gas == 0 {
		return result.Err[args](ErrGasRequired)
	}
	if *entrypoint == "" {
		return result.Err[args](ErrEntrypointRequired)
	}
	return result.Ok(args{
		gas:        *gas,
		entrypoint: *entrypoint,
		args:       *argss,
	})
}
