package state_engine

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"

	"vsc-node/lib/btcvault"
	vdb "vsc-node/modules/db"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/poaseats"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// HeldShareOfFundedVaultOrBlock reports whether the POA-1 bond lock holds
// account (consensus 0.9.0): while any BTC vault generation is in a
// fund-holding status (Active, Retiring, Draining or Inactive, whatever its
// balance), the account is electable, recently active as a witness, in the
// current committee, or a party of any keygen/reshare of such a generation.
// It is a TxResult input, so a read error blocks and retries instead of
// deciding on one node. A stored row that does not decode is the same on every
// node, so it decides (hold) instead of retrying forever.
func (se *StateEngine) HeldShareOfFundedVaultOrBlock(account string, height uint64) bool {
	check := se.heldShareCheck
	if check == nil {
		check = se.heldShareOfFundedVault
	}
	var held bool
	blockingRetry("heldShareOfFundedVault("+account+")", func() error {
		var err error
		held, err = check(account, height)
		if errors.Is(err, vdb.ErrDecode) {
			log.Error("poa-1: a stored row did not decode; HOLDING the bond",
				"account", account, "height", height, "err", err)
			held = true
			return nil
		}
		return err
	})
	return held
}

// heldShareOfFundedVault reads the BTC contract's vault registry, the election
// active at `height` and the keygen/reshare commitments. "Funded" is the
// registry status only: the registry carries no balance, and an Active or
// Inactive generation can receive a deposit at any block while every past
// committee's shares still sign for it.
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
	who := poaseats.NormalizeAccount(account)
	for _, m := range current {
		if poaseats.NormalizeAccount(m) == who {
			return true, nil
		}
	}
	parties, err := se.fundedVaultParties(btcContract, rawV, vaults, height-1)
	if err != nil {
		return true, err
	}
	return parties[who], nil
}

// poaPartyMemo is the POA-1 party set computed for one input: every unstake in
// a block asks against the same registry and the same commitment rows.
type poaPartyMemo struct {
	key     string
	parties map[string]bool
}

// fundedVaultParties returns the party set of every fund-holding generation
// from the commitments strictly below `below`+1 (as GetCommitmentByHeight
// reads). Without a memo each unstake re-read every commitment of the
// generation and one election per commitment epoch (796 on testnet gen 0,
// one more per epoch), and anyone could trigger it with a tiny unstake. The
// memo is keyed on everything the answer depends on: the contract, the height,
// the registry bytes and the count of commitments stored so far (the only
// writer is the vsc.tss_commitment handler, and a late row can carry an older
// block height). Election members are cached per epoch: an election is
// written once.
func (se *StateEngine) fundedVaultParties(btcContract string, rawV []byte, vaults []btcvault.Vault, below uint64) (map[string]bool, error) {
	sum := sha256.Sum256(rawV)
	key := fmt.Sprintf("%s|%d|%d|%x", btcContract, below, se.tssCommitWrites.Load(), sum)
	se.poaCacheMu.Lock()
	if se.poaPartyMemo.key == key && se.poaPartyMemo.parties != nil {
		parties := se.poaPartyMemo.parties
		se.poaCacheMu.Unlock()
		return parties, nil
	}
	se.poaCacheMu.Unlock()

	parties, err := fundedVaultPartySet(vaults, btcContract,
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
		se.poaElectionMembers)
	if err != nil {
		return nil, err
	}
	se.poaCacheMu.Lock()
	se.poaPartyMemo = poaPartyMemo{key: key, parties: parties}
	se.poaCacheMu.Unlock()
	return parties, nil
}

// poaElectionMembers returns the election of `epoch` reduced to its member
// accounts, cached. An absent election is not cached (it may still be stored).
func (se *StateEngine) poaElectionMembers(epoch uint64) (*elections.ElectionResult, error) {
	se.poaCacheMu.Lock()
	if e, ok := se.poaElecMembers[epoch]; ok {
		se.poaCacheMu.Unlock()
		return e, nil
	}
	se.poaCacheMu.Unlock()
	elec, err := se.electionDb.GetElectionStrict(epoch)
	if err != nil && !isTransientReadErr(err) {
		return nil, nil // deterministic absence
	}
	if err != nil {
		return nil, err
	}
	slim := &elections.ElectionResult{}
	for _, m := range elec.Members {
		slim.Members = append(slim.Members, elections.ElectionMember{Account: m.Account})
	}
	se.poaCacheMu.Lock()
	if se.poaElecMembers == nil {
		se.poaElecMembers = make(map[uint64]*elections.ElectionResult)
	}
	se.poaElecMembers[epoch] = slim
	se.poaCacheMu.Unlock()
	return slim, nil
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

// accountInFundedVaultCommittees is the pure core: while any generation is
// fund-holding, is account a member of the current election (currentMembers),
// or a party (commitment bitset) of ANY keygen/reshare commitment of a
// fund-holding generation?
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
	parties, err := fundedVaultPartySet(vaults, btcContract, listCommits, getElection)
	if err != nil {
		return true, err
	}
	return parties[who], nil
}

// fundedVaultPartySet returns every account (normalized) that is a party of
// any keygen/reshare commitment of a fund-holding generation. A read error is
// returned (the caller holds and retries).
func fundedVaultPartySet(
	vaults []btcvault.Vault,
	btcContract string,
	listCommits func(keyId string) ([]tss_db.TssCommitment, error),
	getElection func(epoch uint64) (*elections.ElectionResult, error),
) (map[string]bool, error) {
	parties := make(map[string]bool)
	elecCache := make(map[uint64]*elections.ElectionResult)
	for _, v := range vaults {
		if !btcvault.IsFundHoldingStatus(v.Status) {
			continue
		}
		keyId := btcContract + "-" + btcvault.VaultKeyName(v.Generation)
		rows, err := listCommits(keyId)
		if err != nil {
			return nil, fmt.Errorf("commitments of %s: %w", keyId, err)
		}
		for _, c := range rows {
			elec, cached := elecCache[c.Epoch]
			if !cached {
				elec, err = getElection(c.Epoch)
				if err != nil {
					return nil, fmt.Errorf("election %d: %w", c.Epoch, err)
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
				if bv.Bit(idx) == 1 {
					parties[poaseats.NormalizeAccount(m.Account)] = true
				}
			}
		}
	}
	return parties, nil
}
