package state_engine

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	"vsc-node/modules/db/vsc/pendulum_settlements"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	"vsc-node/modules/incentive-pendulum/rewards"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
	pendulumwasm "vsc-node/modules/incentive-pendulum/wasm"
	ledgerSystem "vsc-node/modules/ledger-system"
	rcSystem "vsc-node/modules/rc-system"
	tss_helpers "vsc-node/modules/tss/helpers"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_runtime "vsc-node/modules/wasm/runtime_ipc"

	"github.com/btcsuite/btcd/btcec/v2"
	btcecdsa "github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/chebyrash/promise"
	"github.com/multiformats/go-multicodec"
	"go.mongodb.org/mongo-driver/mongo"
)

var tssLog = vsclog.Module("tss")
var log = vsclog.Module("se")

type ProcessExtraInfo struct {
	BlockHeight int
	BlockId     string
	Timestamp   string
}

type StateEngine struct {
	sconf systemconfig.SystemConfig
	da    *DataLayer.DataLayer

	//db access
	witnessDb      witnesses.Witnesses
	electionDb     elections.Elections
	contractDb     contracts.Contracts
	contractState  contracts.ContractState
	txDb           transactions.Transactions
	hiveBlocks     hive_blocks.HiveBlocks
	vscBlocks      vscBlocks.VscBlocks
	claimDb        ledgerDb.InterestClaims
	rcDb           rcDb.RcDb
	nonceDb        nonces.Nonces
	tssRequests    tss_db.TssRequests
	tssKeys        tss_db.TssKeys
	tssCommitments tss_db.TssCommitments

	consensusState      consensus_state.ConsensusState
	chainConsensusCache consensus_state.ChainConsensusState
	chainConsensusMu    sync.RWMutex
	consensusRuntime    ConsensusRuntime

	wasm *wasm_runtime.Wasm

	//Nonce map similar to what we use before
	NonceMap map[string]int
	RcMap    map[string]int64
	RcSystem *rcSystem.RcSystem

	// VirtualRoot    []byte
	// VirtualState   []byte

	// LedgerExecutor *LedgerExecutor
	LedgerState  *ledgerSystem.LedgerState
	LedgerSystem ledgerSystem.LedgerSystem

	//Atomic packet; aka -> array of transactions with ops in each
	TxBatch  []TxPacket
	TxOutIds []string

	//Map of txId --> output
	TxOutput map[string]TxOutput

	//Temporary output state as things are being processed.
	TempOutputs     map[string]*contract_session.TempOutput
	ContractResults map[string][]ContractResult

	//First Tx of batch
	firstTxHeight uint64

	slotStatus *SlotStatus

	BlockHeight int

	// pendulumFeed tracks sole-HIVE witness feed + HBD APR (Magi pendulum oracle); updated from Hive blocks.
	pendulumFeed *pendulumoracle.FeedTracker
	// pendulumSettlementsDb stores the per-epoch settlement marker written
	// when applyPendulumSettlement processes a BlockTypePendulumSettlement
	// record. Drives the proposer's `canHold` guard and the
	// `vsc.election_result` handler's "prior epoch must be settled" check.
	pendulumSettlementsDb pendulum_settlements.PendulumSettlements
	// pendulumApplier is the swap-time SDK method's executor.
	pendulumApplier wasm_context.PendulumApplier
	// pendulumGeometry recomputes (V, P, E, T, s) on demand from contract
	// state and committee bond. Read by the applier per swap and by
	// settlement composition; no snapshot persistence layer.
	pendulumGeometry *pendulumoracle.GeometryComputer
	// balanceDb is held so settlement composition can read HIVE_CONSENSUS
	// bonds via BalanceRecord directly, bypassing the op-type-filter trap
	// on LedgerSystem.GetBalance.
	balanceDb ledgerDb.Balances
	// selfHiveUsername is the local node's Hive account name. Currently
	// unused outside of tests now that settlement is leader-agnostic.
	selfHiveUsername string

	// lastProcessedHeight is the height of the most recent Hive block whose
	// ProcessBlock call ran to completion (set via defer so a panic in
	// processing still records the height). It exists so the block producer
	// can synchronise compose-time reads — concretely, MakePendulumSettlement
	// and the signer-side HandleBlockMsg re-derive — with the state engine's
	// slot-boundary ExecuteBatch flush. Without it, an async producer tick
	// or a p2p signing request that arrives before this node's state engine
	// has flushed slot S-1 reads stale ledger/balance state and produces a
	// divergent CID. See WaitForProcessedHeight for the consumer API.
	lastProcessedHeight atomic.Uint64

	// slashRestitution pays harmed parties from safety slashes (FIFO);
	// remainder is burned. Production wiring uses
	// safetyslash.OnLedgerRestitutionAllocator (consensus-safe, reads
	// claim rows from ProtocolSlashRestitutionClaimsAccount written by
	// the vsc.restitution_claim chain op handler). The legacy
	// MemoryRestitutionQueue is kept as a SlashRestitutionAllocator
	// implementation only for tests that pre-date the on-ledger queue
	// — see enqueueSlashRestitutionClaimForTest.
	slashRestitution ledgerSystem.SlashRestitutionAllocator
	// safetyEvidenceSeen deduplicates evidence events per account+kind+evidenceID.
	// Backstop only — the underlying ledger ids in SafetySlashConsensusBond are
	// deterministic and Mongo upserts on (id), so replay never double-debits even
	// when this in-memory map is empty (post-restart).
	safetyEvidenceSeen map[string]uint64
	// seenProposalBySlotProposer tracks first observed block content per (slot, proposer)
	// to detect conflicting proposals in the same slot deterministically.
	//
	// First-seen-wins: whichever block ref the replaying node observes first
	// for a given (slot, account) becomes the reference; any later op from the
	// same account with a different block CID at the same slot triggers
	// vsc_double_block_sign. The producer signed both refs, so the choice of
	// reference is irrelevant to correctness — the map only needs to record
	// "this account already proposed something at this slot".
	seenProposalBySlotProposer map[string]string
	// slashIncidentBpsBySlotAccount accumulates the bps already slashed in the
	// current (slot, account) incident so a producer who hits two block-
	// production kinds at the same slot is correlated under
	// CorrelatedSlashCapBps instead of being slashed independently per kind.
	slashIncidentBpsBySlotAccount map[string]int
	// doubleSignRehydrated guards the one-shot rehydrateDoubleSignMap so it runs
	// only on the first block processed after (re)start.
	doubleSignRehydrated bool
}

//Transaction
// InputArgs string -->
// // - Entrypoint
// // - Args
// // - Contract ID
// // - Account auths
// Intents []interface -->

// //Pass yes or no
// Output interface <--
// // - error
// // - logs
// // - ok: bool

// //State changes
// StateMerkle string <--
// BalanceMapUpdates []interface <-- //Ledger ops
// SideEffects []interface <-- //Placeholder for future

// Finalizes state into pseudo block

func (se *StateEngine) claimHBDInterest(blockHeight uint64, amount int64, txId string) {
	lastClaim := se.claimDb.GetLastClaim(blockHeight - 1)

	claimHeight := uint64(0)
	if lastClaim != nil {
		claimHeight = lastClaim.BlockHeight
	}

	se.LedgerSystem.ClaimHBDInterest(claimHeight, blockHeight, amount, txId)
}

// Gets ranomized schedule of witnesses
// Uses a different PRNG variant from the original used in JS VSC
// Not aiming for exact replica
func (se *StateEngine) GetSchedule(slotHeight uint64) []WitnessSlot {
	lastElection, err := se.electionDb.GetElectionByHeight(slotHeight)

	if err != nil {
		return nil
	}

	witnessList := make([]Witness, 0)

	for _, v := range lastElection.Members {
		witnessList = append(witnessList, Witness{
			Key:     v.Key,
			Account: v.Account,
		})
	}

	//Use the slot height to calculate round start and finish.
	//Slot height is consistent.
	roundInfo := vscBlocks.CalculateRoundInfo(slotHeight)

	randBlock := roundInfo.StartHeight - 1

	hiveBlock, err := se.hiveBlocks.GetBlock(randBlock)

	hash := crypto.SHA256.New()
	if err != nil {
		//Defualt seed
		//This will only happen if the network is very new within the first 1 hour of creation
		hash.Write([]byte("VSC.NETWORK"))
	} else {
		hash.Write([]byte(hiveBlock.BlockID))
	}
	hash.Write([]byte(strconv.Itoa(int(roundInfo.StartHeight))))

	seed := hash.Sum(nil)

	var seed32 [32]byte
	// var seed2 [32]byte
	copy(seed32[:], seed)
	witnessSchedule := GenerateSchedule(slotHeight, witnessList, seed32)

	return witnessSchedule
}

// Implementation note:
// You might be wondering why there is a side batch of TXs which awaits new block processing
// This is done to prevent needing to scan the transaction pool upon node restart
// Instead, this scans the blocks that have been indexed to get the correct state
// However, if we did the original approach of constant execution of hive transactions...
// ...even if no VSC block, that would result in needing scan blocks before the last VSC block to get the correct state
// Even in that scenario, we would still need to scan backwards to get the "wet" state of the stateEngine
// In other words, it adds complexity to the state engine while being less efficient.
// This model is more efficient and best yet, it prevents MEV potential by locking the block execution time to the witness slot.
func (se *StateEngine) ProcessBlock(block hive_blocks.HiveBlock) {
	// Record completion via defer so compose-time waiters
	// (MakePendulumSettlement, HandleBlockMsg) unblock once this block's
	// processing — including any slot-boundary ExecuteBatch flush — is
	// fully done. Survives panics mid-processing.
	defer func() {
		for {
			prev := se.lastProcessedHeight.Load()
			if block.BlockNumber <= prev {
				return
			}
			if se.lastProcessedHeight.CompareAndSwap(prev, block.BlockNumber) {
				return
			}
		}
	}()

	se.BlockHeight = int(block.BlockNumber)
	se.refreshChainConsensusCache()

	// --- Key lifecycle: deprecation and retirement ---
	if electionData, elecErr := se.electionDb.GetElectionByHeight(block.BlockNumber); elecErr == nil {
		currentEpoch := electionData.Epoch

		// Phase 1: deprecate active keys that have reached their expiry epoch.
		if deprecating, err := se.tssKeys.FindDeprecatingKeys(currentEpoch); err == nil {
			for _, k := range deprecating {
				k.Status = tss_db.TssKeyDeprecated
				if tss_db.KeyRetirementEnabled {
					k.DeprecatedHeight = int64(block.BlockNumber)
				}
				se.tssKeys.SetKey(k)
				tssLog.Info(
					"key deprecated",
					"keyId",
					k.Id,
					"expiryEpoch",
					k.ExpiryEpoch,
					"blockHeight",
					block.BlockNumber,
				)
			}
		}
	}

	// Phase 2: retire deprecated keys whose grace period has elapsed (block-height based).
	if tss_db.KeyRetirementEnabled {
		if retiring, err := se.tssKeys.FindNewlyRetired(block.BlockNumber); err == nil {
			for _, k := range retiring {
				k.Status = tss_db.TssKeyRetired
				se.tssKeys.SetKey(k)
				tssLog.Info(
					"key retired",
					"keyId",
					k.Id,
					"deprecatedHeight",
					k.DeprecatedHeight,
					"blockHeight",
					block.BlockNumber,
				)
			}
		}
	}

	blockInfo := struct {
		BlockHeight uint64
		BlockId     string
		Timestamp   string
	}{
		BlockHeight: block.BlockNumber,
		BlockId:     block.BlockID,
		Timestamp:   block.Timestamp,
	}

	if se.pendulumFeed != nil && block.Witness != "" {
		se.pendulumFeed.RecordWitnessBlock(block.Witness)
	}

	//What is active slot?
	// bh = 5
	// 0 - 10
	// prev blk = 1,
	slotInfo := CalculateSlotInfo(block.BlockNumber)
	if se.slotStatus == nil {
		se.slotStatus = &SlotStatus{
			SlotHeight: slotInfo.StartHeight,
			Done:       false,
		}

	}

	// On the first block after (re)start, rebuild the double-sign detector's
	// in-memory map for the slot already in progress. The resume height is this
	// block's height; blocks below it were processed pre-restart and are never
	// replayed, so a first proposal they carried would otherwise be lost and a
	// later competing proposal in the same slot would escape detection.
	if !se.doubleSignRehydrated {
		se.doubleSignRehydrated = true
		se.rehydrateDoubleSignMap(block.BlockNumber)
	}

	for _, virtualOp := range block.VirtualOps {
		if virtualOp.Op.Type == "interest_operation" {
			owner, ok := virtualOp.Op.Value["owner"].(string)
			if !ok {
				continue
			}
			if owner == se.sconf.GatewayWallet() {
				interest, ok := virtualOp.Op.Value["interest"].(map[string]any)
				if !ok {
					fmt.Println("interest_operation: unexpected interest field type", "block", block.BlockNumber)
					continue
				}
				amountStr, ok := interest["amount"].(string)
				if !ok {
					fmt.Println("interest_operation: unexpected amount field type", "block", block.BlockNumber)
					continue
				}
				vInt1, err := strconv.ParseInt(amountStr, 10, 64)
				if err != nil {
					fmt.Println(
						"interest_operation: failed to parse amount",
						"block",
						block.BlockNumber,
						"amount",
						amountStr,
						"err",
						err,
					)
					continue
				}
				se.claimHBDInterest(blockInfo.BlockHeight, vInt1, virtualOp.TrxId)
			}
		}
	}

	for blkIdx, tx := range block.Transactions {
		if se.pendulumFeed != nil {
			se.pendulumFeed.IngestTransactionOps(block.BlockNumber, tx)
		}

		if tx.Operations[0].Type == "custom_json" {
			headerOp := tx.Operations[0]
			opVal := headerOp.Value
			// review2 MED #86/#88 (sweep): id/json come from an
			// untrusted L1 custom_json payload. A bare type assertion
			// panicked block processing on a malformed op. Comma-ok
			// and skip just this system-header handling — the rest of
			// the tx (transfers, user ops) still processes. Hive
			// validates these as strings server-side so this is
			// defense-in-depth, but the assertion no longer crashes
			// the node if the L1 ever diverges.
			Id, idOk := opVal["id"].(string)
			headerJson, jsonOk := opVal["json"].(string)
			RequiredAuths := common.ArrayToStringArray(opVal["required_auths"])

			// Only process system header operations (vsc.fr_sync, vsc.actions)
			// when active auth is present. User operations with posting-only
			// auth are handled in the user operations section below.
			if idOk && jsonOk && len(RequiredAuths) > 0 {
				cj := CustomJson{
					Id:                   Id,
					RequiredAuths:        RequiredAuths,
					RequiredPostingAuths: common.ArrayToStringArray(opVal["required_posting_auths"]),
					Json:                 []byte(headerJson),
				}

				if Id == "vsc.fr_sync" && RequiredAuths[0] == se.sconf.GatewayWallet() {
					log.Debug("vsc.fr_sync", "opVal", opVal)

					frSync := struct {
						StakedAmount   int64 `json:"stake_amt"`
						UnstakedAmount int64 `json:"unstake_amt"`
					}{}
					json.Unmarshal(cj.Json, &frSync)

					fmt.Println("frSync", frSync)

					var amt int64

					if frSync.StakedAmount > 0 {
						amt = frSync.StakedAmount
					} else {
						//Must be negative to indicate unstaking has occurred
						amt = -frSync.UnstakedAmount
					}

					if err := se.LedgerState.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
						Id:          MakeTxId(tx.TransactionID, 0),
						Amount:      amt,
						BlockHeight: blockInfo.BlockHeight + 1,
						Owner:       params.FR_VIRTUAL_ACCOUNT,
						//Fractional reserve update
						Asset: "hbd_savings",
						Type:  "fr_sync",
					}); err != nil {
						log.Error("fr_sync: ledger write failed", "txId", tx.TransactionID, "amount", amt, "err", err)
					}
				}

				if Id == "vsc.actions" && RequiredAuths[0] == se.sconf.GatewayWallet() && !se.chainProcessingSuspended() {
					actionUpdate := map[string]interface{}{}
					err := json.Unmarshal(cj.Json, &actionUpdate)

					if err == nil {
						se.LedgerSystem.IndexActions(actionUpdate, ledgerSystem.ExtraInfo{
							BlockHeight: block.BlockNumber,
							ActionId:    tx.TransactionID,
						})
					}
				}
			}
		}

		//Start by looking for block production
		singleOp := tx.Operations[0]

		//Main pipeline
		if singleOp.Type == "account_update" {
			opValue := singleOp.Value

			// review2 MED #86/#88 (sweep): json_metadata / account from
			// an untrusted L1 account_update payload — comma-ok and
			// skip the witness-update path instead of a bare assertion
			// that panicked block processing.
			jsonMeta, jsonMetaOk := opValue["json_metadata"].(string)
			acct, acctOk := singleOp.Value["account"].(string)
			if jsonMetaOk && acctOk {
				untypedJson := make(map[string]interface{})

				bbytes := []byte(jsonMeta)
				json.Unmarshal(bbytes, &untypedJson)

				rawJson := witnesses.PostingJsonMetadata{}
				json.Unmarshal(bbytes, &rawJson)

				if slices.Contains(rawJson.Services, "vsc.network") {
					// Fix 5 (verify+warn rollout): check the consensus BLS key's
					// proof-of-possession. A valid PoP proves the announcer holds
					// the secret behind the announced BLS pubkey, defeating
					// rogue-key aggregate-signature forgery. For now we only log
					// failures and still store the key, so witnesses that
					// announced before PoP support are not dropped before they
					// re-announce. A later change flips this to rejection (and
					// election exclusion) once all witnesses carry a valid PoP.
					// The check is deterministic, so that future strict mode is
					// consensus-safe.
					if err := verifyAnnouncedBlsPoP(rawJson, acct); err != nil {
						log.Warn("witness announce: BLS proof-of-possession check failed (accepting during rollout)",
							"account", acct, "txId", tx.TransactionID, "err", err)
					}
					inputData := witnesses.SetWitnessUpdateType{
						Account:  acct,
						Height:   blockInfo.BlockHeight,
						TxId:     tx.TransactionID,
						BlockId:  blockInfo.BlockId,
						Metadata: rawJson,
					}
					se.witnessDb.SetWitnessUpdate(inputData)
				}
			}
			continue
		}

		if singleOp.Type == "custom_json" {
			opVal := singleOp.Value
			// review2 MED #86/#88 (sweep): comma-ok the untrusted L1
			// id/json instead of a bare assertion that crashed block
			// processing on a malformed system op.
			cjId, cjIdOk := opVal["id"].(string)
			cjJson, cjJsonOk := opVal["json"].(string)
			cj := CustomJson{
				Id:                   cjId,
				RequiredAuths:        common.ArrayToStringArray(opVal["required_auths"]),
				RequiredPostingAuths: common.ArrayToStringArray(opVal["required_posting_auths"]),
				Json:                 []byte(cjJson),
			}

			// Only process system transactions when active auth is present.
			// User operations with posting-only auth are handled below.
			if cjIdOk && cjJsonOk && len(cj.RequiredAuths) > 0 {
				txSelf := TxSelf{
					BlockHeight:          blockInfo.BlockHeight,
					BlockId:              blockInfo.BlockId,
					Timestamp:            blockInfo.Timestamp,
					Index:                blkIdx,
					OpIndex:              0,
					TxId:                 tx.TransactionID,
					RequiredAuths:        cj.RequiredAuths,
					RequiredPostingAuths: cj.RequiredPostingAuths,
				}
				if se.chainProcessingSuspended() && !isRecoveryAllowlistedCustomJSON(cj.Id) {
					continue
				}
				//Start parsing block
				if cj.Id == "vsc.produce_block" {
					//Process block production
					rawJson := map[string]interface{}{}
					json.Unmarshal(cj.Json, &rawJson)
					// parsedTx := TxProposeBlock{}
					// json.Unmarshal(cj.Json, &parsedTx)

					// parsedTx.ExecuteTx(se)

					schedule := se.GetSchedule(slotInfo.StartHeight)

					var scheduleSlot WitnessSlot

					for _, slot := range schedule {
						if slot.SlotHeight == slotInfo.StartHeight {
							scheduleSlot = slot
							break
						}
					}

					if cj.RequiredAuths[0] == scheduleSlot.Account {
						rawJson := map[string]interface{}{}
						json.Unmarshal(cj.Json, &rawJson)

						parsedBlock := TxProposeBlock{
							Self: txSelf,
							SignedBlock: SignedBlockHeader{
								UnsignedBlockHeader: UnsignedBlockHeader{},
								Signature:           dids.SerializedCircuit{},
							},
						}
						json.Unmarshal(cj.Json, &parsedBlock)
						// First-seen-wins: whichever block ref this replaying node
						// observes first for (slot, account) becomes the reference;
						// any later op from the same account with a different block
						// CID at the same slot proves equivocation. Either way the
						// account signed two distinct refs — choice of reference is
						// irrelevant to correctness. recordFirstSeenProposal seeds on
						// first sight and returns the prior ref so we can detect the
						// conflict. The identical key + first-seen seeding is replayed
						// at startup by rehydrateDoubleSignMap, so a warm restart that
						// lands mid-slot cannot lose the first ref (see its doc).
						slotKey := slotProposerKey(slotInfo.StartHeight, cj.RequiredAuths[0])
						prevBlockRef, exists := se.recordFirstSeenProposal(
							slotInfo.StartHeight,
							cj.RequiredAuths[0],
							parsedBlock.SignedBlock.Block,
						)
						doubleSign := false
						if exists && prevBlockRef != parsedBlock.SignedBlock.Block {
							doubleSign = true
							slashRes := se.slashForEvidenceIfPolicyAllows(
								cj.RequiredAuths[0],
								safetyslash.EvidenceVSCDoubleBlockSign,
								"double-block|"+slotKey+"|"+prevBlockRef+"|"+parsedBlock.SignedBlock.Block,
								tx.TransactionID,
								block.BlockNumber,
								slotKey,
							)
							if slashRes.Ok {
								log.Warn(
									"principal slash applied for double block proposal",
									"account",
									cj.RequiredAuths[0],
									"slot_height",
									slotInfo.StartHeight,
									"tx_id",
									tx.TransactionID,
								)
							}
						}

						outcome := parsedBlock.ValidateDetailed(se)

						switch outcome.Kind {
						case BlockSkip:
							// Transient validation failure (e.g. missing election).
							// Do NOT slash — the producer signed a block we cannot
							// even attempt to verify. A replay with a healthy view
							// will reach a deterministic decision.
							log.Warn("block validation skipped (no slash)",
								"account", cj.RequiredAuths[0], "tx_id", tx.TransactionID,
								"slot_height", slotInfo.StartHeight, "reason", outcome.Reason)
						case BlockStale:
							// Deterministically late: the op confirmed past the
							// producer's slot window. Every node computes this
							// identically from Br[1]/SlotLength/BlockHeight, but a
							// correct-but-slow producer can hit it under network
							// conditions outside its control, so it is a liveness
							// fault, not a safety fault. Reject (not applied)
							// without slashing.
							log.Warn("stale block proposal rejected (no slash)",
								"account", cj.RequiredAuths[0], "tx_id", tx.TransactionID,
								"slot_height", slotInfo.StartHeight, "reason", outcome.Reason)
						case BlockValid:
							se.slotStatus.Done = true
							se.slotStatus.Producer = cj.RequiredAuths[0]
							parsedBlock.ExecuteTx(se)
						case BlockInvalid:
							// Proven-invalid (deterministic). Correlate with any
							// double-sign that already fired in this same incident
							// so we don't stack two independent 10% slashes against
							// CorrelatedSlashCapBps without acknowledging it.
							_ = doubleSign
							slashRes := se.slashForEvidenceIfPolicyAllows(
								cj.RequiredAuths[0],
								safetyslash.EvidenceVSCInvalidBlockProposal,
								"invalid-block|"+tx.TransactionID,
								tx.TransactionID,
								block.BlockNumber,
								slotKey,
							)
							if slashRes.Ok {
								log.Warn(
									"principal slash applied for invalid block proposal",
									"account",
									cj.RequiredAuths[0],
									"tx_id",
									tx.TransactionID,
									"slot_height",
									slotInfo.StartHeight,
								)
							}
						}
					}
					continue
				}
				//# End parsing block

				//# Start parsing system transactions
				if cj.Id == "vsc.create_contract" {
					for idx, auth := range txSelf.RequiredAuths {
						txSelf.RequiredAuths[idx] = "hive:" + auth
					}

					for idx, auth := range txSelf.RequiredPostingAuths {
						txSelf.RequiredPostingAuths[idx] = "hive:" + auth
					}

					parsedTx := TxCreateContract{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					if !se.sconf.OnMainnet() || txSelf.BlockHeight >= params.CONTRACT_DEPLOYMENT_FEE_START_HEIGHT {
						hasFee, feeAmt, feePayer := hasFeePaymentOp(
							tx.Operations,
							params.CONTRACT_DEPLOYMENT_FEE,
							"hbd",
						)
						if !hasFee {
							continue
						}

						txResult := parsedTx.ExecuteTx(se)

						if txResult.Success {
							se.LedgerSystem.Deposit(ledgerSystem.Deposit{
								Id:          MakeTxId(tx.TransactionID, 1),
								Asset:       "hbd",
								Amount:      feeAmt,
								Account:     se.sconf.GatewayWallet(),
								From:        "hive:" + feePayer,
								Memo:        "to=vsc.dao",
								BlockHeight: blockInfo.BlockHeight,
								BIdx:        int64(tx.Index),
								OpIdx:       int64(1),
							})
						} else {
							se.LedgerSystem.Deposit(ledgerSystem.Deposit{
								Id:          MakeTxId(tx.TransactionID, 1),
								Asset:       "hbd",
								Amount:      feeAmt,
								Account:     "hive:" + feePayer,
								From:        "hive:" + feePayer,
								Memo:        "to=" + feePayer,
								BlockHeight: blockInfo.BlockHeight,
								BIdx:        int64(tx.Index),
								OpIdx:       int64(1),
							})
						}

					} else {
						parsedTx.ExecuteTx(se)
					}
					continue
				} else if cj.Id == "vsc.update_contract" {
					if !se.sconf.OnMainnet() || txSelf.BlockHeight >= params.CONTRACT_UPDATE_HEIGHT {
						for idx, auth := range txSelf.RequiredAuths {
							txSelf.RequiredAuths[idx] = "hive:" + auth
						}

						for idx, auth := range txSelf.RequiredPostingAuths {
							txSelf.RequiredPostingAuths[idx] = "hive:" + auth
						}

						parsedTx := TxUpdateContract{
							Self: txSelf,
						}
						json.Unmarshal(cj.Json, &parsedTx)

						hasFee, feeAmt, feePayer := hasFeePaymentOp(tx.Operations, params.CONTRACT_DEPLOYMENT_FEE, "hbd")
						txResult := parsedTx.ExecuteTx(se, hasFee)

						if hasFee {
							if txResult.Success && txResult.CodeUpdated {
								se.LedgerSystem.Deposit(ledgerSystem.Deposit{
									Id:          MakeTxId(tx.TransactionID, 1),
									Asset:       "hbd",
									Amount:      feeAmt,
									Account:     se.sconf.GatewayWallet(),
									From:        "hive:" + feePayer,
									Memo:        "to=vsc.dao",
									BlockHeight: blockInfo.BlockHeight,
									BIdx:        int64(tx.Index),
									OpIdx:       int64(1),
								})
							} else {
								se.LedgerSystem.Deposit(ledgerSystem.Deposit{
									Id:          MakeTxId(tx.TransactionID, 1),
									Asset:       "hbd",
									Amount:      feeAmt,
									Account:     "hive:" + feePayer,
									From:        "hive:" + feePayer,
									Memo:        "to=" + feePayer,
									BlockHeight: blockInfo.BlockHeight,
									BIdx:        int64(tx.Index),
									OpIdx:       int64(1),
								})
							}
						}
					}
					continue
				} else if cj.Id == "vsc.cancel_contract_update" {
					// Cancel a contract update still inside its timelock window.
					// Gated like the timelock itself: a no-op before rollout (no
					// pending updates exist), and historical replay sees no such
					// op, so reindex stays byte-identical. No fee.
					if !se.sconf.OnMainnet() || (params.CONTRACT_UPDATE_TIMELOCK_HEIGHT != 0 && txSelf.BlockHeight >= params.CONTRACT_UPDATE_TIMELOCK_HEIGHT) {
						for idx, auth := range txSelf.RequiredAuths {
							txSelf.RequiredAuths[idx] = "hive:" + auth
						}

						for idx, auth := range txSelf.RequiredPostingAuths {
							txSelf.RequiredPostingAuths[idx] = "hive:" + auth
						}

						parsedTx := TxCancelContractUpdate{
							Self: txSelf,
						}
						json.Unmarshal(cj.Json, &parsedTx)
						parsedTx.ExecuteTx(se)
					}
					continue
				} else if cj.Id == "vsc.election_result" {
					parsedTx := &TxElectionResult{
						Self: txSelf,
					}
					// Pass parsedTx (a *TxElectionResult), not &parsedTx
					// (**TxElectionResult). With the double pointer, a JSON
					// payload of `null` would set the inner pointer to nil
					// and the subsequent ExecuteTx call would panic with a
					// nil receiver.
					json.Unmarshal(cj.Json, parsedTx)
					parsedTx.ExecuteTx(se)
					continue
				} else if cj.Id == "vsc.propose_consensus_version" {
					parsedTx := &TxProposeConsensusVersion{Self: txSelf}
					json.Unmarshal(cj.Json, parsedTx)
					parsedTx.ExecuteTx(se)
					continue
				} else if cj.Id == "vsc.recovery_suspend" {
					parsedTx := &TxRecoverySuspend{Self: txSelf}
					json.Unmarshal(cj.Json, parsedTx)
					parsedTx.ExecuteTx(se)
					continue
				} else if cj.Id == "vsc.recovery_require_version" {
					parsedTx := &TxRecoveryRequireVersion{Self: txSelf}
					json.Unmarshal(cj.Json, parsedTx)
					parsedTx.ExecuteTx(se)
					continue
				}
			} //# End parsing system transactions
		}

		opList := make([]VSCTransaction, 0)

		for opIndex, op := range tx.Operations {

			//# Start parsing gateway transfer operations
			if op.Type == "transfer" {

				if op.Value["from"] == se.sconf.GatewayWallet() {
					continue
				}

				//TODO: Finish up support for directly handling staked transfers

				// review2 MEDIUM #86: these came from untrusted L1 op
				// payloads via bare type assertions; a malformed transfer
				// (amount not an object, missing fields) panicked and
				// crashed block processing. Comma-ok and skip the op.
				amountMap, ok := op.Value["amount"].(map[string]interface{})
				if !ok {
					continue
				}

				var token string
				if amountMap["nai"] == "@@000000021" {
					token = "hive"

				} else if amountMap["nai"] == "@@000000013" {
					token = "hbd"
				}

				// review2 LOW #104: an unrecognised NAI left token == "",
				// and the op was still credited as an empty-asset deposit.
				// Skip transfers whose asset we don't track.
				//
				// Defense-in-depth (milo review follow-up): Hive L1
				// transfers today only carry HIVE/HBD, so token == ""
				// should be unreachable for a real op — but if the L1
				// ever diverges, a transfer of an untracked asset TO the
				// gateway would be silently dropped, leaving funds in the
				// multisig with no L2 credit and no refund trail. Do not
				// credit it (no empty-asset record), but make it loud and
				// auditable so operators can refund manually. State
				// transition is unchanged (still skipped) — log only, so
				// this stays deterministic.
				if token == "" {
					if toStr, _ := op.Value["to"].(string); toStr == "vsc.gateway" {
						fromStr, _ := op.Value["from"].(string)
						log.Warn(
							"review2 #104: untracked-asset transfer to gateway stranded — NOT credited, manual refund required",
							"tx",
							tx.TransactionID,
							"op",
							opIndex,
							"from",
							fromStr,
							"nai",
							amountMap["nai"],
							"amount",
							amountMap["amount"],
							"blockHeight",
							blockInfo.BlockHeight,
						)
					}
					continue
				}

				if op.Type == "transfer_to_savings" && token == "hbd" {
					if token == "hbd" {
						//Labeled as savings as there can be hbd savings, hive savings, and hive staked, but not hbd staked (within hive)
						//Only HBD savings generates APR
						token = "hbd_savings"
					} else {
						//Potentially add failover logic
						//However, balance is considered "untracked" if it is Hive token deposited directly to savings

						continue
					}
				}
				amtStr, ok := amountMap["amount"].(string)
				if !ok {
					continue // review2 #86
				}
				amt, err := strconv.ParseInt(amtStr, 10, 64)
				if err != nil {
					log.Warn(
						"skipping transfer with unparseable amount",
						"tx",
						tx.TransactionID,
						"op",
						opIndex,
						"amount",
						amtStr,
						"err",
						err,
					)
					continue
				}

				if op.Value["to"] == "vsc.gateway" {
					fromStr, ok := op.Value["from"].(string)
					if !ok {
						continue // review2 #86
					}
					// memo is optional on L1; default to "" rather than panic.
					depositMemo, _ := op.Value["memo"].(string)
					depositedFrom := "hive:" + fromStr
					leDeposit := ledgerSystem.Deposit{
						Id:     MakeTxId(tx.TransactionID, opIndex),
						Asset:  token,
						Amount: amt,
						From:   depositedFrom,
						Memo:   depositMemo,

						BlockHeight: blockInfo.BlockHeight,

						BIdx:  int64(tx.Index),
						OpIdx: int64(opIndex),
					}
					depositedTo := se.LedgerSystem.Deposit(leDeposit)

					// create deposit payload for indexing
					depositPayload := make(map[string]interface{})
					depositPayload["from"] = depositedFrom
					depositPayload["to"] = depositedTo
					depositPayload["amount"] = amt
					depositPayload["asset"] = token
					depositPayload["memo"] = depositMemo
					depositPayload["type"] = "deposit"

					txSelf := TxSelf{
						TxId:                 tx.TransactionID,
						BlockHeight:          blockInfo.BlockHeight,
						BlockId:              blockInfo.BlockId,
						Timestamp:            blockInfo.Timestamp,
						Index:                tx.Index,
						OpIndex:              opIndex,
						RequiredAuths:        []string{},
						RequiredPostingAuths: []string{},
					}
					opList = append(opList, TxDeposit{
						Self:   txSelf,
						From:   depositedFrom,
						To:     depositedTo,
						Amount: amt,
						Asset:  token,
						Memo:   depositMemo,
					})
				}
			}
			//# End parsing gateway transfer operations

			//# Start parsing onchain user operations
			if op.Type == "custom_json" {
				opVal := op.Value
				// review2 MED #86/#88 (sweep): untrusted L1 user op —
				// comma-ok id/json and skip just this op instead of a
				// bare assertion that crashed the whole block.
				uId, uIdOk := opVal["id"].(string)
				uJson, uJsonOk := opVal["json"].(string)
				if !uIdOk || !uJsonOk {
					log.Warn("skipping malformed user custom_json op", "tx", tx.TransactionID, "op", opIndex)
					continue
				}
				cj := CustomJson{
					Id:                   uId,
					RequiredAuths:        common.ArrayToStringArray(opVal["required_auths"]),
					RequiredPostingAuths: common.ArrayToStringArray(opVal["required_posting_auths"]),
					Json:                 []byte(uJson),
				}

				for idx, auth := range cj.RequiredAuths {
					cj.RequiredAuths[idx] = "hive:" + auth
				}

				for idx, auth := range cj.RequiredPostingAuths {
					cj.RequiredPostingAuths[idx] = "hive:" + auth
				}

				txSelf := TxSelf{
					TxId:                 tx.TransactionID,
					BlockHeight:          blockInfo.BlockHeight,
					BlockId:              blockInfo.BlockId,
					Timestamp:            blockInfo.Timestamp,
					Index:                tx.Index,
					OpIndex:              opIndex,
					RequiredAuths:        cj.RequiredAuths,
					RequiredPostingAuths: cj.RequiredPostingAuths,
				}

				var vscTx VSCTransaction
				if cj.Id == "vsc.withdraw" {
					parsedTx := TxVSCWithdraw{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					// log.Debug("parsedTx vsc.withdraw", parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.call" {
					parsedTx := TxVscCallContract{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = parsedTx
				} else if cj.Id == "vsc.transfer" {
					parsedTx := TxVSCTransfer{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.stake_hbd" {
					//Fill this in
					parsedTx := TxStakeHbd{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.unstake_hbd" {
					// log.Debug("vsc.unstake_hbd", cj.Json)
					parsedTx := TxUnstakeHbd{
						Self: txSelf,
					}
					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.consensus_stake" {
					parsedTx := TxConsensusStake{
						Self: txSelf,
					}

					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.consensus_unstake" {
					parsedTx := TxConsensusUnstake{
						Self: txSelf,
					}

					json.Unmarshal(cj.Json, &parsedTx)

					vscTx = &parsedTx
				} else if cj.Id == "vsc.tss_sign" {
					if (se.sconf.OnTestnet() || se.sconf.OnDevnet()) && !se.sconf.ConsensusParams().TssIndexed(txSelf.BlockHeight) {
						continue
					}

					signedData := struct {
						Packet []struct {
							KeyId string `json:"key_id"`
							Msg   string `json:"msg"`
							Sig   string `json:"sig"`
						} `json:"packet"`
					}{}

					err := json.Unmarshal(cj.Json, &signedData)

					keyCache := make(map[string]*tss_db.TssKey)
					if err == nil {
						for _, sigPack := range signedData.Packet {
							if keyCache[sigPack.KeyId] == nil {
								tssKey, err := se.tssKeys.FindKey(sigPack.KeyId)
								if err != nil {
									log.Warn("failed to find key", "keyId", sigPack.KeyId, "err", err)
									continue
								}
								if tssKey.Status != tss_db.TssKeyActive {
									log.Warn(
										"signing attempted for non-active key, skipping",
										"keyId",
										tssKey.Id,
										"status",
										tssKey.Status,
									)
									continue
								}
								keyCache[sigPack.KeyId] = &tssKey
							}
							if keyCache[sigPack.KeyId] != nil {
								publicKey, err := hex.DecodeString(keyCache[sigPack.KeyId].PublicKey)
								sigBytes, err1 := hex.DecodeString(sigPack.Sig)
								msgBytes, _ := hex.DecodeString(sigPack.Msg)
								if err == nil && err1 == nil {
									if keyCache[sigPack.KeyId].Algo == tss_db.EcdsaType {
										pubKey, err := btcec.ParsePubKey(publicKey)
										if err != nil {
											log.Warn("invalid TSS public key, skipping", "keyId", sigPack.KeyId, "err", err)
											continue
										}

										signature, err := btcecdsa.ParseDERSignature(sigBytes)
										if err != nil {
											log.Warn("invalid TSS DER signature, skipping", "keyId", sigPack.KeyId, "err", err)
											continue
										}

										sigS := signature.S()
										if sigS.IsOverHalfOrder() {
											log.Warn("TSS signature has high-S (BIP-62 non-canonical), rejecting", "keyId", sigPack.KeyId)
											continue
										}

										verified := signature.Verify(msgBytes, pubKey)

										if verified {
											se.tssRequests.UpdateRequest(tss_db.TssRequest{
												KeyId:  sigPack.KeyId,
												Msg:    sigPack.Msg,
												Sig:    sigPack.Sig,
												Status: tss_db.SignComplete,
											})
										}
									} else if keyCache[sigPack.KeyId].Algo == tss_db.EddsaType {
										pk := ed25519.PublicKey(publicKey)

										edVerify := ed25519.Verify(pk, msgBytes, sigBytes)

										if edVerify {
											se.tssRequests.UpdateRequest(tss_db.TssRequest{
												KeyId:  sigPack.KeyId,
												Msg:    sigPack.Msg,
												Sig:    sigPack.Sig,
												Status: tss_db.SignComplete,
											})
										}
									}
								}
							}
						}
					}
				} else if cj.Id == "vsc.tss_commitment" {

					var commitments []tss_helpers.SignedCommitment

					// Try new array format first, fall back to legacy map format
					if err := json.Unmarshal(cj.Json, &commitments); err != nil {
						commitmentMap := make(map[string]tss_helpers.SignedCommitment)
						if err := json.Unmarshal(cj.Json, &commitmentMap); err != nil {
							tssLog.Warn("vsc.tss_commitment parse error", "txId", tx.TransactionID, "err", err)
							continue
						}
						for _, c := range commitmentMap {
							commitments = append(commitments, c)
						}
					}

					tssLog.Verbose("processing vsc.tss_commitment", "txId", tx.TransactionID, "blockHeight", block.BlockNumber, "count", len(commitments))

					for _, commitment := range commitments {
						tssLog.Verbose("commitment entry", "sessionId", commitment.SessionId, "keyId", commitment.KeyId, "type", commitment.Type, "epoch", commitment.Epoch, "blockHeight", commitment.BlockHeight)

						members := make([]dids.BlsDID, 0)

						// Use the election active at the commitment's block height,
						// not the commitment's epoch. The leader collects BLS signatures
						// from GetElectionByHeight(bh) where bh = commitment.BlockHeight.
						// Using GetElection(commitment.Epoch) returns a different election
						// when the epoch has advanced, causing BLS verification to fail
						// because the member lists (and BLS keys) differ.
						if commitment.BlockHeight+params.TSS_COMMITMENT_MAX_STALENESS < block.BlockNumber {
							tssLog.Warn("stale commitment rejected", "keyId", commitment.KeyId, "commitmentHeight", commitment.BlockHeight, "blockNumber", block.BlockNumber)
							continue
						}

						electionData, elErr := se.electionDb.GetElectionByHeight(commitment.BlockHeight)

						if elErr != nil || electionData.Members == nil {
							tssLog.Warn("election lookup failed", "keyId", commitment.KeyId, "epoch", commitment.Epoch, "blockHeight", commitment.BlockHeight, "err", elErr)
							continue
						}
						for _, mbr := range electionData.Members {
							members = append(members, dids.BlsDID(mbr.Key))
						}

						baseComment := tss_helpers.BaseCommitment{
							Type:       commitment.Type,
							SessionId:  commitment.SessionId,
							KeyId:      commitment.KeyId,
							Commitment: commitment.Commitment,
							PublicKey:  commitment.PublicKey,
							// Metadata:    commitment.Metadata,
							BlockHeight: commitment.BlockHeight,
							Epoch:       commitment.Epoch,
						}

						data, _ := common.EncodeDagCbor(baseComment)

						commitmentCid, err := common.HashBytes(data, multicodec.DagCbor)
						if err != nil {
							tssLog.Warn("CID hash error", "keyId", commitment.KeyId, "err", err)
							continue
						}

						circuit, derr := dids.DeserializeBlsCircuit(dids.SerializedCircuit{
							Signature: commitment.Signature,
							BitVector: commitment.BitSet,
						}, members, commitmentCid)
						if derr != nil || circuit == nil {
							tssLog.Warn("BLS deserialize failed", "keyId", commitment.KeyId, "sessionId", commitment.SessionId, "err", derr)
							continue
						}

						verified, includedDIDs, _ := circuit.Verify()
						tssIndexHeight := se.SystemConfig().ConsensusParams().TssIndexHeight

						if !verified {
							tssLog.Warn("BLS verification failed", "keyId", commitment.KeyId, "sessionId", commitment.SessionId, "type", commitment.Type, "epoch", commitment.Epoch, "cid", commitmentCid)
							continue
						}
						// review2 CRITICAL #6: a valid aggregate is not enough —
						// it must carry >= 2/3 of the election weight, the same
						// rule the leader enforces in waitForSigs. Without this,
						// a sub-quorum commitment (e.g. 3/6) was accepted and
						// could activate a TSS key.
						if !BlsQuorumMet(includedDIDs, electionData.Members, electionData.Weights) {
							tssLog.Warn("BLS sub-quorum commitment rejected", "keyId", commitment.KeyId, "sessionId", commitment.SessionId, "type", commitment.Type, "epoch", commitment.Epoch, "blockHeight", commitment.BlockHeight, "signers", len(includedDIDs), "members", len(electionData.Members))
							continue
						}
						if block.BlockNumber <= tssIndexHeight {
							tssLog.Verbose("skipped (before TssIndexHeight)", "keyId", commitment.KeyId, "blockHeight", block.BlockNumber, "tssIndexHeight", tssIndexHeight)
							continue
						}

						tssLog.Verbose("writing commitment to DB", "keyId", commitment.KeyId, "sessionId", commitment.SessionId, "type", commitment.Type, "epoch", commitment.Epoch, "txId", tx.TransactionID)
						se.tssCommitments.SetCommitmentData(tss_db.TssCommitment{
							Type:        commitment.Type,
							BlockHeight: commitment.BlockHeight,
							Epoch:       commitment.Epoch,
							Commitment:  commitment.Commitment,
							KeyId:       commitment.KeyId,
							PublicKey:   commitment.PublicKey,
							TxId:        tx.TransactionID,
							BitSet:      commitment.BitSet,
						})
						if commitment.Type == "blame" {
							// Blame events stay on-chain for liveness analysis (and future
							// reward-reduction wiring), but are NOT principal-slashed: a
							// missing/late share looks identical to malicious silence from
							// the chain's perspective, and the only true safety violations
							// (divergent shares to different peers) live entirely on the
							// p2p layer where we have no on-chain proof. See
							// modules/incentive-pendulum/safety_slash/policy.go.
							blamedAccounts := blamedAccountsFromBitSet(commitment.BitSet, electionData.Members)
							for _, acct := range blamedAccounts {
								tssLog.Verbose("tss blame recorded (liveness only, no slash)",
									"account", acct, "txId", tx.TransactionID, "sessionId", commitment.SessionId)
							}
						}

						var newKey bool
						savedKeyInfo, _ := se.tssKeys.FindKey(commitment.KeyId)
						if savedKeyInfo.Status == "created" {
							newKey = true
						}

						if commitment.Type == "keygen" || commitment.Type == "reshare" {
							keyInfo, _ := se.tssKeys.FindKey(commitment.KeyId)
							if newKey && commitment.PublicKey != nil {
								keyInfo.PublicKey = *commitment.PublicKey
								keyInfo.CreatedHeight = int64(block.BlockNumber)
								keyInfo.Status = "active"
								keyInfo.Epoch = commitment.Epoch
								if keyInfo.Epochs > 0 {
									keyInfo.ExpiryEpoch = commitment.Epoch + keyInfo.Epochs
								}
								tssLog.Info("key activated", "keyId", keyInfo.Id, "epoch", keyInfo.Epoch, "expiryEpoch", keyInfo.ExpiryEpoch, "blockHeight", block.BlockNumber, "pubKey", keyInfo.PublicKey)
								se.tssKeys.SetKey(keyInfo)
							} else if newKey {
								tssLog.Verbose("keygen/reshare acknowledged (no pubKey)", "keyId", commitment.KeyId, "epoch", commitment.Epoch)
							} else {
								// S8: never rewind an active key's Epoch.
								// keyInfo.Epoch drives the on-disk keystore-path
								// derivation (tss.go: makeKey("key", id, epoch)),
								// so accepting an older commitment's epoch here
								// orphans the live share past retirement.
								if commitment.Epoch <= keyInfo.Epoch {
									tssLog.Warn("rejecting stale keygen/reshare commitment (epoch would not advance)", "keyId", commitment.KeyId, "currentEpoch", keyInfo.Epoch, "commitmentEpoch", commitment.Epoch, "blockHeight", commitment.BlockHeight)
									continue
								}
								keyInfo.Epoch = commitment.Epoch
								tssLog.Info("key epoch updated", "keyId", keyInfo.Id, "epoch", keyInfo.Epoch)
								se.tssKeys.SetKey(keyInfo)
							}
						}
					}
				}
				// NOTE: vsc.tss_ready was removed — readiness is now handled
				// off-chain via BLS-signed gossip attestations in the TSS module.
				// NOTE: vsc.pendulum_settlement (L1 Hive op) was removed —
				// settlement now rides as a BlockTypePendulumSettlement L2 op
				// inside a VSC block, signed by the closing committee's 2/3 BLS
				// aggregate. See modules/incentive-pendulum/settlement/compose.go
				// and applyPendulumSettlement.

				if vscTx != nil {
					opList = append(opList, vscTx)
				}
			}
		}

		//Do not push empty tx packets
		if len(opList) > 0 {
			se.TxBatch = append(se.TxBatch, TxPacket{
				TxId: tx.TransactionID,
				Ops:  opList,
			})

			self := opList[0].TxSelf()

			opDataList := make([]transactions.TransactionOperation, 0)
			opTypesSet := make(map[string]bool)
			for _, vscTx := range opList {
				txData := vscTx.ToData()

				opDataList = append(opDataList, transactions.TransactionOperation{
					RequiredAuths: vscTx.TxSelf().RequiredAuths,
					Type:          vscTx.Type(),
					Idx:           int64(vscTx.TxSelf().OpIndex),
					Data:          txData,
				})
				opTypesSet[vscTx.Type()] = true
			}

			opTypes := make([]string, 0)

			for k := range opTypesSet {
				opTypes = append(opTypes, k)
			}

			var defaultStatus string
			if slices.Contains(opTypes, "deposit") && len(opTypes) == 1 {
				defaultStatus = "CONFIRMED"
			} else {
				defaultStatus = "INCLUDED"
			}

			blkIdx := int64(self.Index)
			se.txDb.Ingest(transactions.IngestTransactionUpdate{
				Id:                   self.TxId,
				RequiredAuths:        self.RequiredAuths,
				RequiredPostingAuths: self.RequiredPostingAuths,
				Status:               defaultStatus,
				Type:                 "hive",
				OpTypes:              opTypes,
				Ops:                  opDataList,
				AnchoredBlock:        &block.BlockID,
				AnchoredHeight:       &block.BlockNumber,
				AnchoredIndex:        &blkIdx,
				Ledger:               make([]ledgerSystem.OpLogEvent, 0),
			})
		}
	}

	//Detects new slot and executes batch if so
	if se.slotStatus.SlotHeight != slotInfo.StartHeight {
		//Updates balances index before next batch can execute
		vscBlock, err := se.vscBlocks.GetBlockByHeight(se.slotStatus.SlotHeight - 1)
		if err != nil && err != mongo.ErrNoDocuments {
			log.Error("GetBlockByHeight failed, falling back to full-range balance scan",
				"slotHeight", se.slotStatus.SlotHeight, "err", err)
		}

		startBlock := uint64(0)
		if vscBlock != nil {
			startBlock = uint64(vscBlock.EndBlock)
		}

		se.UpdateBalances(startBlock, se.slotStatus.SlotHeight)

		se.UpdateRcMap(se.slotStatus.SlotHeight)

		se.RcMap = make(map[string]int64)

		// The slot we just rolled past is finalized: its block
		// (or absence thereof) has been applied, double-sign and
		// correlated-cap state for that slot is no longer reachable
		// by any in-flight detector. Prune slot-keyed maps before
		// they accumulate one entry per (slot, proposer) for the
		// process lifetime.
		se.pruneSafetySlotMaps(se.slotStatus.SlotHeight)
		se.pruneSafetyEvidenceSeen(slotInfo.StartHeight)

		se.slotStatus = &SlotStatus{
			SlotHeight: slotInfo.StartHeight,
			Done:       false,
		}
		se.ExecuteBatch()
	}

	//Executes user action when the slot has been completed
	if se.slotStatus.Done {
		// vscBlock, _ := se.vscBlocks.GetBlockByHeight(se.slotStatus.SlotHeight - 1)

		// startBlock := uint64(0)
		// if vscBlock != nil {
		// 	startBlock = uint64(vscBlock.EndBlock)
		// }

		// se.UpdateBalances(startBlock, se.slotStatus.SlotHeight)

		se.ExecuteBatch()
		//Balances must be updated after the current slot has been fully executed
	}

	if se.pendulumFeed != nil {
		se.pendulumFeed.TickIfDue(block.BlockNumber)
	}
}

// verifyAnnouncedBlsPoP locates the consensus BLS DID key in a witness announce
// payload and verifies its proof-of-possession, bound to the announcing
// account. Returns an error if the consensus key is missing/malformed or the
// PoP is absent or invalid. Pure function of the announce payload, so every
// node reaches the same verdict.
func verifyAnnouncedBlsPoP(meta witnesses.PostingJsonMetadata, account string) error {
	for _, k := range meta.DidKeys {
		if k.CryptoType == "DID-BLS" && k.Type == "consensus" {
			return dids.VerifyBlsPoP(dids.BlsDID(k.Key), account, k.PoP)
		}
	}
	return fmt.Errorf("no consensus BLS key in announce")
}

// executeTxSafely runs a transaction handler with panic recovery so that a
// malformed or attacker-crafted op cannot halt block processing. A panic in
// the handler is converted to a failed TxResult; the failure is recorded
// through the normal oplog flow (TxOutput{Ok:false}) and ExecuteBatch
// continues with the next op in the batch.
func executeTxSafely(
	vscTx VSCTransaction,
	se common_types.StateEngine,
	ledgerSession ledgerSystem.LedgerSession,
	rcSession rcSystem.RcSession,
	callSession *contract_session.CallSession,
	payer string,
) (result TxResult) {
	defer func() {
		if r := recover(); r != nil {
			self := vscTx.TxSelf()
			log.Error(
				"PANIC during transaction execution",
				"txId", self.TxId,
				"type", vscTx.Type(),
				"blockHeight", self.BlockHeight,
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
			result = TxResult{
				Success: false,
				Ret:     "internal error: panic during execution",
				RcUsed:  100,
			}
		}
	}()
	return vscTx.ExecuteTx(se, ledgerSession, rcSession, callSession, payer)
}

// buildTickInputs assembles the per-tick L2 evidence bundle the rewards
// aggregator scores. Pure read on on-chain caches; deterministic across
// nodes given identical chain state. Returns a zero-Committee TickInputs
// when the tick can't be scored (no election, no committee, etc.).
func (se *StateEngine) buildTickInputs(tickHeight uint64) rewards.TickInputs {
	if se == nil || se.electionDb == nil || tickHeight == 0 {
		return rewards.TickInputs{}
	}
	election, err := se.electionDb.GetElectionByHeight(tickHeight)
	if err != nil {
		// Transient electionDb error: skip scoring this tick. The signer
		// path will retry on the next settlement attempt; logged for
		// operator visibility into per-node DB outages.
		log.Warn("pendulum reductions: election lookup failed; tick skipped",
			"tick_height", tickHeight, "err", err)
		return rewards.TickInputs{}
	}
	if len(election.Members) == 0 {
		return rewards.TickInputs{}
	}
	committee := make([]string, 0, len(election.Members))
	for _, m := range election.Members {
		committee = append(committee, m.Account)
	}

	const tickWindow = uint64(pendulumoracle.DefaultTickIntervalBlocks)
	var fromBlock uint64
	if tickHeight > tickWindow {
		fromBlock = tickHeight - tickWindow
	}

	in := rewards.TickInputs{Committee: committee}

	// Block production + attestation: iterate slots in the tick window.
	if se.vscBlocks != nil {
		slotLen := common.CONSENSUS_SPECS.SlotLength
		if slotLen >= 1 {
			firstSlot := fromBlock - (fromBlock % slotLen)
			if firstSlot < fromBlock {
				firstSlot += slotLen
			}
			lastSlot := tickHeight - (tickHeight % slotLen)
			if lastSlot >= firstSlot {
				blocks, err := se.vscBlocks.GetBlocksInSlotRange(firstSlot, lastSlot)
				if err != nil {
					log.Warn(
						"pendulum reductions: vsc_blocks slot-range lookup failed; block production/attestation skipped this tick",
						"tick_height",
						tickHeight,
						"from_slot",
						firstSlot,
						"to_slot",
						lastSlot,
						"err",
						err,
					)
				} else {
					produced := make(map[uint64]struct{}, len(blocks))
					in.BlocksInWindow = make([]rewards.TickBlockHeader, 0, len(blocks))
					for _, b := range blocks {
						produced[uint64(b.SlotHeight)] = struct{}{}
						in.BlocksInWindow = append(in.BlocksInWindow, rewards.TickBlockHeader{
							Signers: append([]string(nil), b.Signers...),
						})
					}
					in.ProducedSlotHeights = produced
					for s := firstSlot; s <= lastSlot; s += slotLen {
						sched := se.GetSchedule(s)
						leader := ""
						for _, slot := range sched {
							if slot.SlotHeight == s {
								leader = slot.Account
								break
							}
						}
						if leader != "" {
							in.Slots = append(in.Slots, rewards.SlotProposer{
								SlotHeight: s,
								Account:    leader,
							})
						}
					}
				}
			}
		}
	}

	// Oracle quote divergence (replaces the retired EvidenceOraclePayloadFraud
	// principal slash). Trusted-group witnesses whose latest published quote
	// drifted >= rewards.OracleQuoteDivergenceThresholdBps from the trusted
	// mean at this tick each accumulate one OracleQuoteDivergenceBps charge.
	// Identical evidence on every replaying node because FeedTracker quotes
	// are sourced deterministically from feed_publish.
	if se.pendulumFeed != nil {
		in.DivergingOracleWitnesses = se.pendulumFeed.DivergingTrustedWitnesses(
			rewards.OracleQuoteDivergenceThresholdBps,
		)
	}

	// TSS commitments: pull all reshare/blame/sign_result types in the window.
	if se.tssCommitments != nil {
		from := fromBlock
		to := tickHeight
		commits, err := se.tssCommitments.FindCommitments(
			nil,
			[]string{"reshare", "blame", "sign_result"},
			nil,
			&from,
			&to,
			0,
			0,
		)
		if err != nil {
			log.Warn("pendulum reductions: tss_commitments lookup failed; TSS reductions skipped this tick",
				"tick_height", tickHeight, "from_block", from, "to_block", to, "err", err)
		} else {
			for _, c := range commits {
				memberAccounts := se.committeeAccountsForEpoch(c.Epoch)
				if len(memberAccounts) == 0 {
					continue
				}
				switch c.Type {
				case "reshare":
					in.Reshares = append(in.Reshares, rewards.ReshareWithCommittee{
						Commitment:   c,
						NewCommittee: memberAccounts,
					})
				case "blame":
					in.Blames = append(in.Blames, rewards.BlameWithCommittee{
						Commitment:     c,
						BlameCommittee: memberAccounts,
					})
				case "sign_result":
					// Sign committee is the election active at the
					// commitment's BlockHeight (the leader collected BLS sigs
					// from that election — same convention as the verifier
					// path at state_engine.go:895).
					signCommittee := se.committeeAccountsAtHeight(c.BlockHeight)
					if len(signCommittee) == 0 {
						continue
					}
					in.Signs = append(in.Signs, rewards.SignResultWithCommittee{
						Commitment:    c,
						SignCommittee: signCommittee,
					})
				}
			}
		}
	}

	return in
}

func (se *StateEngine) committeeAccountsForEpoch(epoch uint64) []string {
	if se == nil || se.electionDb == nil {
		return nil
	}
	res := se.electionDb.GetElection(epoch)
	if res == nil || len(res.Members) == 0 {
		return nil
	}
	out := make([]string, 0, len(res.Members))
	for _, m := range res.Members {
		out = append(out, m.Account)
	}
	return out
}

func (se *StateEngine) committeeAccountsAtHeight(height uint64) []string {
	if se == nil || se.electionDb == nil {
		return nil
	}
	res, err := se.electionDb.GetElectionByHeight(height)
	if err != nil || len(res.Members) == 0 {
		return nil
	}
	out := make([]string, 0, len(res.Members))
	for _, m := range res.Members {
		out = append(out, m.Account)
	}
	return out
}

func (se *StateEngine) ExecuteBatch() {

	lastBlock, err := se.vscBlocks.GetBlockByHeight(se.slotStatus.SlotHeight)
	if err != nil && err != mongo.ErrNoDocuments {
		log.Error("GetBlockByHeight failed in ExecuteBatch, falling back to lastBlockBh=0",
			"slotHeight", se.slotStatus.SlotHeight, "err", err)
	}

	var lastBlockBh uint64
	if lastBlock == nil {
		lastBlockBh = 0
	} else {
		lastBlockBh = uint64(lastBlock.EndBlock)
	}

	// if len(se.TxOutput) > 0 {
	// 	log.Debug("TxOutput pending", se.TxOutput, len(se.TxBatch))
	// }

	// instead of recreating the ledger session for every Hive transaction,
	// we reuse one session for the whole slot so balance checks see prior ops
	se.LedgerState.BlockHeight = lastBlockBh
	ledgerSession := ledgerSystem.NewSession(se.LedgerState)

	for idx, tx := range se.TxBatch {
		var opTypes map[string]bool = make(map[string]bool)
		for _, vscTx := range tx.Ops {
			opTypes[vscTx.Type()] = true
		}

		opTypesList := make([]string, 0)
		for k := range opTypes {
			opTypesList = append(opTypesList, k)
		}
		if len(opTypesList) == 1 && opTypesList[0] == "deposit" {
			continue
		}

		fmt.Println("Executing item in batch", idx, len(se.TxBatch))
		// ledgerSession := se.LedgerSystem.NewSession(lastBlockBh)
		rcSession := se.RcSystem.NewSession(ledgerSession)
		// Pass the current temp outputs so calls within this slot see the
		// latest in-memory state instead of the latest contract state
		callSession := contract_session.NewCallSession(
			se.da,
			se.contractDb,
			se.contractState,
			se.tssKeys,
			lastBlockBh,
			se.TempOutputs,
		)

		outputs := make([]ContractIdResult, 0)
		ok := true
		for idx, vscTx := range tx.Ops {
			fmt.Println("Execute tx.bh", vscTx.TxSelf().BlockHeight)

			if vscTx.Type() == "deposit" {
				continue
			}
			if se.firstTxHeight == 0 {
				se.firstTxHeight = vscTx.TxSelf().BlockHeight - 1
			}
			if len(vscTx.TxSelf().RequiredAuths) == 0 && len(vscTx.TxSelf().RequiredPostingAuths) == 0 {
				log.Debug("TRANSACTION REVERTING - no required auths")
				ok = false
				ledgerSession.Revert()
				break
			}

			var payer string
			if len(vscTx.TxSelf().RequiredAuths) == 0 {
				payer = vscTx.TxSelf().RequiredPostingAuths[0]
			} else {
				payer = vscTx.TxSelf().RequiredAuths[0]
			}

			var contractId string
			lastContractMeta := contracts.ContractMetadata{}
			lastStateCid := ""
			if vscTx.Type() == "call" {
				contractCall, ok := vscTx.(TxVscCallContract)
				if ok {
					contractId = contractCall.ContractId
					if lastTmpOut, exist := se.TempOutputs[contractId]; !exist {
						contractOutput, err := se.contractState.GetLastOutput(contractCall.ContractId, lastBlockBh)
						if err == nil {
							lastContractMeta = contractOutput.Metadata
							lastStateCid = contractOutput.StateMerkle
						}
					} else {
						lastContractMeta = lastTmpOut.Metadata
						lastStateCid = lastTmpOut.Cid
					}
				}
			}
			result := executeTxSafely(vscTx, se, ledgerSession, rcSession, callSession, payer)

			log.Debug(
				"TRANSACTION STATUS",
				"result",
				result,
				"ledger session",
				ledgerSession,
				"idx",
				idx,
				"type",
				vscTx.Type(),
				"rc payer",
				payer,
				"rc used",
				result.RcUsed,
			)

			rcUsed := se.RcMap[payer] // don't crash if payer is not in RC map
			se.RcMap[payer] = rcUsed + result.RcUsed

			if vscTx.Type() == "call" {
				txId := MakeTxId(tx.TxId, idx)
				if !result.Success {
					// If failed, output the error message only
					outputs = []ContractIdResult{{
						ContractId: contractId,
						Output: ContractResult{
							TxId:    txId,
							Ret:     "",
							Success: result.Success,
							Err:     result.Err,
							ErrMsg:  result.Ret,
						},
					}}
					// Append previous output here if not already to make sure the error symbol and message is included in contract output
					if _, exist := se.TempOutputs[contractId]; !exist {
						se.TempOutputs[contractId] = &contract_session.TempOutput{
							Metadata: lastContractMeta,
							Cid:      lastStateCid,

							Deletions: make(map[string]bool),
							Cache:     make(map[string][]byte),
						}
					}
				} else {
					logs := callSession.PopLogs()
					// Sort log keys for deterministic output ordering across nodes.
					// Cross-contract calls produce logs from multiple contracts;
					// unsorted map iteration causes different output order → CID mismatch.
					logIds := make([]string, 0, len(logs))
					for id := range logs {
						logIds = append(logIds, id)
					}
					sort.Strings(logIds)
					for _, id := range logIds {
						log := logs[id]
						if id == contractId {
							outputs = append(outputs, ContractIdResult{
								ContractId: contractId,
								Output: ContractResult{
									TxId:    txId,
									Ret:     result.Ret,
									Success: result.Success,
									Logs:    log.Logs,
									TssOps:  log.TssOps,
								},
							})
						} else {
							outputs = append(outputs, ContractIdResult{
								ContractId: id,
								Output: ContractResult{
									TxId:    txId,
									Ret:     "",
									Success: result.Success,
									Logs:    log.Logs,
									TssOps:  log.TssOps,
								},
							})
						}
					}
				}
			}
			if !result.Success {
				log.Debug("TRANSACTION REVERTING")
				ok = false
				ledgerSession.Revert()
				break
			}
		}
		for _, out := range outputs {
			se.AppendOutput(out.ContractId, out.Output)
		}
		if ok {
			callSession.Commit()
			callOutputs := callSession.ToOutputs()
			for k, v := range callOutputs {
				vv := v
				se.TempOutputs[k] = &vv
			}
		}
		ledgerIds := ledgerSession.Done()

		se.TxOutput[tx.TxId] = TxOutput{
			Ok:        ok,
			LedgerIds: ledgerIds,
		}
		se.TxOutIds = append(se.TxOutIds, tx.TxId)
	}

	se.TxBatch = make([]TxPacket, 0)
}

func (se *StateEngine) UpdateBalances(startBlock, endBlock uint64) {
	//Sets a default start block of 0 if near block 0
	//E2E testing starts at block 0
	var stBlock uint64
	if startBlock == 0 {
		stBlock = 0
	} else {
		stBlock = endBlock - 9
	}

	// review4 HIGH #94 (fail-stop): the consensus_unstake release epoch comes
	// from an election read, and the matured set from a pending-actions read.
	// Swallowing a transient error on either and deferring would diverge this
	// node from peers that read successfully and released — different ledger
	// writes, different payout heights, a fork. Both reads are therefore
	// fail-stop (block until the DB recovers), so a node either releases the
	// same set as everyone else or makes no progress. A deterministic "no
	// election yet" (pre-genesis) is identical across nodes and releases
	// nothing; a genuine epoch-0 election queries normally (nothing is due
	// before epoch 5, so the result is empty either way).
	var epoch uint64
	var records []ledgerDb.ActionRecord
	if se.electionDb != nil {
		if election, found := se.GetElectionInfoOrBlock(endBlock); found {
			epoch = election.Epoch
			records = se.getPendingActionsByEpochOrBlock(election.Epoch, "consensus_unstake")
		}
	}

	completeIds := make([]string, 0)
	ledgerRecords := make([]ledgerDb.LedgerRecord, 0)
	for _, record := range records {
		completeIds = append(completeIds, record.Id)

		ledgerRecords = append(ledgerRecords, ledgerDb.LedgerRecord{
			Id:          record.Id + "#out",
			Amount:      record.Amount,
			Asset:       "hive",
			BlockHeight: endBlock,
			Owner:       record.To,
			Type:        "consensus_unstake",
		})
	}
	// Only mark the unstake actions complete if their payout records actually
	// landed. If the write failed, leaving the actions pending lets the next
	// pass re-attempt them — record IDs are deterministic (record.Id+"#out"),
	// so the retry is idempotent and cannot double-pay. Completing them on a
	// failed write would strand the HIVE permanently.
	if err := se.LedgerState.LedgerDb.StoreLedger(ledgerRecords...); err != nil {
		log.Error("consensus_unstake: ledger write failed; leaving actions pending for retry",
			"epoch", epoch, "count", len(ledgerRecords), "err", err)
	} else {
		se.LedgerState.ActionDb.ExecuteComplete(nil, completeIds...)
	}
	// se.LedgerExecutor.Ls.LedgerDb.StoreLedger(ledgerRecords...)
	// se.LedgerExecutor.Ls.ActionsDb.ExecuteComplete(nil, completeIds...)

	if se.LedgerSystem != nil {
		se.LedgerSystem.FinalizeMaturedSafetySlashBurns(endBlock)
	}

	//log.Debug("stBlock, endBlock", stBlock, endBlock)
	distinctAccounts, err := se.LedgerState.LedgerDb.GetDistinctAccountsRange(stBlock, endBlock)
	if err != nil {
		log.Error("GetDistinctAccountsRange failed, balance snapshots may be incomplete this slot",
			"stBlock", stBlock, "endBlock", endBlock, "err", err)
	}

	//Ensure system:fr_balance is always processed so its hbd_claim stays current.
	//Its interest goes to hive:vsc.dao, so it never appears in distinctAccounts via
	//interest ledger records, but it still needs claim-height updates for correct TWAB.
	hasFr := false
	for _, a := range distinctAccounts {
		if a == params.FR_VIRTUAL_ACCOUNT {
			hasFr = true
			break
		}
	}
	if !hasFr {
		frBal, _ := se.LedgerState.BalanceDb.GetBalanceRecord(params.FR_VIRTUAL_ACCOUNT, endBlock)
		if frBal != nil {
			distinctAccounts = append(distinctAccounts, params.FR_VIRTUAL_ACCOUNT)
		}
	}

	assets := []string{"hbd", "hive", "hbd_savings", "hive_consensus"}

	//Cleanup!
	for _, k := range distinctAccounts {
		ledgerBalances := map[string]int64{}
		prevBalRecord, _ := se.LedgerState.BalanceDb.GetBalanceRecord(k, endBlock)
		var balanceR ledgerDb.BalanceRecord
		var stHeight uint64
		if prevBalRecord != nil {
			balanceR = *prevBalRecord
			stHeight = prevBalRecord.BlockHeight + 1 //Must add one to prevent querying ledger results from same bal record
		}
		for _, asset := range assets {
			if asset == "hbd" {
				ledgerBalances[asset] = balanceR.HBD
			} else if asset == "hive" {
				ledgerBalances[asset] = balanceR.Hive
			} else if asset == "hbd_savings" {
				ledgerBalances[asset] = balanceR.HBD_SAVINGS
			} else if asset == "hive_consensus" {
				ledgerBalances[asset] = balanceR.HIVE_CONSENSUS
			} else {
				ledgerBalances[asset] = 0
			}
		}
		//As of block X or below
		// se.LedgerExecutor.Ls.log.Debug("GetBalance for account", stBlock, stHeight, endBlock)

		// review4 HIGH #118: GetLedgerRange returns `(nil, err)` on Mongo
		// failure. The original form discarded the error and derefed the nil
		// pointer at `len(*ledgerUpdates)`, panicking the slot. Skipping the
		// account on error is also unsafe: HBD_AVG is a path-dependent
		// cumulative sum kept only in the balance snapshot and is never
		// rebuilt from the authoritative ledger (unlike spendable balances,
		// which GetBalance reconstructs from any checkpoint). A single
		// skipped slot would mistime this account's TWAB accumulation and
		// permanently diverge its snapshot — and thus its share of the next
		// HBD-interest distribution — from peers whose DB stayed healthy.
		// Fail-stop instead: block the slot until the read succeeds. See
		// getLedgerRangeOrBlock.
		ledgerUpdates := se.getLedgerRangeOrBlock(k, stHeight, endBlock, "")

		hasLedgerUpdates := ledgerUpdates != nil && len(*ledgerUpdates) > 0

		//Previous claim record
		claimRecord := se.claimDb.GetLastClaim(endBlock)

		var hbdAvg = int64(0)
		var modifyHeight = uint64(0)
		var claimHeight = uint64(0)

		exists := claimRecord != nil
		var claimRecordC ledgerDb.ClaimRecord
		if exists {
			claimRecordC = *claimRecord
			modifyHeight = endBlock
		}

		needsClaimUpdate := claimRecordC.BlockHeight != balanceR.HBD_CLAIM_HEIGHT

		//Skip accounts that have no new ledger records AND no pending claim update
		if !hasLedgerUpdates && !needsClaimUpdate {
			continue
		}

		for _, v := range *ledgerUpdates {
			switch v.Type {
			case ledgerSystem.LedgerTypeSafetySlashHiveBurn,
				ledgerSystem.LedgerTypeSafetySlashHiveBurnPending,
				ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingRelease,
				ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingFinalized,
				ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingCancelled,
				ledgerSystem.LedgerTypeSafetySlashBurnFinalizeCursor,
				ledgerSystem.LedgerTypeSafetyRestitutionClaim,
				ledgerSystem.LedgerTypeSafetyRestitutionClaimConsumed:
				// Protocol meta rows. Burn / pending-burn / finalize-cursor
				// rows live on protocol-owned accounts; restitution claim
				// rows live on ProtocolSlashRestitutionClaimsAccount and
				// represent queue state, never spendable HIVE on the
				// victim's own account (the victim is credited via a
				// separate LedgerTypeSafetySlashRestitution row written by
				// SafetySlashConsensusBond when the queue is allocated).
				continue
			default:
				ledgerBalances[v.Asset] += v.Amount
			}
		}

		if needsClaimUpdate {
			//Need to execute recalculation of the claim
			hbdAvg = 0
			claimHeight = claimRecord.BlockHeight
		} else if prevBalRecord != nil {
			//There is a previous balance record
			//HBD_AVG stores an unnormalized cumulative sum (balance * blocks) since the last claim.
			//Accumulate the previous balance's contribution for blocks since last modification.
			A := endBlock - prevBalRecord.HBD_MODIFY_HEIGHT
			hbdAvg = prevBalRecord.HBD_AVG + prevBalRecord.HBD_SAVINGS*int64(A)
			modifyHeight = endBlock
			claimHeight = prevBalRecord.HBD_CLAIM_HEIGHT
		} else {
			modifyHeight = endBlock
			hbdAvg = 0
		}

		newRecord := ledgerDb.BalanceRecord{
			Account:        k,
			BlockHeight:    endBlock,
			Hive:           ledgerBalances["hive"],
			HIVE_CONSENSUS: ledgerBalances["hive_consensus"],
			HBD:            ledgerBalances["hbd"],
			HBD_SAVINGS:    ledgerBalances["hbd_savings"],
			HBD_AVG:        hbdAvg,
		}

		newRecord.HBD_MODIFY_HEIGHT = modifyHeight

		newRecord.HBD_CLAIM_HEIGHT = claimHeight

		// review4 HIGH #95: UpdateBalanceRecord returns an error on Mongo
		// write failure. Silently dropping it leaves the next slot's TWAB
		// calc reading the stale snapshot, which feeds the HBD-interest
		// distribution path. We can't safely abort the slot here (other
		// accounts already wrote), but we surface the failure so it shows
		// up in operator monitoring instead of vanishing.
		if err := se.LedgerState.BalanceDb.UpdateBalanceRecord(newRecord); err != nil {
			fmt.Println("ExecuteBatch: UpdateBalanceRecord failed", k, endBlock, err)
		}

		se.LedgerState.VirtualLedger[k] = slices.DeleteFunc(
			se.LedgerState.VirtualLedger[k],
			func(v ledgerSystem.LedgerUpdate) bool {
				return v.Type == "deposit"
			},
		)
	}
}

// blockingRetry runs `read` until it returns a nil error, sleeping with
// capped exponential backoff between attempts. It never gives up.
//
// This is the fail-stop primitive for the deterministic state path. A DB
// read that one node completes but another swallows lets the two nodes
// decide a slot/tx outcome differently — a consensus fork. Rather than
// advance on a swallowed error (or panic on a nil result), a node whose DB
// is unavailable simply makes no progress until it recovers (or an operator
// restarts it). Every honest node computes the identical result once its DB
// is reachable. `read` returns nil to stop (success OR a deterministic
// not-found the caller will handle) and a non-nil infra error to keep
// blocking. `what` labels the operation in logs.
//
// (Operator visibility is via logs for now; a health-endpoint surface for
// the stalled state is deferred to the in-flight health PR.)
func blockingRetry(what string, read func() error) {
	const (
		baseDelay = 100 * time.Millisecond
		maxDelay  = 30 * time.Second
	)
	delay := baseDelay
	for attempt := 1; ; attempt++ {
		if err := read(); err == nil {
			if attempt > 1 {
				log.Error("DB read recovered; resuming slot", "op", what, "attempts", attempt)
			}
			return
		} else {
			log.Error("DB read failed; halting slot until DB recovers (fail-stop)",
				"op", what, "attempt", attempt, "retryIn", delay.String(), "err", err)
		}
		time.Sleep(delay)
		if delay < maxDelay {
			if delay *= 2; delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

// getLedgerRangeOrBlock fail-stops on a GetLedgerRange error. HBD_AVG is a
// path-dependent cumulative sum that lives only in the balance snapshot and
// is never reconstructed from the authoritative ledger, so any slot a node
// skips on a swallowed error is unrecoverable — its TWAB, and therefore its
// slice of the next HBD-interest distribution, drifts permanently from peers
// whose DB was healthy. Blocking until the read succeeds keeps the snapshot
// either correct or unwritten.
func (se *StateEngine) getLedgerRangeOrBlock(account string, start, end uint64, asset string) *[]ledgerDb.LedgerRecord {
	var out *[]ledgerDb.LedgerRecord
	blockingRetry(fmt.Sprintf("GetLedgerRange(%s @%d)", account, end), func() error {
		var err error
		out, err = se.LedgerState.LedgerDb.GetLedgerRange(account, start, end, asset)
		return err
	})
	return out
}

// GetElectionInfoOrBlock is the fail-stop election read. It blocks on an
// infra-level election read error but distinguishes the deterministic "no
// election covers this height yet" (mongo.ErrNoDocuments) — which every honest
// node sees identically and the caller handles via found=false — from a
// transient DB failure, which blocks. This closes the partial-fork in the
// consensus_unstake create/release paths where the prior swallow-the-error
// read returned a zero-value epoch. Exported because the unstake tx handler
// reaches it through the common_types.StateEngine interface.
func (se *StateEngine) GetElectionInfoOrBlock(height uint64) (elections.ElectionResult, bool) {
	var out elections.ElectionResult
	var found bool
	blockingRetry(fmt.Sprintf("GetElectionByHeight(%d)", height), func() error {
		election, err := se.electionDb.GetElectionByHeight(height)
		if err == nil {
			out, found = election, true
			return nil
		}
		if errors.Is(err, mongo.ErrNoDocuments) {
			// Deterministic absence (pre-genesis / no election below height):
			// not an infra failure. Stop retrying; report not-found.
			out, found = elections.ElectionResult{}, false
			return nil
		}
		return err // infra failure → keep blocking
	})
	return out, found
}

// getPendingActionsByEpochOrBlock fail-stops on the pending-actions read used
// to release matured consensus_unstakes. Companion to getElectionByHeightOrBlock
// so the whole release decision is all-or-nothing per slot.
func (se *StateEngine) getPendingActionsByEpochOrBlock(epoch uint64, t ...string) []ledgerDb.ActionRecord {
	var out []ledgerDb.ActionRecord
	blockingRetry(fmt.Sprintf("GetPendingActionsByEpoch(%d)", epoch), func() error {
		var err error
		out, err = se.LedgerState.ActionDb.GetPendingActionsByEpoch(epoch, t...)
		return err
	})
	return out
}

func (se *StateEngine) UpdateRcMap(blockHeight uint64) {
	for k, v := range se.RcMap {
		//Get the last rc record
		rcRecord, _ := se.rcDb.GetRecord(k, blockHeight-1)

		var rcBal int64
		if rcRecord.BlockHeight == 0 {
			rcBal = v
		} else {
			frozeAmt := rcSystem.CalculateFrozenBal(rcRecord.BlockHeight, blockHeight, rcRecord.Amount)

			rcBal = frozeAmt + v
		}

		// Cap rcBal to the user's actual balance (+ free amount for Hive accounts)
		// to prevent frozen RC from accumulating beyond what the user owns.
		balAmt := se.LedgerSystem.GetBalance(k, blockHeight, "hbd")
		if strings.HasPrefix(k, "hive:") {
			balAmt = balAmt + params.RC_HIVE_FREE_AMOUNT
		}
		if rcBal > balAmt {
			rcBal = balAmt
		}
		if rcBal < 0 {
			rcBal = 0
		}

		se.rcDb.SetRecord(k, blockHeight, rcBal)
	}
}

// Append a contract output to the output map
func (se *StateEngine) AppendOutput(contractId string, out ContractResult) {
	if se.ContractResults[contractId] == nil {
		se.ContractResults[contractId] = make([]ContractResult, 0)
	}
	se.ContractResults[contractId] = append(se.ContractResults[contractId], out)
}

func (se *StateEngine) Flush() {
	se.ContractResults = make(map[string][]ContractResult)
	se.TempOutputs = make(map[string]*contract_session.TempOutput)
	se.TxOutput = make(map[string]TxOutput)
	se.TxOutIds = make([]string, 0)
	se.firstTxHeight = 0
}

// If there is transactions in the queue, use the last vsc block height to resume
// If not continue parsing from lastBlk
// Need to test
func (se *StateEngine) SaveBlockHeight(lastBlk uint64, lastSavedBlk uint64) uint64 {

	if lastBlk == 0 || lastSavedBlk == 0 {
		return lastSavedBlk
	}
	var outputExists bool
	for _, _ = range se.TxOutput {
		outputExists = true
		break
	}
	if outputExists {
		vscRecord, _ := se.vscBlocks.GetBlockByHeight(lastBlk)
		if vscRecord != nil {
			if lastSavedBlk != uint64(vscRecord.SlotHeight) {
				return uint64(vscRecord.SlotHeight) + 1
			} else {
				return lastSavedBlk
			}
		} else {
			if se.firstTxHeight == 0 {
				return lastSavedBlk
			}
			return se.firstTxHeight - 1
		}
	} else {
		return lastBlk
	}

	// if len(se.TxBatch) > 0 {
	// } else {
	// 	return lastBlk
	// }
}

func (se *StateEngine) DataLayer() common_types.DataLayer {
	return se.da
}

func (se *StateEngine) GetContractInfo(id string, height uint64) (contracts.Contract, bool) {
	contractInfo, err := se.contractDb.ContractById(id, height)

	if err == mongo.ErrNoDocuments {
		return contracts.Contract{}, false
	} else if err != nil {
		fmt.Println("GetContractInfo: db error", "id", id, "height", height, "err", err)
		return contracts.Contract{}, false
	}

	return contractInfo, true
}

func (se *StateEngine) Commit() {

}

func (se *StateEngine) Init() error {
	// One-time migration: deprecate any active keys that pre-date the expiry system.
	// These keys have no ExpiryEpoch and would otherwise reshare forever.
	// deprecated_height=0 means no retirement clock — they stay deprecated until renewed.
	if err := se.tssKeys.DeprecateLegacyKeys(); err != nil {
		tssLog.Warn("DeprecateLegacyKeys failed during init", "err", err)
	}

	// Warm the pendulum FeedTracker before the block consumer starts so the
	// in-memory rolling state (signature window + MA ring + per-witness
	// quotes) matches long-running peers. Without warmup, the first ~300
	// blocks after a restart expose a partial MA to the swap applier and
	// contract env keys — different across nodes that started at different
	// heights, which forks any contract that consumes those values.
	//
	// Failure here is non-fatal: the tracker stays unwarmed, callers see
	// Warmed()=false and degrade gracefully (applier rejects swaps with
	// errSnapshotUnavailable; env returns nil) until natural ingest fills
	// both rings ~400 blocks later.
	if se.pendulumFeed != nil && se.hiveBlocks != nil {
		if err := se.pendulumFeed.Warmup(se.hiveBlocks); err != nil {
			log.Warn("pendulum feed warmup failed; tracker will warm organically", "err", err)
		}
	}

	// One-time bootstrap: seed latestSettledEpoch on networks that pre-date
	// inlined settlement, so an in-place upgrade doesn't deadlock the election
	// proposer's canHold gate. No-op on fresh chains and after the first seed.
	se.seedPendulumSettlement()

	return nil
}

func (se *StateEngine) Start() *promise.Promise[any] {

	return nil
}

func (se *StateEngine) Stop() error {
	return nil
}

func (se *StateEngine) SystemConfig() systemconfig.SystemConfig {
	return se.sconf
}

// LastProcessedHeight returns the height of the most recent Hive block this
// state engine has fully processed. Returns 0 before the first ProcessBlock.
//
// Block producer compose paths use this — through WaitForProcessedHeight —
// to ensure consensus-critical reads (settlement bonds, bucket balance,
// epoch reductions) see all writes flushed by ProcessBlock(slotHeight).
// Without this barrier, an async producer tick or an early-arriving p2p
// signing request can read mid-flush state and produce a divergent CID.
func (se *StateEngine) LastProcessedHeight() uint64 {
	if se == nil {
		return 0
	}
	return se.lastProcessedHeight.Load()
}

// WaitForProcessedHeight blocks until LastProcessedHeight >= height, the
// context is canceled, or the deadline elapses. Polls every 25ms; the
// caller's context bounds the overall wait.
//
// Returns nil on success or the context's error on cancel/deadline. The
// poll interval is short enough (Hive blocks every 3s, slot ~30s) that
// catch-up is observed near-immediately under healthy operation; the
// timeout exists only to protect against a stuck state engine.
func (se *StateEngine) WaitForProcessedHeight(ctx context.Context, height uint64) error {
	if se == nil {
		return nil
	}
	if se.lastProcessedHeight.Load() >= height {
		return nil
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if se.lastProcessedHeight.Load() >= height {
				return nil
			}
		}
	}
}

// PendulumFeedTracker returns the Magi pendulum sole-HIVE / HBD-APR oracle tracker (may be nil in tests).
func (se *StateEngine) PendulumFeedTracker() *pendulumoracle.FeedTracker {
	if se == nil {
		return nil
	}
	return se.pendulumFeed
}

// PendulumApplier implements common_types.StateEngine. Returns the swap-time
// applier wired during construction; nil-safe.
func (se *StateEngine) PendulumApplier() wasm_context.PendulumApplier {
	if se == nil {
		return nil
	}
	return se.pendulumApplier
}

// PendulumOracleEnv implements common_types.StateEngine: values merged into wasm contract env (system.get_env).
func (se *StateEngine) PendulumOracleEnv() map[string]interface{} {
	if se == nil || se.pendulumFeed == nil {
		return nil
	}
	// Withhold env keys until the tracker is warm. The exposed values
	// include the moving average and trusted-witness group, both of which
	// diverge across nodes during the warmup window — any contract that
	// reads them and writes derived state would fork the chain.
	if !se.pendulumFeed.Warmed() {
		return nil
	}
	s := se.pendulumFeed.LastTick()
	// All numeric values are integer-typed: HBD-per-HIVE prices in basis
	// points (BpsScale = 1.0); the wasm host serializer renders them as
	// integer strings.
	m := map[string]interface{}{
		"pendulum.hbd_interest_rate_bps":  s.HBDInterestRateBps,
		"pendulum.hbd_interest_rate_ok":   s.HBDInterestRateOK,
		"pendulum.trusted_hive_price_bps": s.TrustedHivePriceBps,
		"pendulum.trusted_hive_mean_ok":   s.TrustedHiveOK,
		"pendulum.hive_moving_avg_bps":    s.HiveMovingAvgBps,
		"pendulum.hive_ma_ok":             s.HiveMovingAvgOK,
		"pendulum.tick_block_height":      s.TickBlockHeight,
	}
	if len(s.TrustedWitnessGroup) > 0 {
		m["pendulum.trusted_witness_group"] = append([]string(nil), s.TrustedWitnessGroup...)
	}
	return m
}

func New(sconf systemconfig.SystemConfig, da *DataLayer.DataLayer,
	witnessesDb witnesses.Witnesses,
	electionsDb elections.Elections,
	contractDb contracts.Contracts,
	contractStateDb contracts.ContractState,
	txDb transactions.Transactions,
	ledgerDb ledgerDb.Ledger,
	balanceDb ledgerDb.Balances,
	hiveBlocks hive_blocks.HiveBlocks,
	interestClaims ledgerDb.InterestClaims,
	vscBlocks vscBlocks.VscBlocks,
	actionDb ledgerDb.BridgeActions,
	rcDb rcDb.RcDb,
	nonceDb nonces.Nonces,
	tssKeys tss_db.TssKeys,
	tssCommitments tss_db.TssCommitments,
	tssRequests tss_db.TssRequests,
	pendulumSettlementsDb pendulum_settlements.PendulumSettlements,
	consensusStateDb consensus_state.ConsensusState,
	wasm *wasm_runtime.Wasm,
	identityConfig common.IdentityConfig,
) *StateEngine {

	ls := ledgerSystem.New(balanceDb, ledgerDb, interestClaims, actionDb, sconf)

	// {
	// 	BalanceDb: balanceDb,
	// 	LedgerDb:  ledgerDb,
	// 	ClaimDb:   interestClaims,
	// 	ActionsDb: actionDb,
	// 	log:       logger,
	// }

	ledgerState := &ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		BlockHeight:     0,
		LedgerDb:        ledgerDb,
		ActionDb:        actionDb,
		BalanceDb:       balanceDb,
	}

	se := &StateEngine{
		sconf:                         sconf,
		TxOutput:                      make(map[string]TxOutput),
		ContractResults:               make(map[string][]ContractResult),
		TempOutputs:                   make(map[string]*contract_session.TempOutput),
		TxOutIds:                      make([]string, 0),
		slashRestitution:              safetyslash.NewOnLedgerRestitutionAllocator(ledgerDb),
		safetyEvidenceSeen:            make(map[string]uint64),
		seenProposalBySlotProposer:    make(map[string]string),
		slashIncidentBpsBySlotAccount: make(map[string]int),

		da: da,
		// db: db,

		witnessDb:      witnessesDb,
		electionDb:     electionsDb,
		contractDb:     contractDb,
		contractState:  contractStateDb,
		hiveBlocks:     hiveBlocks,
		vscBlocks:      vscBlocks,
		claimDb:        interestClaims,
		txDb:           txDb,
		rcDb:           rcDb,
		nonceDb:        nonceDb,
		RcSystem:       rcSystem.New(rcDb, ls),
		RcMap:          make(map[string]int64),
		tssRequests:    tssRequests,
		tssCommitments: tssCommitments,
		tssKeys:        tssKeys,

		consensusState:   consensusStateDb,
		consensusRuntime: NewConsensusRuntime(),

		wasm: wasm,

		// LedgerExecutor: &ledgerSystem.LedgerExecutor{
		// 	VirtualLedger: make(map[string][]ledgerSystem.LedgerUpdate),
		// 	Ls:            ls,
		// },
		LedgerSystem: ls,
		LedgerState:  ledgerState,

		pendulumFeed:          pendulumoracle.NewFeedTracker(sconf.OnMainnet()),
		pendulumSettlementsDb: pendulumSettlementsDb,
		balanceDb:             balanceDb,
	}
	if identityConfig != nil {
		se.selfHiveUsername = identityConfig.Get().HiveUsername
	}

	se.pendulumGeometry = pendulumoracle.NewGeometryComputer(
		&pendulumPoolReserveReader{
			states: &liveContractStateKeyReader{
				contractState: se.contractState,
				da:            se.da,
			},
		},
		&pendulumCommitteeBondReader{se: se},
	)
	se.pendulumApplier = pendulumwasm.New(
		&liveGeometryReader{
			computer:          se.pendulumGeometry,
			feed:              se.pendulumFeed,
			whitelist:         func() []string { return sconf.PendulumPoolWhitelist() },
			effectiveStakeNum: 2,
			effectiveStakeDen: 3,
		},
		func() []string { return sconf.PendulumPoolWhitelist() },
		pendulumwasm.DefaultConfig(),
	)

	return se
}

// applyPrincipalSlashForProvableEvidence debits HIVE_CONSENSUS for a single
// evidence line. Currently invoked only by block-production safety detectors
// (double-block-sign and invalid-block-proposal); other on-chain signals like
// TSS blame and oracle quote divergence are routed to the liveness path
// instead — see modules/incentive-pendulum/safety_slash/policy.go.
// Uses the shared restitution queue and delayed-burn policy so behaviour
// stays uniform if additional provable detectors are added later.
func (se *StateEngine) applyPrincipalSlashForProvableEvidence(
	accountHive string,
	slashBps int,
	txID string,
	blockHeight uint64,
	evidenceKind string,
) ledgerSystem.LedgerResult {
	if se == nil || se.LedgerSystem == nil {
		return ledgerSystem.LedgerResult{Ok: false, Msg: "state engine or ledger not configured"}
	}
	return se.LedgerSystem.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         accountHive,
		SlashBps:        slashBps,
		TxID:            txID,
		BlockHeight:     blockHeight,
		EvidenceKind:    evidenceKind,
		Restitution:     se.slashRestitution,
		BurnDelayBlocks: safetyslash.DefaultSafetySlashBurnDelayBlocks,
	})
}

func (se *StateEngine) evidenceKey(accountHive, kind string) string {
	return normalizeHiveAccount(accountHive) + "|" + kind
}

func (se *StateEngine) evidenceSeenKey(accountHive, kind, evidenceID string) string {
	return se.evidenceKey(accountHive, kind) + "|" + evidenceID
}

// slotProposerKey builds the in-memory map / incident key for a
// (slot, proposer) pair as "slotHeightDecimal|normalizedAccount". This is the
// single source of truth for the format consumed by slashForEvidenceIfPolicyAllows
// (incident cap) and inverted by slotHeightFromSlotKey (prune). The double-sign
// detector and rehydrateDoubleSignMap MUST use it so a restarted node and a
// continuously-running node produce byte-identical keys.
func slotProposerKey(slotHeight uint64, account string) string {
	return strconv.FormatUint(slotHeight, 10) + "|" + normalizeHiveAccount(account)
}

// recordFirstSeenProposal records blockCID as the first proposal observed for
// (slotHeight, account) when none is recorded yet (first-seen-wins). It returns
// the previously-recorded CID and whether one already existed, letting the
// caller detect equivocation (existed && prev != blockCID). It never overwrites
// an existing entry, so a duplicate or conflicting later proposal leaves the
// reference unchanged. Shared by the live detector and the startup rehydrate so
// both seed identically.
func (se *StateEngine) recordFirstSeenProposal(
	slotHeight uint64,
	account, blockCID string,
) (prev string, existed bool) {
	if se.seenProposalBySlotProposer == nil {
		se.seenProposalBySlotProposer = make(map[string]string)
	}
	key := slotProposerKey(slotHeight, account)
	prev, existed = se.seenProposalBySlotProposer[key]
	if !existed {
		se.seenProposalBySlotProposer[key] = blockCID
	}
	return prev, existed
}

// rehydrateDoubleSignMap re-seeds seenProposalBySlotProposer for the slot in
// progress at resumeHeight. On a warm restart the node resumes from a persisted
// checkpoint that is NOT guaranteed to sit on a slot boundary (SaveBlockHeight
// returns the raw block height whenever the last produced block carried no
// executable txs), so it can land past a producer's first proposal but before a
// competing second one in the same slot. Blocks below resumeHeight are never
// replayed, so without this the first ref is lost and the later equivocation
// escapes detection — diverging this replica's ledger from peers that never
// restarted. We replay ONLY the first-seen-wins seeding over the already-
// processed portion of the current slot, [slotStart, resumeHeight-1]: no
// validation, no slashing, no ledger writes — purely reconstructing the
// in-memory map a continuously-running node would already hold. Idempotent: a
// later normal ExecuteTx of those same ops only re-confirms the existing ref.
func (se *StateEngine) rehydrateDoubleSignMap(resumeHeight uint64) {
	if se == nil || se.hiveBlocks == nil || resumeHeight == 0 {
		return
	}
	slotStart := CalculateSlotInfo(resumeHeight).StartHeight
	if slotStart >= resumeHeight {
		// Resuming exactly at a slot boundary: the whole slot is replayed
		// normally, so there is nothing already-processed to recover.
		return
	}
	// All blocks in [slotStart, resumeHeight-1] belong to the same slot, so the
	// scheduled producer is resolved once. Empty means no schedule is known for
	// this slot yet (e.g. fresh sync) — nothing to seed.
	scheduled := se.scheduledProducer(slotStart)
	if scheduled == "" {
		return
	}
	blocks, err := se.hiveBlocks.FetchStoredBlocks(slotStart, resumeHeight-1)
	if err != nil {
		log.Warn("double-sign map rehydrate: fetch failed (continuing without it)",
			"slot", slotStart, "resume", resumeHeight, "err", err)
		return
	}
	seeded := 0
	for _, blk := range blocks {
		seeded += se.seedProposalsFromStoredBlock(blk, scheduled)
	}
	if seeded > 0 {
		log.Info("double-sign map rehydrated after restart",
			"slot", slotStart, "fromHeight", slotStart, "toHeight", resumeHeight-1, "entries", seeded)
	}
}

// scheduledProducer returns the account scheduled to produce the slot starting
// at slotHeight, or "" if none is found. Mirrors the schedule lookup the
// produce_block handler uses for its auth gate.
func (se *StateEngine) scheduledProducer(slotHeight uint64) string {
	for _, slot := range se.GetSchedule(slotHeight) {
		if slot.SlotHeight == slotHeight {
			return slot.Account
		}
	}
	return ""
}

// seedProposalsFromStoredBlock replays ONLY the double-sign detector's
// first-seen seeding for one stored Hive block: it mirrors the produce_block
// parse + scheduled-producer auth gate + slot key in ProcessBlock and must stay
// in sync with that handler. scheduledAccount is the producer scheduled for the
// block's slot (passed in so the caller resolves the schedule once). Returns the
// number of newly-seeded map entries.
func (se *StateEngine) seedProposalsFromStoredBlock(block hive_blocks.HiveBlock, scheduledAccount string) int {
	slotHeight := CalculateSlotInfo(block.BlockNumber).StartHeight
	seeded := 0
	for _, tx := range block.Transactions {
		if len(tx.Operations) == 0 {
			continue
		}
		op := tx.Operations[0]
		if op.Type != "custom_json" {
			continue
		}
		cjId, _ := op.Value["id"].(string)
		cjJson, cjJsonOk := op.Value["json"].(string)
		if cjId != "vsc.produce_block" || !cjJsonOk {
			continue
		}
		requiredAuths := common.ArrayToStringArray(op.Value["required_auths"])
		if len(requiredAuths) == 0 || requiredAuths[0] != scheduledAccount {
			continue
		}
		parsedBlock := TxProposeBlock{}
		if err := json.Unmarshal([]byte(cjJson), &parsedBlock); err != nil {
			continue
		}
		if _, existed := se.recordFirstSeenProposal(slotHeight, requiredAuths[0], parsedBlock.SignedBlock.Block); !existed {
			seeded++
		}
	}
	return seeded
}

// slotHeightFromSlotKey extracts the slot height encoded as the decimal
// prefix of a slot-keyed in-memory map entry. Slot keys produced by the
// detector and slashForEvidenceIfPolicyAllows use the format
// "slotHeightDecimal|account"; this helper inverts that for prune logic.
// Returns 0 if the key is malformed (which trips the pruner into deleting
// the entry, since 0 is always <= the finalized slot threshold).
func slotHeightFromSlotKey(k string) uint64 {
	sep := strings.IndexByte(k, '|')
	if sep < 0 {
		return 0
	}
	h, err := strconv.ParseUint(k[:sep], 10, 64)
	if err != nil {
		return 0
	}
	return h
}

// pruneSafetySlotMaps drops entries from the per-slot in-memory maps
// whose slot is at or before finalizedSlotHeight. Called from the slot
// transition path so the maps stay bounded by the active slot horizon
// instead of growing for the lifetime of the validator process.
//
// Replay safety: nothing on the consensus path depends on a stale slot
// remaining in either map. seenProposalBySlotProposer is a first-seen-
// wins record of "did this account already propose for this slot?" and
// is irrelevant once the slot has finalized; slashIncidentBpsBySlotAccount
// is a correlated-cap accumulator scoped to a single incident. Re-entering
// either map for a finalized slot would only happen if we replayed an
// already-applied slash, in which case the deterministic ledger ids in
// SafetySlashConsensusBond upsert and converge regardless.
func (se *StateEngine) pruneSafetySlotMaps(finalizedSlotHeight uint64) {
	if se == nil {
		return
	}
	for k := range se.seenProposalBySlotProposer {
		if slotHeightFromSlotKey(k) <= finalizedSlotHeight {
			delete(se.seenProposalBySlotProposer, k)
		}
	}
	for k := range se.slashIncidentBpsBySlotAccount {
		if slotHeightFromSlotKey(k) <= finalizedSlotHeight {
			delete(se.slashIncidentBpsBySlotAccount, k)
		}
	}
}

// pruneSafetyEvidenceSeen drops entries from the dedup map that were
// recorded more than (2 * MaxSafetySlashBurnDelayBlocks) blocks before
// currentHeight. The window is intentionally conservative: a reversed
// slash plus a re-slash for the same evidence at the burn-delay boundary
// must remain dedup-protected, but anything beyond twice the maximum
// configurable delay cannot still be in flight.
//
// safetyEvidenceSeen is documented as a backstop; replay safety is
// guaranteed by deterministic ledger ids in SafetySlashConsensusBond +
// Mongo upsert semantics, so even a fully-empty map cannot cause a double
// debit. The prune is a memory hygiene measure.
func (se *StateEngine) pruneSafetyEvidenceSeen(currentHeight uint64) {
	if se == nil || se.safetyEvidenceSeen == nil {
		return
	}
	keepWindow := 2 * params.MaxSafetySlashBurnDelayBlocks
	if currentHeight <= keepWindow {
		return
	}
	threshold := currentHeight - keepWindow
	for k, h := range se.safetyEvidenceSeen {
		if h < threshold {
			delete(se.safetyEvidenceSeen, k)
		}
	}
}

// recordEvidenceAndShouldSlash returns true when this is the first time we
// have observed this (account, kind, evidenceID) tuple in the current process
// lifetime. Replay-safe even when this map is empty (e.g. after restart):
// SafetySlashConsensusBond derives deterministic ledger ids from the same
// inputs, and the underlying Mongo store upserts by id, so a redundant call
// rewrites the same row instead of double-debiting.
func (se *StateEngine) recordEvidenceAndShouldSlash(accountHive, kind, evidenceID string, blockHeight uint64) bool {
	if se == nil {
		return false
	}
	if se.safetyEvidenceSeen == nil {
		se.safetyEvidenceSeen = make(map[string]uint64)
	}
	acct := normalizeHiveAccount(accountHive)
	id := strings.TrimSpace(evidenceID)
	if acct == "" || strings.TrimSpace(kind) == "" || id == "" {
		// Reject blank account/kind/evidence id. Every detector passes a
		// non-empty, deterministic evidence id; a blank id is a programming
		// error, and falling back to the block height (the previous behavior)
		// would collapse distinct evidence at the same height into one dedup
		// entry. Refuse to slash rather than mask the bug.
		return false
	}
	seenKey := se.evidenceSeenKey(acct, kind, id)
	if seenH, ok := se.safetyEvidenceSeen[seenKey]; ok && seenH <= blockHeight {
		return false
	}
	se.safetyEvidenceSeen[seenKey] = blockHeight
	return true
}

// slashForEvidenceIfPolicyAllows is the single policy-enforcing entrypoint
// every detector calls. incidentKey, when non-empty, correlates multiple
// evidence kinds that arose from the same on-chain incident (today: the
// (slot, account) tuple for block-production faults). When the running total
// for an incident already exceeds CorrelatedSlashCapBps, the additional
// evidence is recorded but does not produce a further ledger debit.
func (se *StateEngine) slashForEvidenceIfPolicyAllows(
	accountHive, kind, evidenceID, txID string,
	blockHeight uint64,
	incidentKey string,
) ledgerSystem.LedgerResult {
	if se == nil {
		return ledgerSystem.LedgerResult{Ok: false, Msg: "state engine not configured"}
	}
	if !safetyslash.SafetySlashEnabled {
		// Principal slashing temporarily disabled (see safetyslash.SafetySlashEnabled).
		// Detectors still run and log; they just don't debit the consensus bond.
		return ledgerSystem.LedgerResult{Ok: false, Msg: "safety slashing disabled"}
	}
	if !se.recordEvidenceAndShouldSlash(accountHive, kind, evidenceID, blockHeight) {
		return ledgerSystem.LedgerResult{Ok: false, Msg: "duplicate evidence"}
	}
	rawBps := safetyslash.SlashBpsForEvidenceKind(kind)
	if rawBps <= 0 {
		return ledgerSystem.LedgerResult{Ok: false, Msg: "unsupported evidence kind"}
	}
	bps := rawBps
	if incidentKey != "" {
		if se.slashIncidentBpsBySlotAccount == nil {
			se.slashIncidentBpsBySlotAccount = make(map[string]int)
		}
		already := se.slashIncidentBpsBySlotAccount[incidentKey]
		// Effective bps after correlation: cap minus what's already been
		// applied for this incident, never exceeding the kind's own bps.
		room := safetyslash.CorrelatedSlashCapBps - already
		if room <= 0 {
			return ledgerSystem.LedgerResult{Ok: false, Msg: "incident already at correlated cap"}
		}
		if rawBps < room {
			bps = rawBps
		} else {
			bps = room
		}
		se.slashIncidentBpsBySlotAccount[incidentKey] = already + bps
	} else {
		// No incident grouping: still apply the per-evidence cap so a single
		// kind cannot exceed CorrelatedSlashCapBps on its own (defensive;
		// today both kinds' bps are well under the cap).
		bps = safetyslash.EffectiveCorrelatedBps(
			[]int{rawBps},
			safetyslash.CorrelatedSlashCapBps,
		)
	}
	if bps <= 0 {
		return ledgerSystem.LedgerResult{Ok: false, Msg: "no headroom under correlated cap"}
	}
	return se.applyPrincipalSlashForProvableEvidence(accountHive, bps, txID, blockHeight, kind)
}

func blamedAccountsFromBitSet(bitsetHex string, members []elections.ElectionMember) []string {
	bs := strings.TrimSpace(strings.TrimPrefix(bitsetHex, "0x"))
	if bs == "" || len(members) == 0 {
		return nil
	}
	raw, err := hex.DecodeString(bs)
	if err != nil || len(raw) == 0 {
		return nil
	}
	out := make([]string, 0)
	seen := make(map[string]struct{})
	for memberIdx, m := range members {
		byteIdx := memberIdx / 8
		bitIdx := uint(memberIdx % 8)
		if byteIdx >= len(raw) {
			break
		}
		if (raw[byteIdx] & (1 << bitIdx)) == 0 {
			continue
		}
		acct := normalizeHiveAccount(m.Account)
		if acct == "" {
			continue
		}
		if _, ok := seen[acct]; ok {
			continue
		}
		seen[acct] = struct{}{}
		out = append(out, acct)
	}
	return out
}

// enqueueSlashRestitutionClaimForTest registers remaining HIVE loss owed to a
// victim. Payouts are sourced from future safety-slash proceeds (FIFO) before
// protocol burn.
//
// CONSENSUS WARNING: this method is intentionally unexported. It only takes
// effect when the underlying allocator is the legacy
// safetyslash.MemoryRestitutionQueue — tests that pre-date the on-ledger
// queue swap that allocator in via SetSlashRestitutionForTest. In production
// the allocator is safetyslash.OnLedgerRestitutionAllocator and claims arrive
// via the vsc.restitution_claim block-content tx (LedgerSystem.EnqueueRestitutionClaim);
// this helper is a no-op in that wiring, which is the desired behaviour.
func (se *StateEngine) enqueueSlashRestitutionClaimForTest(c ledgerSystem.SlashRestitutionClaim) {
	if se == nil || se.slashRestitution == nil {
		return
	}
	if mq, ok := se.slashRestitution.(*safetyslash.MemoryRestitutionQueue); ok {
		mq.Enqueue(c)
	}
}

// SetSlashRestitutionForTest replaces the slash restitution allocator with
// a caller-provided implementation. Only intended for unit tests that need
// to assert behaviour against an in-memory queue rather than the on-ledger
// allocator. Production paths must stay on
// safetyslash.OnLedgerRestitutionAllocator (the wiring set in newStateEngine).
func (se *StateEngine) SetSlashRestitutionForTest(a ledgerSystem.SlashRestitutionAllocator) {
	if se == nil {
		return
	}
	se.slashRestitution = a
}
