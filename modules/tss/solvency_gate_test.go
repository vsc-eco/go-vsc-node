package tss

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	stateEngine "vsc-node/modules/state-processing"
)

// fakeSolvencyScheduler is a minimal GetScheduler for the gate tests: only
// BtcKeysignHalted matters here.
type fakeSolvencyScheduler struct{ halted bool }

func (f *fakeSolvencyScheduler) GetSchedule(uint64) []stateEngine.WitnessSlot { return nil }
func (f *fakeSolvencyScheduler) TssMinimumConsensusVersion(uint64) consensusversion.Version {
	return consensusversion.Version{}
}
func (f *fakeSolvencyScheduler) BtcKeysignHalted() bool { return f.halted }

// TestBtcKeysignFrozen_FlagAndScope covers the deterministic FLAG layer and the
// BTC-only scoping. The SIGNAL layer stays inert here (MainnetConfig ships an
// empty BtcVaultAddresses => fail open), so nil contractState/da are never
// dereferenced — which is exactly the production/test invariant we rely on.
func TestBtcKeysignFrozen_FlagAndScope(t *testing.T) {
	sconf := systemconfig.MainnetConfig()
	btc := sconf.OracleParams().ContractId("BTC")
	if btc == "" {
		t.Fatal("expected a mainnet BTC contract id to be configured")
	}
	btcKey := btc + "-main"
	nonBtcKey := "vsc1SomeEthKeyNotBtc-main"

	cases := []struct {
		name   string
		halted bool
		keyId  string
		want   bool
	}{
		{"flag off, BTC key -> not frozen", false, btcKey, false},
		{"flag on, BTC key -> frozen", true, btcKey, true},
		{"flag on, non-BTC key -> not frozen (scope)", true, nonBtcKey, false},
		{"flag on, empty key -> not frozen", true, "", false},
		{"flag on, bare contract id (no -suffix) -> not frozen", true, btc, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &TssManager{sconf: sconf, scheduler: &fakeSolvencyScheduler{halted: tc.halted}}
			if got := mgr.btcKeysignFrozen(tc.keyId); got != tc.want {
				t.Fatalf("btcKeysignFrozen(%q, halted=%v) = %v, want %v", tc.keyId, tc.halted, got, tc.want)
			}
		})
	}
}

func TestParseSupplySats(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"123456", 123456, true},
		{`"7890"`, 7890, true},
		{"  42  ", 42, true},
		{"0", 0, true},
		{"", 0, false},
		{"abc", 0, false},
		{"-5", 0, false},
		{"1.5", 0, false},
		{"0x10", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseSupplySats([]byte(tc.in))
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("parseSupplySats(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// flagOnConfig wraps a real SystemConfig, overriding ONLY ConsensusParams so the
// vault-rotation-v2 flag reads as ACTIVE while OracleParams (the BTC contract id
// used by isBtcVaultKey) stays real. Embedding the interface gives every other
// method for free — no 15-method fake.
type flagOnConfig struct {
	systemconfig.SystemConfig
	cp params.ConsensusParams
}

func (f flagOnConfig) ConsensusParams() params.ConsensusParams { return f.cp }

// TestShouldSkipReshareForVaultRotation pins the load-bearing per-keyId gate-off
// decision at the tss.go reshare loop (M1.3, U-1): the BTC vault key is skipped
// ONLY when the rotation flag is active and only at/after the pinned height;
// every OTHER chain keeps resharing (per-keyId, never loop-level); and the whole
// thing is INERT (never skips) while the flag is off — the property that lets the
// binary dark-launch before governance pins an activation height.
func TestShouldSkipReshareForVaultRotation(t *testing.T) {
	base := systemconfig.MainnetConfig()
	btc := base.OracleParams().ContractId("BTC")
	if btc == "" {
		t.Fatal("expected a mainnet BTC contract id to be configured")
	}
	btcKey := btc + "-main"
	siblingKey := "vsc1SomeEthKeyNotBtc-main"

	// Flag ON at height 100, keeping real mainnet OracleParams via embedding.
	cpOn := base.ConsensusParams() // returned by value → safe to mutate our copy
	cpOn.VaultRotationV2ActivationHeight = 100
	onCfg := flagOnConfig{SystemConfig: base, cp: cpOn}

	cases := []struct {
		name  string
		sconf systemconfig.SystemConfig
		keyId string
		bh    uint64
		want  bool
	}{
		// Flag OFF (real MainnetConfig, activation height 0): NEVER skip — inert.
		{"flag off, BTC key -> never skip (inert)", base, btcKey, 1 << 40, false},
		{"flag off, sibling key -> never skip", base, siblingKey, 1 << 40, false},
		// Flag ON: skip ONLY the BTC vault key, and only at/after the height.
		{"flag on, below height, BTC key -> not yet", onCfg, btcKey, 99, false},
		{"flag on, at height, BTC key -> skip", onCfg, btcKey, 100, true},
		{"flag on, above height, BTC key -> skip", onCfg, btcKey, 200, true},
		{"flag on, sibling key -> keep resharing (per-keyId)", onCfg, siblingKey, 200, false},
		{"flag on, empty key -> not BTC -> keep", onCfg, "", 200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &TssManager{sconf: tc.sconf}
			if got := mgr.shouldSkipReshareForVaultRotation(tc.keyId, tc.bh); got != tc.want {
				t.Fatalf("shouldSkipReshareForVaultRotation(%q, bh=%d) = %v, want %v", tc.keyId, tc.bh, got, tc.want)
			}
		})
	}
}
