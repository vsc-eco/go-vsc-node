package tss

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/hive"
	"vsc-node/lib/utils"
	"vsc-node/lib/vsclog"
	"vsc-node/modules/common"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/witnesses"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	libp2p "vsc-node/modules/p2p"
	tss_helpers "vsc-node/modules/tss/helpers"

	stateEngine "vsc-node/modules/state-processing"

	ecKeyGen "github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
	btss "github.com/bnb-chain/tss-lib/v2/tss"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multicodec"
	"github.com/vsc-eco/hivego"

	"github.com/chebyrash/promise"
	gorpc "github.com/libp2p/go-libp2p-gorpc"

	flatfs "github.com/ipfs/go-ds-flatfs"
)

var log = vsclog.Module("tss")

const (
	TSS_SIGN_INTERVAL           = 50            // 50 L1 blocks
	TSS_ROTATE_INTERVAL         = 20 * 5        // 5 minutes in L1 blocks
	TSS_MESSAGE_RETRY_COUNT     = 3             // Number of retries for failed messages
	TSS_BAN_THRESHOLD_PERCENT   = 60            // Failure rate threshold for bans
	TSS_BAN_GRACE_PERIOD_EPOCHS = 3             // Epochs before new nodes can be banned (as int for comparison)
	MAX_RESHARE_RETRIES         = 3             // Maximum number of reshare retries on timeout
	BLAME_EXPIRE                = uint64(28800) // 24 hour blame
	TSS_BLAME_EPOCH_COUNT       = (4 * 7) - 1   // Number of past epochs to include in blame scoring
)

type TssManager struct {
	p2p    *libp2p.P2PServer
	pubsub libp2p.PubSubService[p2pMessage]
	server *gorpc.Server
	client *gorpc.Client

	sigChannels map[string]chan sigMsg

	tssRequests    tss_db.TssRequests
	tssKeys        tss_db.TssKeys
	tssCommitments tss_db.TssCommitments

	keyStore *flatfs.Datastore

	//Generates a fresh set of local key params.
	//A new set of fresh pre params will be available after depletion
	preParams chan ecKeyGen.LocalPreParams

	witnessDb  witnesses.Witnesses
	electionDb elections.Elections
	config     common.IdentityConfig
	sconf      systemconfig.SystemConfig
	VStream    *blockconsumer.HiveConsumer
	scheduler  GetScheduler
	hiveClient hive.HiveTransactionCreator

	//Active list of actions occurring
	queuedActions []QueuedAction
	lock          sync.Mutex
	preParamsLock sync.Mutex

	actionMap      map[string]Dispatcher
	sessionMap     map[string]sessionInfo
	sessionResults map[string]sessionResultEntry

	// Message buffer for early-arriving messages before dispatcher registration.
	// Priority queue keyed by block height for O(1) eviction of stale sessions.
	messageBuffer *sessionBuffer
	bufferLock    sync.RWMutex

	// Retry count for reshare operations (in-memory to avoid DB dependency)
	retryCounts   map[string]int
	retryCountsMu sync.Mutex

	// Last block height seen by BlockTick, used for session admission filtering
	lastBlockHeight atomic.Uint64

	// Metrics for observability
	metrics *Metrics
}

// getRetryCount returns the retry count for a given key ID
func (tssMgr *TssManager) getRetryCount(keyId string) int {
	tssMgr.retryCountsMu.Lock()
	defer tssMgr.retryCountsMu.Unlock()
	return tssMgr.retryCounts[keyId]
}

// incrementRetryCount increments the retry count for a given key ID
func (tssMgr *TssManager) incrementRetryCount(keyId string) {
	tssMgr.retryCountsMu.Lock()
	defer tssMgr.retryCountsMu.Unlock()
	tssMgr.retryCounts[keyId]++
}

// ClearQueuedActions clears any pending retry actions. Used by tests to prevent
// stale retries from previous phases from interfering with later phases.
func (tssMgr *TssManager) ClearQueuedActions() {
	tssMgr.bufferLock.Lock()
	tssMgr.queuedActions = tssMgr.queuedActions[:0]
	tssMgr.bufferLock.Unlock()
	tssMgr.retryCountsMu.Lock()
	tssMgr.retryCounts = make(map[string]int)
	tssMgr.retryCountsMu.Unlock()
}

type bufferedMessage struct {
	Data    []byte
	From    string
	IsBrcst bool
	Cmt     string
	CmtFrom string
	Time    time.Time
}

func (tssMgr *TssManager) Receive() {}

func (tssMgr *TssManager) GeneratePreParams() {
	locked := tssMgr.preParamsLock.TryLock()
	if locked {
		if len(tssMgr.preParams) == 0 {
			log.Info("need to generate preparams")
			preParams, err := ecKeyGen.GeneratePreParams(time.Minute)
			if err == nil {
				tssMgr.preParams <- *preParams
			}
		}
		tssMgr.preParamsLock.Unlock()
	}
}

func (tssMgr *TssManager) BlockTick(bh uint64, headHeight *uint64) {
	tssMgr.lastBlockHeight.Store(bh)

	//First check if we are in sync or not
	if bh < *headHeight-20 {
		return
	}

	if tssMgr.sconf.ConsensusParams().TssIndexHeight > bh {
		return
	}

	slotInfo := stateEngine.CalculateSlotInfo(bh)

	schedule := tssMgr.scheduler.GetSchedule(slotInfo.StartHeight)

	var witnessSlot *stateEngine.WitnessSlot
	for _, slot := range schedule {
		if slot.SlotHeight == slotInfo.StartHeight {
			witnessSlot = &slot
			break
		}
	}

	// tssMgr.activeActions
	if witnessSlot != nil {
		isLeader := witnessSlot.Account == tssMgr.config.Get().HiveUsername

		keyLocks := make(map[string]bool)
		generatedActions := make([]QueuedAction, 0)
		if bh%TSS_ROTATE_INTERVAL == 0 {

			electionData, err := tssMgr.electionDb.GetElectionByHeight(bh)
			if err != nil || electionData.Members == nil {
				log.Warn("election data missing, skipping rotate", "blockHeight", bh, "err", err)
				return
			}

			epoch := electionData.Epoch
			reshareKeys, _ := tssMgr.tssKeys.FindEpochKeys(epoch)

			for _, key := range reshareKeys {
				generatedActions = append(generatedActions, QueuedAction{
					Type:  ReshareAction,
					KeyId: key.Id,
					Algo:  tss_helpers.SigningAlgo(key.Algo),
				})
				keyLocks[key.Id] = true
			}
			newKeys, _ := tssMgr.tssKeys.FindNewKeys(bh)

			for _, key := range newKeys {
				generatedActions = append(generatedActions, QueuedAction{
					Type:  KeyGenAction,
					KeyId: key.Id,
					Algo:  tss_helpers.SigningAlgo(key.Algo),
				})
				keyLocks[key.Id] = true
			}
		}
		if bh%TSS_SIGN_INTERVAL == 0 {
			signingRequests, _ := tssMgr.tssRequests.FindUnsignedRequests(bh)

			for _, signReq := range signingRequests {
				keyInfo, _ := tssMgr.tssKeys.FindKey(signReq.KeyId)
				if keyInfo.Status != tss_db.TssKeyActive {
					log.Warn(
						"signing attempted for non-active key, skipping",
						"keyId",
						keyInfo.Id,
						"status",
						keyInfo.Status,
					)
					continue
				}
				if !keyLocks[signReq.KeyId] {
					rawMsg, err := hex.DecodeString(signReq.Msg)
					if err == nil {
						generatedActions = append(generatedActions, QueuedAction{
							Type:  SignAction,
							KeyId: signReq.KeyId,
							Args:  rawMsg,
							Algo:  tss_helpers.SigningAlgo(keyInfo.Algo),
						})
					}
				}
			}
		}
		// Consume any recovery-scheduled actions (e.g., reshare retries after timeouts).
		// Skip queued actions whose key is already locked by a fresh action in this block
		// (the fresh reshare takes priority). Queued reshares that survive filtering also
		// populate keyLocks so they block signing, matching normal reshare/sign precedence.
		tssMgr.bufferLock.Lock()
		if len(tssMgr.queuedActions) > 0 {
			for _, qa := range tssMgr.queuedActions {
				if keyLocks[qa.KeyId] {
					log.Verbose("dropping queued action, key already locked by current block",
						"type", qa.Type, "keyId", qa.KeyId, "blockHeight", bh)
					continue
				}
				generatedActions = append(generatedActions, qa)
				if qa.Type == ReshareAction || qa.Type == KeyGenAction {
					keyLocks[qa.KeyId] = true
				}
			}
			tssMgr.queuedActions = tssMgr.queuedActions[:0]
		}
		tssMgr.bufferLock.Unlock()
		if len(generatedActions) > 0 {
			tssMgr.RunActions(generatedActions, witnessSlot.Account, isLeader, bh)
		}
	}

	// Keystore cleanup: delete flatfs entries for newly retired keys.
	if tss_db.KeyRetirementEnabled {
		if retiredKeys, err := tssMgr.tssKeys.FindNewlyRetired(bh); err == nil {
			for _, key := range retiredKeys {
				dsKey := makeKey("key", key.Id, int(key.Epoch))
				if delErr := tssMgr.keyStore.Delete(context.Background(), dsKey); delErr != nil {
					log.Error("keystore delete failed", "keyId", key.Id, "epoch", key.Epoch, "err", delErr)
				} else {
					log.Info("keystore deleted (retired)", "keyId", key.Id, "epoch", key.Epoch)
				}
			}
		}
	}
}

type score struct {
	Account          string
	Score            int
	Weight           int
	FirstEpoch       uint64
	EpochsSinceFirst uint64
}

type ScoreMap struct {
	BannedNodes map[string]bool
}

func (tss *TssManager) BlameScore() ScoreMap {
	weightMap := make(map[string]int)
	nodeFirstEpoch := make(map[string]uint64) // Track first epoch each node appeared

	initialElection, err := tss.electionDb.GetElectionByHeight(math.MaxInt64 - 1)
	if err != nil || initialElection.Members == nil {
		return ScoreMap{BannedNodes: make(map[string]bool)}
	}

	// Build set of current election members — only these will be scored
	currentMembers := make(map[string]bool, len(initialElection.Members))
	for _, member := range initialElection.Members {
		currentMembers[member.Account] = true
	}

	electionMap := make(map[uint64]elections.ElectionResult, 0)
	electionMap[initialElection.Epoch] = initialElection

	previousElections := tss.electionDb.GetPreviousElections(initialElection.Epoch, TSS_BLAME_EPOCH_COUNT)
	for _, election := range previousElections {
		electionMap[election.Epoch] = election

		// Track first appearance of each node (current members only)
		for _, member := range election.Members {
			if currentMembers[member.Account] {
				if _, exists := nodeFirstEpoch[member.Account]; !exists {
					nodeFirstEpoch[member.Account] = election.Epoch
				}
			}
		}
	}

	currentEpoch := initialElection.Epoch
	blameMap := make(map[string]int)
	timeoutBlameMap := make(map[string]int) // Separate tracking for timeouts vs errors
	errorBlameMap := make(map[string]int)

	for _, election := range electionMap {
		blames, _ := tss.tssCommitments.GetBlames(&election.Epoch)
		for _, member := range election.Members {
			if currentMembers[member.Account] {
				weightMap[member.Account] += len(blames)
			}
		}

		for _, blame := range blames {
			bv := big.NewInt(0)
			blameBytes, _ := base64.RawURLEncoding.DecodeString(blame.Commitment)
			bv = bv.SetBytes(blameBytes)

			// Determine if this is a timeout or error based on metadata
			isTimeout := false
			if blame.Metadata != nil && blame.Metadata.Error != nil {
				// Check if error indicates timeout
				errMsg := *blame.Metadata.Error
				isTimeout = len(errMsg) > 0 && (errMsg == "timeout" || errMsg == "TIMEOUT")
			}

			for idx, member := range election.Members {
				if bv.Bit(idx) == 1 && currentMembers[member.Account] {
					blameMap[member.Account] += 1
					tss.metrics.IncrementBlameCount(member.Account)
					if isTimeout {
						timeoutBlameMap[member.Account] += 1
					} else {
						errorBlameMap[member.Account] += 1
					}
				}
			}
		}
	}

	sortedArray := make([]score, 0)
	for account, weight := range weightMap {
		firstEpoch, exists := nodeFirstEpoch[account]
		epochsSinceFirst := uint64(0)
		if exists && currentEpoch >= firstEpoch {
			epochsSinceFirst = currentEpoch - firstEpoch
		}

		sortedArray = append(sortedArray, score{
			Account:          account,
			Score:            blameMap[account],
			Weight:           weight,
			FirstEpoch:       firstEpoch,
			EpochsSinceFirst: epochsSinceFirst,
		})
	}

	slices.SortFunc(sortedArray, func(a, b score) int {
		return b.Score - a.Score
	})

	bannedNodes := make(map[string]bool)
	bannedList := make([]string, 0)
	gracePeriodExemptions := make([]string, 0)

	for _, entry := range sortedArray {
		// Check grace period for new nodes
		gracePeriod := uint64(TSS_BAN_GRACE_PERIOD_EPOCHS)
		if entry.EpochsSinceFirst < gracePeriod {
			gracePeriodExemptions = append(gracePeriodExemptions, entry.Account)
			log.Verbose(
				"node in grace period, exempt from ban",
				"account",
				entry.Account,
				"score",
				entry.Score,
				"weight",
				entry.Weight,
				"epochsSinceFirst",
				entry.EpochsSinceFirst,
				"gracePeriod",
				gracePeriod,
			)
			continue
		}

		// Use configurable threshold instead of hardcoded 25%
		thresholdPercent := TSS_BAN_THRESHOLD_PERCENT
		failureRate := float64(entry.Score) / float64(entry.Weight) * 100.0

		if entry.Weight > 0 && entry.Score > entry.Weight*thresholdPercent/100 {
			bannedNodes[entry.Account] = true
			bannedList = append(bannedList, entry.Account)

			timeoutCount := timeoutBlameMap[entry.Account]
			errorCount := errorBlameMap[entry.Account]

			log.Verbose(
				"node banned",
				"account",
				entry.Account,
				"score",
				entry.Score,
				"weight",
				entry.Weight,
				"failureRate",
				failureRate,
				"threshold",
				thresholdPercent,
				"timeoutBlames",
				timeoutCount,
				"errorBlames",
				errorCount,
				"epochsSinceFirst",
				entry.EpochsSinceFirst,
			)
		} else {
			log.Verbose("node not banned", "account", entry.Account, "score", entry.Score, "weight", entry.Weight, "failureRate", failureRate, "threshold", thresholdPercent)
		}
	}

	if len(bannedList) > 0 {
		log.Verbose(
			"ban summary",
			"totalBanned",
			len(bannedList),
			"bannedNodes",
			bannedList,
			"gracePeriodExemptions",
			len(gracePeriodExemptions),
		)
	} else {
		log.Verbose("ban summary, no nodes banned", "gracePeriodExemptions", len(gracePeriodExemptions))
	}

	return ScoreMap{
		BannedNodes: bannedNodes,
	}
}

//Call process
// - Action is added to the queued list
// - Block Tick interval is triggered on 40 block (2 minute) intervals
// - Top 10-15 actions in the queue is executed via RunActions
// - Dispatcher instance is created; Handling p2p translation and others
// - Application specific instance is created (i.e ed25519, secp256k1, etc)

func (tssMgr *TssManager) RunActions(actions []QueuedAction, leader string, isLeader bool, bh uint64) {
	locked := tssMgr.lock.TryLock()

	log.Trace(
		"RunActions called",
		"account",
		tssMgr.config.Get().HiveUsername,
		"blockHeight",
		bh,
		"isLeader",
		isLeader,
		"locked",
		locked,
	)
	if !locked {
		log.Verbose("RunActions skipped, lock held by previous batch", "blockHeight", bh)
		return
	}

	currentElection, err := tssMgr.electionDb.GetElectionByHeight(uint64(bh))
	log.Trace("election lookup result", "election", currentElection, "err", err)
	if err != nil || currentElection.Members == nil {
		if err != nil {
			log.Trace("election lookup error", "err", err)
		} else {
			log.Warn("election data missing, skipping actions", "blockHeight", bh)
		}
		tssMgr.lock.Unlock()
		return
	}

	blameMap := tssMgr.BlameScore()

	log.Info(
		"running actions",
		"blockHeight",
		bh,
		"isLeader",
		isLeader,
		"actionCount",
		len(actions),
		"bannedNodes",
		len(blameMap.BannedNodes),
	)

	dispatchers := make([]Dispatcher, 0)
	for idx, action := range actions {

		var sessionId string
		if action.Type == KeyGenAction {
			participants := make([]Participant, 0)

			sessionId = "keygen-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId
			lastBlame, err := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "blame")

			var isBlame bool
			bitset := big.NewInt(0)
			if err == nil {
				if int64(lastBlame.BlockHeight) > int64(bh)-int64(BLAME_EXPIRE) {
					bitBytes, _ := base64.RawURLEncoding.DecodeString(lastBlame.Commitment)
					bitset.SetBytes(bitBytes)
					isBlame = true
				}
			}

			excludedAccounts := make([]string, 0)
			for idx, member := range currentElection.Members {
				if isBlame {
					if bitset.Bit(idx) == 1 {
						excludedAccounts = append(excludedAccounts, member.Account+" (blamed)")
						continue
					}
				}
				//if node is banned
				if blameMap.BannedNodes[member.Account] {
					excludedAccounts = append(excludedAccounts, member.Account+" (banned)")
					continue
				}
				participants = append(participants, Participant{
					Account: member.Account,
				})
			}

			participantAccounts := make([]string, 0)
			for _, p := range participants {
				participantAccounts = append(participantAccounts, p.Account)
			}
			log.Verbose(
				"creating keygen session",
				"sessionId",
				sessionId,
				"keyId",
				action.KeyId,
				"epoch",
				currentElection.Epoch,
				"blockHeight",
				bh,
				"participants",
				participantAccounts,
				"excluded",
				excludedAccounts,
				"hasBlame",
				isBlame,
				"lastBlameHeight",
				lastBlame.BlockHeight,
			)

			if len(participants) < 2 {
				log.Warn(
					"insufficient participants for keygen, minimum 2 required",
					"sessionId",
					sessionId,
					"participants",
					len(participants),
				)
				continue
			}

			dispatcher := &KeyGenDispatcher{
				BaseDispatcher: BaseDispatcher{
					startLock:    sync.Mutex{},
					tssMgr:       tssMgr,
					participants: participants,
					p2pMsg:       make(chan btss.Message, 2*len(participants)),
					sessionId:    sessionId,
					done:         make(chan struct{}),

					keyId: action.KeyId,
					algo:  action.Algo,

					epoch: currentElection.Epoch,
				},
			}
			dispatcher.msgCtx, dispatcher.cancelMsgs = context.WithCancel(context.Background())
			dispatcher.startLock.TryLock()

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.bufferLock.Lock()
			tssMgr.actionMap[sessionId] = dispatcher
			tssMgr.bufferLock.Unlock()

		} else if action.Type == SignAction {
			sessionId = "sign-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId

			commitment, err := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "reshare", "keygen")

			bv := big.NewInt(0)

			if err == nil {
				commitmentBytes, _ := base64.RawURLEncoding.DecodeString(commitment.Commitment)
				bv = bv.SetBytes(commitmentBytes)
			}

			keyInfo, _ := tssMgr.tssKeys.FindKey(action.KeyId)

			participants := make([]Participant, 0)

			// Fix 4: Use the commitment's epoch election for bitset mapping,
			// not currentElection which may have different members/order.
			commitElection := tssMgr.electionDb.GetElection(commitment.Epoch)
			if commitElection == nil || commitElection.Members == nil {
				log.Warn("cannot find commit election", "epoch", commitElection.Epoch)
				continue
			}
			for midx, member := range commitElection.Members {
				if bv.Bit(midx) == 1 {
					participants = append(participants, Participant{
						Account: member.Account,
					})
				}
			}

			// Capture pre-filter size for correct threshold calculation
			origSignCommitteeSize := len(participants)

			// Readiness check: ping each participant's TSS RPC layer to filter out zombie nodes
			participants = tssMgr.checkParticipantReadiness(participants, sessionId, "SIGN")
			origThreshold, _ := tss_helpers.GetThreshold(origSignCommitteeSize)
			if len(participants) < origThreshold+1 {
				log.Warn("insufficient participants for signing", "sessionId", sessionId, "connected", len(participants), "needed", origThreshold+1)
				continue
			}

			prevCommitType := ""
			if err == nil {
				prevCommitType = commitment.Type
			}

			dispatcher := &SignDispatcher{
				BaseDispatcher: BaseDispatcher{
					startLock:    sync.Mutex{},
					algo:         action.Algo,
					tssMgr:       tssMgr,
					participants: participants,
					p2pMsg:       make(chan btss.Message, 2*len(participants)),
					sessionId:    sessionId,
					done:         make(chan struct{}),
					keyId:        action.KeyId,

					keystore: tssMgr.keyStore,

					epoch: keyInfo.Epoch,
				},
				msg:                action.Args,
				prevCommitmentType: prevCommitType,
				origCommitteeSize:  origSignCommitteeSize,
			}
			dispatcher.msgCtx, dispatcher.cancelMsgs = context.WithCancel(context.Background())
			dispatcher.startLock.TryLock()

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.bufferLock.Lock()
			tssMgr.actionMap[sessionId] = dispatcher
			tssMgr.bufferLock.Unlock()
		} else if action.Type == ReshareAction {

			sessionId = "reshare-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId

			log.Verbose("creating reshare session", "sessionId", sessionId, "keyId", action.KeyId, "blockHeight", bh)
			commitment, err := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "keygen", "reshare")
			lastBlame, _ := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "blame")

			//This should either be equal but never less in practical terms
			//However, we can add further checks
			if commitment.Epoch >= currentElection.Epoch || err != nil {
				log.Verbose("skipping reshare, commitment epoch meets or exceeds current", "sessionId", sessionId, "keyId", action.KeyId, "commitmentEpoch", commitment.Epoch, "currentEpoch", currentElection.Epoch, "err", err)
				continue
			}

			var isBlame bool
			blameBits := big.NewInt(0)
			if lastBlame.Type == "blame" {
				if lastBlame.BlockHeight > commitment.BlockHeight && int64(lastBlame.BlockHeight) > int64(bh)-int64(BLAME_EXPIRE) {
					isBlame = true
					blameBytes, _ := base64.RawURLEncoding.DecodeString(lastBlame.Commitment)
					blameBits = blameBits.SetBytes(blameBytes)
				}
			}

			commitmentElection := tssMgr.electionDb.GetElection(commitment.Epoch)
			if commitmentElection == nil || commitmentElection.Members == nil {
				log.Warn("commitment election missing, skipping reshare", "epoch", commitment.Epoch, "sessionId", sessionId)
				continue
			}

			commitmentBytes, err := base64.RawURLEncoding.DecodeString(commitment.Commitment)

			bitset := big.NewInt(0)
			bitset = bitset.SetBytes(commitmentBytes)

			commitedMembers := make([]Participant, 0)

			log.Trace("commitment lookup", "commitment", commitment, "err", err)
			log.Trace("bitset details", "bitset", bitset, "commitmentBytes", commitmentBytes)
			for idx, member := range commitmentElection.Members {
				if idx < bitset.BitLen() && bitset.Bit(idx) == 1 {
					commitedMembers = append(commitedMembers, Participant{
						Account: member.Account,
					})
				}
			}

			newParticipants := make([]Participant, 0)
			excludedNodes := make([]string, 0)

			for idx, member := range currentElection.Members {
				log.Trace("blame check for member", "isBlame", isBlame, "idx", idx, "blameBit", blameBits.Bit(idx))
				if isBlame {
					if blameBits.Bit(idx) == 1 {
						excludedNodes = append(excludedNodes, member.Account)
						log.Verbose("excluding blamed node from reshare", "sessionId", sessionId, "account", member.Account)
						continue
					}
				}
				if blameMap.BannedNodes[member.Account] {
					excludedNodes = append(excludedNodes, member.Account)
					log.Verbose("excluding banned node from reshare", "sessionId", sessionId, "account", member.Account)
					continue
				}
				newParticipants = append(newParticipants, Participant{
					Account: member.Account,
				})
			}

			log.Verbose("reshare participant selection", "sessionId", sessionId, "oldParticipants", len(commitedMembers), "newParticipants", len(newParticipants), "excluded", len(excludedNodes), "excludedNodes", excludedNodes)
			log.Trace("new participants list", "newParticipants", newParticipants)

			// Capture pre-filter sizes — these are needed by the dispatcher for correct
			// threshold calculation in tss-lib. Using post-filter sizes corrupts the key.
			origOldSize := len(commitedMembers)
			origNewSize := len(newParticipants)

			// Filter both old and new participants by connectivity
			// Threshold is based on original counts (before filtering) since the key was created with that many participants
			origOldThreshold, _ := tss_helpers.GetThreshold(origOldSize)
			origNewThreshold, _ := tss_helpers.GetThreshold(origNewSize)
			commitedMembers = tssMgr.checkParticipantReadiness(commitedMembers, sessionId, "RESHARE-OLD")
			newParticipants = tssMgr.checkParticipantReadiness(newParticipants, sessionId, "RESHARE-NEW")

			// Pre-flight checks: validate participant set meets minimum threshold (at least 2)
			minNewRequired := origNewThreshold + 1
			if minNewRequired < 2 {
				minNewRequired = 2
			}
			if len(newParticipants) < minNewRequired {
				log.Warn("insufficient new participants for reshare", "sessionId", sessionId, "participants", len(newParticipants), "required", minNewRequired, "threshold", origNewThreshold)
				continue
			}

			// Pre-flight check: verify old participants are available
			if len(commitedMembers) < origOldThreshold+1 {
				log.Warn("insufficient old participants for reshare", "sessionId", sessionId, "oldParticipants", len(commitedMembers), "required", origOldThreshold+1, "threshold", origOldThreshold)
				continue
			}

			log.Verbose("reshare pre-flight checks passed", "sessionId", sessionId, "oldParticipants", len(commitedMembers), "newParticipants", len(newParticipants))

			dispatcher := &ReshareDispatcher{
				BaseDispatcher: BaseDispatcher{
					startLock:    sync.Mutex{},
					algo:         tss_helpers.SigningAlgo(action.Algo),
					tssMgr:       tssMgr,
					participants: commitedMembers,
					p2pMsg:       make(chan btss.Message, 4*(len(commitedMembers)+len(newParticipants))),
					sessionId:    sessionId,
					done:         make(chan struct{}),
					keyId:        action.KeyId,
					epoch:        commitment.Epoch,

					keystore:    tssMgr.keyStore,
					blockHeight: bh,
					isReshare:   true,
				},
				newParticipants:    newParticipants,
				newEpoch:           currentElection.Epoch,
				origOldSize:        origOldSize,
				origNewSize:        origNewSize,
				prevCommitmentType: commitment.Type,
			}
			dispatcher.msgCtx, dispatcher.cancelMsgs = context.WithCancel(context.Background())
			dispatcher.startLock.TryLock()

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.bufferLock.Lock()
			tssMgr.actionMap[sessionId] = dispatcher
			bufferSize := tssMgr.messageBuffer.MsgCount(sessionId)
			tssMgr.bufferLock.Unlock()

			// Trigger replay of any buffered messages for this session
			if bufferSize > 0 {
				log.Verbose("dispatcher registered, will replay buffered messages", "sessionId", sessionId, "bufferSize", bufferSize)
			}
		}
		// Determine action type from the action
		var actionType ActionType
		switch action.Type {
		case KeyGenAction:
			actionType = ActionTypeKeyGen
		case SignAction:
			actionType = ActionTypeSign
		case ReshareAction:
			actionType = ActionTypeReshare
		default:
			actionType = ""
		}

		tssMgr.bufferLock.Lock()
		tssMgr.sessionMap[sessionId] = sessionInfo{
			leader: leader,
			bh:     bh,
			action: actionType,
		}
		tssMgr.bufferLock.Unlock()
	}

	startedDispatcher := make([]Dispatcher, 0)
	for _, dispatcher := range dispatchers {
		log.Trace("dispatcher started")
		//If start fails then done is never possible
		//todo: handle this better
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic in dispatcher Start: %v", r)
					log.Error("panic recovered in dispatcher.Start", "sessionId", dispatcher.SessionId(), "panic", r)
				}
			}()
			err = dispatcher.Start()
		}()

		if err == nil {
			startedDispatcher = append(startedDispatcher, dispatcher)
		} else {
			sessionId := dispatcher.SessionId()
			tssMgr.bufferLock.Lock()
			delete(tssMgr.sigChannels, sessionId)
			delete(tssMgr.actionMap, sessionId)
			tssMgr.messageBuffer.Delete(sessionId)
			delete(tssMgr.sessionMap, sessionId)
			tssMgr.bufferLock.Unlock()
		}
		log.Trace("dispatcher Start result", "err", err)
	}

	// Release the lock immediately after starting dispatchers.
	// The Done/Await goroutine below only reads from dispatchers and writes
	// to shared maps protected by bufferLock. Holding tssMgr.lock through
	// the entire await (including timeouts) blocks subsequent RunActions calls,
	// causing cascading signing failures when reshare retries overlap with
	// the next sign interval.
	tssMgr.lock.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic recovered in Done/Await goroutine", "panic", r)
			}
		}()

		signedResults := make([]KeySignResult, 0)
		commitableResults := make([]tss_helpers.BaseCommitment, 0)

		// Await all dispatchers concurrently so that each dispatcher's
		// sessionResults entry is stored as soon as IT finishes, rather than
		// being blocked behind slower dispatchers in a sequential loop.
		// This prevents a race where fast nodes clean up sessionResults
		// before slow nodes (delayed by an earlier timeout) send ask_sigs.
		var resultsMu sync.Mutex
		var awaitWg sync.WaitGroup
		for _, dsc := range startedDispatcher {
			awaitWg.Add(1)
			go func(dsc Dispatcher) {
				defer awaitWg.Done()

				resultPtr, err := dsc.Done().Await(context.Background())

				sessionId := dsc.SessionId()
				tssMgr.bufferLock.Lock()
				delete(tssMgr.sigChannels, sessionId)
				delete(tssMgr.actionMap, sessionId)
				tssMgr.messageBuffer.Delete(sessionId)
				delete(tssMgr.sessionMap, sessionId)
				tssMgr.bufferLock.Unlock()
				if err != nil {
					log.Warn("session dispatch rejected", "sessionId", dsc.SessionId(), "err", err)
					return
				}

				result := *resultPtr
				if result.Type() == KeyGenResultType {
					res := result.(KeyGenResult)

					res.BlockHeight = bh
					tssMgr.bufferLock.Lock()
					tssMgr.sessionResults[dsc.SessionId()] = sessionResultEntry{result: res, blockHeight: bh}
					tssMgr.bufferLock.Unlock()

					log.Info(
						"keygen success",
						"sessionId",
						res.SessionId,
						"keyId",
						res.KeyId,
						"blockHeight",
						res.BlockHeight,
						"epoch",
						res.Epoch,
						"pubKey",
						fmt.Sprintf("%x", res.PublicKey),
						"commitment",
						res.Commitment,
					)

					commitment := result.Serialize()
					resultsMu.Lock()
					commitableResults = append(commitableResults, commitment)
					resultsMu.Unlock()
				} else if result.Type() == KeySignResultType {
					res := result.(KeySignResult)

					res.BlockHeight = bh
					tssMgr.bufferLock.Lock()
					tssMgr.sessionResults[dsc.SessionId()] = sessionResultEntry{result: res, blockHeight: bh}
					tssMgr.bufferLock.Unlock()

					resultsMu.Lock()
					signedResults = append(signedResults, res)
					resultsMu.Unlock()
				} else if result.Type() == ReshareResultType {
					res := result.(ReshareResult)

					res.BlockHeight = bh
					tssMgr.bufferLock.Lock()
					tssMgr.sessionResults[dsc.SessionId()] = sessionResultEntry{result: res, blockHeight: bh}
					tssMgr.bufferLock.Unlock()

					log.Info("reshare success", "sessionId", res.SessionId, "keyId", res.KeyId, "blockHeight", res.BlockHeight, "epoch", res.NewEpoch, "commitment", res.Commitment)

					commitment := result.Serialize()
					commitment.BlockHeight = bh
					resultsMu.Lock()
					commitableResults = append(commitableResults, commitment)
					resultsMu.Unlock()

				} else if result.Type() == ErrorType {
					res := result.(ErrorResult)
					res.BlockHeight = bh

					if res.tssErr != nil {
						culprits := make([]string, 0)
						for _, n := range res.tssErr.Culprits() {
							culprits = append(culprits, string(n.GetId()))
						}
						log.Verbose("TSS error result with culprits", "sessionId", res.SessionId, "keyId", res.KeyId, "blockHeight", res.BlockHeight, "epoch", res.Epoch, "culprits", culprits, "err", res.tssErr.Error())
					} else if res.err != nil {
						log.Verbose("internal error result", "sessionId", res.SessionId, "keyId", res.KeyId, "blockHeight", res.BlockHeight, "epoch", res.Epoch, "err", res.err)
					}

					tssMgr.bufferLock.Lock()
					tssMgr.sessionResults[dsc.SessionId()] = sessionResultEntry{result: res, blockHeight: bh}
					tssMgr.bufferLock.Unlock()

					commitment := result.Serialize()
					commitment.BlockHeight = bh
					resultsMu.Lock()
					commitableResults = append(commitableResults, commitment)
					resultsMu.Unlock()
				} else if result.Type() == TimeoutType {
					res := result.(TimeoutResult)

					res.BlockHeight = bh
					tssMgr.bufferLock.Lock()
					tssMgr.sessionResults[dsc.SessionId()] = sessionResultEntry{result: res, blockHeight: bh}
					tssMgr.bufferLock.Unlock()

					log.Warn("timeout result", "sessionId", res.SessionId, "keyId", res.KeyId, "blockHeight", res.BlockHeight, "epoch", res.Epoch, "culprits", res.Culprits)
					commitment := result.Serialize()
					commitment.BlockHeight = bh
					resultsMu.Lock()
					commitableResults = append(commitableResults, commitment)
					resultsMu.Unlock()

					// Schedule automatic retry for reshare timeouts with retry limit
					if dsc.KeyId() != "" {
						keyInfo, err := tssMgr.tssKeys.FindKey(dsc.KeyId())
						if err == nil {
							// Check retry count from session (in-memory) to avoid DB dependency
							retryCount := tssMgr.getRetryCount(dsc.KeyId())
							if retryCount < MAX_RESHARE_RETRIES {
								// Calculate exponential backoff delay based on retry count
								retryDelay := time.Duration(retryCount+1) * tssMgr.sconf.TssParams().ReshareSyncDelay

								log.Info("scheduling reshare retry for timeout", "sessionId", dsc.SessionId(), "keyId", dsc.KeyId(), "retryCount", retryCount+1, "maxRetries", MAX_RESHARE_RETRIES, "delay", retryDelay)

								// Increment retry count
								tssMgr.incrementRetryCount(dsc.KeyId())

								// Schedule retry with delay
								// Use the correct action type: keygen timeouts should retry as keygen,
								// not reshare (reshare requires an existing commitment in the DB).
								retryType := ReshareAction
								if _, isKeygen := dsc.(*KeyGenDispatcher); isKeygen {
									retryType = KeyGenAction
								}
								go func() {
									time.Sleep(retryDelay)
									tssMgr.bufferLock.Lock()
									tssMgr.queuedActions = append(tssMgr.queuedActions, QueuedAction{
										Type:  retryType,
										KeyId: dsc.KeyId(),
										Algo:  tss_helpers.SigningAlgo(keyInfo.Algo),
									})
									tssMgr.bufferLock.Unlock()
								}()
							} else {
								log.Error("max reshare retries exceeded", "sessionId", dsc.SessionId(), "keyId", dsc.KeyId(), "maxRetries", MAX_RESHARE_RETRIES)
								// Could trigger alert or manual intervention here
							}
						}
					}
				}
			}(dsc)
		}
		awaitWg.Wait()

		// Lock was already released before this goroutine started.

		if isLeader {
			go func() {
				if len(signedResults) > 0 {

					//Signed Results submission
					sigPacket := make([]map[string]any, 0)
					for _, signResult := range signedResults {
						sigPacket = append(sigPacket, map[string]any{
							"key_id": signResult.KeyId,
							"msg":    hex.EncodeToString(signResult.Msg),
							"sig":    hex.EncodeToString(signResult.Signature),
						})
					}

					rawJson, _ := json.Marshal(map[string]any{
						"packet": sigPacket,
					})
					deployOp := hivego.CustomJsonOperation{
						RequiredAuths:        []string{tssMgr.config.Get().HiveUsername},
						RequiredPostingAuths: []string{},
						Id:                   "vsc.tss_sign",
						Json:                 string(rawJson),
					}

					// wif := tssMgr.config.Get().HiveActiveKey

					hiveTx := tssMgr.hiveClient.MakeTransaction([]hivego.HiveOperation{
						deployOp,
					})
					tssMgr.hiveClient.PopulateSigningProps(&hiveTx, nil)
					sig, _ := tssMgr.hiveClient.Sign(hiveTx)
					hiveTx.AddSig(sig)
					txId, err := tssMgr.hiveClient.Broadcast(hiveTx)
					if err != nil {
						log.Trace("signature broadcast error", "err", err)
					}
					log.Trace("signature broadcast result", "txId", txId)
				}
			}()

			if len(commitableResults) > 0 {
				time.Sleep(tssMgr.sconf.TssParams().CommitDelay)

				log.Info("starting multi-sig collection", "blockHeight", bh, "count", len(commitableResults))

				commitedResults := make(map[string]struct {
					err        error
					circuit    *dids.SerializedCircuit
					commitment tss_helpers.BaseCommitment
				})
				var commitedMu sync.Mutex
				var wg sync.WaitGroup
				for _, commitResult := range commitableResults {
					wg.Add(1)
					go func() {
						commitResult.BlockHeight = bh

						log.Verbose(
							"collecting sigs",
							"sessionId",
							commitResult.SessionId,
							"keyId",
							commitResult.KeyId,
							"type",
							commitResult.Type,
							"epoch",
							commitResult.Epoch,
						)

						bytes, _ := common.EncodeDagCbor(commitResult)
						signableCid, _ := common.HashBytes(bytes, multicodec.DagCbor)

						ctx, cancel := context.WithTimeout(
							context.Background(),
							tssMgr.sconf.TssParams().WaitForSigsTimeout,
						)
						defer cancel()

						serializedCircuit, err := tssMgr.waitForSigs(
							ctx,
							signableCid,
							commitResult.SessionId,
							&currentElection,
						)

						if err != nil {
							log.Warn(
								"waitForSigs failed",
								"sessionId",
								commitResult.SessionId,
								"keyId",
								commitResult.KeyId,
								"type",
								commitResult.Type,
								"err",
								err,
							)
						} else {
							log.Verbose("waitForSigs OK", "sessionId", commitResult.SessionId, "keyId", commitResult.KeyId, "type", commitResult.Type)
							commitedMu.Lock()
							commitedResults[commitResult.SessionId] = struct {
								err        error
								circuit    *dids.SerializedCircuit
								commitment tss_helpers.BaseCommitment
							}{
								err:        err,
								circuit:    serializedCircuit,
								commitment: commitResult,
							}
							commitedMu.Unlock()
						}

						wg.Done()
					}()
				}
				wg.Wait()

				var canCommit bool = false
				sigPacket := make([]map[string]any, 0)
				for _, signResult := range commitedResults {
					if signResult.err == nil {
						canCommit = true
						sigPacket = append(sigPacket, map[string]any{
							"type":         signResult.commitment.Type,
							"session_id":   signResult.commitment.SessionId,
							"key_id":       signResult.commitment.KeyId,
							"commitment":   signResult.commitment.Commitment,
							"epoch":        signResult.commitment.Epoch,
							"pub_key":      signResult.commitment.PublicKey,
							"block_height": signResult.commitment.BlockHeight,

							"signature": signResult.circuit.Signature,
							"bv":        signResult.circuit.BitVector,
						})
					}
				}

				if !canCommit {
					log.Warn(
						"no results reached threshold",
						"blockHeight",
						bh,
						"total",
						len(commitableResults),
						"committed",
						len(commitedResults),
					)
				}

				rawJson, err := json.Marshal(sigPacket)

				if canCommit {
					log.Info("broadcasting commitment to Hive", "blockHeight", bh, "sessions", len(commitedResults))
					deployOp := hivego.CustomJsonOperation{
						RequiredAuths:        []string{tssMgr.config.Get().HiveUsername},
						RequiredPostingAuths: []string{},
						Id:                   "vsc.tss_commitment",
						Json:                 string(rawJson),
					}

					hiveTx := tssMgr.hiveClient.MakeTransaction([]hivego.HiveOperation{
						deployOp,
					})
					tssMgr.hiveClient.PopulateSigningProps(&hiveTx, nil)
					sig, _ := tssMgr.hiveClient.Sign(hiveTx)
					hiveTx.AddSig(sig)
					_, err = tssMgr.hiveClient.Broadcast(hiveTx)
					if err != nil {
						log.Error("Hive broadcast failed", "blockHeight", bh, "err", err)
					} else {
						log.Info("Hive broadcast OK", "blockHeight", bh)
					}
				}
			}
		}

		// Clean up stale sessionResults entries. Keep them for 40 blocks (~2 min)
		// so that slower nodes (delayed by a timed-out action in their batch)
		// can still respond to ask_sigs from faster nodes.
		const sessionResultMaxAge uint64 = 40
		tssMgr.bufferLock.Lock()
		for id, entry := range tssMgr.sessionResults {
			if bh > entry.blockHeight+sessionResultMaxAge {
				delete(tssMgr.sessionResults, id)
			}
		}
		tssMgr.bufferLock.Unlock()
	}()
}

func (tssMgr *TssManager) setToCommitment(participants []Participant, epoch uint64) string {
	electionData := tssMgr.electionDb.GetElection(epoch)

	bitset := &big.Int{}
	for _, p := range participants {
		for nIdx, mbr := range electionData.Members {
			if mbr.Account == p.Account {
				bitset = bitset.SetBit(bitset, nIdx, 1)
				break
			}
		}
	}

	return base64.RawURLEncoding.EncodeToString(bitset.Bytes())
}

func (tssMgr *TssManager) waitForSigs(
	ctx context.Context,
	cid cid.Cid,
	sessionId string,
	election *elections.ElectionResult,
) (*dids.SerializedCircuit, error) {
	weightTotal := uint64(0)
	for _, weight := range election.Weights {
		weightTotal += weight
	}

	members := make([]dids.Member, 0)

	for _, member := range election.Members {
		members = append(members, dids.BlsDID(member.Key))
	}
	log.Trace("waitForSigs members", "members", members)
	blsCircuit := dids.NewBlsCircuitGenerator(members)

	tssMgr.bufferLock.Lock()
	tssMgr.sigChannels[sessionId] = make(chan sigMsg, 16)
	tssMgr.bufferLock.Unlock()

	tssMgr.pubsub.Send(p2pMessage{
		Type:    "ask_sigs",
		Account: tssMgr.config.Get().HiveUsername,
		Data: map[string]interface{}{
			"session_id": sessionId,
		},
	})

	log.Trace("waiting for sigs", "commitedCid", cid)
	circuit, _ := blsCircuit.Generate(cid)

	var errRes error
	var res *dids.SerializedCircuit

	proc1 := make(chan struct{}, 1)
	go func() {
		signedWeight := uint64(0)

		// common.has
		tssMgr.bufferLock.RLock()
		sigChan := tssMgr.sigChannels[sessionId]
		tssMgr.bufferLock.RUnlock()

		signedMap := make(map[string]bool)
		for signedWeight*3 < weightTotal*2 {
			var msg sigMsg
			select {
			case <-ctx.Done():
				return
			case msg = <-sigChan:
			}

			var member dids.Member
			var memberAccount string
			var index int = -1
			for i, data := range election.Members {
				if data.Account == msg.Account {
					member = dids.BlsDID(data.Key)
					index = i
					break
				}
			}

			if index == -1 {
				continue
			}

			added, err := circuit.AddAndVerify(member, msg.Sig)

			log.Trace("sig add and verify result", "added", added, "err", err)

			if added {
				signedWeight += election.Weights[index]
				signedMap[memberAccount] = true
			}
		}
		finalizedCiruit, err := circuit.Finalize()

		log.Trace("finalized circuit result", "circuit", finalizedCiruit, "err", err)

		serialized, err := finalizedCiruit.Serialize()

		if err != nil {
			errRes = err
		}

		res = serialized
		proc1 <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		log.Trace("context expired waiting for sigs")
		return nil, ctx.Err() // Return error if canceled
	case <-proc1:
		return res, errRes
	}
}

func (tssMgr *TssManager) Init() error {
	tssMgr.VStream.RegisterBlockTick("tss-mgr", tssMgr.BlockTick, true)
	return nil
}

func (tssMgr *TssManager) KeyGen(keyId string, algo tss_helpers.SigningAlgo) int {
	tssMgr.bufferLock.Lock()
	tssMgr.queuedActions = append(tssMgr.queuedActions, QueuedAction{
		Type:  KeyGenAction,
		KeyId: keyId,
		Algo:  algo,
		Args:  nil,
	})
	n := len(tssMgr.queuedActions)
	tssMgr.bufferLock.Unlock()

	return n
}

func (tssMgr *TssManager) KeySign(msgs []byte, keyId string) (int, error) {
	keyInfo, err := tssMgr.tssKeys.FindKey(keyId)

	if err != nil {
		return 0, err
	}
	tssMgr.bufferLock.Lock()
	tssMgr.queuedActions = append(tssMgr.queuedActions, QueuedAction{
		Type:  SignAction,
		KeyId: keyId,
		Algo:  tss_helpers.SigningAlgo(keyInfo.Algo),
		Args:  msgs,
	})
	n := len(tssMgr.queuedActions)
	tssMgr.bufferLock.Unlock()
	return n, nil
}

func (tssMgr *TssManager) KeyReshare(keyId string) (int, error) {
	keyInfo, err := tssMgr.tssKeys.FindKey(keyId)

	if err != nil {
		return 0, err
	}
	tssMgr.bufferLock.Lock()
	tssMgr.queuedActions = append(tssMgr.queuedActions, QueuedAction{
		Type:  ReshareAction,
		KeyId: keyId,
		Algo:  tss_helpers.SigningAlgo(keyInfo.Algo),
		Args:  nil,
	})
	n := len(tssMgr.queuedActions)
	tssMgr.bufferLock.Unlock()
	return n, nil
}

func (tssMgr *TssManager) Start() *promise.Promise[any] {
	tssRpc := TssRpc{
		mgr: tssMgr,
	}
	server := gorpc.NewServer(tssMgr.p2p.Host(), protocolId)
	err := server.RegisterName("vsc.tss", &tssRpc)

	if err != nil {
		return utils.PromiseReject[any](err)
	}

	client := gorpc.NewClientWithServer(tssMgr.p2p.Host(), protocolId, server)

	tssMgr.client = client
	tssMgr.server = server

	tssMgr.startP2P()

	go tssMgr.GeneratePreParams()

	ctx := context.Background()

	go func() {
		//Every one minute
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				tssMgr.GeneratePreParams()
			}
		}
	}()
	return utils.PromiseResolve[any](nil)
}

func (tssMgr *TssManager) Stop() error {
	tssMgr.stopP2P()
	return nil
}

func New(
	p2p *libp2p.P2PServer,
	tssKeys tss_db.TssKeys,
	tssRequests tss_db.TssRequests,
	tssCommitments tss_db.TssCommitments,
	witnessDb witnesses.Witnesses,
	electionDb elections.Elections,
	vstream *blockconsumer.HiveConsumer,
	se GetScheduler,
	config common.IdentityConfig,
	sconf systemconfig.SystemConfig,
	keystore *flatfs.Datastore,
	hiveClient hive.HiveTransactionCreator,
) *TssManager {
	preParams := make(chan ecKeyGen.LocalPreParams, 1)

	return &TssManager{
		preParamsLock: sync.Mutex{},
		sigChannels:   make(map[string]chan sigMsg),

		keyStore: keystore,

		config:     config,
		sconf:      sconf,
		scheduler:  se,
		preParams:  preParams,
		p2p:        p2p,
		hiveClient: hiveClient,

		tssKeys:        tssKeys,
		metrics:        GetMetrics(),
		tssRequests:    tssRequests,
		tssCommitments: tssCommitments,

		witnessDb:  witnessDb,
		electionDb: electionDb,
		VStream:    vstream,

		queuedActions:  make([]QueuedAction, 0),
		actionMap:      make(map[string]Dispatcher),
		messageBuffer:  newSessionBuffer(),
		sessionMap:     make(map[string]sessionInfo),
		sessionResults: make(map[string]sessionResultEntry),
		retryCounts:    make(map[string]int),
	}
}

//Processes:
//
// # Key Generate
// - Generate action is queued
// - Node developes a list of candidate nodes: this can be sourced from witness list, witness list filtered by blamed nodes or a set of sharded pools for a v2 or v3 model
// - All nodes wait until sync time period, which can be between 5-10 minutes
// - Nodes then start relevant P2P channels and TSS parties in "start" mode
// - Nodes then start the process of generating a TSS key, sourcing pre-generated preparams
// - Once all nodes have confirmed the creation of the TSS key, a 2/3 majority signed confirmation will be created and posted
// - If TSS keygen fails, then blame manager will record misbehaving nodes to exclude from future keygen events
// - Actively bonded nodes will be recorded within every reshare & generation event (for use within smart contracts or others)
// ## interface structure laid out
// - Actions are general purpose interfaces that define the following: Start function, optional blame function, update party list, and finish output.
// - This can be keygen, sign, or reshare
// - It also contains some minimal message resharing logic

// # Key Sign
// - Similar process to the above. However, the submitting node will submit the signature to chain. If the submitting node fails to submit, then a round robin submission process will occur to prevent redundancy failures.
//
// # Key reshare
