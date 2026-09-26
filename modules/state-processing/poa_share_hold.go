package state_engine

import (
	"encoding/base64"
	"fmt"
	"math/big"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/poaseats"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// HeldShareOfFundedVaultOrBlock reports whether account holds a share of a BTC
// vault generation that still holds funds (POA-1, consensus 0.9.0). It is a
// TxResult input, so read errors block and retry instead of deciding on one
// node.
func (se *StateEngine) HeldShareOfFundedVaultOrBlock(account string, height uint64) bool {
	check := se.heldShareCheck
	if check == nil {
		check = se.heldShareOfFundedVault
	}
	var held bool
	blockingRetry("heldShareOfFundedVault("+account+")", func() error {
		var err error
		held, err = check(account, height)
		return err
	})
	return held
}

// heldShareOfFundedVault reads the BTC contract's vault registry, the election
// active at `height` and the keygen/reshare commitments, and asks
// accountInFundedVaultCommittees.
//
// Why the current election and electability too: those accounts are, or are
// about to become, parties of the active generation (a reshare lands after its
// election, and an election's members are fixed at its anchor, before it lands).
// An unstake accepted before that reshare would otherwise pay out while the
// share it then holds still signs (THORChain: no unbond while active or ready,
// then a lockup).
//
// Why every commitment and not only the latest: a reshare keeps the public key,
// so the shares of EVERY past committee of a generation still combine into a
// valid signature while that generation is funded. THORChain avoids this by
// never resharing (every churn is a fresh keygen and the funds migrate), and
// keeps a node's bond locked while it is a member of any vault that still holds
// funds. This is that rule for Magi's reshared generations.
//
// Consensus inputs only: the registry at the height, the commitment rows, and
// the elections they decode against. Never the tss_keys collection (a node
// rewrites its statuses at startup). Transient errors are returned; a
// deterministic absence decides.
func (se *StateEngine) heldShareOfFundedVault(account string, height uint64) (bool, error) {
	if se.sconf == nil || se.tssCommitments == nil || se.electionDb == nil || height == 0 {
		return false, nil
	}
	btcContract := se.sconf.OracleParams().ContractId("BTC")
	if btcContract == "" {
		return false, nil
	}
	var transient error
	reader := se.btcContractStateReaderAtStrict(btcContract, height, &transient)
	rawV, ok := reader("v")
	if transient != nil {
		return true, transient
	}
	if !ok || len(rawV) == 0 {
		return false, nil
	}
	vaults, err := btcvault.UnmarshalVaultRegistry(rawV)
	if err != nil {
		// Present but undecodable is corrupt committed state, identical on every
		// node: fail closed, as the retiring-member bond lock does.
		return true, nil
	}
	funded := false
	for _, v := range vaults {
		funded = funded || btcvault.IsFundHoldingStatus(v.Status)
	}
	if !funded {
		return false, nil
	}
	// An account the proposer could still put in a committee holds, or is about
	// to hold, a share: it is an electable witness now, or it announced within
	// the exit window (an election decided at its anchor may not have landed
	// yet). The seat exit-halt's own guard, applied to every bonded account.
	if held, err := se.electableOrRecentlyActive(account, height); err != nil || held {
		return true, err
	}
	var current []string
	elec, eerr := se.electionDb.GetElectionByHeight(height)
	if isTransientReadErr(eerr) {
		return true, eerr
	}
	if eerr == nil {
		for _, m := range elec.Members {
			current = append(current, m.Account)
		}
	}
	below := height - 1 // commitments strictly below the height, as GetCommitmentByHeight reads
	return accountInFundedVaultCommittees(vaults, btcContract, account, current,
		func(keyId string) ([]tss_db.TssCommitment, error) {
			rows, err := se.tssCommitments.FindCommitmentsSimple(&keyId, []string{"keygen", "reshare"}, nil, nil, &below, 0)
			if err != nil && isTransientReadErr(err) {
				return nil, err
			}
			if err != nil {
				return nil, nil // deterministic absence
			}
			return rows, nil
		},
		func(epoch uint64) (*elections.ElectionResult, error) {
			elec, err := se.electionDb.GetElectionStrict(epoch)
			if err != nil && !isTransientReadErr(err) {
				return nil, nil // deterministic absence
			}
			return elec, err
		})
}

// electableOrRecentlyActive is the exit-halt's electability guard (RG-1 and
// RG-1c in poaExitHalt) for any account: electable now, or any witness
// announcement within the exit window. Read errors are returned (retry).
func (se *StateEngine) electableOrRecentlyActive(account string, height uint64) (bool, error) {
	if electable, err := se.isElectableWitnessE(account, height); err != nil || electable {
		return true, err
	}
	return se.hadRecentWitnessActivityE(account, height, se.sconf.ConsensusParams().EffectivePoaExitHalt())
}

// accountInFundedVaultCommittees is the pure core: while any generation holds
// funds, is account a member of the current election (currentMembers), or a
// party (commitment bitset) of ANY keygen/reshare commitment of a fund-holding
// generation?
func accountInFundedVaultCommittees(
	vaults []btcvault.Vault,
	btcContract, account string,
	currentMembers []string,
	listCommits func(keyId string) ([]tss_db.TssCommitment, error),
	getElection func(epoch uint64) (*elections.ElectionResult, error),
) (bool, error) {
	who := poaseats.NormalizeAccount(account)
	if who == "" {
		return false, nil
	}
	funded := false
	for _, v := range vaults {
		if btcvault.IsFundHoldingStatus(v.Status) {
			funded = true
			break
		}
	}
	if !funded {
		return false, nil
	}
	for _, m := range currentMembers {
		if poaseats.NormalizeAccount(m) == who {
			return true, nil
		}
	}
	elecCache := make(map[uint64]*elections.ElectionResult)
	for _, v := range vaults {
		if !btcvault.IsFundHoldingStatus(v.Status) {
			continue
		}
		keyId := btcContract + "-" + btcvault.VaultKeyName(v.Generation)
		rows, err := listCommits(keyId)
		if err != nil {
			return true, fmt.Errorf("commitments of %s: %w", keyId, err)
		}
		for _, c := range rows {
			elec, cached := elecCache[c.Epoch]
			if !cached {
				elec, err = getElection(c.Epoch)
				if err != nil {
					return true, fmt.Errorf("election %d: %w", c.Epoch, err)
				}
				elecCache[c.Epoch] = elec
			}
			if elec == nil {
				continue
			}
			bv := new(big.Int)
			if raw, derr := base64.RawURLEncoding.DecodeString(c.Commitment); derr == nil {
				bv.SetBytes(raw)
			}
			for idx, m := range elec.Members {
				if bv.Bit(idx) == 1 && poaseats.NormalizeAccount(m.Account) == who {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
