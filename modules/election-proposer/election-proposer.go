package election_proposer

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/lib/hive"
	"vsc-node/lib/utils"
	"vsc-node/lib/vsclog"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	"vsc-node/modules/incentive-pendulum/rewards"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"
	libp2p "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
	blsu "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

var log = vsclog.Module("ep")

type electionProposer struct {
	conf  common.IdentityConfig
	sconf systemconfig.SystemConfig

	p2p     *libp2p.P2PServer
	service libp2p.PubSubService[p2pMessage]

	witnesses witnesses.Witnesses
	elections elections.Elections
	balanceDb ledgerDb.Balances
	vscBlocks vscBlocks.VscBlocks

	da *datalayer.DataLayer

	txCreator hive.HiveTransactionCreator

	hiveConsumer *blockconsumer.HiveConsumer
	se           *stateEngine.StateEngine
	bh           uint64
	headHeight   *uint64

	sigChannels map[uint64]chan *signResponse
	signingInfo *struct {
		epoch   uint64
		block   uint64
		cid     string
		circuit *dids.PartialBlsCircuit
	}

	electionMu sync.Mutex
	sigMu      sync.Mutex
	sigRunning bool

	// scoreMapMinSamples is the per-instance floor for the ban filter. See
	// the documentation on the (removed) package-level default for the why.
	// Resolved once in New() so the production default (500) is locked at
	// process start and cannot drift mid-run.
	scoreMapMinSamples uint64
}

type ElectionProposer = *electionProposer

var _ a.Plugin = &electionProposer{}

// TODO: Add a way to get the current consensus version
// TODO: Add a way to get the witness active score
func New(
	p2p *libp2p.P2PServer,
	witnesses witnesses.Witnesses,
	elections elections.Elections,
	vscBlocks vscBlocks.VscBlocks,
	balanceDb ledgerDb.Balances,
	da *datalayer.DataLayer,
	txCreator hive.HiveTransactionCreator,
	conf common.IdentityConfig,
	sconf systemconfig.SystemConfig,
	se *stateEngine.StateEngine,
	hiveConsumer *blockconsumer.HiveConsumer,
) ElectionProposer {
	return &electionProposer{
		conf:               conf,
		p2p:                p2p,
		witnesses:          witnesses,
		elections:          elections,
		vscBlocks:          vscBlocks,
		balanceDb:          balanceDb,
		da:                 da,
		txCreator:          txCreator,
		sconf:              sconf,
		se:                 se,
		hiveConsumer:       hiveConsumer,
		sigChannels:        make(map[uint64]chan *signResponse),
		scoreMapMinSamples: resolveScoreMapMinSamples(sconf),
	}
}

// resolveScoreMapMinSamples returns the per-instance ban-filter sample floor.
// Production default is 500 (about 25 elections at ElectionInterval=20).
// VSC_ELECTION_SCOREMAP_MIN_SAMPLES is honoured ONLY on devnet (NetId
// "vsc-devnet") so testnet/mainnet operators cannot accidentally diverge
// the consensus-critical threshold across nodes. Devnet tests rely on the
// override to activate the ban filter within a runnable test horizon.
func resolveScoreMapMinSamples(sconf systemconfig.SystemConfig) uint64 {
	const defaultMinSamples = uint64(500)
	if sconf == nil || sconf.NetId() != "vsc-devnet" {
		return defaultMinSamples
	}
	v := os.Getenv("VSC_ELECTION_SCOREMAP_MIN_SAMPLES")
	if v == "" {
		return defaultMinSamples
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return defaultMinSamples
	}
	return n
}

// Init implements aggregate.Plugin.
func (e *electionProposer) Init() error {
	e.hiveConsumer.RegisterBlockTick("election-proposer", e.blockTick, true)
	return nil
}

// Start implements aggregate.Plugin.
func (e *electionProposer) Start() *promise.Promise[any] {
	err := e.startP2P()
	if err != nil {
		return utils.PromiseReject[any](err)
	}
	return utils.PromiseResolve[any](nil)
}

// Stop implements aggregate.Plugin.
func (e *electionProposer) Stop() error {
	return e.stopP2P()
}

func (e *electionProposer) blockTick(bh uint64, headHeight *uint64) {
	e.electionMu.Lock()
	defer e.electionMu.Unlock()

	e.bh = bh
	e.headHeight = headHeight

	if e.canHold() {

		slotInfo := stateEngine.CalculateSlotInfo(bh)

		schedule := e.se.GetSchedule(slotInfo.StartHeight)

		//Select current slot as per consensus algorithm
		var witnessSlot *stateEngine.WitnessSlot
		for _, slot := range schedule {
			if slot.SlotHeight == slotInfo.StartHeight {
				witnessSlot = &slot
				break
			}
		}

		if witnessSlot != nil {
			if witnessSlot.Account == e.conf.Get().HiveUsername && bh%common.CONSENSUS_SPECS.SlotLength == 0 {
				e.HoldElection(bh)
			}
		}
	}
}

func (e *electionProposer) canHold() bool {
	if e.headHeight == nil {
		return false
	}
	if *e.headHeight > 50 && e.bh < *e.headHeight-50 {
		return false
	}

	electionInterval := e.sconf.ConsensusParams().ElectionInterval
	if e.bh < electionInterval {
		return false // Too early in chain for elections
	}

	result, _ := e.elections.GetElectionByHeight(e.bh)

	if result.BlockHeight >= e.bh-electionInterval {
		return false
	}

	// Pendulum settlement gate. `result.Epoch` is the active (closing) epoch
	// N at e.bh; this proposer is about to propose epoch N+1, which embeds
	// the settlement record for epoch N. Composing epoch N's record needs
	// epoch N-1 already settled (ComposeRecord's PrevEpoch must be N-1 for a
	// continuous chain), so the gate requires latestSettled >= N-1.
	//
	// Gating on N (the old behaviour) deadlocked: epoch N is only settled
	// once epoch N+1 lands, so "N settled before proposing N+1" can never
	// be satisfied. Genesis path: closing epoch 0/1 have no prior settlement
	// to gate on, and `result.Epoch >= 2` also guards the unsigned `- 1`.
	if result.Epoch >= 2 && e.se != nil {
		if e.se.GetLatestSettledEpoch() < result.Epoch-1 {
			return false
		}
	}

	return true
}

func (e *electionProposer) GenerateElection() (elections.ElectionHeader, elections.ElectionData, error) {
	// TODO: get latest block

	return e.GenerateElectionAtBlock(e.bh)
}

// Generates a raw election graph from local data
func (e *electionProposer) GenerateElectionAtBlock(
	blk uint64,
) (elections.ElectionHeader, elections.ElectionData, error) {
	witnesses, err := e.witnesses.GetWitnessesAtBlockHeight(blk, witnesses.EnabledOnly())
	if err != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}
	electionResult, err := e.elections.GetElectionByHeight(blk - 1)
	if err != nil && err != mongo.ErrNoDocuments {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}

	// TODO: Add a way to get the witness active score
	// const scoreChart = await this.self.witness.getWitnessActiveScore(blk)
	// scoreChart := map[string]uint64{}

	return e.GenerateFullElection(witnesses, electionResult.Epoch, electionResult.ProtocolVersion, blk)
}

const DEFAULT_NEW_NODE_WEIGHT = uint64(10)

var REQUIRED_ELECTION_MEMBERS = []string{
	// "vaultec.vsc",
} // TODO: Set this to a list of required election members

const VSC_ELECTION_TX_ID = "vsc.election_result"

// scoreMapMinSamples is the lower bound on (sum of) block-signing samples
// scoreMap must observe before its BannedNodes list is allowed to filter
// the next committee. Below this many samples — early epochs, or a node
// that's still catching up — the score threshold (75%) over a tiny
// denominator can mis-ban everyone, so we skip the filter and rely on
// later elections (with more history) to catch persistent delinquency.
//
// Resolved per-instance via resolveScoreMapMinSamples (see New). The default
// (500, about 25 elections at ElectionInterval=20) is fixed on testnet and
// mainnet; only devnet honours the VSC_ELECTION_SCOREMAP_MIN_SAMPLES override.

func (e *electionProposer) GenerateFullElection(
	witnessList []witnesses.Witness,
	previousEpoch uint64,
	consensusVersion uint64,
	blockHeight uint64,
) (elections.ElectionHeader, elections.ElectionData, error) {
	witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		return w.ProtocolVersion < consensusVersion
	})

	// ensure the list is in a deterministic order
	slices.SortFunc(
		witnessList,
		func(a witnesses.Witness, b witnesses.Witness) int {
			return strings.Compare(a.Account, b.Account)
		},
	)

	previousElection := e.elections.GetElection(previousEpoch)

	var etype string
	var firstElection bool
	if previousElection != nil {
		etype = previousElection.Type
	} else {
		etype = "initial"
		firstElection = true
	}

	// #24: apply the dead-letter BannedNodes filter from scoreMap. Without
	// this the per-witness score is computed but never read, so persistent
	// under-participators are still eligible for the next committee. We
	// skip on:
	//   - the genesis election (no prior history)
	//   - missing vscBlocks dependency (test harnesses)
	//   - early epochs where samples < scoreMapMinSamples (avoids the
	//     tiny-denominator misban issue when the chain has just started)
	//   - any error reading scoreMap (degrade to the no-filter behaviour
	//     rather than blocking election production on a transient DB read)
	//
	// Determinism note: scoreMap reads vscBlocks.GetBlocksByElection from
	// the LOCAL DB. Verifiers in HandleMessage rebuild the election header
	// via the same path, so divergent block indexing between proposer and
	// verifier yields divergent CIDs and the BLS aggregate fails (election
	// retries next slot — safety preserved, liveness affected). The
	// scoreMapMinSamples=500 floor caps the divergence window. A cleaner
	// long-term fix would have the proposer publish its BannedNodes set
	// alongside the election so verifiers re-derive against the proposed
	// input. Tracked as a follow-up.
	if !firstElection && e.vscBlocks != nil {
		sm, smErr := e.scoreMap()
		if smErr != nil {
			log.Warn("scoreMap failed; skipping ban filter", "err", smErr)
		} else if sm.Samples >= e.scoreMapMinSamples && len(sm.BannedNodes) > 0 {
			banned := make(map[string]bool, len(sm.BannedNodes))
			for _, n := range sm.BannedNodes {
				banned[n] = true
			}
			before := len(witnessList)
			witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
				if banned[w.Account] {
					log.Verbose("skipping banned witness", "account", w.Account, "samples", sm.Samples, "score", sm.Map[w.Account])
					return true
				}
				return false
			})
			if len(witnessList) < before {
				log.Info("bannedNodes filter applied", "removed", before-len(witnessList), "samples", sm.Samples)
			}
		}
	}

	stakedMap := map[string]uint64{}
	defaultWeightMap := map[string]uint64{}
	nodesWithStake := uint64(0)
	for _, w := range witnessList {
		// electionResult.
		// if etype == "initial" {
		// 	weightMap[w.Account] = 10
		// } else {
		// 	balRecord, _ := e.balanceDb.GetBalanceRecord("hive:"+w.Account, blockHeight)
		// 	weightMap[w.Account] = uint64(balRecord.HIVE_CONSENSUS)
		// }
		balRecord, err := e.balanceDb.GetBalanceRecord("hive:"+w.Account, blockHeight)
		if err != nil {
			return elections.ElectionHeader{}, elections.ElectionData{}, err
		}
		if balRecord != nil {
			if balRecord.HIVE_CONSENSUS >= e.sconf.ConsensusParams().MinStake {
				nodesWithStake++
				stakedMap[w.Account] = uint64(balRecord.HIVE_CONSENSUS)
			}
		}
		defaultWeightMap[w.Account] = DEFAULT_NEW_NODE_WEIGHT
	}

	var pType string
	weightMap := map[string]uint64{}
	if nodesWithStake >= uint64(e.sconf.ConsensusParams().MinMembers) || etype == "staked" {
		pType = "staked"
		weightMap = stakedMap
	} else {
		pType = "initial"
		weightMap = defaultWeightMap
	}

	witnessList = slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		weight, included := weightMap[w.Account]
		return !included || weight == 0
	})

	optionalNodes := slices.DeleteFunc(witnessList, func(w witnesses.Witness) bool {
		return slices.Contains(REQUIRED_ELECTION_MEMBERS, w.Account)
	})

	totalOptionalWeight := utils.Sum(
		utils.Map(optionalNodes, func(w witnesses.Witness) uint64 {
			return weightMap[w.Account]
		}),
	)

	distWeight := computeRequiredMemberWeight(totalOptionalWeight, uint64(len(REQUIRED_ELECTION_MEMBERS)))

	// review2 MEDIUM #66: a witness consensus key comes from untrusted L1
	// account data; a single malformed key must not panic (crash) every
	// proposer. Deterministically skip the witness instead — all nodes
	// read the same witness records, so dropping the same bad-key members
	// keeps the proposed election identical across the network.
	members := make([]elections.ElectionMember, 0, len(witnessList))
	for _, w := range witnessList {
		key, err := w.ConsensusKey()
		if err != nil {
			log.Warn("skipping witness with invalid consensus key", "account", w.Account, "err", err)
			continue
		}
		members = append(members, elections.ElectionMember{
			Key:     key.String(),
			Account: w.Account,
		})
	}

	weights := make([]uint64, len(members))
	for i, member := range members {
		if slices.Contains(REQUIRED_ELECTION_MEMBERS, member.Account) {
			weights[i] = distWeight
		} else {
			weights[i] = weightMap[member.Account]
		}
	}

	electionData := elections.ElectionData{}

	if firstElection {
		electionData.Epoch = 0
	} else {
		electionData.Epoch = previousEpoch + 1
	}
	electionData.Members = members
	electionData.NetId = e.sconf.NetId()
	electionData.ProtocolVersion = consensusVersion
	electionData.Weights = weights
	electionData.Type = pType

	// Pendulum settlement (inlined). Skipped on the genesis election because
	// there's no closing committee to settle. On any other epoch we compose
	// the settlement record for `previousEpoch` (the epoch this election is
	// rotating away from) against the same blockHeight the election itself
	// uses, so every signer's re-derive sees identical inputs. ComposeRecord
	// failure aborts the election attempt — the proposer returns the error
	// without broadcasting, and the next slot's leader retries.
	if !firstElection && e.se != nil && previousElection != nil && previousEpoch > 0 {
		settlementMembers := make([]string, 0, len(previousElection.Members))
		for _, m := range previousElection.Members {
			acct := m.Account
			if !strings.HasPrefix(acct, "hive:") {
				acct = "hive:" + acct
			}
			settlementMembers = append(settlementMembers, acct)
		}

		balanceReader := e.se.PendulumBalanceRecordReader()
		if balanceReader == nil {
			return elections.ElectionHeader{}, elections.ElectionData{},
				fmt.Errorf("pendulum settlement: balance reader unavailable")
		}
		bonds := pendulumsettlement.ReadCommitteeBonds(
			balanceReader,
			settlementMembers,
			previousElection.BlockHeight,
			blockHeight,
		)
		bucket := e.se.PendulumNodesBucketBalance(blockHeight)
		prevSettled := e.se.GetLatestSettledEpoch()
		tickInterval := e.se.PendulumOracleTickInterval()

		var reductions map[string]int
		if provider := e.se.PendulumEpochInputsProvider(); provider != nil {
			reductions = rewards.ComputeReductionsForEpoch(
				provider,
				previousElection.BlockHeight,
				blockHeight,
				tickInterval,
			)
		}

		rec, composeErr := pendulumsettlement.ComposeRecord(pendulumsettlement.ComposeInputs{
			Epoch:               previousEpoch,
			PrevEpoch:           prevSettled,
			EpochStartBh:        previousElection.BlockHeight,
			SlotHeight:          blockHeight,
			CommitteeBonds:      bonds,
			BucketBalanceHBD:    bucket,
			ReductionsByAccount: reductions,
		})
		if composeErr != nil {
			return elections.ElectionHeader{}, elections.ElectionData{},
				fmt.Errorf("pendulum settlement: compose failed: %w", composeErr)
		}
		if rec == nil {
			return elections.ElectionHeader{}, elections.ElectionData{},
				fmt.Errorf("pendulum settlement: compose returned nil record")
		}
		electionData.Settlement = rec
	}

	cid, err := electionData.Cid()
	if err != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}

	cborNode, _ := electionData.Node()
	ses := datalayer.NewSession(e.da)

	ses.Put(cborNode.RawData(), cid)
	ses.Commit()

	electionHeader := elections.ElectionHeader{}
	electionHeader.Data = cid.String()
	electionHeader.Epoch = electionData.Epoch
	electionHeader.NetId = electionData.NetId
	electionHeader.Type = electionData.Type

	return electionHeader, electionData, nil
}

type ElectionOptions struct {
	OverrideMinimumMemberCount int
}

func (ep *electionProposer) HoldElection(blk uint64, options ...ElectionOptions) error {
	if ep.signingInfo != nil {
		log.Warn("election already in progress; skipping new attempt", "block_height", blk)
		return errors.New("election already in progress")
	}

	electionResult, err := ep.elections.GetElectionByHeight(blk - 1)
	firstElection := false
	if err == mongo.ErrNoDocuments {
		firstElection = true
	} else if err != nil {
		log.Error("GetElectionByHeight failed", "block_height", blk, "err", err)
		return err
	}

	electionHeader, electionData, err := ep.GenerateElectionAtBlock(blk)

	if err != nil {
		log.Error("GenerateElectionAtBlock failed", "block_height", blk, "err", err)
		return err
	}

	if firstElection {
		electionResultJson := struct {
			elections.ElectionHeader
		}{
			electionHeader,
		}
		jsonBytes, err := json.Marshal(electionResultJson)
		if err != nil {
			log.Error(
				"marshal first-election broadcast failed",
				"block_height",
				blk,
				"epoch",
				electionHeader.Epoch,
				"err",
				err,
			)
			return err
		}

		op := ep.txCreator.CustomJson(
			[]string{ep.conf.Get().HiveUsername},
			[]string{},
			VSC_ELECTION_TX_ID,
			string(jsonBytes),
		)

		tx := ep.txCreator.MakeTransaction([]hivego.HiveOperation{op})

		ep.txCreator.PopulateSigningProps(&tx, nil)

		sig, err := ep.txCreator.Sign(tx)
		if err != nil {
			log.Error("sign first-election tx failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
			return fmt.Errorf("failed to update account: %w", err)
		}

		tx.AddSig(sig)

		txId, err := ep.txCreator.Broadcast(tx)
		if err != nil {
			return err
		}

		log.Info("first election proposed", "block_height", blk, "epoch", electionHeader.Epoch, "tx_id", txId)

		return nil
	} else {

		// console.log("electionData - holding election", electionData)

		var minimumMemberCount int
		if len(options) > 0 {
			minimumMemberCount = options[0].OverrideMinimumMemberCount
		} else {
			minimumMemberCount = ep.sconf.ConsensusParams().MinMembers
		}

		if len(electionData.Members) < minimumMemberCount {
			log.Warn("election minimum member count not met",
				"block_height", blk,
				"epoch", electionHeader.Epoch,
				"members", len(electionData.Members),
				"minimum", minimumMemberCount)
			return errors.New("Minimum network config not met for election. Skipping.")
		}

		cid, err := electionHeader.Cid()
		if err != nil {
			log.Error("compute election header CID failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
			return err
		}

		memberAccounts := make([]string, 0, len(electionData.Members))
		for _, m := range electionData.Members {
			memberAccounts = append(memberAccounts, m.Account)
		}
		log.Verbose("election header built",
			"block_height", blk,
			"epoch", electionHeader.Epoch,
			"cid", cid.String(),
			"data_cid", electionHeader.Data,
			"member_count", len(electionData.Members),
			"members", strings.Join(memberAccounts, ","))

		var memberKeys []dids.BlsDID
		if firstElection {
			w, err := ep.witnesses.GetWitnessesAtBlockHeight(blk)

			if err != nil {
				log.Error("GetWitnessesAtBlockHeight failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
				return err
			}
			res := resultJoin(utils.Map(w, func(w witnesses.Witness) result.Result[dids.BlsDID] {
				return resultWrap(w.ConsensusKey())
			})...)

			if res.IsErr() {
				return res.UnwrapErr()
			}
			memberKeys = res.Unwrap()
		} else {
			memberKeys = electionResult.MemberKeys()
		}

		circuit, err := dids.NewBlsCircuitGenerator(memberKeys).Generate(cid)
		if err != nil {
			log.Error("generate BLS circuit failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
			return err
		}

		ep.signingInfo = &struct {
			epoch   uint64
			block   uint64
			cid     string
			circuit *dids.PartialBlsCircuit
		}{
			epoch:   electionHeader.Epoch,
			block:   blk,
			cid:     cid.String(),
			circuit: &circuit,
		}

		// sig, err := signCid(ep.conf, cid)

		// if err != nil {
		// 	return err
		// }

		signReq := signRequest{
			Epoch:       electionHeader.Epoch,
			BlockHeight: blk,
		}

		reqJson, _ := json.Marshal(signReq)
		go func() {
			time.Sleep(4 * time.Millisecond)
			ep.service.Send(p2pMessage{
				Type: "sign_request",
				Op:   "hold_election",
				Data: string(reqJson),
			})
		}()

		ep.sigChannels[ep.signingInfo.epoch] = make(chan *signResponse)

		log.Info("waiting for signatures",
			"block_height", blk,
			"epoch", electionHeader.Epoch,
			"timeout", 30*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		signedWeight, err := ep.waitForSigs(ctx, &electionResult)
		delete(ep.sigChannels, ep.signingInfo.epoch)

		if err != nil {
			return err
		}

		log.Info("signature collection complete",
			"block_height", blk,
			"epoch", electionHeader.Epoch,
			"signed_weight", signedWeight)
		ep.signingInfo = nil

		finalCircuit, err := circuit.Finalize()
		if err != nil {
			log.Error("finalize BLS circuit failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
			return err
		}

		bv := finalCircuit.RawBitVector()
		votedWeight := uint64(0)
		totalWeight := uint64(0)
		for i := 0; i < bv.BitLen(); i++ {
			if bv.Bit(i) == 1 {
				votedWeight += electionResult.Weights[i]
			}
			totalWeight += electionResult.Weights[i]
		}

		blocksSinceLastElection := blk
		if !firstElection {
			blocksSinceLastElection = blk - electionResult.BlockHeight
		}
		voteMajority := elections.MinimalRequiredElectionVotes(blocksSinceLastElection, totalWeight)

		if (votedWeight >= voteMajority) || firstElection {
			// Send out Election to Hive

			circuit, err := finalCircuit.Serialize()
			if err != nil {
				log.Error("serialize BLS circuit failed", "block_height", blk, "epoch", electionHeader.Epoch, "err", err)
				return err
			}

			electionResultJson := struct {
				elections.ElectionHeader
				Signature dids.SerializedCircuit `json:"signature"`
			}{
				electionHeader,
				*circuit,
			}

			jsonBytes, err := json.Marshal(electionResultJson)
			if err != nil {
				return err
			}

			op := ep.txCreator.CustomJson([]string{ep.conf.Get().HiveUsername}, []string{}, VSC_ELECTION_TX_ID, string(jsonBytes))

			tx := ep.txCreator.MakeTransaction([]hivego.HiveOperation{op})

			ep.txCreator.PopulateSigningProps(&tx, nil)

			sig, err := ep.txCreator.Sign(tx)
			if err != nil {
				return fmt.Errorf("failed to update account: %w", err)
			}

			tx.AddSig(sig)

			_, err = ep.txCreator.Broadcast(tx)

			if err != nil {
				return fmt.Errorf("failed to update account: %w", err)
			}
		}
		return nil
	}
}

type ScoreMap struct {
	Map         map[string]uint64
	BannedNodes []string
	Members     []string
	Samples     uint64
}

func (ep *electionProposer) scoreMap() (ScoreMap, error) {
	const electionCount = 4

	// #24: must be 0-length slice with cap N, NOT length-N. The old form
	// pre-pended 4 zero-valued ElectionResults, so the loop below called
	// GetBlocksByElection(0) four extra times and inflated `samples` by
	// 4× the count of genesis blocks. Bans were dead code at the time so
	// it didn't matter; now that committee selection reads BannedNodes
	// it would over-ban witnesses against a fictionally large denominator.
	elections := make([]elections.ElectionResult, 0, electionCount)
	election, err := ep.elections.GetElectionByHeight(math.MaxInt64)
	if err != nil {
		return ScoreMap{}, err
	}

	elections = append(elections, election)

	for i := uint64(1); i < electionCount; i++ {
		// Guard against uint64 underflow on early chains where the latest
		// epoch is < i. Without this, election.Epoch - i wraps to a very
		// large number, GetElection returns nil, and the loop breaks
		// anyway — but only by accident. Be explicit.
		if election.Epoch < i {
			break
		}

		prevElection := ep.elections.GetElection(election.Epoch - i)

		if prevElection == nil {
			break
		}
		elections = append(elections, *prevElection)

	}
	samples := uint64(0)
	witnessMap := map[string]bool{}
	scoreMap := map[string]uint64{}

	for _, election := range elections {
		blocks, err := ep.vscBlocks.GetBlocksByElection(election.Epoch)
		if err != nil {
			return ScoreMap{}, err
		}

		for _, block := range blocks {
			for _, member := range block.Signers {
				scoreMap[member] += 1
			}
		}
		for _, mbr := range election.Members {
			witnessMap[mbr.Account] = true
		}
		samples += uint64(len(blocks))
	}

	// review4 HIGH #40: sort before iterating so Members and BannedNodes are
	// deterministic. Map iteration order is randomised by Go; if this slice
	// is ever consumed by code that depends on order (BLS bitset, slicing
	// at quorum cutoff, tie-break), divergent ordering would break
	// consensus across honest nodes.
	members := make([]string, 0, len(witnessMap))
	for member := range witnessMap {
		members = append(members, member)
	}
	sort.Strings(members)

	bannedNodes := []string{}
	for _, member := range members {
		if scoreMap[member] < samples*75/100 {
			bannedNodes = append(bannedNodes, member)
		}
	}

	return ScoreMap{
		Map:         scoreMap,
		Members:     members,
		Samples:     samples,
		BannedNodes: bannedNodes,
	}, nil
}

func (ep *electionProposer) makeElection(blk uint64) (elections.ElectionHeader, elections.ElectionData, error) {
	electionResult, err := ep.elections.GetElectionByHeight(blk - 1)

	if err != nil {
		return elections.ElectionHeader{}, elections.ElectionData{}, err
	}

	if blk-electionResult.BlockHeight < ep.sconf.ConsensusParams().ElectionInterval {
		return elections.ElectionHeader{}, elections.ElectionData{}, errors.New("next election not ready")
	}
	electionHeader, electionData, err := ep.GenerateElectionAtBlock(blk)

	return electionHeader, electionData, err
}

func (ep *electionProposer) waitForSigs(ctx context.Context, election *elections.ElectionResult) (uint64, error) {
	if ep.signingInfo == nil {
		return 0, errors.New("no block signing info")
	}

	ep.sigMu.Lock()
	if ep.sigRunning {
		ep.sigMu.Unlock()
		return 0, errors.New("waitForSigs already running")
	}
	ep.sigRunning = true
	ep.sigMu.Unlock()
	defer func() {
		ep.sigMu.Lock()
		ep.sigRunning = false
		ep.sigMu.Unlock()
	}()

	weightTotal := uint64(0)
	for _, weight := range election.Weights {
		weightTotal += weight
	}

	end := make(chan struct{})

	// Capture references before goroutine starts so the goroutine
	// does not need to access ep.signingInfo (which may become nil on timeout).
	sigChan := ep.sigChannels[ep.signingInfo.epoch]
	circuit := ep.signingInfo.circuit
	block := ep.signingInfo.block
	epoch := ep.signingInfo.epoch
	expectedCid := ep.signingInfo.cid
	weightRequired := weightTotal * 8 / 10

	var err error
	var signedWeight uint64
	go func() {
		signedWeight = 0

		for signedWeight < weightRequired {
			var signResp *signResponse
			select {
			case <-ctx.Done():
				log.Verbose("signature collector exiting",
					"block_height", block,
					"epoch", epoch,
					"reason", ctx.Err(),
					"signed_weight", signedWeight,
					"weight_required", weightRequired)
				end <- struct{}{}
				return
			case signResp = <-sigChan:
			}

			if signResp == nil {
				log.Warn("signature channel closed before quorum",
					"block_height", block,
					"epoch", epoch,
					"signed_weight", signedWeight,
					"weight_required", weightRequired)
				break
			}

			sigBytes, err := base64.URLEncoding.DecodeString(signResp.Sig)

			if err != nil {
				continue
			}

			sig := blsu.Signature{}
			sig96 := [96]byte{}
			copy(sig96[:], sigBytes[:])
			err = sig.Deserialize(&sig96)

			if err != nil {
				continue
			}

			sigStr := signResp.Sig
			account := signResp.Account

			var member dids.Member
			var index int
			for i, data := range election.Members {
				if data.Account == account {
					member = dids.BlsDID(data.Key)
					index = i
					break
				}
			}

			c := *circuit

			added, err := c.AddAndVerify(member, sigStr)

			if err != nil {
				log.Warn("signature aggregate failed",
					"block_height", block,
					"epoch", epoch,
					"account", account,
					"err", err)
			} else if added {
				signedWeight += election.Weights[index]
				log.Verbose("signature aggregated",
					"block_height", block,
					"epoch", epoch,
					"account", account,
					"added_weight", election.Weights[index],
					"signed_weight", signedWeight,
					"weight_required", weightRequired)
			} else {
				log.Warn("signature ignored",
					"block_height", block,
					"epoch", epoch,
					"account", account,
					"expected_cid", expectedCid,
					"expected_msg_hex", hex.EncodeToString(c.Msg().Bytes()),
					"received_sig", sigStr,
					"reason", "already aggregated or did not verify")
			}
		}
		log.Verbose("signature collector reached quorum",
			"block_height", block,
			"epoch", epoch,
			"signed_weight", signedWeight,
			"weight_required", weightRequired)
		end <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		log.Warn("signature collection timeout",
			"block_height", block,
			"epoch", epoch,
			"reason", ctx.Err(),
			"signed_weight", signedWeight,
			"weight_required", weightRequired)
		<-end // Wait for goroutine to finish before returning
		return signedWeight, nil
	case <-end:
		return signedWeight, err
	}
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}

func resultJoin[T any](results ...result.Result[T]) (res result.Result[[]T]) {
	for _, r := range results {
		if r.IsOk() {
			res = result.Ok(append(res.Unwrap(), r.Unwrap()))
		} else {
			return result.Map(r, func(T) []T {
				return nil
			})
		}
	}
	return res
}
