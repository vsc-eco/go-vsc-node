package wasm_runtime

// forbiddenEntrypoints is the denylist of WASM exports that a
// contract caller (vsc.call action) MUST NOT be able to invoke
// directly. These names belong to language runtimes (TinyGo's Go
// allocator, AssemblyScript's runtime helpers) or to the dispatch
// shim itself, and are never legitimate user actions.
//
// Pentest finding F30: the unfiltered dispatch path at wasm.go let
// `alloc` be called against every deployed Go contract for ~3.1M gas
// per call by anonymous accounts.
//
// New names should be added here aggressively — false positives
// (a contract whose action name happens to collide) are a build-time
// rename, false negatives are a chain-wide DoS surface.
var forbiddenEntrypoints = map[string]struct{}{
	// TinyGo / Go runtime
	"alloc":  {},
	"free":   {},
	"malloc": {},

	// WASI / wasmedge dispatch
	"_initialize": {}, // run once automatically by Wasm.Execute itself
	"_start":      {},

	// AssemblyScript runtime helpers
	"__new":       {},
	"__pin":       {},
	"__unpin":     {},
	"__retain":    {},
	"__release":   {},
	"__collect":   {},
	"__rtti_base": {},
	"__alloc":     {},
	"realloc":     {},
}

// isForbiddenEntrypoint reports whether a vsc.call action name maps
// to a runtime-internal export that user transactions must never
// reach. The check is exact-match (case-sensitive): a contract is
// free to expose an action named "Alloc" or "Initialize" for its own
// purposes, and only the WASM-runtime spelling is blocked.
//
// An empty name is rejected because no contract registers an action
// under the empty string and falling through with "" would cause
// wasmedge to fail in a way that's harder to debug.
func isForbiddenEntrypoint(name string) bool {
	if name == "" {
		return true
	}
	_, blocked := forbiddenEntrypoints[name]
	return blocked
}
