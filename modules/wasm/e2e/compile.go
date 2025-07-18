package wasm_e2e

import (
	"io"
	"os"
	"os/exec"
)

func Compile(wkdir string) (string, error) {
	//Add support for the following:
	// . /home/app/.wasmedge/env && go build -buildvcs=false -o vm-runner vsc-node/cmd/vm-runner

	exec.Command("go", "build", "-buildvcs=false", "-o", "vm-runner", "vsc-node/cmd/vm-runner")
	prefix := wkdir + "/modules/wasm/e2e/tmp/"
	path := prefix + "main.wasm"
	//-no-debug
	cmd := exec.Command("bash", "-c", "tinygo build -gc=custom -scheduler=none -panic=trap -target=wasm-unknown -o "+path+" modules/wasm/e2e/go_wasm/main.go") // Example: list files in the current directory

	// cmd.Env = append(cmd.Environ(), "GOOS=js")
	// cmd.Env = append(cmd.Env, "GOARCH=wasm")

	cmdOut, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	go io.Copy(os.Stdout, cmdOut)
	cmdErr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	go io.Copy(os.Stderr, cmdErr)
	cmdIn, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	go io.Copy(cmdIn, os.Stdin)
	err = cmd.Start()
	if err != nil {
		return "", err
	}
	return path, cmd.Wait()
}
