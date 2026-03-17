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
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/hive"
	"vsc-node/lib/utils"
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

const (
	TSS_SIGN_INTERVAL           = 50            // 50 L1 blocks
	TSS_ROTATE_INTERVAL         = 20 * 5        // 5 minutes in L1 blocks
	TSS_MESSAGE_RETRY_COUNT     = 3             // Number of retries for failed messages
	TSS_BAN_THRESHOLD_PERCENT   = 60            // Failure rate threshold for bans
	TSS_BAN_GRACE_PERIOD_EPOCHS = 3             // Epochs before new nodes can be banned (as int for comparison)
	MAX_RESHARE_RETRIES         = 3             // Maximum number of reshare retries on timeout
	BLAME_EXPIRE                = uint64(28800) // 24 hour blame
	TSS_BLAME_EPOCH_COUNT       = (4 * 7) - 1  // Number of past epochs to include in blame scoring
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
	sessionResults map[string]DispatcherResult

	// Message buffer for early-arriving messages before dispatcher registration
	messageBuffer map[string][]bufferedMessage
	bufferLock    sync.RWMutex

	// Retry count for reshare operations (in-memory to avoid DB dependency)
	retryCounts   map[string]int
	retryCountsMu sync.Mutex

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
			fmt.Println("Need to generate preparams")
			preParams, err := ecKeyGen.GeneratePreParams(time.Minute)
			if err == nil {
				tssMgr.preParams <- *preParams
			}
		}
		tssMgr.preParamsLock.Unlock()
	}
}

func (tssMgr *TssManager) BlockTick(bh uint64, headHeight *uint64) {
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
				fmt.Printf("[TSS] Election data missing at height %d, skipping rotate (err=%v)\n", bh, err)
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
		// Consume any recovery-scheduled actions (e.g., reshare retries after timeouts)
		tssMgr.bufferLock.Lock()
		if len(tssMgr.queuedActions) > 0 {
			generatedActions = append(generatedActions, tssMgr.queuedActions...)
			tssMgr.queuedActions = tssMgr.queuedActions[:0]
		}
		tssMgr.bufferLock.Unlock()
		if len(generatedActions) > 0 {
			tssMgr.RunActions(generatedActions, witnessSlot.Account, isLeader, bh)
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
		blames, _ := tss.tssCommitments.GetBlames(tss_db.ByEpoch(election.Epoch), tss_db.ByType("blame"))
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
			fmt.Printf("[TSS] [BLAME] Node in grace period (exempt from ban): account=%s score=%d weight=%d epochsSinceFirst=%d gracePeriod=%d\n",
				entry.Account, entry.Score, entry.Weight, entry.EpochsSinceFirst, gracePeriod)
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

			fmt.Printf("[TSS] [BLAME] Node banned: account=%s score=%d weight=%d failureRate=%.2f%% threshold=%d%% timeoutBlames=%d errorBlames=%d epochsSinceFirst=%d\n",
				entry.Account, entry.Score, entry.Weight, failureRate, thresholdPercent, timeoutCount, errorCount, entry.EpochsSinceFirst)
		} else {
			fmt.Printf("[TSS] [BLAME] Node not banned: account=%s score=%d weight=%d failureRate=%.2f%% threshold=%d%%\n",
				entry.Account, entry.Score, entry.Weight, failureRate, thresholdPercent)
		}
	}

	if len(bannedList) > 0 {
		fmt.Printf("[TSS] [BLAME] Ban summary: totalBanned=%d bannedNodes=%v gracePeriodExemptions=%d\n",
			len(bannedList), bannedList, len(gracePeriodExemptions))
	} else {
		fmt.Printf("[TSS] [BLAME] Ban summary: no nodes banned gracePeriodExemptions=%d\n", len(gracePeriodExemptions))
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

	fmt.Println("Running Actions", tssMgr.config.Get().HiveUsername, bh, "isLeader", isLeader, "locked", locked)
	if !locked {
		fmt.Printf("[TSS] [ACTIONS] RunActions skipped: lock held by previous batch blockHeight=%d\n", bh)
		return
	}

	currentElection, err := tssMgr.electionDb.GetElectionByHeight(uint64(bh))
	fmt.Println("err (197)", currentElection, err)
	if err != nil || currentElection.Members == nil {
		if err != nil {
			fmt.Println("err", err)
		} else {
			fmt.Printf("[TSS] [ACTIONS] Election data missing at height %d, skipping actions\n", bh)
		}
		tssMgr.lock.Unlock()
		return
	}

	blameMap := tssMgr.BlameScore()

	fmt.Printf("[TSS] [ACTIONS] Running actions blockHeight=%d isLeader=%v actionCount=%d bannedNodes=%d\n",
		bh, isLeader, len(actions), len(blameMap.BannedNodes))

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
			fmt.Printf("[TSS] [KEYGEN] Creating session sessionId=%s keyId=%s epoch=%d blockHeight=%d participants=%v excluded=%v hasBlame=%v lastBlameHeight=%d\n",
				sessionId, action.KeyId, currentElection.Epoch, bh, participantAccounts, excludedAccounts, isBlame, lastBlame.BlockHeight)

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

			for midx, member := range currentElection.Members {
				if bv.Bit(midx) == 1 {
					participants = append(participants, Participant{
						Account: member.Account,
					})
				}
			}

			// Readiness check: ping each participant's TSS RPC layer to filter out zombie nodes
			participants = tssMgr.checkParticipantReadiness(participants, sessionId, "SIGN")
			origThreshold, _ := tss_helpers.GetThreshold(len(currentElection.Members))
			if len(participants) < origThreshold+1 {
				fmt.Printf("[TSS] [SIGN] Not enough connected participants for signing sessionId=%s connected=%d needed=%d\n",
					sessionId, len(participants), origThreshold+1)
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
			}
			dispatcher.startLock.TryLock()

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.bufferLock.Lock()
			tssMgr.actionMap[sessionId] = dispatcher
			tssMgr.bufferLock.Unlock()
		} else if action.Type == ReshareAction {

			sessionId = "reshare-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId

			fmt.Printf("[TSS] [RESHARE] Creating reshare action sessionId=%s keyId=%s blockHeight=%d\n",
				sessionId, action.KeyId, bh)
			commitment, err := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "keygen", "reshare")
			lastBlame, _ := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "blame")

			//This should either be equal but never less in practical terms
			//However, we can add further checks
			if commitment.Epoch >= currentElection.Epoch || err != nil {
				fmt.Printf("[TSS] [RESHARE] Skipping reshare sessionId=%s keyId=%s commitmentEpoch=%d currentEpoch=%d err=%v\n",
					sessionId, action.KeyId, commitment.Epoch, currentElection.Epoch, err)
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
				fmt.Printf("[TSS] [RESHARE] Commitment election missing for epoch %d, skipping reshare sessionId=%s\n",
					commitment.Epoch, sessionId)
				continue
			}

			commitmentBytes, err := base64.RawURLEncoding.DecodeString(commitment.Commitment)

			bitset := big.NewInt(0)
			bitset = bitset.SetBytes(commitmentBytes)

			commitedMembers := make([]Participant, 0)

			fmt.Println("commitment, err", commitment, err)
			fmt.Println("bitset", bitset, commitmentBytes)
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
				fmt.Println("isBlame", isBlame, idx, blameBits.Bit(idx))
				if isBlame {
					if blameBits.Bit(idx) == 1 {
						excludedNodes = append(excludedNodes, member.Account)
						fmt.Printf("[TSS] [RESHARE] Excluding blamed node from reshare sessionId=%s account=%s\n",
							sessionId, member.Account)
						continue
					}
				}
				if blameMap.BannedNodes[member.Account] {
					excludedNodes = append(excludedNodes, member.Account)
					fmt.Printf("[TSS] [RESHARE] Excluding banned node from reshare sessionId=%s account=%s\n",
						sessionId, member.Account)
					continue
				}
				newParticipants = append(newParticipants, Participant{
					Account: member.Account,
				})
			}

			fmt.Printf("[TSS] [RESHARE] Participant selection sessionId=%s oldParticipants=%d newParticipants=%d excluded=%d excludedNodes=%v\n",
				sessionId, len(commitedMembers), len(newParticipants), len(excludedNodes), excludedNodes)
			fmt.Println("newParticipants", newParticipants)

			// Filter both old and new participants by connectivity
			// Threshold is based on original counts (before filtering) since the key was created with that many participants
			origOldThreshold, _ := tss_helpers.GetThreshold(len(commitedMembers))
			origNewThreshold, _ := tss_helpers.GetThreshold(len(newParticipants))
			commitedMembers = tssMgr.checkParticipantReadiness(commitedMembers, sessionId, "RESHARE-OLD")
			newParticipants = tssMgr.checkParticipantReadiness(newParticipants, sessionId, "RESHARE-NEW")

			// Pre-flight checks: validate participant set meets minimum threshold
			if len(newParticipants) < origNewThreshold+1 {
				fmt.Printf("[TSS] [RESHARE] ERROR: Insufficient new participants sessionId=%s participants=%d required=%d threshold=%d\n",
					sessionId, len(newParticipants), origNewThreshold+1, origNewThreshold)
				continue
			}

			// Pre-flight check: verify old participants are available
			if len(commitedMembers) < origOldThreshold+1 {
				fmt.Printf("[TSS] [RESHARE] ERROR: Insufficient old participants sessionId=%s oldParticipants=%d required=%d threshold=%d\n",
					sessionId, len(commitedMembers), origOldThreshold+1, origOldThreshold)
				continue
			}

			fmt.Printf("[TSS] [RESHARE] Pre-flight checks passed sessionId=%s oldParticipants=%d newParticipants=%d\n",
				sessionId, len(commitedMembers), len(newParticipants))

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
				prevCommitmentType: commitment.Type,
			}
			dispatcher.startLock.TryLock()

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.bufferLock.Lock()
			tssMgr.actionMap[sessionId] = dispatcher
			bufferSize := len(tssMgr.messageBuffer[sessionId])
			tssMgr.bufferLock.Unlock()

			// Trigger replay of any buffered messages for this session
			if bufferSize > 0 {
				fmt.Printf("[TSS] [RESHARE] Dispatcher registered, will replay buffered messages sessionId=%s bufferSize=%d\n",
					sessionId, bufferSize)
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
		fmt.Println("dispatcher started!")
		//If start fails then done is never possible
		//todo: handle this better
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic in dispatcher Start: %v", r)
					fmt.Printf("[TSS] [ACTIONS] PANIC recovered in dispatcher.Start sessionId=%s panic=%v\n",
						dispatcher.SessionId(), r)
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
			delete(tssMgr.messageBuffer, sessionId)
			delete(tssMgr.sessionMap, sessionId)
			tssMgr.bufferLock.Unlock()
		}
		fmt.Println("Start() err", err)
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
				fmt.Printf("[TSS] [ACTIONS] PANIC recovered in Done/Await goroutine panic=%v\n", r)
			}
		}()

		signedResults := make([]KeySignResult, 0)
		commitableResults := make([]tss_helpers.BaseCommitment, 0)
		for _, dsc := range startedDispatcher {
			resultPtr, err := dsc.Done().Await(context.Background())
			// fmt.Println("result, err", resultPtr, err)

			sessionId := dsc.SessionId()
			tssMgr.bufferLock.Lock()
			delete(tssMgr.sigChannels, sessionId)
			delete(tssMgr.actionMap, sessionId)
			delete(tssMgr.messageBuffer, sessionId)
			delete(tssMgr.sessionMap, sessionId)
			tssMgr.bufferLock.Unlock()
			if err != nil {
				fmt.Printf("[TSS] [KEYGEN] Dispatcher rejected sessionId=%s err=%v\n", dsc.SessionId(), err)
				continue
			}

			result := *resultPtr
			if result.Type() == KeyGenResultType {
				res := result.(KeyGenResult)

				res.BlockHeight = bh
				tssMgr.bufferLock.Lock()
				tssMgr.sessionResults[dsc.SessionId()] = res
				tssMgr.bufferLock.Unlock()

				fmt.Printf("[TSS] [KEYGEN] SUCCESS sessionId=%s keyId=%s blockHeight=%d epoch=%d pubKey=%x commitment=%s\n",
					res.SessionId, res.KeyId, res.BlockHeight, res.Epoch, res.PublicKey, res.Commitment)

				commitment := result.Serialize()
				commitableResults = append(commitableResults, commitment)
			} else if result.Type() == KeySignResultType {
				res := result.(KeySignResult)

				res.BlockHeight = bh
				tssMgr.bufferLock.Lock()
				tssMgr.sessionResults[dsc.SessionId()] = res
				tssMgr.bufferLock.Unlock()

				signedResults = append(signedResults, res)
			} else if result.Type() == ReshareResultType {
				res := result.(ReshareResult)

				res.BlockHeight = bh
				tssMgr.bufferLock.Lock()
				tssMgr.sessionResults[dsc.SessionId()] = res
				tssMgr.bufferLock.Unlock()

				fmt.Printf("[TSS] [RESHARE] SUCCESS sessionId=%s keyId=%s blockHeight=%d epoch=%d commitment=%s\n",
					res.SessionId, res.KeyId, res.BlockHeight, res.NewEpoch, res.Commitment)

				commitment := result.Serialize()
				commitment.BlockHeight = bh
				commitableResults = append(commitableResults, commitment)

			} else if result.Type() == ErrorType {
				res := result.(ErrorResult)
				res.BlockHeight = bh

				if res.tssErr != nil {
					culprits := make([]string, 0)
					for _, n := range res.tssErr.Culprits() {
						culprits = append(culprits, string(n.GetId()))
					}
					fmt.Printf("[TSS] [KEYGEN] ERROR (tss) sessionId=%s keyId=%s blockHeight=%d epoch=%d culprits=%v err=%v\n",
						res.SessionId, res.KeyId, res.BlockHeight, res.Epoch, culprits, res.tssErr.Error())
				} else if res.err != nil {
					fmt.Printf("[TSS] [KEYGEN] ERROR (internal) sessionId=%s keyId=%s blockHeight=%d epoch=%d err=%v\n",
						res.SessionId, res.KeyId, res.BlockHeight, res.Epoch, res.err)
				}

				tssMgr.bufferLock.Lock()
				tssMgr.sessionResults[dsc.SessionId()] = res
				tssMgr.bufferLock.Unlock()

				commitment := result.Serialize()
				commitment.BlockHeight = bh
				commitableResults = append(commitableResults, commitment)
			} else if result.Type() == TimeoutType {
				res := result.(TimeoutResult)

				res.BlockHeight = bh
				tssMgr.bufferLock.Lock()
				tssMgr.sessionResults[dsc.SessionId()] = res
				tssMgr.bufferLock.Unlock()

				fmt.Printf("[TSS] [TIMEOUT] Timeout result sessionId=%s keyId=%s blockHeight=%d epoch=%d culprits=%v\n",
					res.SessionId, res.KeyId, res.BlockHeight, res.Epoch, res.Culprits)
				commitment := result.Serialize()
				commitment.BlockHeight = bh
				commitableResults = append(commitableResults, commitment)

				// Schedule automatic retry for reshare timeouts with retry limit
				if dsc.KeyId() != "" {
					keyInfo, err := tssMgr.tssKeys.FindKey(dsc.KeyId())
					if err == nil {
						// Check retry count from session (in-memory) to avoid DB dependency
						retryCount := tssMgr.getRetryCount(dsc.KeyId())
						if retryCount < MAX_RESHARE_RETRIES {
							// Calculate exponential backoff delay based on retry count
							retryDelay := time.Duration(retryCount+1) * tssMgr.sconf.TssParams().ReshareSyncDelay

							fmt.Printf("[TSS] [RECOVERY] Scheduling reshare retry for timeout sessionId=%s keyId=%s retryCount=%d/%d delay=%v\n",
								dsc.SessionId(), dsc.KeyId(), retryCount+1, MAX_RESHARE_RETRIES, retryDelay)

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
							fmt.Printf("[TSS] [RECOVERY] Max reshare retries exceeded sessionId=%s keyId=%s maxRetries=%d\n",
								dsc.SessionId(), dsc.KeyId(), MAX_RESHARE_RETRIES)
							// Could trigger alert or manual intervention here
						}
					}
				}
			}
		}

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
						fmt.Println("Broadcast err", err)
					}
					fmt.Println("signature.txId", txId)
				}
			}()

			if len(commitableResults) > 0 {
				time.Sleep(tssMgr.sconf.TssParams().CommitDelay)

				fmt.Printf("[TSS] [COMMIT] Starting multi-sig collection blockHeight=%d count=%d\n", bh, len(commitableResults))

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

						fmt.Printf("[TSS] [COMMIT] Collecting sigs sessionId=%s keyId=%s type=%s epoch=%d\n",
							commitResult.SessionId, commitResult.KeyId, commitResult.Type, commitResult.Epoch)

						bytes, _ := common.EncodeDagCbor(commitResult)
						signableCid, _ := common.HashBytes(bytes, multicodec.DagCbor)

						ctx, cancel := context.WithTimeout(context.Background(), tssMgr.sconf.TssParams().WaitForSigsTimeout)
						defer cancel()

						serializedCircuit, err := tssMgr.waitForSigs(ctx, signableCid, commitResult.SessionId, &currentElection)

						if err != nil {
							fmt.Printf("[TSS] [COMMIT] waitForSigs FAILED sessionId=%s keyId=%s type=%s err=%v\n",
								commitResult.SessionId, commitResult.KeyId, commitResult.Type, err)
						} else {
							fmt.Printf("[TSS] [COMMIT] waitForSigs OK sessionId=%s keyId=%s type=%s\n",
								commitResult.SessionId, commitResult.KeyId, commitResult.Type)
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
				sigPacket := make(map[string]any, 0)
				for _, signResult := range commitedResults {
					if signResult.err == nil {
						canCommit = true
						sigPacket[signResult.commitment.SessionId] = map[string]any{
							"type":         signResult.commitment.Type,
							"session_id":   signResult.commitment.SessionId,
							"key_id":       signResult.commitment.KeyId,
							"commitment":   signResult.commitment.Commitment,
							"epoch":        signResult.commitment.Epoch,
							"pub_key":      signResult.commitment.PublicKey,
							"block_height": signResult.commitment.BlockHeight,

							"signature": signResult.circuit.Signature,
							"bv":        signResult.circuit.BitVector,
						}
					}
				}

				if !canCommit {
					fmt.Printf("[TSS] [COMMIT] No results reached threshold blockHeight=%d total=%d committed=%d\n",
						bh, len(commitableResults), len(commitedResults))
				}

				rawJson, err := json.Marshal(sigPacket)

				if canCommit {
					fmt.Printf("[TSS] [COMMIT] Broadcasting to Hive blockHeight=%d sessions=%d\n", bh, len(commitedResults))
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
						fmt.Printf("[TSS] [COMMIT] Hive broadcast FAILED blockHeight=%d err=%v\n", bh, err)
					} else {
						fmt.Printf("[TSS] [COMMIT] Hive broadcast OK blockHeight=%d\n", bh)
					}
				}
			}
		}
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

func (tssMgr *TssManager) waitForSigs(ctx context.Context, cid cid.Cid, sessionId string, election *elections.ElectionResult) (*dids.SerializedCircuit, error) {
	weightTotal := uint64(0)
	for _, weight := range election.Weights {
		weightTotal += weight
	}

	members := make([]dids.Member, 0)

	for _, member := range election.Members {
		members = append(members, dids.BlsDID(member.Key))
	}
	fmt.Println("waitForSigs.members", members)
	blsCircuit := dids.NewBlsCircuitGenerator(members)

	tssMgr.bufferLock.Lock()
	tssMgr.sigChannels[sessionId] = make(chan sigMsg)
	tssMgr.bufferLock.Unlock()

	tssMgr.pubsub.Send(p2pMessage{
		Type:    "ask_sigs",
		Account: tssMgr.config.Get().HiveUsername,
		Data: map[string]interface{}{
			"session_id": sessionId,
		},
	})

	fmt.Println("WAITING FOR SIGS commitedCid", cid)
	circuit, _ := blsCircuit.Generate(cid)

	var errRes error
	var res *dids.SerializedCircuit

	proc1 := make(chan struct{})
	go func() {
		signedWeight := uint64(0)

		// common.has
		tssMgr.bufferLock.RLock()
		sigChan := tssMgr.sigChannels[sessionId]
		tssMgr.bufferLock.RUnlock()

		signedMap := make(map[string]bool)
		for signedWeight*3 < weightTotal*2 {
			msg := <-sigChan

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

			fmt.Println("added, err", added, err)

			if added {
				signedWeight += election.Weights[index]
				signedMap[memberAccount] = true
			}
		}
		finalizedCiruit, err := circuit.Finalize()

		fmt.Println("finalizedCiruit, err", finalizedCiruit, err)

		serialized, err := finalizedCiruit.Serialize()

		if err != nil {
			errRes = err
		}

		res = serialized
		proc1 <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		fmt.Println("CONTEXT EXPIRED!!")
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
		messageBuffer:  make(map[string][]bufferedMessage),
		sessionMap:     make(map[string]sessionInfo),
		sessionResults: make(map[string]DispatcherResult),
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
