//go:build !windows

package wasm

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

func home() string {
	return os.Getenv("HOME")
}

func source(file string) (bool, error) {
	cmd := exec.Command("bash", "-c", "source \""+file+"\" && echo '<<<ENVIRONMENT>>>' && env")
	bs, err := cmd.CombinedOutput()
	if err != nil {
		println(string(bs))
		if _, ok := err.(*exec.ExitError); ok {
			return false, nil
		}
		return false, err
	}
	s := bufio.NewScanner(bytes.NewReader(bs))
	start := false
	for s.Scan() {
		if s.Text() == "<<<ENVIRONMENT>>>" {
			start = true
		} else if start {
			kv := strings.SplitN(s.Text(), "=", 2)
			if len(kv) == 2 {
				os.Setenv(kv[0], kv[1])
			}
		}
	}
	return start, nil
}

func setup() error {
	installed, err := source(home() + "/.wasmedge/env")
	if err != nil {
		return err
	}
	if installed {
		return nil
	}

	// curl -sSf https://raw.githubusercontent.com/WasmEdge/WasmEdge/master/utils/install.sh | bash -s -- -v $VERSION
	res, err := http.Get("https://raw.githubusercontent.com/WasmEdge/WasmEdge/master/utils/install.sh")
	if err != nil {
		return err
	}
	cmd := exec.Command("bash", "-s", "--", "-v", "0.13.4")
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
	go func() {
		io.Copy(cmdIn, res.Body)
		cmdIn.Close()
	}()
	err = cmd.Start()
	if err != nil {
		return err
	}
	return cmd.Wait()
}
