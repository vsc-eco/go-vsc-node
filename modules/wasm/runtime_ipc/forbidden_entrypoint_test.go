package wasm_runtime

import "testing"

// Regression test for the F30 pentest finding:
//
// The WASM dispatch path at wasm.go:612 forwarded the user-supplied
// `entrypoint` (= t.Action) to wasmedge.ExecuteRegistered("contract",
// entrypoint, ...) without filtering. Compiled Go contracts on Magi
// expose runtime-internal symbols like `alloc` (~3.1M gas per call),
// `__new`, `_initialize`, etc. With no allowlist anywhere in
// state-processing or wasm/, any account can invoke these against any
// deployed contract.
//
// Fix: a denylist of well-known WASM-internal / language-runtime
// exports, applied before vm.ExecuteRegistered. The list catches the
// names the pentest exercised (`alloc`, `_initialize`) plus the
// AssemblyScript / TinyGo runtime helpers that are conventionally
// internal.
//
// This test pins both the membership of the denylist and the
// case-sensitivity of the check so future regressions are obvious.

func TestF30_ForbiddenEntrypoints(t *testing.T) {
	t.Run("PentestExercisedNamesBlocked", func(t *testing.T) {
		blocked := []string{"alloc", "_initialize"}
		for _, name := range blocked {
			if !isForbiddenEntrypoint(name) {
				t.Errorf("entrypoint %q must be blocked (pentest exercised it)", name)
			}
		}
	})

	t.Run("InternalRuntimeExportsBlocked", func(t *testing.T) {
		// AssemblyScript / TinyGo runtime helpers — never legitimate
		// as user-visible actions.
		blocked := []string{
			"__new", "__retain", "__release", "__rtti_base",
			"_start", "realloc", "free",
		}
		for _, name := range blocked {
			if !isForbiddenEntrypoint(name) {
				t.Errorf("internal runtime export %q must be blocked", name)
			}
		}
	})

	t.Run("EmptyEntrypointBlocked", func(t *testing.T) {
		if !isForbiddenEntrypoint("") {
			t.Error("empty entrypoint must be blocked — no contract uses it as an action")
		}
	})

	t.Run("NormalActionsAllowed", func(t *testing.T) {
		// Real action names from production contracts (DEX, mappings, NFT).
		allowed := []string{
			"init", "swap", "add_liquidity", "remove_liquidity",
			"register_token", "register_pool",
			"map", "unmap", "unmapETH", "unmapERC20",
			"transfer", "transferFrom", "approve",
			"adminMint", "setVault", "registerPublicKey",
			"confirmSpend", "addBlocks", "seedBlocks",
			"createKey", "renewKey", "registerRouter",
			"pause", "unpause", "migrate", "getInfo",
		}
		for _, name := range allowed {
			if isForbiddenEntrypoint(name) {
				t.Errorf("legitimate action %q must NOT be blocked", name)
			}
		}
	})

	t.Run("CaseSensitive", func(t *testing.T) {
		// "Alloc" and "ALLOC" should still be allowed (some contract
		// could conceivably name an action that way). The denylist is
		// the exact runtime-export name, not a case-insensitive match.
		allowed := []string{"Alloc", "ALLOC", "Initialize", "INIT"}
		for _, name := range allowed {
			if isForbiddenEntrypoint(name) {
				t.Errorf("case variant %q must NOT be blocked (denylist is exact-match)", name)
			}
		}
	})
}
