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

const TSS_SIGN_INTERVAL = 50 //* 2

// 5 minutes in blocks
const TSS_ROTATE_INTERVAL = 20 * 5

const TSS_ACTIVATE_HEIGHT = 102_083_000

// 24 hour blame
var BLAME_EXPIRE = uint64(24 * 60 * 20)

// TSS Reshare configuration constants
const TSS_RESHARE_SYNC_DELAY = 5 * time.Second      // Reduced from 15s to 5s
const TSS_RESHARE_TIMEOUT = 2 * time.Minute         // Increased from 1 minute to 2 minutes
const TSS_MESSAGE_RETRY_COUNT = 3                   // Number of retries for failed messages
const TSS_MESSAGE_RETRY_DELAY = 1 * time.Second     // Base delay for retries
const TSS_BAN_THRESHOLD_PERCENT = 25                // Failure rate threshold for bans (25%)
const TSS_BAN_GRACE_PERIOD_EPOCHS = 4               // Epochs before new nodes can be banned
const TSS_BUFFERED_MESSAGE_MAX_AGE = 1 * time.Minute // Maximum age for buffered messages

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

	if TSS_ACTIVATE_HEIGHT > bh {
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
		// fmt.Println("witnessSlot expression", witnessSlot.Account, tssMgr.config.Get().HiveUsername, bh%TSS_INTERVAL == 0)
		// if witnessSlot.Account == tssMgr.config.Get().HiveUsername && bh%TSS_INTERVAL == 0 {
		// 	fmt.Println("ME slot")
		// }

		isLeader := witnessSlot.Account == tssMgr.config.Get().HiveUsername

		keyLocks := make(map[string]bool)
		generatedActions := make([]QueuedAction, 0)
		if bh%TSS_ROTATE_INTERVAL == 0 {

			electionData, _ := tssMgr.electionDb.GetElectionByHeight(bh)

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
					fmt.Println("err", err)
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

			// cl := min(len(tssMgr.queuedActions), 5)
			// top5Actions := tssMgr.queuedActions[:cl]
			// tssMgr.RunActions(top5Actions, witnessSlot.Account, isLeader, bh)

			// tssMgr.queuedActions = slices.Delete(tssMgr.queuedActions, 0, cl)
		}
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

type scoreMap struct {
	bannedNodes map[string]bool
}

func (tss *TssManager) BlameScore() scoreMap {
	weightMap := make(map[string]int)
	nodeFirstEpoch := make(map[string]uint64) // Track first epoch each node appeared

	initialElection, _ := tss.electionDb.GetElectionByHeight(math.MaxInt64 - 1)
	elections := make(map[uint64]elections.ElectionResult, 0)
	elections[initialElection.Epoch] = initialElection

	epoch := initialElection.Epoch
	//Pull the last 7 days worth of elections
	epochCount := (4 * 7) - 1
	for i := 0; i < epochCount; i++ {
		epoch--
		election := tss.electionDb.GetElection(epoch)
		if election == nil {
			break
		}
		elections[epoch] = *election
		
		// Track first appearance of each node
		for _, member := range election.Members {
			if _, exists := nodeFirstEpoch[member.Account]; !exists {
				nodeFirstEpoch[member.Account] = epoch
			}
		}
	}

	currentEpoch := initialElection.Epoch
	blameMap := make(map[string]int)
	timeoutBlameMap := make(map[string]int)  // Separate tracking for timeouts vs errors
	errorBlameMap := make(map[string]int)

	for _, election := range elections {
		blames, _ := tss.tssCommitments.GetBlames(tss_db.ByEpoch(election.Epoch), tss_db.ByType("blame"))
		for _, member := range election.Members {
			weightMap[member.Account] += len(blames)
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
				if bv.Bit(idx) == 1 {
					blameMap[member.Account] += 1
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
			Account: account,
			Score:   blameMap[account],
			Weight:  weight,
			FirstEpoch: firstEpoch,
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
		if entry.EpochsSinceFirst < TSS_BAN_GRACE_PERIOD_EPOCHS {
			gracePeriodExemptions = append(gracePeriodExemptions, entry.Account)
			fmt.Printf("[TSS] [BLAME] Node in grace period (exempt from ban): account=%s score=%d weight=%d epochsSinceFirst=%d gracePeriod=%d\n",
				entry.Account, entry.Score, entry.Weight, entry.EpochsSinceFirst, TSS_BAN_GRACE_PERIOD_EPOCHS)
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
	// scoreMapBytes, _ := json.MarshalIndent(blameMap, "", "  ")
	// fmt.Println("scoreMap", string(scoreMapBytes), sortedArray)
	// fmt.Println("BlamedNodes", bannedNodes)

	return scoreMap{
		bannedNodes: bannedNodes,
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
		return
	}

	currentElection, err := tssMgr.electionDb.GetElectionByHeight(uint64(bh))

	fmt.Println("err (197)", currentElection, err)
	if err != nil {
		fmt.Println("err", err)
		// tssMgr
		tssMgr.lock.Unlock()
		return
	}

	blameMap := tssMgr.BlameScore()
	
	fmt.Printf("[TSS] [ACTIONS] Running actions blockHeight=%d isLeader=%v actionCount=%d bannedNodes=%d\n",
		bh, isLeader, len(actions), len(blameMap.bannedNodes))

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

			fmt.Println("bitset", bitset, isBlame)
			fmt.Println("lastBlame, err", lastBlame, err)
			fmt.Println("lastBlame.BlockHeight", lastBlame.BlockHeight, bh-BLAME_EXPIRE)
			for _, member := range currentElection.Members {
				// if isBlame {
				// 	if bitset.Bit(idx) == 1 {
				// 		continue
				// 	}
				// }
				//if node is banned
				if blameMap.bannedNodes[member.Account] {
					continue
				}
				participants = append(participants, Participant{
					Account: member.Account,
				})
			}

			fmt.Println("keygen dispatcher", participants)

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
				msg: action.Args,
			}
			dispatcher.startLock.TryLock()

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.bufferLock.Lock()
			tssMgr.actionMap[sessionId] = dispatcher
			tssMgr.bufferLock.Unlock()
		} else if action.Type == ReshareAction {

			participants := make([]Participant, 0)
			sessionId = "reshare-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId

			fmt.Printf("[TSS] [RESHARE] Creating reshare action sessionId=%s keyId=%s blockHeight=%d\n",
				sessionId, action.KeyId, bh)
			fmt.Println("reshare sessionId", sessionId)
			commitment, err := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "keygen", "reshare")
			lastBlame, _ := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "blame")

			//This should either be equal but never less in practical terms
			//However, we can add further checks
			if commitment.Epoch >= currentElection.Epoch || err != nil {
				fmt.Printf("[TSS] [RESHARE] Skipping reshare sessionId=%s keyId=%s commitmentEpoch=%d currentEpoch=%d err=%v\n",
					sessionId, action.KeyId, commitment.Epoch, currentElection.Epoch, err)
				fmt.Println("reshare skipping")
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
			//No idea what was happening here
			// if commitment.Type != "reshare" {
			// 	if lastBlame.BlockHeight > commitment.BlockHeight && lastBlame.BlockHeight > bh-BLAME_EXPIRE {
			// 		isBlame = true
			// 	}
			// }

			fmt.Println("commitment", commitment)
			commitmentElection := tssMgr.electionDb.GetElection(commitment.Epoch)

			commitmentBytes, err := base64.RawURLEncoding.DecodeString(commitment.Commitment)

			bitset := big.NewInt(0)
			bitset = bitset.SetBytes(commitmentBytes)

			commitedMembers := make([]Participant, 0)

			fmt.Println("commitment, err", commitment, err)
			fmt.Println("bitset", bitset, commitmentBytes)
			for idx, member := range commitmentElection.Members {
				if bitset.Bit(idx) == 1 {
					commitedMembers = append(commitedMembers, Participant{
						Account: member.Account,
					})
				}
			}

			newParticipants := make([]Participant, 0)
			excludedNodes := make([]string, 0)

			for idx, member := range currentElection.Members {
				fmt.Println("isBlame", isBlame, idx, blameBits.Bit(idx))
				// if isBlame {
				// 	if blameBits.Bit(idx) == 1 {
				// 		//Deny inclusion into next commitee due to previous blame
				// 		continue
				// 	}
				// }
				//if node is banned
				if blameMap.bannedNodes[member.Account] {
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

			dispatcher := &ReshareDispatcher{
				BaseDispatcher: BaseDispatcher{
					startLock:    sync.Mutex{},
					algo:         tss_helpers.SigningAlgo(action.Algo),
					tssMgr:       tssMgr,
					participants: commitedMembers,
					p2pMsg:       make(chan btss.Message, 4*len(participants)),
					sessionId:    sessionId,
					done:         make(chan struct{}),
					keyId:        action.KeyId,
					epoch:        commitment.Epoch,

					keystore:    tssMgr.keyStore,
					blockHeight: bh,
				},
				newParticipants: newParticipants,
				newEpoch:        currentElection.Epoch,
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
		tssMgr.sessionMap[sessionId] = sessionInfo{
			leader: leader,
			bh:     bh,
		}
	}

	startedDispatcher := make([]Dispatcher, 0)
	for _, dispatcher := range dispatchers {
		fmt.Println("dispatcher started!")
		//If start fails then done is never possible
		//todo: handle this better
		err := dispatcher.Start()

		if err == nil {
			startedDispatcher = append(startedDispatcher, dispatcher)
		} else {
			sessionId := dispatcher.SessionId()
			delete(tssMgr.sigChannels, sessionId)
			tssMgr.bufferLock.Lock()
			delete(tssMgr.actionMap, sessionId)
			delete(tssMgr.messageBuffer, sessionId)
			tssMgr.bufferLock.Unlock()
			delete(tssMgr.sessionMap, sessionId)
		}
		fmt.Println("Start() err", err)
	}

	go func() {

		signedResults := make([]KeySignResult, 0)
		commitableResults := make([]tss_helpers.BaseCommitment, 0)
		for _, dsc := range startedDispatcher {
			resultPtr, err := dsc.Done().Await(context.Background())
			// fmt.Println("result, err", resultPtr, err)

			sessionId := dsc.SessionId()
			delete(tssMgr.sigChannels, sessionId)
			tssMgr.bufferLock.Lock()
			delete(tssMgr.actionMap, sessionId)
			delete(tssMgr.messageBuffer, sessionId)
			tssMgr.bufferLock.Unlock()
			delete(tssMgr.sessionMap, sessionId)
			if err != nil {

				fmt.Println("Done() err", err, dsc.SessionId())
				//handle error
				continue
			}

			result := *resultPtr
			if result.Type() == KeyGenResultType {
				res := result.(KeyGenResult)

				res.BlockHeight = bh
				tssMgr.sessionResults[dsc.SessionId()] = res

				commitment := result.Serialize()
				commitableResults = append(commitableResults, commitment)
			} else if result.Type() == KeySignResultType {
				res := result.(KeySignResult)

				res.BlockHeight = bh
				tssMgr.sessionResults[dsc.SessionId()] = res

				signedResults = append(signedResults, res)
			} else if result.Type() == ReshareResultType {
				res := result.(ReshareResult)

				res.BlockHeight = bh
				tssMgr.sessionResults[dsc.SessionId()] = res
				commitment := result.Serialize()
				commitment.BlockHeight = bh
				commitableResults = append(commitableResults, commitment)

			} else if result.Type() == ErrorType {
				//TODO: Handle errors better. For now, ignore them

				// res := result.(ErrorResult)

				// res.BlockHeight = bh
				// tssMgr.sessionResults[dsc.SessionId()] = res
				// commitment := result.Serialize()
				// commitment.BlockHeight = bh
				// commitableResults = append(commitableResults, commitment)
			} else if result.Type() == TimeoutType {
				res := result.(TimeoutResult)

				res.BlockHeight = bh
				tssMgr.sessionResults[dsc.SessionId()] = res

				fmt.Printf("[TSS] [TIMEOUT] Timeout result sessionId=%s keyId=%s blockHeight=%d epoch=%d culprits=%v\n",
					res.SessionId, res.KeyId, res.BlockHeight, res.Epoch, res.Culprits)
				fmt.Println("Timeout Result", res, time.Now().String())
				commitment := result.Serialize()
				commitment.BlockHeight = bh
				commitableResults = append(commitableResults, commitment)
			}
		}
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
					// tssMgr.hiveClient.Broadcast([]hivego.HiveOperation{
					// 	deployOp,
					// }, &wif)
				}
			}()

			if len(commitableResults) > 0 {
				time.Sleep(5 * time.Second)

				commitedResults := make(map[string]struct {
					err        error
					circuit    *dids.SerializedCircuit
					commitment tss_helpers.BaseCommitment
				})
				var wg sync.WaitGroup
				for _, commitResult := range commitableResults {
					wg.Add(1)
					go func() {
						commitResult.BlockHeight = bh

						jj, _ := json.Marshal(commitResult)
						fmt.Println("Sign commitResult", commitResult, string(jj))
						bytes, _ := common.EncodeDagCbor(commitResult)

						signableCid, _ := common.HashBytes(bytes, multicodec.DagCbor)

						ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
						defer cancel()
						fmt.Println("commitResult", bytes, err)

						serializedCircuit, err := tssMgr.waitForSigs(ctx, signableCid, commitResult.SessionId, &currentElection)

						fmt.Println("serializedCircuit, err", serializedCircuit, err)

						if err == nil {
							commitedResults[commitResult.SessionId] = struct {
								err        error
								circuit    *dids.SerializedCircuit
								commitment tss_helpers.BaseCommitment
							}{
								err:        err,
								circuit:    serializedCircuit,
								commitment: commitResult,
							}
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

				rawJson, err := json.Marshal(sigPacket)

				fmt.Println("json.Marshal <err>", err)
				if canCommit {
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
						fmt.Println("Broadcast err", err)
					}
				}
			}
		}

		// fmt.Println("UNLOCKING!!!", tssMgr.config.Get().HiveUsername)
		tssMgr.lock.Unlock()
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

	tssMgr.sigChannels[sessionId] = make(chan sigMsg)

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
		sigChan := tssMgr.sigChannels[sessionId]

		signedMap := make(map[string]bool)
		for signedWeight < (weightTotal * 2 / 3) {
			msg := <-sigChan

			// fmt.Println("EMT", msg)
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

//Remaining important things to do:
// - Blame manager and error management
// - Register key data on chain
// - SDK bindings for key generation, signing, and deletion (optional support). Charge accordingly.
// - RC cost for signing ?
// - Key reshare support (key rotation)
// - Signing return sig through Done()
// - BLS sign output results

// func (tssMgr *TssManager) AskForSigs(sessionId string) {
// 	result := tssMgr.sessionResults[sessionId]

// 	result.Serialize()
// 	electionData, err := tssMgr.electionDb.GetElectionByHeight(math.MaxInt64)

// 	if err != nil {
// 		return
// 	}

// 	didList := make([]dids.BlsDID, 0)

// 	for _, member := range electionData.Members {
// 		didList = append(didList, dids.BlsDID(member.Key))
// 	}

// 	circuit := dids.NewBlsCircuitGenerator(didList)

// 	var signedMembers = make(map[dids.BlsDID]bool, 0)
// 	var msgCid cid.Cid
// 	partialCircuit, _ := circuit.Generate(msgCid)

// 	signedWeight := 0
// 	if tssMgr.sigChannels[sessionId] != nil {
// 		go func() {
// 			for {
// 				sig := <-tssMgr.sigChannels[sessionId]

// 				var member dids.BlsDID
// 				var index int
// 				for idx, mbr := range electionData.Members {
// 					if mbr.Account == sig.Account {
// 						member = dids.BlsDID(mbr.Key)
// 						index = idx
// 						break
// 					}
// 				}

// 				// fmt.Println("sig", sig)
// 				//Prevent replay attacks / double aggregation
// 				if !signedMembers[member] {
// 					verified, _ := partialCircuit.AddAndVerify(member, sig.Sig)

// 					if verified {
// 						signedMembers[member] = true
// 						signedWeight += int(electionData.Weights[index])
// 					}
// 				}
// 			}
// 		}()

// 		tssMgr.sigChannels[sessionId] = make(chan sigMsg)

// 		tssMgr.pubsub.Send(p2pMessage{
// 			Type:    "ask_sigs",
// 			Account: tssMgr.config.Get().HiveUsername,
// 			Data: map[string]interface{}{
// 				"session_id": sessionId,
// 			},
// 		})
// 	}
// }

func (tssMgr *TssManager) Init() error {
	tssMgr.VStream.RegisterBlockTick("tss-mgr", tssMgr.BlockTick, true)
	return nil
}

func (tssMgr *TssManager) KeyGen(keyId string, algo tss_helpers.SigningAlgo) int {
	tssMgr.queuedActions = append(tssMgr.queuedActions, QueuedAction{
		Type:  KeyGenAction,
		KeyId: keyId,
		Algo:  algo,
		Args:  nil,
	})

	return len(tssMgr.queuedActions)
}

func (tssMgr *TssManager) KeySign(msgs []byte, keyId string) (int, error) {
	keyInfo, err := tssMgr.tssKeys.FindKey(keyId)

	if err != nil {
		return 0, err
	}
	tssMgr.queuedActions = append(tssMgr.queuedActions, QueuedAction{
		Type:  SignAction,
		KeyId: keyId,
		Algo:  tss_helpers.SigningAlgo(keyInfo.Algo),
		Args:  msgs,
	})
	return len(tssMgr.queuedActions), nil
}

func (tssMgr *TssManager) KeyReshare(keyId string) (int, error) {
	keyInfo, err := tssMgr.tssKeys.FindKey(keyId)

	if err != nil {
		return 0, err
	}
	tssMgr.queuedActions = append(tssMgr.queuedActions, QueuedAction{
		Type:  ReshareAction,
		KeyId: keyId,
		Algo:  tss_helpers.SigningAlgo(keyInfo.Algo),
		Args:  nil,
	})
	return len(tssMgr.queuedActions), nil
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
	keystore *flatfs.Datastore,
	hiveClient hive.HiveTransactionCreator,
) *TssManager {
	preParams := make(chan ecKeyGen.LocalPreParams, 1)

	return &TssManager{
		preParamsLock: sync.Mutex{},
		sigChannels:   make(map[string]chan sigMsg),

		keyStore: keystore,

		config:     config,
		scheduler:  se,
		preParams:  preParams,
		p2p:        p2p,
		hiveClient: hiveClient,

		tssKeys:        tssKeys,
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
