package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"vsc-node/modules/aggregate"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"
)

func main() {
	args := parseArgs().UnwrapOrElse(func(err error) args {
		fmt.Println("args error:", err)
		flag.Usage()
		os.Exit(1)
		return args{}
	})

	wasm := wasm_runtime_ipc.New()
	a := aggregate.New(
		[]aggregate.Plugin{
			wasm,
		},
	)

	err := a.Run()
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "exec start:", time.Now())
	res := wasm.Execute(args.gas, args.entrypoint, args.args, args.runtime).MapErr(func(err error) error {
		fmt.Println("execution error:", err)
		os.Exit(1)
		return nil
	}).Unwrap()
	fmt.Fprintln(os.Stderr, "\n\nres", res)
}
