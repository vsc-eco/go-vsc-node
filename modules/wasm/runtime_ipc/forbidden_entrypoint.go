package wasm_runtime

import "strings"

// forbiddenEntrypoints is the exact-match denylist of WASM exports
// that a contract caller (vsc.call action) MUST NOT be able to invoke
// directly. These names belong to language runtimes (TinyGo's Go
// allocator, AssemblyScript's runtime helpers) or to the dispatch
// shim itself, and are never legitimate user actions.
//
// Pentest finding F30: the unfiltered dispatch path at wasm.go let
// `alloc` be called against every deployed Go contract for ~3.1M gas
// per call by anonymous accounts.
//
// This map only needs to carry runtime exports that do NOT match one
// of forbiddenPrefixes below (single-underscore WASI entrypoints and
// the bare TinyGo/Go allocator symbols). Anything matching a reserved
// prefix is covered by the prefix rule and need not be enumerated.
//
// New names should be added here aggressively — false positives
// (a contract whose action name happens to collide) are a build-time
// rename, false negatives are a chain-wide DoS surface.
var forbiddenEntrypoints = map[string]struct{}{
	// TinyGo / Go runtime allocator (no reserved prefix)
	"alloc":   {},
	"free":    {},
	"malloc":  {},
	"realloc": {},

	// WASI / wasmedge dispatch — single underscore, NOT caught by "__"
	"_initialize": {}, // run once automatically by Wasm.Execute itself
	"_start":      {},
}

// forbiddenPrefixes denies whole families of runtime-internal exports
// without having to enumerate every spelling. These prefixes are
// reserved by toolchains and never appear on a legitimate user action:
//
//   - "__"        AssemblyScript runtime (__new/__pin/__retain/...) and
//     wasm-bindgen (__wbindgen_*, __data_end, __heap_base,
//     __externref_*). Double underscore is conventionally
//     reserved; no production action uses it.
//   - "runtime."  Go / TinyGo runtime exports (runtime.alloc, etc.).
//     The '.' cannot occur in a contract action name.
//   - "syscall."  TinyGo/wasi syscall shims (syscall.seek is exported
//     by Magi's own test wasms). Same '.' argument.
//
// Prefix denial is strictly more durable than the explicit list: a new
// AssemblyScript/wasm-bindgen helper or syscall shim is blocked the day
// the toolchain emits it, with no node change.
var forbiddenPrefixes = []string{
	"__",
	"runtime.",
	"syscall.",
}

// isForbiddenEntrypoint reports whether a vsc.call action name maps
// to a runtime-internal export that user transactions must never
// reach. The check is exact-match (case-sensitive) against
// forbiddenEntrypoints, plus a case-sensitive prefix match against
// forbiddenPrefixes: a contract is free to expose an action named
// "Alloc" or "Initialize" for its own purposes, and only the
// WASM-runtime spelling is blocked.
//
// An empty name is rejected because no contract registers an action
// under the empty string and falling through with "" would cause
// wasmedge to fail in a way that's harder to debug.
func isForbiddenEntrypoint(name string) bool {
	if name == "" {
		return true
	}
	if _, blocked := forbiddenEntrypoints[name]; blocked {
		return true
	}
	for _, p := range forbiddenPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
