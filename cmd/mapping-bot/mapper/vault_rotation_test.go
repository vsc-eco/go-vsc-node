package mapper

import (
	"errors"
	"testing"

	"vsc-node/lib/btcvault"
)

func vault(gen uint32, status btcvault.VaultStatus) btcvault.Vault {
	return btcvault.Vault{Generation: gen, Status: status}
}

// The driver's whole policy lives in decideVaultAction, so it is tested directly
// rather than through a chain. Each case is a state the rotation actually reaches
// on devnet.
func TestDecideVaultAction(t *testing.T) {
	cases := []struct {
		name          string
		vaults        []btcvault.Vault
		counts        map[uint32]int
		sweepInFlight bool
		want          vaultAction
	}{
		{
			// A non-vault mapping contract (dash/ltc/...) and a pre-rotation BTC deploy
			// both look like this. The driver must stay completely out of the way.
			name:   "no vault registry",
			vaults: nil,
			counts: map[uint32]int{},
			want:   vaultNoop,
		},
		{
			name:   "single active generation — nothing to drain",
			vaults: []btcvault.Vault{vault(0, btcvault.VaultStatusActive)},
			counts: map[uint32]int{0: 3},
			want:   vaultNoop,
		},
		{
			// THE bug this driver exists to fix: gen-0 rotated out, still holding BTC,
			// and nothing was ever building the sweep.
			name:   "retiring generation still funded — build a tranche",
			vaults: []btcvault.Vault{vault(0, btcvault.VaultStatusRetiring), vault(1, btcvault.VaultStatusActive)},
			counts: map[uint32]int{0: 2, 1: 1},
			want:   vaultMigrate,
		},
		{
			name:   "draining generation still funded — build the next tranche",
			vaults: []btcvault.Vault{vault(0, btcvault.VaultStatusDraining), vault(1, btcvault.VaultStatusActive)},
			counts: map[uint32]int{0: 1},
			want:   vaultMigrate,
		},
		{
			// Stacking a second sweep would draw the fee reserve down twice and race the
			// same UTXO set — the in-flight one is already being settled by the spend
			// pipeline.
			name:          "sweep already in flight — wait, do not stack tranches",
			vaults:        []btcvault.Vault{vault(0, btcvault.VaultStatusDraining), vault(1, btcvault.VaultStatusActive)},
			counts:        map[uint32]int{0: 2},
			sweepInFlight: true,
			want:          vaultNoop,
		},
		{
			name:   "drained — advance the lifecycle",
			vaults: []btcvault.Vault{vault(0, btcvault.VaultStatusDraining), vault(1, btcvault.VaultStatusActive)},
			counts: map[uint32]int{1: 3}, // gen-0 holds nothing
			want:   vaultRetire,
		},
		{
			// S5/L4-C1: a late or reorged deposit can re-fund an already-emptied
			// generation. It must be swept again, not allowed to escape with the gen.
			name:   "INACTIVE generation re-funded by a late deposit — sweep it again",
			vaults: []btcvault.Vault{vault(0, btcvault.VaultStatusInactive), vault(1, btcvault.VaultStatusActive)},
			counts: map[uint32]int{0: 1},
			want:   vaultMigrate,
		},
		{
			name:   "inactive and empty — keep advancing toward purge",
			vaults: []btcvault.Vault{vault(0, btcvault.VaultStatusInactive), vault(1, btcvault.VaultStatusActive)},
			counts: map[uint32]int{},
			want:   vaultRetire,
		},
		{
			// A purged generation is done; it must not drag the driver back into work.
			name:   "purged generation is finished",
			vaults: []btcvault.Vault{vault(0, btcvault.VaultStatusPurged), vault(1, btcvault.VaultStatusActive)},
			counts: map[uint32]int{1: 2},
			want:   vaultNoop,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := decideVaultAction(tc.vaults, tc.counts, tc.sweepInFlight)
			if got != tc.want {
				t.Fatalf("decideVaultAction = %v, want %v", got, tc.want)
			}
		})
	}
}

// An uneconomic residual is NOT transient: retrying migrateVault forever would
// never drain the generation. It has to route to writeOffDust instead, or the
// rotation wedges permanently.
func TestIsUneconomicResidual(t *testing.T) {
	uneconomic := []string{
		"migration tranche value too small to cover the sweep fee",
		"migration fee exceeds half the tranche value — sweep deferred",
	}
	for _, msg := range uneconomic {
		if !isUneconomicResidual(errors.New(msg)) {
			t.Errorf("expected %q to be treated as an uneconomic residual (must route to writeOffDust)", msg)
		}
	}

	// Anything else must NOT be written off — writing off dust debits Supply and
	// deletes registry UTXOs, so a false positive destroys recoverable funds.
	other := []string{
		"insufficient fee reserve",
		"contract is paused",
		"no active successor vault to migrate into",
	}
	for _, msg := range other {
		if isUneconomicResidual(errors.New(msg)) {
			t.Errorf("%q must NOT be treated as an uneconomic residual — writeOffDust deletes UTXOs and debits Supply", msg)
		}
	}
	if isUneconomicResidual(nil) {
		t.Error("nil error must not be an uneconomic residual")
	}
}

func TestIsNothingToMigrate(t *testing.T) {
	if !isNothingToMigrate(errors.New("no retiring or draining vault to migrate")) {
		t.Error("the benign no-op abort should be recognised")
	}
	if isNothingToMigrate(errors.New("insufficient fee reserve")) {
		t.Error("a real failure must not be swallowed as benign")
	}
}

// The bot attaches ONE global RcLimit to every L2 tx (default 10_000). Vault ops are
// SPV/secp256k1-heavy and need ~8M — at the default every migrateVault would abort
// with cost-limit-exceeded and the rotation would silently never move.
func TestRcLimitForRaisesVaultOpsOnly(t *testing.T) {
	b := &Bot{BotConfig: NewMappingBotConfig(t.TempDir())}

	for _, action := range []string{"migrateVault", "retireVault", "writeOffDust"} {
		if got := b.rcLimitFor(action); got != vaultOpRcLimit {
			t.Errorf("rcLimitFor(%q) = %d, want the raised vault limit %d", action, got, vaultOpRcLimit)
		}
	}

	// Every existing action must keep the operator's configured value — silently
	// raising rc_limit on an HBD-moving op reserves HBD against RC and surfaces as a
	// spurious insufficient-balance.
	configured := b.BotConfig.RcLimit()
	for _, action := range []string{"map", "confirmSpend", "unmap"} {
		if got := b.rcLimitFor(action); got != configured {
			t.Errorf("rcLimitFor(%q) = %d, want the configured %d (must not be raised)", action, got, configured)
		}
	}
}

// The vault ops are owner-only, but the bot calls as its own did:pkh identity. If it
// is not the owner every op is rejected forever — the driver must recognise that and
// stop, not hammer the chain burning RC on guaranteed rejections.
func TestIsNotOwner(t *testing.T) {
	if !isNotOwner(errors.New("action must be performed by the contract owner")) {
		t.Error("the owner-only rejection must be recognised")
	}
	if isNotOwner(errors.New("insufficient fee reserve")) {
		t.Error("an unrelated failure must not latch the not-owner state")
	}
	if isNotOwner(nil) {
		t.Error("nil must not latch the not-owner state")
	}
}

func TestNotOwnerLatchesOnce(t *testing.T) {
	const cid = "vsc1TestNotOwnerLatch"
	if isNotOwnerLatched(cid) {
		t.Fatal("should not start latched")
	}
	if !markNotOwner(cid) {
		t.Error("the first report should be the loud one")
	}
	if markNotOwner(cid) {
		t.Error("subsequent reports must be suppressed (one message, not one per block)")
	}
	if !isNotOwnerLatched(cid) {
		t.Error("the driver must stay disabled once latched")
	}
}
