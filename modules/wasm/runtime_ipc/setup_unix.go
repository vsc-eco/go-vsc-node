//go:build !windows

package wasm_runtime

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// wasmEdgeVersion is the pinned WasmEdge release. The install script is fetched
// from the immutable release tag (not the mutable "master" branch) and its
// integrity is verified against wasmEdgeInstallSHA256 before being executed.
const wasmEdgeVersion = "0.13.4"

// wasmEdgeInstallURL points at the install.sh pinned to the release tag rather
// than the mutable master branch, so the content cannot change underneath us.
const wasmEdgeInstallURL = "https://raw.githubusercontent.com/WasmEdge/WasmEdge/" + wasmEdgeVersion + "/utils/install.sh"

// wasmEdgeInstallSHA256 is the SHA-256 of the WasmEdge 0.13.4 utils/install.sh.
// Computed from https://raw.githubusercontent.com/WasmEdge/WasmEdge/0.13.4/utils/install.sh
// The script is never piped to bash unless its hash matches this constant.
const wasmEdgeInstallSHA256 = "89460d9ea15f097e2831c099ee8adb6975b9ffff8a919b33874a655cd54420c0"

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

	// curl -sSf <pinned-tag>/utils/install.sh, verify sha256, then bash -s -- -v $VERSION
	res, err := http.Get(wasmEdgeInstallURL)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("wasmedge install: unexpected HTTP status %d fetching %s", res.StatusCode, wasmEdgeInstallURL)
	}

	// Read the script fully and verify its integrity before executing it.
	script, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(script)
	got := hex.EncodeToString(sum[:])
	if got != wasmEdgeInstallSHA256 {
		return fmt.Errorf("wasmedge install: checksum mismatch for %s: got %s want %s", wasmEdgeInstallURL, got, wasmEdgeInstallSHA256)
	}

	cmd := exec.Command("bash", "-s", "--", "-v", wasmEdgeVersion)
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
		io.Copy(cmdIn, bytes.NewReader(script))
		cmdIn.Close()
	}()
	err = cmd.Start()
	if err != nil {
		return err
	}
	return cmd.Wait()
}
