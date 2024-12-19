package main

import (
	"context"
	js_ipc "vsc-node/lib/js-ipc"
)

func main() {
	ipc := js_ipc.New(true)

	err := ipc.Init()
	if err != nil {
		panic(err)
	}

	_, err = ipc.Start().Await(context.Background())
	if err != nil {
		panic(err)
	}

	println("started node process")

	ipc.SendMessage([]byte(`{"foo":"bar"}`))

	err = ipc.WaitFinished()
	if err != nil {
		panic(err)
	}

	err = ipc.Stop()
	if err != nil {
		panic(err)
	}
}
