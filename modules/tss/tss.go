package tss

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"
	"vsc-node/lib/dids"
	"vsc-node/lib/utils"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/tss_keys"
	"vsc-node/modules/db/vsc/tss_requests"
	"vsc-node/modules/db/vsc/witnesses"
	libp2p "vsc-node/modules/p2p"
	tss_helpers "vsc-node/modules/tss/helpers"
	"vsc-node/modules/vstream"

	stateEngine "vsc-node/modules/state-processing"

	ecKeyGen "github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
	btss "github.com/bnb-chain/tss-lib/v2/tss"
	"github.com/ipfs/go-cid"

	"github.com/chebyrash/promise"
	gorpc "github.com/libp2p/go-libp2p-gorpc"
	rpc "github.com/libp2p/go-libp2p-gorpc"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	flatfs "github.com/ipfs/go-ds-flatfs"
)

const TSS_INTERVAL = 20 //* 2

type TssManager struct {
	p2p    *libp2p.P2PServer
	pubsub libp2p.PubSubService[p2pMessage]
	server *gorpc.Server
	client *gorpc.Client

	sigChannels map[string]chan sigMsg

	tssRequests tss_requests.TssRequests
	tssKeys     tss_keys.TssKeys

	keyStore *flatfs.Datastore

	//Generates a fresh set of local key params.
	//A new set of fresh pre params will be available after depletion
	preParams chan ecKeyGen.LocalPreParams

	witnessDb  witnesses.Witnesses
	electionDb elections.Elections
	config     common.IdentityConfig
	VStream    *vstream.VStream
	scheduler  GetScheduler

	//Active list of actions occurring
	queuedActions []QueuedAction
	lock          sync.Mutex

	actionMap      map[string]Dispatcher
	sessionMap     map[string]sessionInfo
	sessionResults map[string]DispatcherResult
}

type sessionInfo struct {
	leader string
	bh     uint64
}

func (tssMgr *TssManager) Receive() {}

func (tssMgr *TssManager) GeneratePreParams() {
	if len(tssMgr.preParams) == 0 {
		fmt.Println("Need to generate preparams")
		preParams, err := ecKeyGen.GeneratePreParams(time.Minute)
		if err == nil {
			tssMgr.preParams <- *preParams
		}
	}
}

func (tssMgr *TssManager) BlockTick(bh uint64, headHeight *uint64) {
	//First check if we are in sync or not
	if bh < *headHeight-10 {
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

	// fmt.Println("witnessSlot", witnessSlot, schedule)
	// tssMgr.activeActions
	if witnessSlot != nil {
		// fmt.Println("witnessSlot expression", witnessSlot.Account, tssMgr.config.Get().HiveUsername, bh%TSS_INTERVAL == 0)
		// if witnessSlot.Account == tssMgr.config.Get().HiveUsername && bh%TSS_INTERVAL == 0 {
		// 	fmt.Println("ME slot")
		// }

		isWitness := witnessSlot.Account == tssMgr.config.Get().HiveUsername

		if bh%TSS_INTERVAL == 0 {
			cl := min(len(tssMgr.queuedActions), 5)
			top5Actions := tssMgr.queuedActions[:cl]
			tssMgr.RunActions(top5Actions, witnessSlot.Account, isWitness, bh)

			tssMgr.queuedActions = slices.Delete(tssMgr.queuedActions, 0, cl)
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
	if !locked {
		return
	}

	election, err := tssMgr.electionDb.GetElectionByHeight(uint64(bh))

	if err != nil {
		return
	}

	participants := make([]Participant, 0)

	for _, member := range election.Members {
		participants = append(participants, Participant{
			Account: member.Account,
		})
	}

	dispatchers := make([]Dispatcher, 0)
	for idx, action := range actions {
		var sessionId string
		if action.Type == KeyGenAction {

			sessionId = "keygen-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId
			dispatcher := &KeyGenDispatcher{
				BaseDispatcher: BaseDispatcher{
					tssMgr:       tssMgr,
					participants: participants,
					p2pMsg:       make(chan btss.Message, 2*len(participants)),
					sessionId:    sessionId,
					done:         make(chan struct{}),

					keyId: action.KeyId,
				},
				Algo:       action.Algo,
				secretChan: make(chan tss_helpers.KeygenLocalState),
			}

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.actionMap[sessionId] = dispatcher

		} else if action.Type == SignAction {

			sessionId = "sign-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId

			dispatcher := &SignDispatcher{
				BaseDispatcher: BaseDispatcher{
					tssMgr:       tssMgr,
					participants: participants,
					p2pMsg:       make(chan btss.Message, 2*len(participants)),
					sessionId:    sessionId,
					done:         make(chan struct{}),

					keyId: action.KeyId,

					keystore: tssMgr.keyStore,
				},
				Algo: action.Algo,
			}

			dispatchers = append(dispatchers, dispatcher)
			tssMgr.actionMap[sessionId] = dispatcher
		} else if action.Type == ReshareAction {
			sessionId = "reshare-" + strconv.Itoa(int(bh)) + "-" + strconv.Itoa(idx) + "-" + action.KeyId

			fmt.Println("reshare", action.KeyId)
			dispatcher := &ReshareDispatcher{
				BaseDispatcher: BaseDispatcher{
					tssMgr:       tssMgr,
					participants: participants,
					p2pMsg:       make(chan btss.Message, 2*len(participants)),
					sessionId:    sessionId,
					done:         make(chan struct{}),

					keyId: action.KeyId,

					keystore: tssMgr.keyStore,
				},
				epoch:           4,
				Algo:            action.Algo,
				newParticipants: participants,
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
		dispatcher.Start()
	}

	go func() {

		for _, dsc := range dispatchers {
			resultPtr, err := dsc.Done().Await(context.Background())
			fmt.Println("result, err", resultPtr, err)

			if err != nil {
				//handle error
				continue
			}

			result := *resultPtr
			if result.Type() == KeyGenResultType {
				res := result.(KeyGenResult)

				k := makeKey("key", dsc.KeyId())

				fmt.Println("k is", string(k.Bytes()))

				err := tssMgr.keyStore.Put(context.Background(), k, res.SavedSecret)

				if err != nil {
					fmt.Println("<err>", err)
					return
				}

				tssMgr.AskForSigs(dsc.SessionId())
				tssMgr.sessionResults[dsc.SessionId()] = res
			} else if result.Type() == KeySignResultType {
				res := result.(KeySignResult)

				res.Type()

				tssMgr.AskForSigs(dsc.SessionId())
				tssMgr.sessionResults[dsc.SessionId()] = res
			} else if result.Type() == ReshareResultType {
				res := result.(ReshareResult)

				k := makeKey("key", dsc.KeyId())
				fmt.Println("key saved", dsc.KeyId())
				err := tssMgr.keyStore.Put(context.Background(), k, res.NewSecret)

				fmt.Println("<err>", err)

				tssMgr.AskForSigs(dsc.SessionId())
				tssMgr.sessionResults[dsc.SessionId()] = res
			} else if result.Type() == ErrorType {
				res := result.(ErrorResult)

				res.Type()

				badNodes := make([]string, 0)
				for _, p := range res.tssErr.Culprits() {
					badNodes = append(badNodes, string(p.Key))
				}

				fmt.Println("badNodes", badNodes)
				tssMgr.sessionResults[dsc.SessionId()] = res
			} else if result.Type() == TimeoutType {
				res := result.(TimeoutResult)

				res.Type()

				fmt.Println("culprits", res.Culprits)

				tssMgr.sessionResults[dsc.SessionId()] = res
			}
		}
		tssMgr.lock.Unlock()
	}()
}

//Remaining important things to do:
// - Blame manager and error management
// - Register key data on chain
// - SDK bindings for key generation, signing, and deletion (optional support). Charge accordingly.
// - RC cost for signing ?
// - Key reshare support (key rotation)
// - Signing return sig through Done()
// - BLS sign output results

func (tssMgr *TssManager) AskForSigs(sessionId string) {
	electionData, err := tssMgr.electionDb.GetElectionByHeight(math.MaxInt64)

	if err != nil {
		return
	}

	didList := make([]dids.BlsDID, 0)

	for _, member := range electionData.Members {
		didList = append(didList, dids.BlsDID(member.Key))
	}

	circuit := dids.NewBlsCircuitGenerator(didList)

	var signedMembers = make(map[dids.BlsDID]bool, 0)
	var msgCid cid.Cid
	partialCircuit, err := circuit.Generate(msgCid)

	signedWeight := 0
	if tssMgr.sigChannels[sessionId] != nil {
		go func() {
			for {
				sig := <-tssMgr.sigChannels[sessionId]

				var member dids.BlsDID
				var index int
				for idx, mbr := range electionData.Members {
					if mbr.Account == sig.Account {
						member = dids.BlsDID(mbr.Key)
						index = idx
						break
					}
				}

				fmt.Println("sig", sig)

				//Prevent replay attacks / double aggregation
				if !signedMembers[member] {
					verified, err := partialCircuit.AddAndVerify(member, sig.Sig)

					fmt.Println("verified <err>", verified, err)
					if verified {
						signedMembers[member] = true
						signedWeight += int(electionData.Weights[index])
					}
				}
			}
		}()

		tssMgr.sigChannels[sessionId] = make(chan sigMsg, 0)

		tssMgr.pubsub.Send(p2pMessage{
			Type: "ask_sigs",
			Data: map[string]interface{}{
				"session_id": sessionId,
			},
		})
	}
}

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

func (tssMgr *TssManager) KeySign(msgs []byte, keyId string, algo tss_helpers.SigningAlgo) int {
	tssMgr.queuedActions = append(tssMgr.queuedActions, QueuedAction{
		Type:  SignAction,
		KeyId: keyId,
		Algo:  algo,
		Args:  [][]byte{msgs},
	})
	return len(tssMgr.queuedActions)
}

func (tssMgr *TssManager) KeyReshare(keyId string, algo tss_helpers.SigningAlgo) int {
	tssMgr.queuedActions = append(tssMgr.queuedActions, QueuedAction{
		Type:  ReshareAction,
		KeyId: keyId,
		Algo:  algo,
		Args:  nil,
	})
	return len(tssMgr.queuedActions)
}

func (tss *TssManager) SendMsg(sessionId string, participant Participant, moniker string, msg []byte, isBroadcast bool, commiteeType string) error {
	witness, err := tss.witnessDb.GetWitnessAtHeight(participant.Account, nil)

	// fmt.Println("participant.Account", participant.Account)
	if err != nil {
		fmt.Println("GetWitnessAtHeight", err)
		return err
	}

	peerId, err := peer.Decode(witness.PeerId)

	if err != nil {
		return err
	}

	tMsg := TMsg{
		IsBroadcast: isBroadcast,
		SessionId:   sessionId,
		Type:        "msg",
		Data:        msg,
		Cmt:         commiteeType,
	}
	tRes := TRes{}

	fmt.Println("Sending message from", tss.config.Get().HiveUsername, "to", participant.Account, "isBroadcast", isBroadcast)
	return tss.client.Call(peerId, "vsc.tss", "ReceiveMsg", &tMsg, &tRes)
}

var protocolId = protocol.ID("/vsc.network/tss/1.0.0")

func (tssMgr *TssManager) Start() *promise.Promise[any] {
	tssRpc := TssRpc{
		mgr: tssMgr,
	}
	server := rpc.NewServer(tssMgr.p2p.Host(), protocolId)
	err := server.RegisterName("vsc.tss", &tssRpc)

	if err != nil {
		return utils.PromiseReject[any](err)
	}

	client := rpc.NewClientWithServer(tssMgr.p2p.Host(), protocolId, server)

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
	tssKeys tss_keys.TssKeys,
	tssRequests tss_requests.TssRequests,
	witnessDb witnesses.Witnesses,
	electionDb elections.Elections,
	vstream *vstream.VStream,
	se GetScheduler,
	config common.IdentityConfig,
	keystore *flatfs.Datastore,
) *TssManager {
	preParams := make(chan ecKeyGen.LocalPreParams, 1)

	return &TssManager{

		sigChannels: make(map[string]chan sigMsg),

		keyStore: keystore,

		config:    config,
		scheduler: se,
		preParams: preParams,
		p2p:       p2p,

		tssKeys:     tssKeys,
		tssRequests: tssRequests,

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
