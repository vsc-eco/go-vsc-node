package tss

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
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
