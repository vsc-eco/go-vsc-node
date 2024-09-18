package main

import (
	js_ipc "vsc-node/lib/js-ipc"
)

func main() {
	ipc := js_ipc.New(true)

	err := ipc.Init()
	if err != nil {
		panic(err)
	}

	err = ipc.Start()
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
