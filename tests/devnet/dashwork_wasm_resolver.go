package devnet

import (
	"os"
	"path/filepath"
)

// resolveDashWorkWasm returns the absolute path to a dash-mapping or
// dash-forwarder wasm, preferring (in order):
//  1. the absolute path in the `envVar` env var (if set + readable)
//  2. the `dev.wasm` (regtest build) inside the sibling
//     `dash_work/utxo-mapping/<dirName>/bin/` checkout — this is the
//     up-to-date feature build, which has the regtest enablers
//     (`seedInternalHbd`, `setAllowedTargetImmediate`) the op=call
//     devnet tests depend on
//  3. whatever the `defaultLookup` helper finds (typically the stale
//     sibling `testnet/utxo-mapping/...testnet.wasm` that
//     DashMappingContractPath / DashForwarderContractPath default to —
//     useful for tests that don't need the regtest enablers, like the
//     auth-only smoke or the forwarder-lock test)
//
// Returns "" if none of the candidates is readable. Caller decides
// whether to fail-fast or proceed with a warning.
func resolveDashWorkWasm(envVar, dirName string, defaultLookup func() (string, error)) string {
	if envP := os.Getenv(envVar); envP != "" {
		if info, err := os.Stat(envP); err == nil && !info.IsDir() && info.Size() > 0 {
			return envP
		}
	}
	devCandidate := filepath.Join(findSourceRoot(), "..", "dash_work", "utxo-mapping", dirName, "bin", "dev.wasm")
	if abs, err := filepath.Abs(devCandidate); err == nil {
		if info, ferr := os.Stat(abs); ferr == nil && !info.IsDir() && info.Size() > 0 {
			return abs
		}
	}
	if defaultLookup != nil {
		if p, err := defaultLookup(); err == nil {
			return p
		}
	}
	return ""
}
