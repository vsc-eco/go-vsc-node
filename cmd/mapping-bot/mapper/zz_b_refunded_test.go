package mapper

import (
	"testing"

	"vsc-node/lib/btcvault"
)

// B: a superseded gen that went INACTIVE (empty) and was then RE-FUNDED by a late deposit
// to its still-matchable address must route to retireVault, not migrateVault. migrateVault
// only sweeps RETIRING/DRAINING, so routing it to migrate would leave it funded-but-INACTIVE
// forever; retireVault's REVERT puts it back to DRAINING so a later cycle can sweep it.
func TestB_RefundedInactiveGenRoutesToRetire(t *testing.T) {
	vaults := []btcvault.Vault{
		{Generation: 0, Status: btcvault.VaultStatusInactive},
		{Generation: 1, Status: btcvault.VaultStatusActive},
	}
	counts := map[uint32]int{0: 2} // gen 0 re-funded with 2 UTXOs

	if action, _ := decideVaultAction(vaults, counts, false); action != vaultRetire {
		t.Fatalf("a re-funded INACTIVE gen must route to retireVault for the REVERT, got %v", action)
	}
	// Even with a sweep in flight, the REVERT (status-only) still runs - it builds no sweep.
	if action, _ := decideVaultAction(vaults, counts, true); action != vaultRetire {
		t.Fatalf("the INACTIVE REVERT must run regardless of an in-flight sweep, got %v", action)
	}

	// Control: a funded DRAINING gen still migrates (unchanged behaviour).
	vaults[0].Status = btcvault.VaultStatusDraining
	if action, _ := decideVaultAction(vaults, counts, false); action != vaultMigrate {
		t.Fatalf("a funded DRAINING gen must still migrate, got %v", action)
	}
	// Control: a funded DRAINING gen with a sweep in flight waits (no tranche stacking).
	if action, _ := decideVaultAction(vaults, counts, true); action != vaultNoop {
		t.Fatalf("a funded DRAINING gen with a sweep in flight must wait, got %v", action)
	}
}
