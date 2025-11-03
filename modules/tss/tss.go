package tss

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
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
	libp2p "vsc-node/modules/p2p"
	tss_helpers "vsc-node/modules/tss/helpers"
	"vsc-node/modules/vstream"

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

const TSS_SIGN_INTERVAL = 20 //* 2

// 5 minutes in blocks
const TSS_ROTATE_INTERVAL = 20 * 5

const TSS_ACTIVATE_HEIGHT = 100_870_050

// 24 hour blame
var BLAME_EXPIRE = uint64(24 * 60 * 20)

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
	VStream    *vstream.VStream
	scheduler  GetScheduler
	hiveClient hive.HiveTransactionCreator

	//Active list of actions occurring
	queuedActions []QueuedAction
	lock          sync.Mutex
	preParamsLock sync.Mutex

	actionMap      map[string]Dispatcher
	sessionMap     map[string]sessionInfo
	sessionResults map[string]DispatcherResult
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
	if bh < *headHeight-10 {
		return
	}

	if bh > TSS_ACTIVATE_HEIGHT {
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

	participants := make([]Participant, 0)

	dispatchers := make([]Dispatcher, 0)
	for idx, action := range actions {
		var sessionId string
		if action.Type == KeyGenAction {
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
			for idx, member := range currentElection.Members {
				if isBlame {
					if bitset.Bit(idx) == 1 {
						continue
					}
				}
				participants = append(participants, Participant{
					Account: member.Account,
				})
			}

			fmt.Println("keygen dispatcher", participants)
			dispatcher := &KeyGenDispatcher{
				BaseDispatcher: BaseDispatcher{
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

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.actionMap[sessionId] = dispatcher

		} else if action.Type == SignAction {
			sessionId = "sign-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId

			// commitment, _ := tssMgr.tssCommitments.GetLatestCommitment(action.KeyId, "reshare")

			keyInfo, _ := tssMgr.tssKeys.FindKey(action.KeyId)

			participants := make([]Participant, 0)

			for _, member := range currentElection.Members {
				participants = append(participants, Participant{
					Account: member.Account,
				})
			}

			dispatcher := &SignDispatcher{
				BaseDispatcher: BaseDispatcher{
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

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.actionMap[sessionId] = dispatcher
		} else if action.Type == ReshareAction {
			sessionId = "reshare-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId

			fmt.Println("reshare sessionId", sessionId)
			commitment, err := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "keygen", "reshare")
			lastBlame, _ := tssMgr.tssCommitments.GetCommitmentByHeight(action.KeyId, bh, "blame")

			//This should either be equal but never less in practical terms
			//However, we can add further checks
			if commitment.Epoch >= currentElection.Epoch {
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

			for idx, member := range currentElection.Members {
				fmt.Println("isBlame", isBlame, idx, blameBits.Bit(idx))
				if isBlame {
					if blameBits.Bit(idx) == 1 {
						//Deny inclusion into next commitee due to previous blame
						continue
					}
				}
				newParticipants = append(newParticipants, Participant{
					Account: member.Account,
				})
			}

			// fmt.Println("reshare", action.KeyId)
			// newParticipants := make([]Participant, len(participants))
			// copy(newParticipants, participants)

			dispatcher := &ReshareDispatcher{
				BaseDispatcher: BaseDispatcher{
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

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.actionMap[sessionId] = dispatcher
		}
		tssMgr.sessionMap[sessionId] = sessionInfo{
			leader: leader,
			bh:     bh,
		}
	}

	for _, dispatcher := range dispatchers {
		fmt.Println("dispatcher started!")
		err := dispatcher.Start()

		fmt.Println("Start() err", err)
	}

	go func() {

		signedResults := make([]KeySignResult, 0)
		commitableResults := make([]tss_helpers.BaseCommitment, 0)
		for _, dsc := range dispatchers {
			resultPtr, err := dsc.Done().Await(context.Background())
			// fmt.Println("result, err", resultPtr, err)

			if err != nil {

				fmt.Println("Done() err", err)
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
				res := result.(ErrorResult)

				res.BlockHeight = bh
				tssMgr.sessionResults[dsc.SessionId()] = res
				commitment := result.Serialize()
				commitment.BlockHeight = bh
				commitableResults = append(commitableResults, commitment)
			} else if result.Type() == TimeoutType {
				res := result.(TimeoutResult)

				res.BlockHeight = bh
				tssMgr.sessionResults[dsc.SessionId()] = res

				fmt.Println("Timeout Result")
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
						RequiredAuths:        []string{},
						RequiredPostingAuths: []string{tssMgr.config.Get().HiveUsername},
						Id:                   "vsc.tss_sign",
						Json:                 string(rawJson),
					}

					// wif := tssMgr.config.Get().HiveActiveKey

					hiveTx := tssMgr.hiveClient.MakeTransaction([]hivego.HiveOperation{
						deployOp,
					})
					sig, _ := tssMgr.hiveClient.Sign(hiveTx)
					hiveTx.AddSig(sig)
					tssMgr.hiveClient.PopulateSigningProps(&hiveTx, nil)
					tssMgr.hiveClient.Broadcast(hiveTx)
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

						commitedResults[commitResult.SessionId] = struct {
							err        error
							circuit    *dids.SerializedCircuit
							commitment tss_helpers.BaseCommitment
						}{
							err:        err,
							circuit:    serializedCircuit,
							commitment: commitResult,
						}

						wg.Done()
					}()
				}
				wg.Wait()

				sigPacket := make(map[string]any, 0)
				for _, signResult := range commitedResults {
					if signResult.err == nil {
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
				deployOp := hivego.CustomJsonOperation{
					RequiredAuths:        []string{tssMgr.config.Get().HiveUsername},
					RequiredPostingAuths: []string{},
					Id:                   "vsc.tss_commitment",
					Json:                 string(rawJson),
				}

				hiveTx := tssMgr.hiveClient.MakeTransaction([]hivego.HiveOperation{
					deployOp,
				})
				sig, _ := tssMgr.hiveClient.Sign(hiveTx)
				hiveTx.AddSig(sig)
				tssMgr.hiveClient.PopulateSigningProps(&hiveTx, nil)
				tssMgr.hiveClient.Broadcast(hiveTx)
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

	select {
	case <-ctx.Done():
		fmt.Println("CONTEXT EXPIRED!!")
		return nil, ctx.Err() // Return error if canceled
	default:
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
			return nil, err
		}

		return serialized, nil
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
	vstream *vstream.VStream,
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
