package systemconfig

import (
	"errors"
	"testing"

	pendulum "vsc-node/modules/incentive-pendulum"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	pendulumwasm "vsc-node/modules/incentive-pendulum/wasm"
	wasm_context "vsc-node/modules/wasm/context"
)

// stubGeometryAlwaysBalanced returns a stable s=0.5 geometry — enough to
// get past the geometry guard so the whitelist guard is the load-bearing
// rejection in the audit-#81 scenario.
type stubGeometryAlwaysBalanced struct{}

func (stubGeometryAlwaysBalanced) GeometryAt(_ uint64) (pendulumoracle.GeometryOutputs, bool) {
	g := pendulumoracle.GeometryOutputs{
		OK:   true,
		V:    500_000,
		P:    250_000,
		E:    1_000_000,
		T:    1_000_000,
		SBps: pendulum.BpsScale / 2,
	}
	return g, true
}

// TestAuditUnfixed_81_MainnetEmptyWhitelistSilentlyDisablesPendulum pins
// audit finding #81: MainnetConfig() leaves pendulumPoolWhitelist nil
// (system-config.go:192-205). The accompanying comment promises a
// fallback to "PendulumBolt's DAO-owner check" (line 202), but the
// wasm-side ApplySwapFees applier only consults the whitelist getter —
// so with mainnet's empty list, every contract fails contractWhitelisted
// and Pendulum is silently disabled on mainnet.
//
// Post-fix: either (a) emit a WARN at boot when OnMainnet() && empty
// whitelist, or (b) wire the DAO-owner fallback into the applier's
// contractWhitelisted path so DAO-owned pools succeed without an
// explicit whitelist entry.
func TestAuditUnfixed_81_MainnetEmptyWhitelistSilentlyDisablesPendulum(t *testing.T) {
	cfg := MainnetConfig()

	// 1. Mainnet must report itself as mainnet (sanity) and have an empty
	// PendulumPoolWhitelist.
	if !cfg.OnMainnet() {
		t.Fatalf("expected MainnetConfig().OnMainnet() == true")
	}
	if w := cfg.PendulumPoolWhitelist(); len(w) != 0 {
		t.Fatalf("expected mainnet whitelist to default to empty, got %v", w)
	}

	// 2. Wire an Applier to that whitelist getter, exactly as the state
	// engine does in production. With an empty whitelist, every contract
	// is rejected as not-whitelisted — even contracts that the audit
	// comment implies should fall through to the DAO-owner check.
	whitelistGetter := pendulumwasm.WhitelistGetter(func() []string {
		return cfg.PendulumPoolWhitelist()
	})

	a := pendulumwasm.New(
		stubGeometryAlwaysBalanced{},
		whitelistGetter,
		pendulumwasm.DefaultConfig(),
	)

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hbd",
		AssetOut: "hive",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}

	// 3. Any contract — including names that look like DAO-owned pools —
	// is rejected. The whitelist is the only gate; the DAO-owner fallback
	// promised in the comment is not actually wired.
	for _, contractID := range []string{
		"anyContractID",
		"vsc1DaoOwnedPool",                     // would-be DAO pool
		"vsc1BbGEc5XXqptJj7dC6AkToRZb4tJ6vi44Rn", // testnet whitelist entry, not on mainnet
	} {
		t.Run(contractID, func(t *testing.T) {
			accrueCalls := 0
			accrue := wasm_context.AccrueNodeBucketFn(func(_ int64) error {
				accrueCalls++
				return nil
			})

			res := a.ApplySwapFees(contractID, "tx-1", 100, args, accrue)
			if !res.IsErr() {
				t.Fatalf("expected mainnet swap to be rejected (empty whitelist), got success")
			}
			// Error must mention "not whitelisted" to confirm we hit the
			// whitelist guard rather than some other failure mode (e.g.
			// geometry missing). The applier wraps with contracts.SDK_ERROR
			// + errNotWhitelisted via errors.Join, so errors.Is works.
			if got := res.UnwrapErr(); got == nil || !looksLikeNotWhitelisted(got) {
				t.Fatalf("expected not-whitelisted error, got %v", got)
			}
			if accrueCalls != 0 {
				t.Fatalf("expected no accrual on rejected swap, got %d calls", accrueCalls)
			}
		})
	}
}

// looksLikeNotWhitelisted matches the joined error chain ApplySwapFees
// returns when the whitelist guard fires. We can't import the unexported
// errNotWhitelisted sentinel, but the wrapped error message is stable.
func looksLikeNotWhitelisted(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if e.Error() == "contract not whitelisted" {
			return true
		}
	}
	// errors.Join wraps multiple errors; the message of the outer error
	// contains both. Cheap substring fallback for that case.
	return containsSubstr(err.Error(), "contract not whitelisted")
}

func containsSubstr(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
