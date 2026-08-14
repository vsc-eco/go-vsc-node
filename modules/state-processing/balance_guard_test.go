package state_engine

import (
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

// TestNegativeSpendableBalance verifies the pure detector used by the
// ledger-corruption fail-stop in UpdateBalances: it fires on a negative
// spendable balance and stays dormant on healthy/zero balances.
func TestNegativeSpendableBalance(t *testing.T) {
	tests := []struct {
		name         string
		rec          ledgerDb.BalanceRecord
		wantAsset    string
		wantNegative bool
	}{
		{"all zero", ledgerDb.BalanceRecord{}, "", false},
		{"healthy positive", ledgerDb.BalanceRecord{HBD: 100, HBD_SAVINGS: 5324, Hive: 1000, HIVE_CONSENSUS: 50}, "", false},
		// A "max" op (daveks unstaking his full 5324) lands on exactly 0 — legitimate, must NOT trip.
		{"exact-zero max op", ledgerDb.BalanceRecord{HBD_SAVINGS: 0}, "", false},
		// The 2026-08 halt: 5271 - 5324 = -53 on the corrupted nodes.
		{"corrupted hbd_savings (-53)", ledgerDb.BalanceRecord{HBD_SAVINGS: -53}, "hbd_savings", true},
		{"corrupted hbd", ledgerDb.BalanceRecord{HBD: -1}, "hbd", true},
		{"corrupted hive", ledgerDb.BalanceRecord{Hive: -1000}, "hive", true},
		{"corrupted hive_consensus", ledgerDb.BalanceRecord{HIVE_CONSENSUS: -1}, "hive_consensus", true},
		// HBD_AVG is a cumulative TWAB accumulator, not a spendable balance — never guarded.
		{"negative HBD_AVG is ignored", ledgerDb.BalanceRecord{HBD_AVG: -999, HBD_SAVINGS: 10}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asset, _, negative := negativeSpendableBalance(tt.rec)
			if negative != tt.wantNegative {
				t.Fatalf("negative = %v, want %v (rec = %+v)", negative, tt.wantNegative, tt.rec)
			}
			if negative && asset != tt.wantAsset {
				t.Fatalf("asset = %q, want %q", asset, tt.wantAsset)
			}
		})
	}
}

// TestIsGuardedAccount verifies the guard is scoped to real Hive-backed user
// accounts, so it can never trip on system/protocol/did bookkeeping accounts.
func TestIsGuardedAccount(t *testing.T) {
	guarded := []string{"hive:daveks", "hive:tibfox.vsc", "hive:v4vapp.vsc"}
	unguarded := []string{"system:fr_balance", "did:vsc:oracle:btc", "", "hive", "notahive:prefix"}
	for _, a := range guarded {
		if !isGuardedAccount(a) {
			t.Errorf("isGuardedAccount(%q) = false, want true", a)
		}
	}
	for _, a := range unguarded {
		if isGuardedAccount(a) {
			t.Errorf("isGuardedAccount(%q) = true, want false", a)
		}
	}
}
