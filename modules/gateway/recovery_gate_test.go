package gateway

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// rosterConfig wraps a real SystemConfig, overriding ONLY ConsensusParams so a
// recovery-multisig roster can be present or absent. Embedding the interface
// gives every other method for free.
type rosterConfig struct {
	systemconfig.SystemConfig
	cp params.ConsensusParams
}

func (c rosterConfig) ConsensusParams() params.ConsensusParams { return c.cp }

func withRoster(base systemconfig.SystemConfig, accounts []string, threshold int) systemconfig.SystemConfig {
	cp := base.ConsensusParams()
	cp.RecoveryMultisigAccounts = accounts
	cp.RecoveryMultisigThreshold = threshold
	return rosterConfig{SystemConfig: base, cp: cp}
}

// B15/B11 regression guard — the live mainnet exposure.
//
// Removing the vsc.dao OWNER backstop from the gateway authority used to be gated
// on the consensus version ALONE. Once removed, a committee that wedges below the
// signing threshold has NO on-chain recovery, and the only sanctioned recovery
// mechanism (vsc.recovery_suspend) is inert unless a recovery-multisig roster is
// configured. Mainnet advanced its version floor past the activation line while
// that roster was empty on every network — so the backstop was already gone with
// nothing behind it, guarding a live gateway balance.
//
// The version gate is necessary but NOT sufficient: decentralisation must also
// require a usable roster.
func TestShouldDecentralizeOwner_RequiresARecoveryRoster(t *testing.T) {
	base := systemconfig.MainnetConfig()

	// The version at which gateway decentralization is in force.
	activeVersion := consensusversion.V0_2_0
	if !consensusversion.GatewayDecentralizationActive(activeVersion) {
		t.Fatal("fixture: expected V0_2_0 to activate gateway decentralization")
	}

	t.Run("active version but NO roster -> backstop RETAINED", func(t *testing.T) {
		// This is exactly mainnet's live state: floor past the line, roster empty.
		if base.ConsensusParams().RecoveryMultisigAccounts != nil &&
			len(base.ConsensusParams().RecoveryMultisigAccounts) != 0 {
			t.Skip("mainnet now ships a roster; update this guard")
		}
		ms := &MultiSig{sconf: base}
		if ms.shouldDecentralizeOwner(activeVersion) {
			t.Fatal("decentralization must be SUPPRESSED with no recovery roster: " +
				"removing the owner backstop would leave a wedged committee with no on-chain recovery")
		}
	})

	t.Run("active version WITH a roster -> backstop removed", func(t *testing.T) {
		ms := &MultiSig{sconf: withRoster(base, []string{"hive:a", "hive:b", "hive:c"}, 2)}
		if !ms.shouldDecentralizeOwner(activeVersion) {
			t.Fatal("with a usable roster configured, decentralization must proceed")
		}
	})

	t.Run("roster present but version INACTIVE -> still retained", func(t *testing.T) {
		ms := &MultiSig{sconf: withRoster(base, []string{"hive:a", "hive:b"}, 2)}
		var belowLine consensusversion.Version // 0.0.0
		if ms.shouldDecentralizeOwner(belowLine) {
			t.Fatal("the version gate must still apply: a roster alone does not activate decentralization")
		}
	})

	t.Run("unusable roster (threshold exceeds members) -> retained", func(t *testing.T) {
		// RecoveryMultisigConfigured requires threshold <= len(accounts); a roster
		// that can never be satisfied must not count as recovery.
		ms := &MultiSig{sconf: withRoster(base, []string{"hive:a"}, 5)}
		if ms.shouldDecentralizeOwner(activeVersion) {
			t.Fatal("a roster whose threshold can never be met is not a recovery path")
		}
	})

	t.Run("nil sconf -> fail safe", func(t *testing.T) {
		ms := &MultiSig{}
		if ms.shouldDecentralizeOwner(activeVersion) {
			t.Fatal("must fail safe (retain the backstop) when config is unavailable")
		}
	})
}
