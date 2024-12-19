package main

import (
	"encoding/hex"
	"flag"
	"fmt"

	"github.com/JustinKnueppel/go-result"
)

type args struct {
	byteCode   []byte
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
	byteCode := flag.String("bytecode", "", "a hex encoded string of the wasm byte code to execute")
	gas := flag.Uint("gas", 0, "the amount of gas allocated to the wasm execution context")
	entrypoint := flag.String("entrypoint", "", "the function exported from the wasm instance that should be called")
	argss := flag.String("args", "", "the arguments that are passed into the entrypoint")
	flag.Parse()
	return result.AndThen(
		resultWrap(hex.DecodeString(*byteCode)),
		func(byteCode []byte) result.Result[args] {
			if len(byteCode) == 0 {
				return result.Err[args](ErrByteCodeRequired)
			}
			if *gas == 0 {
				return result.Err[args](ErrGasRequired)
			}
			if *entrypoint == "" {
				return result.Err[args](ErrEntrypointRequired)
			}
			return result.Ok(args{
				byteCode:   byteCode,
				gas:        *gas,
				entrypoint: *entrypoint,
				args:       *argss,
			})
		},
	)
}
