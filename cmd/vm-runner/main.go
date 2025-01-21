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
	args := parseArgs().MapErr(func(err error) error {
		fmt.Println("args error:", err)
		flag.Usage()
		os.Exit(1)
		return nil
	}).Unwrap()

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
	res := wasm.Execute(args.gas, args.entrypoint, args.args).MapErr(func(err error) error {
		fmt.Println("execution error:", err)
		os.Exit(1)
		return nil
	}).Unwrap()
	fmt.Fprintln(os.Stderr, "\n\nres", res)
}
