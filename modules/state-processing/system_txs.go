package state_engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"
	transactionpool "vsc-node/modules/transaction-pool"
	wasm_runtime "vsc-node/modules/wasm/runtime"

	"github.com/ipfs/go-cid"
	dagCbor "github.com/ipfs/go-ipld-cbor"
	"github.com/multiformats/go-multicodec"
	mh "github.com/multiformats/go-multihash"
	"go.mongodb.org/mongo-driver/mongo"
)

type ContractOutput struct {
	Id         string                     `json:"id"`
	ContractId string                     `json:"contract_id"`
	Inputs     []string                   `json:"inputs"`
	Metadata   contracts.ContractMetadata `json:"metadata"`
	//This might not be used

	Results     []contracts.ContractOutputResult `json:"results"      bson:"results"`
	StateMerkle string                           `json:"state_merkle"`

	// Legacy, moved to results array
	TssOps []tss_db.TssOp `json:"tss_ops"`
}

func (output *ContractOutput) Ingest(se *StateEngine, txSelf TxSelf, slotHeight int64) {
	se.Flush()

	txOuts := make(map[string][]int)

	for idx, inputId := range output.Inputs {
		inputTxId := strings.Split(inputId, "-")[0]
		txOuts[inputTxId] = append(txOuts[inputTxId], idx)
	}

	for txId, txOutIdxs := range txOuts {
		se.txDb.SetOutput(transactions.SetResultUpdate{
			Id: txId,
			Output: &transactions.TransactionOutput{
				Id:    output.Id,
				Index: txOutIdxs,
			},
		})
	}

	if se.sconf.OnMainnet() || se.sconf.ConsensusParams().TssIndexed(txSelf.BlockHeight) {
		// for testnet, index only above tss index height
		tssOps := output.TssOps
		for _, res := range output.Results {
			tssOps = append(tssOps, res.TssOps...)
		}

		for _, tssOp := range tssOps {
			switch tssOp.Type {
			case "create":
				tssLog.Verbose("creating TSS key", "keyId", tssOp.KeyId, "algo", tssOp.Args, "epochs", tssOp.Epochs)
				_, err := se.tssKeys.FindKey(tssOp.KeyId)

				if err == mongo.ErrNoDocuments {
					se.tssKeys.InsertKey(tssOp.KeyId, tss_db.TssKeyAlgo(tssOp.Args), tssOp.Epochs)
				}
			case "renew":
				key, err := se.tssKeys.FindKey(tssOp.KeyId)
				renewable := err == nil && tssOp.Epochs > 0 &&
					(key.Status == tss_db.TssKeyActive || key.Status == tss_db.TssKeyDeprecated)
				if renewable {
					electionData, elecErr := se.electionDb.GetElectionByHeight(txSelf.BlockHeight)
					if elecErr == nil {
						maxExpiry := electionData.Epoch + tss_db.MaxKeyEpochs
						// base on current epoch so you can't renew infinitely into the future
						newExpiry := electionData.Epoch + tssOp.Epochs
						if newExpiry > maxExpiry {
							newExpiry = maxExpiry
						}
						key.ExpiryEpoch = newExpiry
						// Reactivate deprecated keys.
						if key.Status == tss_db.TssKeyDeprecated {
							key.Status = tss_db.TssKeyActive
							key.DeprecatedHeight = 0
						}
						tssLog.Info(
							"key renewed",
							"keyId",
							key.Id,
							"newExpiryEpoch",
							key.ExpiryEpoch,
							"status",
							key.Status,
						)
						se.tssKeys.SetKey(key)
					}
				}
			case "sign":
				se.tssRequests.SetSignedRequest(tss_db.TssRequest{
					KeyId:  tssOp.KeyId,
					Status: "unsigned",
					Msg:    tssOp.Args,
				})
				// if err == mongo.ErrNoDocuments {
				// 	se.tssKeys.InsertKey(tssOp.KeyId, tss_db.TssKeyAlgo(tssOp.Args))
				// }
			}
		}
	}

	go func() {
		cid, err := cid.Parse(output.StateMerkle)
		if err == nil {
			db := datalayer.NewDataBinFromCid(se.da, cid)
			list, _ := db.List("")
			if list != nil {
				for _, v := range *list {
					cidz, err := db.Get(v)
					if err == nil {
						se.da.Get(*cidz, &common_types.GetOptions{})
					}
				}
			}
		}
	}()
	//Set output history
	se.contractState.IngestOutput(contracts.IngestOutputArgs{
		Id:         output.Id,
		ContractId: output.ContractId,

		Metadata:    output.Metadata,
		StateMerkle: output.StateMerkle,
		Inputs:      output.Inputs,
		Results:     output.Results,

		AnchoredBlock:  txSelf.BlockId,
		AnchoredHeight: slotHeight,
		AnchoredId:     txSelf.TxId,
		AnchoredIndex:  int64(txSelf.Index),
	})
}

type TxCreateContract struct {
	Self TxSelf `json:"-"`

	Version      string               `json:"__v"`
	NetId        string               `json:"net_id"`
	Name         string               `json:"name"`
	Code         string               `json:"code"`
	Runtime      wasm_runtime.Runtime `json:"runtime"`
	Owner        string               `json:"owner"`
	Description  string               `json:"description"`
	StorageProof StorageProof         `json:"storage_proof"`
}

func (tx TxCreateContract) Type() string {
	return "create_contract"
}

func (tx TxCreateContract) TxSelf() TxSelf {
	return tx.Self
}

const CONTRACT_DATA_AVAILABLITY_PROOF_REQUIRED_HEIGHT = 84162592

// ProcessTx implements VSCTransaction.
func (tx *TxCreateContract) ExecuteTx(se *StateEngine) TxResult {
	if wasm_runtime.NewFromString(tx.Runtime.String()).IsErr() {
		return TxResult{
			Success: false,
			Ret:     "runtime name is invalid",
		}
	}

	if len(tx.Self.RequiredAuths) == 0 {
		return TxResult{
			Success: false,
			Ret:     "cannot create contract with posting auths",
		}
	}

	election, err := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Sprintf("failed to get election at height %d: %v", tx.Self.BlockHeight, err),
		}
	}

	verified := tx.StorageProof.Verify(election, se.sconf)
	if !verified {
		return TxResult{
			Success: false,
			Ret:     "invalid storage proof",
		}
	}

	// review2 HIGH #36: tx.Code is user-supplied — cid.MustParse panics on a
	// malformed CID and crashes the node. Fail the tx instead.
	cidz, cidErr := cid.Decode(tx.Code)
	if cidErr != nil {
		return TxResult{
			Success: false,
			Ret:     "invalid contract code cid",
		}
	}
	go func() {
		se.da.Get(cidz, &common_types.GetOptions{})
	}()

	id := common.ContractId(tx.Self.TxId, tx.Self.OpIndex)

	var owner string
	if tx.Owner == "" {
		owner = tx.Self.RequiredAuths[0]
	} else {
		owner = tx.Owner
		if !strings.HasPrefix(owner, "hive:") && !strings.HasPrefix(owner, "did:") {
			owner = "hive:" + owner
		}
	}

	se.contractDb.RegisterContract(id, contracts.Contract{
		Code:           tx.Code,
		Name:           tx.Name,
		Description:    tx.Description,
		Creator:        tx.Self.RequiredAuths[0],
		Owner:          owner,
		Proposer:       tx.Self.RequiredAuths[0],
		TxId:           tx.Self.TxId,
		CreationHeight: tx.Self.BlockHeight,
		// Deploys are not timelocked; RegisterContract defaults activation to
		// creation height when left zero.
		ActivationHeight: tx.Self.BlockHeight,
		Runtime:          tx.Runtime,
	})

	return TxResult{
		Success: true,
	}
}

func (tx *TxCreateContract) ToData() map[string]interface{} {
	return map[string]interface{}{
		"__v":           tx.Version,
		"net_id":        tx.NetId,
		"name":          tx.Name,
		"code":          tx.Code,
		"owner":         tx.Owner,
		"description":   tx.Description,
		"storage_proof": tx.StorageProof,
		"runtime":       tx.Runtime,
	}
}

type StorageProof struct {
	Hash      string                 `json:"hash"`
	Signature dids.SerializedCircuit `json:"signature"`
}

// TODO: Define everything else that'll happen with this
func (sp *StorageProof) Verify(electionInfo elections.ElectionResult, sconf systemconfig.SystemConfig) bool {
	didMembers := make([]dids.BlsDID, 0)
	for _, v := range electionInfo.Members {
		didMembers = append(didMembers, dids.BlsDID(v.Key))
	}
	cid, err := cid.Parse(sp.Hash)

	if err != nil {
		return false
	}
	circuit, err := dids.DeserializeBlsCircuit(sp.Signature, didMembers, cid)

	if err != nil {
		return false
	}
	verified, includedDids, err := circuit.Verify()

	if !verified || err != nil || len(includedDids) < sconf.ConsensusParams().MinSpSigners {
		return false
	}

	return true
}

type TxUpdateContract struct {
	Self         TxSelf                `json:"-"`
	NetId        string                `json:"net_id"`
	Id           string                `json:"id"`
	Name         string                `json:"name"`
	Description  string                `json:"description"`
	Owner        string                `json:"owner,omitempty"`
	Runtime      *wasm_runtime.Runtime `json:"runtime,omitempty"`
	Code         string                `json:"code,omitempty"`
	StorageProof *StorageProof         `json:"storage_proof,omitempty"`
}

type UpdateContractResult struct {
	Success     bool
	CodeUpdated bool
	Err         string
}

func (tx TxUpdateContract) Type() string {
	return "update_contract"
}

func (tx TxUpdateContract) TxSelf() TxSelf {
	return tx.Self
}

func (tx *TxUpdateContract) ToData() map[string]interface{} {
	return map[string]interface{}{
		"net_id":        tx.NetId,
		"id":            tx.Id,
		"name":          tx.Name,
		"description":   tx.Description,
		"owner":         tx.Owner,
		"runtime":       tx.Runtime,
		"code":          tx.Code,
		"storage_proof": tx.StorageProof,
	}
}

func (tx *TxUpdateContract) ExecuteTx(se *StateEngine, hasFee bool) UpdateContractResult {
	if len(tx.Self.RequiredAuths) == 0 {
		return UpdateContractResult{
			Success: false,
			Err:     "cannot update contract with posting auths",
		}
	}
	existing, err := se.contractDb.ContractById(tx.Id, tx.Self.BlockHeight)
	if err != nil {
		return UpdateContractResult{
			Success: false,
			Err:     "failed to retrieve contract to update",
		}
	}
	if tx.Self.RequiredAuths[0] != existing.Owner {
		return UpdateContractResult{
			Success: false,
			Err:     "not owner",
		}
	}
	updatedContract := contracts.Contract{
		Name:        tx.Name,
		Description: tx.Description,
		Creator:     existing.Creator,
		Owner:       existing.Owner,
		Runtime:     existing.Runtime,
		Code:        existing.Code,

		// contract update history
		TxId:           tx.Self.TxId,
		CreationHeight: tx.Self.BlockHeight,
		Proposer:       tx.Self.RequiredAuths[0],
		// Queue the whole update behind the network timelock. Until this height
		// the previously-active version (existing) keeps running; ContractById
		// ignores this row while activation_height > the query height.
		ActivationHeight: se.contractUpdateActivationHeight(tx.Self.BlockHeight, se.ActiveConsensusVersion(tx.Self.BlockHeight)),
	}
	if tx.Owner != "" {
		// update owner
		if !strings.HasPrefix(tx.Owner, "hive:") && !strings.HasPrefix(tx.Owner, "did:") {
			updatedContract.Owner = "hive:" + tx.Owner
		} else {
			updatedContract.Owner = tx.Owner
		}
	}
	if hasFee && tx.Code != "" && tx.Code != existing.Code {
		// update contract code
		if wasm_runtime.NewFromString(tx.Runtime.String()).IsErr() {
			return UpdateContractResult{
				Success: false,
				Err:     "runtime name is invalid",
			}
		}
		election, err := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight)
		if err != nil {
			return UpdateContractResult{
				Success: false,
				Err:     "failed to get election",
			}
		}
		verified := tx.StorageProof.Verify(election, se.sconf)
		if !verified {
			return UpdateContractResult{
				Success: false,
				Err:     "invalid storage proof",
			}
		}
		// review2 HIGH #36: tx.Code is user-supplied — guard the parse.
		cidz, cidErr := cid.Decode(tx.Code)
		if cidErr != nil {
			return UpdateContractResult{
				Success: false,
				Err:     "invalid contract code cid",
			}
		}
		go func() {
			se.da.Get(cidz, &common_types.GetOptions{})
		}()
		updatedContract.Code = tx.Code
		updatedContract.Runtime = *tx.Runtime
	}
	se.contractDb.RegisterContract(tx.Id, updatedContract)

	return UpdateContractResult{
		Success:     true,
		CodeUpdated: updatedContract.Code != existing.Code,
	}
}

// contractUpdateActivationHeight returns the height at which an update submitted
// at submitHeight becomes the active code. It is submitHeight (immediate) when
// the network has no timelock, or — on mainnet — while the chain-active consensus
// version has not reached 0.2.0, so a full reindex reproduces historical state
// byte-for-byte. Otherwise it is submitHeight + the network's (non-overridable)
// timelock. `active` is the chain-active consensus version at submitHeight,
// resolved by the caller (se.ActiveConsensusVersion) and passed in so this policy
// stays a pure function of (network, submitHeight, version).
func (se *StateEngine) contractUpdateActivationHeight(submitHeight uint64, active consensusversion.Version) uint64 {
	blocks := se.sconf.ContractUpdateTimelockBlocks()
	if blocks == 0 {
		return submitHeight
	}
	if se.sconf.OnMainnet() {
		// Pre-v0.2.0 (floor below 0.2.0): updates stay immediate so a full reindex
		// reproduces historical state byte-for-byte.
		if !consensusversion.ContractUpdateTimelockActive(active) {
			return submitHeight
		}
	}
	return submitHeight + blocks
}

// TxCancelContractUpdate cancels a contract update that is still in its timelock
// window (queued but not yet active). Owner-gated and free.
type TxCancelContractUpdate struct {
	Self  TxSelf `json:"-"`
	NetId string `json:"net_id"`
	Id    string `json:"id"`
	// TxId optionally targets a single queued update (the tx that queued it). When
	// empty, every pending update for the contract is cancelled.
	TxId string `json:"tx_id,omitempty"`
}

type CancelContractUpdateResult struct {
	Success   bool
	Cancelled int
	Err       string
}

func (tx TxCancelContractUpdate) Type() string {
	return "cancel_contract_update"
}

func (tx TxCancelContractUpdate) TxSelf() TxSelf {
	return tx.Self
}

func (tx *TxCancelContractUpdate) ToData() map[string]interface{} {
	return map[string]interface{}{
		"net_id": tx.NetId,
		"id":     tx.Id,
		"tx_id":  tx.TxId,
	}
}

func (tx *TxCancelContractUpdate) ExecuteTx(se *StateEngine) CancelContractUpdateResult {
	if len(tx.Self.RequiredAuths) == 0 {
		return CancelContractUpdateResult{
			Success: false,
			Err:     "cannot cancel contract update with posting auths",
		}
	}
	// Authorize against the CURRENTLY ACTIVE owner. A queued owner transfer has
	// not taken effect yet, so the existing owner stays in control and can cancel
	// it (e.g. revert a stolen-key hand-off within the window).
	existing, err := se.contractDb.ContractById(tx.Id, tx.Self.BlockHeight)
	if err != nil {
		return CancelContractUpdateResult{
			Success: false,
			Err:     "failed to retrieve contract to cancel update",
		}
	}
	if tx.Self.RequiredAuths[0] != existing.Owner {
		return CancelContractUpdateResult{
			Success: false,
			Err:     "not owner",
		}
	}
	var targetTx *string
	if tx.TxId != "" {
		targetTx = &tx.TxId
	}
	cancelled, err := se.contractDb.CancelPendingUpdate(tx.Id, tx.Self.BlockHeight, tx.Self.BlockHeight, tx.Self.TxId, targetTx)
	if err != nil {
		return CancelContractUpdateResult{
			Success: false,
			Err:     "failed to cancel pending update",
		}
	}
	return CancelContractUpdateResult{
		Success:   true,
		Cancelled: cancelled,
	}
}

type TxElectionResult struct {
	Self TxSelf

	BlockHeight uint64
	Data        string                 `json:"data"`
	Epoch       uint64                 `json:"epoch"`
	NetId       string                 `json:"net_id"`
	EType       string                 `json:"type"`
	Signature   dids.SerializedCircuit `json:"signature"`
}

func (tx TxElectionResult) Type() string {
	return "election_result"
}

func (tx TxElectionResult) TxSelf() TxSelf {
	return tx.Self
}

// aggregatePotentialWeight sums the signing weight of all members of an
// election: 1 per member when the committee is unweighted, otherwise the sum of
// the per-member Weights. GV-L11: the previous inline form ASSIGNED
// potentialWeight = int(Weights[idx]) in the weighted branch instead of
// accumulating, so it held only the LAST member's weight rather than the total.
// Extracted as a pure function so the accumulation is directly testable and
// consistent with the totalWeight computation in ExecuteTx (which already uses
// += correctly).
func aggregatePotentialWeight(election *elections.ElectionResult) int {
	if election == nil {
		return 0
	}
	if len(election.Weights) == 0 {
		return len(election.Members)
	}
	potentialWeight := 0
	for idx := range election.Members {
		if idx < len(election.Weights) {
			potentialWeight += int(election.Weights[idx])
		}
	}
	return potentialWeight
}

// ProcessTx implements VSCTransaction.
func (tx *TxElectionResult) ExecuteTx(se *StateEngine) {
	// ctx := context.Background()
	if tx.Epoch == 0 {
		electionResult := se.electionDb.GetElection(0)

		if electionResult == nil {
			parsedCid, err := cid.Parse(tx.Data)
			if err != nil {
				return
			}
			node, _ := se.da.Get(parsedCid, nil)

			dagNode, _ := dagCbor.Decode(node.RawData(), mh.SHA2_256, -1)
			elecResult := elections.ElectionResult{}
			bbytes, _ := dagNode.MarshalJSON()
			json.Unmarshal(bbytes, &elecResult)

			elecResult.Proposer = tx.Self.RequiredAuths[0]
			elecResult.BlockHeight = tx.Self.BlockHeight
			elecResult.TxId = tx.Self.TxId
			elecResult.Epoch = tx.Epoch
			elecResult.NetId = tx.NetId
			elecResult.Data = tx.Data

			//Store
			se.electionDb.StoreElection(elecResult)
		}
	} else {
		//Validate normally
		prevElection := se.electionDb.GetElection(tx.Epoch - 1)
		if prevElection == nil {
			log.Debug("election ignored: no previous election", "epoch", tx.Epoch)
			return
		}

		if prevElection.Epoch >= tx.Epoch {
			log.Debug("election ignored: out of order", "epoch", tx.Epoch, "prevEpoch", prevElection.Epoch)
			return
		}

		// Ignore duplicate election results from the dedup epoch onwards
		if se.sconf.ConsensusParams().ElectionDupeFixActive(tx.Epoch) {
			if existing := se.electionDb.GetElection(tx.Epoch); existing != nil {
				log.Verbose("duplicate election ignored", "epoch", tx.Epoch)
				return
			}
		}

		//Weight that is calculated by aggregating number of signers
		potentialWeight := aggregatePotentialWeight(prevElection)
		memberDids := make([]dids.BlsDID, 0)
		for _, value := range prevElection.Members {
			memberDids = append(memberDids, dids.BlsDID(value.Key))
		}
		_ = potentialWeight

		verifyObj := map[string]interface{}{
			"__t":    "approve_election",
			"data":   tx.Data,
			"epoch":  tx.Epoch,
			"net_id": tx.NetId,
			"type":   tx.EType,
		}
		verifyData, _ := common.EncodeDagCbor(verifyObj)

		verifyHash, err := cid.Prefix{
			Version:  1,
			Codec:    uint64(multicodec.DagCbor),
			MhType:   mh.SHA2_256,
			MhLength: -1,
		}.Sum(verifyData)

		if err != nil {
			log.Debug("failed to build election verify hash", "epoch", tx.Epoch, "err", err)
			return
		}

		parsedCid, _ := cid.Parse(tx.Data)

		blsCircuit, err := dids.DeserializeBlsCircuit(tx.Signature, memberDids, verifyHash)
		if err != nil {
			return
		}

		verified, includedDids, err := blsCircuit.Verify()

		totalWeight := uint64(0)
		if len(prevElection.Weights) == 0 {
			totalWeight = uint64(len(prevElection.Members))
		} else {
			for _, v := range prevElection.Weights {
				totalWeight = totalWeight + v
			}
		}

		blocksLastElection := tx.Self.BlockHeight - prevElection.BlockHeight
		minimums := elections.MinimalRequiredElectionVotes(blocksLastElection, totalWeight, elections.ResultVersion(*prevElection))

		realWeight := uint64(0)
		bv := blsCircuit.RawBitVector()
		for idx := range prevElection.Members {
			if bv.Bit(idx) == 1 {
				if len(prevElection.Weights) == 0 {
					realWeight += 1
				} else {
					realWeight += prevElection.Weights[idx]
				}
			}
		}
		log.Verbose("election verify",
			"epoch", tx.Epoch,
			"verified", verified,
			"realWeight", realWeight,
			"minWeight", minimums,
			"includedDids", len(includedDids),
		)

		if verified && realWeight >= minimums {
			// Fetch and decode the body BEFORE the settlement gate so we
			// can apply any inlined settlement. The settlement now lands
			// atomically with the election (carried inside the body the
			// header references); applying it here advances
			// latestSettledEpoch so the defensive gate below passes for
			// the new flow.
			se.da.GetDag(parsedCid)
			node, _ := se.da.Get(parsedCid, nil)
			dagNode, _ := dagCbor.Decode(node.RawData(), mh.SHA2_256, -1)
			elecResult := elections.ElectionResult{
				Proposer:    tx.Self.RequiredAuths[0],
				BlockHeight: tx.Self.BlockHeight,
				TxId:        tx.Self.TxId,
			}
			elecResult.Epoch = tx.Epoch
			elecResult.NetId = tx.NetId
			elecResult.Data = tx.Data

			bbytes, _ := dagNode.MarshalJSON()
			json.Unmarshal(bbytes, &elecResult)

			// Apply inlined pendulum settlement BEFORE StoreElection so
			// validatePendulumSettlement's GetElectionByHeight(SnapshotRangeTo)
			// resolves to the CLOSING committee, not the incoming one. Two
			// cases:
			//
			//   1) Settlement field is non-nil — the proposer composed and
			//      embedded the closing epoch's payout. Apply it. The new
			//      production flow always takes this branch for tx.Epoch >= 1.
			//
			//   2) Settlement field is nil AND the closing epoch isn't already
			//      settled — historical / replay path. Synthesise a zero-
			//      payout marker so latestSettledEpoch still advances and the
			//      gate sequence below stays consistent. Per design
			//      ("no settlement op == 0 settlement"), bucket HBD for that
			//      epoch is forfeited. Skip the synthesis entirely if a prior
			//      BlockTypePendulumSettlement block-tx already settled the
			//      epoch — that path also advances latestSettledEpoch.
			if tx.Epoch >= 1 {
				if elecResult.Settlement != nil {
					se.applyPendulumSettlement(*elecResult.Settlement, tx.Self.BlockHeight)
				} else if tx.Epoch >= 2 && se.GetLatestSettledEpoch() < tx.Epoch-1 {
					markerRec := pendulumsettlement.SettlementRecord{
						Epoch:               tx.Epoch - 1,
						PrevEpoch:           se.GetLatestSettledEpoch(),
						SnapshotRangeFrom:   prevElection.BlockHeight,
						SnapshotRangeTo:     tx.Self.BlockHeight,
						BucketBalanceHBD:    0,
						TotalDistributedHBD: 0,
						ResidualHBD:         0,
						RewardReductions:    []pendulumsettlement.RewardReductionEntry{},
						Distributions:       []pendulumsettlement.DistributionEntry{},
					}
					log.Info("election_result: synthesising 0-settlement marker for replay",
						"tx_epoch", tx.Epoch,
						"marker_epoch", markerRec.Epoch,
						"latest_settled_before", markerRec.PrevEpoch)
					se.applyPendulumSettlement(markerRec, tx.Self.BlockHeight)
				}
			}

			// Pendulum settlement gate (defence in depth). For new-flow
			// elections the apply step above advanced latestSettledEpoch
			// to tx.Epoch-1 so this is unreachable. For historical replay
			// elections that fell into the synthesis branch, the synthetic
			// marker likewise advanced it. The gate stays here as a guard
			// against future code paths bypassing apply, or a malformed
			// embedded settlement that applyPendulumSettlement silently
			// rejected (mis-ordered, validation failed, etc.) — in that
			// case the election is deferred until something else settles
			// the closing epoch.
			//
			// Genesis path: when prevElection.Epoch == 0 there is no closing
			// epoch to settle, so the gate only applies once a real first
			// epoch has been served (tx.Epoch >= 2).
			if tx.Epoch >= 2 {
				latestSettled := se.GetLatestSettledEpoch()
				if latestSettled < tx.Epoch-1 {
					log.Warn("pendulum settlement gate: election_result deferred",
						"tx_epoch", tx.Epoch,
						"required_settled_epoch", tx.Epoch-1,
						"latest_settled_epoch", latestSettled)
					return
				}
			}

			if err := se.electionDb.StoreElection(elecResult); err != nil {
				log.Error("failed to store election", "epoch", tx.Epoch, "err", err)
			}
			log.Info("election processed",
				"epoch", tx.Epoch,
				"proposer", elecResult.Proposer,
				"new_members", len(elecResult.Members),
				"new_weight", sumWeights(elecResult.Weights, len(elecResult.Members)),
				"participation", fmt.Sprintf("%d/%d", realWeight, totalWeight),
			)
		} else {
			log.Debug("election failed verification", "epoch", tx.Epoch, "verified", verified, "realWeight", realWeight, "minWeight", minimums)
		}

	}
}

func (tx *TxElectionResult) ToData() map[string]interface{} {

	return map[string]interface{}{
		"epoch":     tx.Epoch,
		"net_id":    tx.NetId,
		"data":      tx.Data,
		"signature": tx.Signature,
	}
}

// TxProposeConsensusVersion schedules a target major.consensus to switch to at ActivationEpoch.
// Activation requires the stake-readiness guard to pass at election build (see election-proposer).
type TxProposeConsensusVersion struct {
	Self            TxSelf
	NetId           string `json:"net_id"`
	Major           uint64 `json:"major"`
	Consensus       uint64 `json:"consensus"`
	NonConsensus    uint64 `json:"non_consensus"`
	ActivationEpoch uint64 `json:"activation_epoch"`
}

func (tx TxProposeConsensusVersion) Type() string {
	return "propose_consensus_version"
}

func (tx TxProposeConsensusVersion) TxSelf() TxSelf {
	return tx.Self
}

func (tx *TxProposeConsensusVersion) ExecuteTx(se *StateEngine) {
	se.executeProposeConsensusVersion(tx)
}

// TxRecoverySuspend sets chain-global processing suspension until recovery_require_version (multisig only).
type TxRecoverySuspend struct {
	Self TxSelf
}

func (tx TxRecoverySuspend) Type() string {
	return "recovery_suspend"
}

func (tx TxRecoverySuspend) TxSelf() TxSelf {
	return tx.Self
}

func (tx *TxRecoverySuspend) ExecuteTx(se *StateEngine) {
	se.executeRecoverySuspend(tx)
}

// TxRecoveryRequireVersion clears suspension and sets adopted / minimum version (multisig only).
type TxRecoveryRequireVersion struct {
	Self          TxSelf
	Major         uint64 `json:"major"`
	Consensus     uint64 `json:"consensus"`
	NonConsensus  uint64 `json:"non_consensus"`
	Reason        string `json:"reason,omitempty"`
	CheckpointRef string `json:"checkpoint_ref,omitempty"`
}

func (tx TxRecoveryRequireVersion) Type() string {
	return "recovery_require_version"
}

func (tx TxRecoveryRequireVersion) TxSelf() TxSelf {
	return tx.Self
}

func (tx *TxRecoveryRequireVersion) ExecuteTx(se *StateEngine) {
	se.executeRecoveryRequireVersion(tx)
}

type TxProposeBlock struct {
	Self TxSelf

	//ReplayId should be deprecated soon
	NetId       string            `json:"net_id"`
	SignedBlock SignedBlockHeader `json:"signed_block"`

	Signers      []string `json:"-"`
	Epoch        uint64   `json:"-"`
	SigningScore uint64   `json:"-"`
	SigningTotal uint64   `json:"-"`
}

func (tx *TxProposeBlock) Type() string {
	return "propose_block"
}

func (tx *TxProposeBlock) TxSelf() TxSelf {
	return tx.Self
}

// BlockValidationKind classifies a block proposal for the principal-slash
// detector. Exactly one kind applies per proposal.
type BlockValidationKind int

const (
	// BlockSkip: validation could not be attempted (transient/environmental,
	// e.g. election lookup or header-hash failure). This is the zero value, so
	// an unset outcome defaults to the safe no-op — never apply, never slash. A
	// replay with a healthy view reaches a deterministic verdict.
	BlockSkip BlockValidationKind = iota
	// BlockValid: the BLS aggregate verifies, meets 2/3, and the block is in
	// window. Apply it.
	BlockValid
	// BlockInvalid: provably invalid from the proposal bytes alone (malformed
	// CID, non-deserializable signature, failed BLS verify, sub-2/3 score).
	// Every replaying node decides this identically — slash.
	BlockInvalid
	// BlockStale: deterministically late — the op confirmed at/after the end of
	// the producer's slot window. A liveness fault (slow producer / Hive
	// congestion) that a correct node can hit under conditions outside its
	// control, NOT a provable safety fault. Reject the block but do NOT slash;
	// reward-reduction handles tardiness elsewhere.
	BlockStale
)

// BlockValidationOutcome carries the result of TxProposeBlock validation: a
// single verdict plus an optional human-readable reason. The Kind split lets
// the detector distinguish a provable safety fault (BlockInvalid → slash) from
// a transient skip or a deterministic-but-honest staleness, so a corrupt
// election cache, a transient DB error during replay, or a slow-but-honest
// producer never yields a false-positive slash.
type BlockValidationOutcome struct {
	Kind BlockValidationKind
	// Reason is logged for non-applied verdicts so operators can see why a
	// proposal was skipped, rejected as stale, or slashed. Empty for BlockValid.
	Reason string
}

func (t *TxProposeBlock) Validate(se *StateEngine) bool {
	return t.ValidateDetailed(se).Kind == BlockValid
}

// ValidateDetailed mirrors Validate but exposes the skip-vs-invalid
// distinction needed by the principal-slash detectors.
func (t *TxProposeBlock) ValidateDetailed(se *StateEngine) BlockValidationOutcome {
	elecResult, err := se.electionDb.GetElectionByHeight(t.Self.BlockHeight)
	if err != nil {
		// Election lookup failure is environmental, not fault of producer.
		return BlockValidationOutcome{Kind: BlockSkip, Reason: "election lookup failed: " + err.Error()}
	}
	memberDids := make([]dids.BlsDID, 0)
	for _, member := range elecResult.Members {
		memberDids = append(memberDids, dids.BlsDID(member.Key))
	}

	blockCid, cidErr := cid.Parse(t.SignedBlock.Block)
	if cidErr != nil {
		// Malformed CID: the block can't be checked at all. Treat as
		// proven-invalid (deterministic decision from the bytes), not skip.
		return BlockValidationOutcome{Kind: BlockInvalid, Reason: "malformed block CID"}
	}
	blockHeader := vscBlocks.VscHeader{
		Type:    t.SignedBlock.Type,
		Version: t.SignedBlock.Version,
		Headers: struct {
			Br    [2]int  "refmt:\"br\""
			Prevb *string "refmt:\"prevb\""
		}{
			Br:    t.SignedBlock.Headers.Br,
			Prevb: t.SignedBlock.Headers.PrevBlock,
		},
		MerkleRoot: t.SignedBlock.MerkleRoot,
		Block:      blockCid,
	}

	headerCid, hashErr := se.da.HashObject(blockHeader)
	if hashErr != nil || headerCid == nil {
		return BlockValidationOutcome{Kind: BlockSkip, Reason: "header hash failed"}
	}

	circuit, dErr := dids.DeserializeBlsCircuit(t.SignedBlock.Signature, memberDids, *headerCid)
	if dErr != nil {
		// Malformed signature bytes — deterministic from the proposal.
		return BlockValidationOutcome{Kind: BlockInvalid, Reason: "malformed BLS signature"}
	}

	// Staleness: the op confirmed at/after the end of the producer's slot
	// window (Br[1] is the slot start height). All inputs here are on-chain and
	// identical on every replaying node, but a correct-but-slow producer can hit
	// this under normal Hive congestion / BLS-collection latency outside its
	// control, so it is a liveness fault — reject the block but do NOT slash.
	if uint64(t.SignedBlock.Headers.Br[1])+CONSENSUS_SPECS.SlotLength <= t.Self.BlockHeight {
		return BlockValidationOutcome{
			Kind: BlockStale,
			Reason: fmt.Sprintf(
				"block confirmed past slot window: slot=%d cutoff=%d confirmed=%d",
				t.SignedBlock.Headers.Br[1],
				uint64(t.SignedBlock.Headers.Br[1])+CONSENSUS_SPECS.SlotLength,
				t.Self.BlockHeight,
			),
		}
	}

	verified, includedDids, vErr := circuit.Verify()
	if vErr != nil || !verified {
		return BlockValidationOutcome{Kind: BlockInvalid, Reason: "BLS aggregate failed verification"}
	}

	signingScore, total := elections.CalculateSigningScore(circuit, elecResult)
	t.SigningScore = signingScore
	t.SigningTotal = total

	for _, did := range includedDids {
		for _, member := range elecResult.Members {
			if did.String() == member.Key {
				t.Signers = append(t.Signers, member.Account)
			}
		}
	}
	t.Epoch = elecResult.Epoch

	if signingScore > ((total * 2) / 3) {
		return BlockValidationOutcome{Kind: BlockValid}
	}
	return BlockValidationOutcome{Kind: BlockInvalid, Reason: "signing score below 2/3 threshold"}
}

// ProcessTx implements VSCTransaction.
func (t *TxProposeBlock) ExecuteTx(se *StateEngine) {
	start := time.Now()

	blockCid, err := cid.Parse(t.SignedBlock.Block)
	if err != nil {
		return
	}
	node, err := se.da.GetDag(blockCid)
	if err != nil || node == nil {
		return
	}
	jsonBytes, err := node.MarshalJSON()
	if err != nil {
		return
	}
	blockContentC := vscBlocks.VscBlock{}
	// json.Unmarshal(jsonBytes, &blockContent)

	se.da.GetObject(blockCid, &blockContentC, common_types.GetOptions{})

	slotInfo := CalculateSlotInfo(t.Self.BlockHeight)

	se.vscBlocks.StoreHeader(vscBlocks.VscHeaderRecord{
		Id: t.Self.TxId,

		MerkleRoot: blockContentC.MerkleRoot,
		Proposer:   t.Self.RequiredAuths[0],
		SigRoot:    blockContentC.SigRoot,

		EndBlock:     t.SignedBlock.Headers.Br[1],
		StartBlock:   t.SignedBlock.Headers.Br[0] + 1,
		BlockContent: t.SignedBlock.Block,

		// SlotHeight: ,

		Signers:    t.Signers,
		Epoch:      t.Epoch,
		SlotHeight: int(slotInfo.StartHeight),
		Stats: struct {
			Size uint64 `bson:"size"`
		}{
			Size: uint64(len(jsonBytes)),
		},
		Ts:        t.Self.Timestamp,
		DebugData: blockContentC,
	})

	se.logMagiBlock(t, &blockContentC, slotInfo.StartHeight, start)

	txsToInjest := make([]TxPacket, 0)

	nonceUpdates := make(map[string]uint64)

	type confirmedNonceInfo struct {
		RequiredAuths []string
		Nonces        map[uint64]bool
	}
	confirmedNonces := make(map[string]*confirmedNonceInfo)

	//At this point of the process a call should be made to state engine
	//To kick off finalization of the inflight state
	//Such as transfers, contract calls, etc
	//New TXs should be indexed at this point
	for idx, txInfo := range blockContentC.Transactions {
		tx := BlockTx{
			Id:   txInfo.Id,
			Op:   txInfo.Op,
			Type: txInfo.Type,
		}
		//Things to Process
		// - Contract executionll
		// - Transfers, withdraws
		// - New TXs (repeat process in state engine)
		//Note: VSC txs can be processed immediately once anchored on chain
		//Thus: TX confirmation is 30s maximum
		//Author: @vaultec81

		txContainer, err := tx.Decode(se.da, TxSelf{
			TxId:        txInfo.Id,
			Index:       idx,
			BlockHeight: uint64(t.SignedBlock.Headers.Br[1]),
			BlockId:     t.Self.BlockId,
			Timestamp:   t.Self.Timestamp,
		})
		if err != nil {
			log.Error("block tx decode failed, skipping", "id", txInfo.Id, "idx", idx, "err", err)
			continue
		}

		if txContainer.Type() == "transaction" {
			tx, err := txContainer.AsTransaction()
			if err != nil {
				log.Error("block tx AsTransaction failed, skipping", "id", txInfo.Id, "idx", idx, "err", err)
				continue
			}

			tx.Ingest(se, t.Self.TxId, TxSelf{
				BlockId:     t.Self.BlockId,
				BlockHeight: uint64(t.SignedBlock.Headers.Br[1]),
				//
				Index: idx,
			})

			if len(tx.Headers.RequiredAuths) == 0 {
				continue
			}
			keyId := transactionpool.HashKeyAuths(tx.Headers.RequiredAuths)
			if nonceUpdates[keyId] < tx.Headers.Nonce || nonceUpdates[keyId] == 0 {
				nonceUpdates[keyId] = tx.Headers.Nonce
			}

			if confirmedNonces[keyId] == nil {
				confirmedNonces[keyId] = &confirmedNonceInfo{
					RequiredAuths: tx.Headers.RequiredAuths,
					Nonces:        make(map[uint64]bool),
				}
			}
			confirmedNonces[keyId].Nonces[tx.Headers.Nonce] = true

			txs, txErr := tx.ToTransaction()
			if txErr != nil {
				// The tx contains an op that can't be executed exactly as
				// submitted (unknown type / undecodable payload). Fail the WHOLE
				// tx: execute no op, mark it FAILED, and charge the payer a fixed
				// RC. Deterministic — every node hits the identical resolve error
				// (RequiredAuths is non-empty here, guaranteed by the guard above).
				log.Warn("offchain tx invalid; failing entire tx",
					"id", tx.Cid().String(), "err", txErr)
				txsToInjest = append(txsToInjest, TxPacket{
					TxId:    tx.Cid().String(),
					Invalid: true,
					Payer:   tx.Headers.RequiredAuths[0],
				})
			} else {
				txsToInjest = append(txsToInjest, TxPacket{
					TxId: tx.Cid().String(),
					Ops:  txs,
				})
			}
		} else if txContainer.Type() == "output" {
			contractOutput, err := txContainer.AsContractOutput()
			if err != nil {
				log.Error("block tx AsContractOutput failed, skipping", "id", txInfo.Id, "idx", idx, "err", err)
				continue
			}

			contractOutput.Ingest(se, TxSelf{
				BlockId:     t.Self.BlockId,
				BlockHeight: t.Self.BlockHeight,
				TxId:        t.Self.TxId,
			}, int64(se.slotStatus.SlotHeight))

		} else if txContainer.Type() == "oplog" {
			oplog, err := txContainer.AsOplog(uint64(t.SignedBlock.Headers.Br[1]))
			if err != nil {
				log.Error("block tx AsOplog failed, skipping", "id", txInfo.Id, "idx", idx, "err", err)
				continue
			}
			oplog.ExecuteTx(se)
		} else if txContainer.Type() == "pendulum_settlement" {
			rec, ok := txContainer.AsPendulumSettlement()
			if !ok {
				log.Warn("pendulum settlement: failed to decode block tx", "id", txInfo.Id)
				continue
			}
			// The closing committee's 2/3 BLS aggregate over the carrying VSC
			// block already validated the bytes; applyPendulumSettlement trusts
			// the payload and pairs the bucket debit with per-account credits.
			se.applyPendulumSettlement(rec, uint64(t.SignedBlock.Headers.Br[1]))
		} else if txContainer.Type() == "safety_slash_reverse" {
			rec, ok := txContainer.AsSafetySlashReverse()
			if !ok {
				log.Warn("safety slash reverse: failed to decode block tx", "id", txInfo.Id)
				continue
			}
			se.applySafetySlashReverse(rec, txInfo.Id, uint64(t.SignedBlock.Headers.Br[1]))
		}
	}

	for k, v := range nonceUpdates {
		se.nonceDb.SetNonce(k, v+1)
	}

	for _, info := range confirmedNonces {
		nonces := make([]uint64, 0, len(info.Nonces))
		for n := range info.Nonces {
			nonces = append(nonces, n)
		}
		se.txDb.InvalidateCompetingTransactions(info.RequiredAuths, nonces)
	}

	se.TxBatch = append(txsToInjest, se.TxBatch...)
}

type SignedBlockHeader struct {
	UnsignedBlockHeader
	Signature dids.SerializedCircuit `json:"signature"`
}

type UnsignedBlockHeader struct {
	Type    string `json:"__t"`
	Version string `json:"__v"`
	Headers struct {
		PrevBlock *string `json:"prevb"`
		Br        [2]int  `json:"br"`
	} `json:"headers"`
	//Define a potential struct to streamline merkle proofs.
	//Maybe convert to that struct too
	MerkleRoot *string `json:"merkle_root"`
	Block      string  `json:"block"`
}

type BlockContent struct {
	Headers struct {
		PrevBlock string `json:"prevb"`
	} `json:"headers"`
	Transactions []BlockTx `json:"txs"`

	//Maybe in future make a magic merkle root class with a prototype of string..
	//..and functions to do proof verification
	MerkleRoot string `json:"merkle_root"`
	SigRoot    string `json:"sig_root"`
}

// Reference pointer to the transaction itself
type BlockTx struct {
	Id string  `json:"id"`
	Op *string `json:"op,omitempty"`
	// 1 input
	// 2 output
	// 5 anchor
	// 6 oplog

	Type int `json:"type"`
}

func (bTx *BlockTx) Decode(da *datalayer.DataLayer, txSelf TxSelf) (TransactionContainer, error) {
	txCid, err := cid.Parse(bTx.Id)
	if err != nil {
		return TransactionContainer{}, fmt.Errorf("invalid tx CID %q: %w", bTx.Id, err)
	}

	dagNode := getDagOrBlock(da, txCid, fmt.Sprintf("GetDag(blockTx %s)", bTx.Id))

	tx := TransactionContainer{
		da:      da,
		Id:      bTx.Id,
		TypeInt: bTx.Type,
		Self:    txSelf,
	}
	tx.Decode(dagNode.RawData())

	return tx, nil
}

// shortCid abbreviates a CID string to its trailing 8 characters for log
// readability. Mirrors a git short-sha.
func shortCid(c string) string {
	if len(c) <= 8 {
		return c
	}
	return c[len(c)-8:]
}

// sumWeights returns the total weight of an election. If weights are nil
// (unweighted election), returns the member count.
func sumWeights(weights []uint64, memberCount int) uint64 {
	if len(weights) == 0 {
		return uint64(memberCount)
	}
	total := uint64(0)
	for _, w := range weights {
		total += w
	}
	return total
}

// logMagiBlock emits a one-line summary for a finalized Magi (VSC L2) block.
// During live sync, every block is logged. During catchup, the log is
// throttled to once per 10,000 L1 blocks and tagged with indexing=true so
// operators can tell the node is still working through history.
func (se *StateEngine) logMagiBlock(t *TxProposeBlock, blk *vscBlocks.VscBlock, slot uint64, start time.Time) {
	liveSynced := se.IsLiveSynced(int(t.Self.BlockHeight))

	if !liveSynced && t.Self.BlockHeight-se.lastMagiLogHeight < 10000 {
		return
	}
	se.lastMagiLogHeight = t.Self.BlockHeight

	hiveHead, err := se.hiveBlocks.GetHighestBlock()
	if err != nil {
		hiveHead = t.Self.BlockHeight
	}
	fields := []any{
		"slot", slot,
		"cid", shortCid(t.SignedBlock.Block),
		"txs", len(blk.Transactions),
		"proposer", t.Self.RequiredAuths[0],
		"participation", fmt.Sprintf("%d/%d", t.SigningScore, t.SigningTotal),
		"hive_head", hiveHead,
	}
	if liveSynced {
		fields = append(fields, "elapsed_ms", time.Since(start).Milliseconds())
		log.Info("Magi block", fields...)
	} else {
		log.Info("Magi block (indexing)", fields...)
	}
}
