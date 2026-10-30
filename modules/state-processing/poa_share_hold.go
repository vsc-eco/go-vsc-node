package state_engine

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"vsc-node/lib/btcvault"
	"vsc-node/modules/common/consensusversion"
	vdb "vsc-node/modules/db"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/poaseats"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// HeldShareOfFundedVaultOrBlock reports whether the POA-1 bond lock holds
// account (consensus 0.9.0): while any BTC vault generation is in a
// bond-locking status (Active, Retiring or Draining, whatever its balance; see
// poaLocksBond), the account is electable, recently active as a witness, in
// the current committee, or a party of any keygen/reshare of such a generation.
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
// registry status only (poaLocksBond): the registry carries no balance, and an
// Active generation can receive a deposit at any block while every past
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
// Why Inactive releases it: an Inactive generation has been swept empty. Its
// grace period (mainnet 4320 BTC blocks) exists only so a late deposit to an
// old address is still credited, and a late deposit turns it back to Draining,
// which locks again. Holding every past party of the old key for that whole
// grace kept ex-members who left months ago locked for ~30 days after every
// migration. The generation's last committee stays locked through the grace by
// the retiring-member lock (vaultrotation eligibility, L4-C1), which this does
// not change.
//
// Consensus inputs only: the registry at the height, the commitment rows, and
// the elections they decode against. Never the tss_keys collection (a node
// rewrites its statuses at startup). Transient errors are returned; a
// deterministic absence decides.
func (se *StateEngine) heldShareOfFundedVault(account string, height uint64) (bool, error) {
	if se.sconf == nil || se.tssCommitments == nil || se.electionDb == nil || height == 0 {
		return false, nil
	}
	evmRegs, err := se.evmVaultRegistries(height)
	if err != nil {
		return true, err
	}
	registered := map[string]bool{}
	evmRegFunded := false
	for _, r := range evmRegs {
		registered[r.contract] = true
		if r.corrupt {
			return true, nil // present but undecodable: fail closed, as for BTC
		}
		for _, v := range r.vaults {
			evmRegFunded = evmRegFunded || poaLocksBond(v.Status)
		}
	}
	evmFunded, evmHolders, err := se.evmVaultHolders(height, registered)
	if err != nil {
		return true, err
	}
	evmFunded = evmFunded || evmRegFunded
	btcContract := se.sconf.OracleParams().ContractId("BTC")
	var rawV []byte
	var vaults []btcvault.Vault
	btcFunded := false
	if btcContract != "" {
		var transient error
		reader := se.btcContractStateReaderAtStrict(btcContract, height, &transient)
		raw, ok := reader("v")
		if transient != nil {
			return true, transient
		}
		if ok && len(raw) > 0 {
			vs, err := btcvault.UnmarshalVaultRegistry(raw)
			if err != nil {
				// Present but undecodable is corrupt committed state, identical on
				// every node: fail closed, as the retiring-member bond lock does.
				return true, nil
			}
			rawV, vaults = raw, vs
			for _, v := range vs {
				btcFunded = btcFunded || poaLocksBond(v.Status)
			}
		}
	}
	if !btcFunded && !evmFunded {
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
	if evmHolders[who] {
		return true, nil
	}
	// A01: every keygen/reshare party of a funded EVM generation, the BTC rule.
	for _, r := range evmRegs {
		parties, perr := se.fundedVaultParties(r.contract, btcvault.EvmVaultKeyBase, r.raw, r.vaults, height-1)
		if perr != nil {
			return true, perr
		}
		if parties[who] {
			return true, nil
		}
	}
	if !btcFunded {
		return false, nil
	}
	parties, err := se.fundedVaultParties(btcContract, btcvault.BtcVaultKeyBase, rawV, vaults, height-1)
	if err != nil {
		return true, err
	}
	return parties[who], nil
}

// evmVaultHolders is POA-1 for the EVM bridges' vault keys (C2, consensus
// 0.13.0, consensusversion.EvmVaultCustodyActive): whether any such key exists
// below height, and the accounts named by each key's newest keygen/reshare
// commitment below height. An EVM bridge key holds funds from its keygen on
// (any block can bring a deposit) and is never drained, so its existence is
// what "funded" means here.
//
// Only the newest committee is bound, not every past one as for a BTC
// generation: a BTC generation drains and releases its parties, but this key
// is reshared for good, so binding every committee that ever held it would lock
// those bonds forever. The newest holders are exactly the accounts C1 keeps
// readiness-eligible (vaultrotation.ComputeRetiringSignerSet), so nobody signs
// for the vault with a released bond. Retiring older shares is the key
// rotation's job (C3).
//
// A01: a bridge with a vault registry (registered) is held by its registry
// instead, every party of every funded generation as for BTC, and is skipped.
func (se *StateEngine) evmVaultHolders(height uint64, registered map[string]bool) (bool, map[string]bool, error) {
	keys := se.sconf.OracleParams().EvmVaultKeyIds(height)
	if len(keys) == 0 || !consensusversion.EvmVaultCustodyActive(se.ActiveConsensusVersion(height)) {
		return false, nil, nil
	}
	funded := false
	holders := map[string]bool{}
	for _, keyId := range keys {
		if registered[strings.TrimSuffix(keyId, "-"+btcvault.EvmVaultKeyBase)] {
			continue
		}
		c, err := se.tssCommitments.GetCommitmentByHeight(keyId, height, "reshare", "keygen")
		if err != nil {
			if isTransientReadErr(err) {
				return true, nil, err
			}
			continue // no keygen yet: no key, nothing held for it
		}
		funded = true
		elec, err := se.poaElectionMembers(c.Epoch)
		if err != nil {
			return true, nil, err
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
				holders[poaseats.NormalizeAccount(m.Account)] = true
			}
		}
	}
	return funded, holders, nil
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
func (se *StateEngine) fundedVaultParties(btcContract, base string, rawV []byte, vaults []btcvault.Vault, below uint64) (map[string]bool, error) {
	sum := sha256.Sum256(rawV)
	key := fmt.Sprintf("%s|%s|%d|%d|%x", btcContract, base, below, se.tssCommitWrites.Load(), sum)
	se.poaCacheMu.Lock()
	if se.poaPartyMemo.key == key && se.poaPartyMemo.parties != nil {
		parties := se.poaPartyMemo.parties
		se.poaCacheMu.Unlock()
		return parties, nil
	}
	se.poaCacheMu.Unlock()

	parties, err := fundedVaultPartySet(vaults, btcContract, base,
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

// poaLocksBond is the POA-1 status set: Active, Retiring and Draining. It is
// btcvault.IsFundHoldingStatus without Inactive (see heldShareOfFundedVault
// for why). Only the bond lock uses it; reshare skipping, deposit crediting and
// the retiring-member lock keep counting Inactive as fund-holding.
func poaLocksBond(s btcvault.VaultStatus) bool {
	return btcvault.IsFundHoldingStatus(s) && s != btcvault.VaultStatusInactive
}

// accountInFundedVaultCommittees is the pure core: while any generation is
// bond-locking (poaLocksBond), is account a member of the current election
// (currentMembers), or a party (commitment bitset) of ANY keygen/reshare
// commitment of a bond-locking generation?
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
		if poaLocksBond(v.Status) {
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
	parties, err := fundedVaultPartySet(vaults, btcContract, btcvault.BtcVaultKeyBase, listCommits, getElection)
	if err != nil {
		return true, err
	}
	return parties[who], nil
}

// fundedVaultPartySet returns every account (normalized) that is a party of
// any keygen/reshare commitment of a bond-locking generation. A read error is
// returned (the caller holds and retries).
func fundedVaultPartySet(
	vaults []btcvault.Vault,
	btcContract string,
	base string,
	listCommits func(keyId string) ([]tss_db.TssCommitment, error),
	getElection func(epoch uint64) (*elections.ElectionResult, error),
) (map[string]bool, error) {
	parties := make(map[string]bool)
	elecCache := make(map[uint64]*elections.ElectionResult)
	for _, v := range vaults {
		if !poaLocksBond(v.Status) {
			continue
		}
		keyId := btcContract + "-" + btcvault.VaultKeyNameFor(base, v.Generation)
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

// evmVaultRegistry is a rotating EVM vault's registry at a height (A01).
type evmVaultRegistry struct {
	contract string
	raw      []byte
	vaults   []btcvault.Vault
	corrupt  bool
}

// evmVaultRegistries reads the vault registry of every EVM bridge whose vault
// rolls keys at height (consensus 0.13.0). A bridge with no registry yet is
// left out (its key is held by the C2 newest-holders rule).
func (se *StateEngine) evmVaultRegistries(height uint64) ([]evmVaultRegistry, error) {
	var out []evmVaultRegistry
	for _, c := range se.evmRotatingContracts(height) {
		var transient error
		raw, ok := se.btcContractStateReaderAtStrict(c, height, &transient)("v")
		if transient != nil {
			return nil, transient
		}
		if !ok || len(raw) == 0 {
			continue
		}
		vs, err := btcvault.UnmarshalVaultRegistry(raw)
		out = append(out, evmVaultRegistry{contract: c, raw: raw, vaults: vs, corrupt: err != nil})
	}
	return out, nil
}
