//go:build windows

package wasm

import (
	"io"
	"os"
	"os/exec"
)

func setup() error {
	cmd := exec.Command("winget", "install", "wasmedge", "-v", "0.13.4")
	cmdOut, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	go io.Copy(os.Stdout, cmdOut)
	cmdErr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	go io.Copy(os.Stderr, cmdErr)
	cmdIn, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	go io.Copy(cmdIn, os.Stdin)
	err = cmd.Start()
	if err != nil {
		return err
	}
	return cmd.Wait()
}
