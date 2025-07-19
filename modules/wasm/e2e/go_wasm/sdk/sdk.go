package sdk

import (
	_ "vsc-node/modules/wasm/e2e/gp_wasm/sdk/runtime"
)

//go:wasmimport sdk console.log
func Log(s *string)
