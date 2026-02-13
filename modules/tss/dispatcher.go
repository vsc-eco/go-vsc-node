package tss

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	// "github.com/btcsuite/btcd/btcec"

	"vsc-node/lib/utils"
	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/bnb-chain/tss-lib/v2/common"
	keyGenSecp256k1 "github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
	reshareSecp256k1 "github.com/bnb-chain/tss-lib/v2/ecdsa/resharing"
	keySignSecp256k1 "github.com/bnb-chain/tss-lib/v2/ecdsa/signing"
	keyGenEddsa "github.com/bnb-chain/tss-lib/v2/eddsa/keygen"
	reshareEddsa "github.com/bnb-chain/tss-lib/v2/eddsa/resharing"
	keySignEddsa "github.com/bnb-chain/tss-lib/v2/eddsa/signing"
	btss "github.com/bnb-chain/tss-lib/v2/tss"
	"github.com/chebyrash/promise"
	"github.com/decred/dcrd/dcrec/edwards/v2"
	"github.com/eager7/dogd/btcec"
	"github.com/ipfs/go-datastore"
)

type Participant struct {
	Account string
	PartyId *btss.PartyID
}

type SavedSecret struct {
	Data []byte
}

type Dispatcher interface {
	Start() error

	SessionId() string
	KeyId() string
	HandleP2P(msg []byte, from string, isBrcst bool, cmt string, fromCmt string)
	Done() *promise.Promise[DispatcherResult]
}

type ReshareDispatcher struct {
	BaseDispatcher

	//Filled externally
	newParticipants []Participant
	newEpoch        uint64

	//Filled internally
	newParty btss.Party
	newPids  btss.SortedPartyIDs
	result   *ReshareResult
}

func (dispatcher *ReshareDispatcher) Start() error {
	startTime := time.Now()
	fmt.Printf("[TSS] [RESHARE] Starting reshare sessionId=%s keyId=%s epoch=%d newEpoch=%d blockHeight=%d\n",
		dispatcher.sessionId, dispatcher.keyId, dispatcher.epoch, dispatcher.newEpoch, dispatcher.blockHeight)

	sortedPids, myParty, p2pCtx := dispatcher.baseInfo()

	userId := dispatcher.tssMgr.config.Get().HiveUsername

	fmt.Printf("[TSS] [RESHARE] Reshare participants: old=%d new=%d sessionId=%s\n",
		len(sortedPids), len(dispatcher.newParticipants), dispatcher.sessionId)

	epochIdx := makeEpochIdx(int(dispatcher.newEpoch))
	newPids := make([]*btss.PartyID, 0)
	for idx, p := range dispatcher.newParticipants {
		i := big.NewInt(0)
		i = i.SetBytes([]byte(p.Account))

		//modify the key value to trick tss-lib into thinking the ID is different
		if epochIdx != 0 {
			i = i.Mul(i, big.NewInt(int64(epochIdx+1)))
		}

		pi := btss.NewPartyID(p.Account, dispatcher.sessionId+"-new", i)
		dispatcher.newParticipants[idx].PartyId = pi

		newPids = append(newPids, pi)
	}

	//@todo add fail case when node is not in next party
	//@todo add fail case when node is not in old party
	var myNewParty *btss.PartyID
	dispatcher.newPids = btss.SortPartyIDs(newPids)
	for _, p := range dispatcher.newPids {
		if p.Id == userId {
			myNewParty = p
		}
	}

	newP2pCtx := btss.NewPeerContext(dispatcher.newPids)

	//@todo double check threshold calculation is correct
	threshold, _ := tss_helpers.GetThreshold(len(sortedPids))
	threshold++ //Add one

	newThreshold, _ := tss_helpers.GetThreshold(len(dispatcher.newPids))
	// newThreshold++

	fmt.Printf("[TSS] [RESHARE] Thresholds: old=%d/%d new=%d/%d sessionId=%s\n",
		threshold, len(sortedPids), newThreshold, len(dispatcher.newPids), dispatcher.sessionId)
	fmt.Println("newSortedPids", len(dispatcher.newPids), newThreshold, threshold)
	//epoch: 5 <-- actual data
	//epoch: 7 <-- likely empty

	savedKeyData, err := dispatcher.keystore.Get(context.Background(), makeKey("key", dispatcher.keyId, int(dispatcher.epoch)))

	fmt.Printf("[TSS] [RESHARE] Key retrieval: keyId=%s epoch=%d err=%v dataLen=%d sessionId=%s\n",
		dispatcher.keyId, dispatcher.epoch, err, len(savedKeyData), dispatcher.sessionId)
	fmt.Println("mem", err, len(savedKeyData))
	if err != nil {
		fmt.Printf("[TSS] [RESHARE] ERROR: Failed to retrieve key data: keyId=%s epoch=%d err=%v sessionId=%s\n",
			dispatcher.keyId, dispatcher.epoch, err, dispatcher.sessionId)
		return err
	}

	if myParty == nil && myNewParty == nil {
		err := fmt.Errorf("node not part of old or new committee")
		fmt.Printf("[TSS] [RESHARE] ERROR: %v sessionId=%s userId=%s\n", err, dispatcher.sessionId, userId)
		return err
	}

	fmt.Printf("[TSS] [RESHARE] Party membership: oldParty=%v newParty=%v sessionId=%s\n",
		myParty != nil, myNewParty != nil, dispatcher.sessionId)

	if dispatcher.algo == tss_helpers.SigningAlgoEcdsa {
		keydata := keyGenSecp256k1.LocalPartySaveData{}

		err = json.Unmarshal(savedKeyData, &keydata)

		fmt.Println("err", err)
		if err != nil {
			return err
		}

		end := make(chan *keyGenSecp256k1.LocalPartySaveData)
		endOld := make(chan *keyGenSecp256k1.LocalPartySaveData)

		save := keyGenSecp256k1.NewLocalPartySaveData(len(dispatcher.newPids))

		go dispatcher.reshareMsgs()

		fmt.Printf("[TSS] [RESHARE] Waiting 15s before starting parties sessionId=%s\n", dispatcher.sessionId)
		time.Sleep(15 * time.Second)

		go func() {
			//Check if old party is not nil (ie node is part of old committee)
			if myParty != nil {
				fmt.Printf("[TSS] [RESHARE] Starting old party ECDSA sessionId=%s partyId=%s\n",
					dispatcher.sessionId, myParty.Id)
				params := btss.NewReSharingParameters(btss.S256(), p2pCtx, newP2pCtx, myParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)

				dispatcher.party = reshareSecp256k1.NewLocalParty(params, keydata, dispatcher.p2pMsg, endOld)

				err := dispatcher.party.Start()

				if err != nil {
					fmt.Printf("[TSS] [RESHARE] ERROR: Old party start failed sessionId=%s err=%v\n",
						dispatcher.sessionId, err)
					fmt.Println("err", err)
					dispatcher.err = err
				} else {
					fmt.Printf("[TSS] [RESHARE] Old party started successfully sessionId=%s\n", dispatcher.sessionId)
				}
			}
		}()
		go func() {
			//Check if new party is not nil (ie will be in new committee)
			if myNewParty != nil {
				fmt.Printf("[TSS] [RESHARE] Starting new party ECDSA sessionId=%s partyId=%s\n",
					dispatcher.sessionId, myNewParty.Id)
				newParams := btss.NewReSharingParameters(btss.S256(), p2pCtx, newP2pCtx, myNewParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)

				dispatcher.newParty = reshareSecp256k1.NewLocalParty(newParams, save, dispatcher.p2pMsg, end)

				err := dispatcher.newParty.Start()

				if err != nil {
					fmt.Printf("[TSS] [RESHARE] ERROR: New party start failed sessionId=%s err=%v\n",
						dispatcher.sessionId, err)
					fmt.Println("err", err)
					dispatcher.err = err
				} else {
					fmt.Printf("[TSS] [RESHARE] New party started successfully sessionId=%s\n", dispatcher.sessionId)
				}
			}
		}()
		go func() {
			<-endOld
		}()
		go func() {
			for {
				reshareResult := <-end
				fmt.Printf("[TSS] [RESHARE] Reshare result received ECDSA sessionId=%s partyId=%s hasPubKey=%v\n",
					dispatcher.sessionId, dispatcher.newParty.PartyID().Id, reshareResult.ECDSAPub != nil)
				fmt.Println("reshareResult ECDSA", reshareResult, dispatcher.newParty.PartyID().Id)

				// fmt.Println("ECDSA reshareResult", reshareResult, reshareResult.ECDSAPub != nil)
				if reshareResult.ECDSAPub != nil {
					duration := time.Since(startTime)
					fmt.Printf("[TSS] [RESHARE] Reshare completed successfully ECDSA sessionId=%s duration=%v keyId=%s newEpoch=%d\n",
						dispatcher.sessionId, duration, dispatcher.keyId, dispatcher.newEpoch)

					keydata, _ := json.Marshal(reshareResult)

					k := makeKey("key", dispatcher.keyId, int(dispatcher.newEpoch))
					dispatcher.keystore.Put(context.Background(), k, keydata)

					dispatcher.result = &ReshareResult{
						Commitment:  dispatcher.tssMgr.setToCommitment(dispatcher.newParticipants, dispatcher.newEpoch),
						KeyId:       dispatcher.keyId,
						SessionId:   dispatcher.sessionId,
						NewEpoch:    dispatcher.newEpoch,
						BlockHeight: dispatcher.blockHeight,
					}

					dispatcher.done <- struct{}{}
				}
			}
		}()
	} else if dispatcher.algo == tss_helpers.SigningAlgoEddsa {
		fmt.Println("Starting reshareDispatcher!!")
		fmt.Println("dispatcher.newParticipants", dispatcher.newParticipants)
		fmt.Println("dispatcher.newEpoch", dispatcher.newEpoch)
		fmt.Println("dispatcher.algo", dispatcher.participants)

		keydata := keyGenEddsa.LocalPartySaveData{}

		err = json.Unmarshal(savedKeyData, &keydata)

		fmt.Println("err", err)
		if err != nil {
			return err
		}

		end := make(chan *keyGenEddsa.LocalPartySaveData)
		endOld := make(chan *keyGenEddsa.LocalPartySaveData)

		save := keyGenEddsa.NewLocalPartySaveData(len(dispatcher.newPids))

		go dispatcher.reshareMsgs()

		time.Sleep(15 * time.Second)

		go func() {
			if myParty != nil {
				params := btss.NewReSharingParameters(btss.Edwards(), p2pCtx, newP2pCtx, myParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)
				dispatcher.party = reshareEddsa.NewLocalParty(params, keydata, dispatcher.p2pMsg, endOld)

				fmt.Println("dispatcher.party", dispatcher.party)

				err := dispatcher.party.Start()

				if err != nil {
					fmt.Println("err", err)
					dispatcher.err = err
				}
			}
		}()

		go func() {
			if myNewParty != nil {
				newParams := btss.NewReSharingParameters(btss.Edwards(), p2pCtx, newP2pCtx, myNewParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)

				dispatcher.newParty = reshareEddsa.NewLocalParty(newParams, save, dispatcher.p2pMsg, end)

				fmt.Println("dispatcher.newParty", dispatcher.newParty)
				err := dispatcher.newParty.Start()

				if err != nil {
					fmt.Println("newParty.start()err", err)
					dispatcher.err = err
				}
			}
		}()

		go func() {
			<-endOld
		}()
		go func() {
			for {
				reshareResult := <-end

				// fmt.Println("pre-check reshareResult", reshareResult, dispatcher.newParty.PartyID().Id)

				fmt.Println("reshareResult EDDSA", reshareResult, dispatcher.newParty.PartyID().Id)
				if reshareResult.EDDSAPub == nil {
					continue
				}

				keydata, _ := json.Marshal(reshareResult)

				k := makeKey("key", dispatcher.keyId, int(dispatcher.newEpoch))
				dispatcher.keystore.Put(context.Background(), k, keydata)

				dispatcher.result = &ReshareResult{
					Commitment:  dispatcher.tssMgr.setToCommitment(dispatcher.newParticipants, dispatcher.newEpoch),
					KeyId:       dispatcher.keyId,
					SessionId:   dispatcher.sessionId,
					NewEpoch:    dispatcher.newEpoch,
					BlockHeight: dispatcher.blockHeight,
				}

				dispatcher.done <- struct{}{}
			}
		}()
	}

	dispatcher.baseStart()

	return nil
}

func (dispatcher *ReshareDispatcher) Done() *promise.Promise[DispatcherResult] {
	return promise.New(func(resolve func(DispatcherResult), reject func(error)) {
		<-dispatcher.done

		fmt.Printf("[TSS] [RESHARE] Done() called sessionId=%s timeout=%v hasTssErr=%v hasErr=%v\n",
			dispatcher.sessionId, dispatcher.timeout, dispatcher.tssErr != nil, dispatcher.err != nil)
		fmt.Println("OKAYISH", dispatcher.timeout, dispatcher.tssErr, dispatcher.err)
		if dispatcher.timeout {
			culprits := make(map[string]bool, 0)
			oldCulprits := make([]string, 0)
			newCulprits := make([]string, 0)
			if dispatcher.party != nil {
				for _, p := range dispatcher.party.WaitingFor() {
					culprits[p.Id] = true
					oldCulprits = append(oldCulprits, p.Id)
				}
			}
			if dispatcher.newParty != nil {
				for _, p := range dispatcher.newParty.WaitingFor() {
					culprits[p.Id] = true
					newCulprits = append(newCulprits, p.Id)
				}
			}

			fmt.Printf("[TSS] [RESHARE] TIMEOUT: sessionId=%s keyId=%s oldCulprits=%v newCulprits=%v totalCulprits=%d\n",
				dispatcher.sessionId, dispatcher.keyId, oldCulprits, newCulprits, len(culprits))

			culpritsList := make([]string, 0)
			for c := range culprits {
				culpritsList = append(culpritsList, c)
			}
			resolve(TimeoutResult{
				tssMgr:   dispatcher.tssMgr,
				Culprits: culpritsList,

				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,
				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			})
			return
		}

		if dispatcher.tssErr != nil {
			fmt.Printf("[TSS] [RESHARE] ERROR: TSS error occurred sessionId=%s keyId=%s err=%v\n",
				dispatcher.sessionId, dispatcher.keyId, dispatcher.tssErr)
			resolve(ErrorResult{
				tssErr:      dispatcher.tssErr,
				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,
				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			})
			return
		}

		if dispatcher.err != nil {
			fmt.Printf("[TSS] [RESHARE] ERROR: Dispatcher error occurred sessionId=%s keyId=%s err=%v\n",
				dispatcher.sessionId, dispatcher.keyId, dispatcher.err)
			fmt.Println("dispatcher.err", dispatcher.err)
			reject(dispatcher.err)
			return
		}
		fmt.Printf("[TSS] [RESHARE] SUCCESS: Reshare completed sessionId=%s keyId=%s newEpoch=%d\n",
			dispatcher.sessionId, dispatcher.keyId, dispatcher.newEpoch)
		fmt.Println("dispatcher.result", dispatcher.result)
		resolve(*dispatcher.result)
	})
}

func (dispatcher *ReshareDispatcher) HandleP2P(input []byte, fromStr string, isBrcst bool, cmt string, fromCmt string) {
	dispatcher.startWait()

	sortedIds, _, _ := dispatcher.baseInfo()

	var from *btss.PartyID

	if fromCmt == "old" {
		for _, p := range sortedIds {
			if p.Id == fromStr {
				from = p
				break
			}
		}
	} else if fromCmt == "new" {

		for _, p := range dispatcher.newParticipants {
			if p.PartyId.Id == fromStr {
				from = p.PartyId
				break
			}
		}
	}

	if from == nil {
		fmt.Printf("[TSS] [RESHARE] WARN: Received message from unknown participant sessionId=%s from=%s fromCmt=%s\n",
			dispatcher.sessionId, fromStr, fromCmt)
		return
	}

	fmt.Printf("[TSS] [RESHARE] Received message sessionId=%s from=%s isBroadcast=%v cmt=%s fromCmt=%s msgLen=%d\n",
		dispatcher.sessionId, fromStr, isBrcst, cmt, fromCmt, len(input))

	if cmt == "both" || cmt == "old" {
		go func() {
			ok, err := dispatcher.party.UpdateFromBytes(input, from, isBrcst)
			if err != nil {
				fmt.Printf("[TSS] [RESHARE] ERROR: UpdateFromBytes failed (old party) sessionId=%s from=%s ok=%v err=%v\n",
					dispatcher.sessionId, fromStr, ok, err)
				fmt.Println("UpdateFromBytes", ok, err)
				dispatcher.tssErr = err
			} else {
				dispatcher.lastMsg = time.Now()
				fmt.Printf("[TSS] [RESHARE] Message processed successfully (old party) sessionId=%s from=%s\n",
					dispatcher.sessionId, fromStr)
			}
		}()
	}
	if cmt == "both" || cmt == "new" {
		if dispatcher.newParty != nil {
			go func() {
				ok, err := dispatcher.newParty.UpdateFromBytes(input, from, isBrcst)

				if err != nil {
					fmt.Printf("[TSS] [RESHARE] ERROR: UpdateFromBytes failed (new party) sessionId=%s from=%s ok=%v err=%v\n",
						dispatcher.sessionId, fromStr, ok, err)
					fmt.Println("UpdateFromBytes", ok, err)
					dispatcher.tssErr = err
				} else {
					dispatcher.lastMsg = time.Now()
					fmt.Printf("[TSS] [RESHARE] Message processed successfully (new party) sessionId=%s from=%s\n",
						dispatcher.sessionId, fromStr)
				}
			}()
		}
	}
}

func (dispatcher *ReshareDispatcher) reshareMsgs() {
	go func() {
		for {
			msg := <-dispatcher.p2pMsg

			var commiteeType string
			if msg.IsToOldAndNewCommittees() {
				commiteeType = "both"
			} else if msg.IsToOldCommittee() {
				commiteeType = "old"
			} else if !msg.IsToOldCommittee() {
				commiteeType = "new"
			}

			var cmtFrom string
			for _, old := range dispatcher.participants {
				if slices.Compare(old.PartyId.GetKey(), msg.GetFrom().GetKey()) == 0 {
					cmtFrom = "old"
					break
				}
			}

			for _, newP := range dispatcher.newParticipants {
				if slices.Compare(newP.PartyId.GetKey(), msg.GetFrom().GetKey()) == 0 {
					cmtFrom = "new"
					break
				}
			}

			for _, to := range msg.GetTo() {
				bytes, _, err := msg.WireBytes()

				if err != nil {
					fmt.Printf("[TSS] [RESHARE] ERROR: WireBytes failed sessionId=%s to=%s err=%v\n",
						dispatcher.sessionId, to.Id, err)
					dispatcher.err = err
				}

				go func(targetId string, msgBytes []byte) {
					sendStart := time.Now()
					err = dispatcher.tssMgr.SendMsg(dispatcher.sessionId, Participant{
						Account: targetId,
					}, "", msgBytes, msg.IsBroadcast(), commiteeType, cmtFrom)
					sendDuration := time.Since(sendStart)
					if err != nil {
						fmt.Printf("[TSS] [RESHARE] ERROR: SendMsg failed sessionId=%s to=%s isBroadcast=%v msgLen=%d duration=%v err=%v\n",
							dispatcher.sessionId, targetId, msg.IsBroadcast(), len(msgBytes), sendDuration, err)
						fmt.Println("SendMsg direct info", err, targetId, len(msgBytes))
					} else {
						fmt.Printf("[TSS] [RESHARE] SendMsg success sessionId=%s to=%s isBroadcast=%v msgLen=%d duration=%v\n",
							dispatcher.sessionId, targetId, msg.IsBroadcast(), len(msgBytes), sendDuration)
					}
				}(to.Id, bytes)
			}
		}
	}()
}

type SignDispatcher struct {
	BaseDispatcher

	msg []byte

	result *KeySignResult
}

func (dispatcher *SignDispatcher) Start() error {

	sortedPids, myParty, p2pCtx := dispatcher.baseInfo()
	if myParty == nil {
		return fmt.Errorf("node not part of keygen committee")
	}

	fmt.Println("Dispatcher sign start", dispatcher, dispatcher.algo, tss_helpers.SigningAlgoEcdsa)
	if dispatcher.algo == tss_helpers.SigningAlgoEcdsa {
		m, err := tss_helpers.MsgToHashInt(dispatcher.msg, tss_helpers.SigningAlgoEcdsa)

		if err != nil {
			fmt.Println("sign.<err> 1", err)

			return err
		}
		keydata := keyGenSecp256k1.LocalPartySaveData{}
		k := makeKey("key", dispatcher.keyId, int(dispatcher.epoch))

		rawKey, err := dispatcher.keystore.Get(context.Background(), k)

		if err != nil {
			fmt.Println("sign.<err> 2", err)

			return err
		}
		json.Unmarshal(rawKey, &keydata)
		threshold, err := tss_helpers.GetThreshold(len(sortedPids))
		threshold++ //Add one

		if err != nil {

			fmt.Println("sign.<err> 3", err)
			return nil
		}

		params := btss.NewParameters(btss.S256(), p2pCtx, myParty, len(sortedPids), threshold)
		end := make(chan *common.SignatureData)

		dispatcher.party = keySignSecp256k1.NewLocalParty(m, params, keydata, dispatcher.p2pMsg, end)

		go dispatcher.handleMsgs()
		time.Sleep(15 * time.Second)
		go func() {
			fmt.Println("Starting Sign ECDSA")
			err := dispatcher.party.Start()

			if err != nil {
				fmt.Println("err", err)
				dispatcher.err = err
			}
		}()
		go func() {
			sigResult := <-end

			fmt.Println("sigResult.Signature", sigResult.Signature)

			// sigResult.R

			derSig := btcec.Signature{
				R: new(big.Int).SetBytes(sigResult.R),
				S: new(big.Int).SetBytes(sigResult.S),
			}

			dispatcher.result = &KeySignResult{
				Msg:       dispatcher.msg,
				Signature: derSig.Serialize(),
				KeyId:     dispatcher.keyId,
			}

			dispatcher.done <- struct{}{}
		}()

	} else if dispatcher.algo == tss_helpers.SigningAlgoEddsa {
		m, err := tss_helpers.MsgToHashInt(dispatcher.msg, tss_helpers.SigningAlgoEddsa)

		if err != nil {
			return err
		}
		keydata := keyGenEddsa.LocalPartySaveData{}

		k := makeKey("key", dispatcher.keyId, int(dispatcher.epoch))

		rawKey, err := dispatcher.keystore.Get(context.Background(), k)

		if err != nil {
			return err
		}

		err = json.Unmarshal(rawKey, &keydata)

		if err != nil {
			return err
		}

		// json.Unmarshal(dispatcher.savedKeyData, &keydata)
		threshold, err := tss_helpers.GetThreshold(len(sortedPids))
		threshold++ //Add one

		if err != nil {
			return nil
		}

		params := btss.NewParameters(btss.Edwards(), p2pCtx, myParty, len(sortedPids), threshold)
		end := make(chan *common.SignatureData)

		dispatcher.party = keySignEddsa.NewLocalParty(m, params, keydata, dispatcher.p2pMsg, end)

		go dispatcher.handleMsgs()
		time.Sleep(15 * time.Second)
		go func() {
			err := dispatcher.party.Start()

			if err != nil {
				fmt.Println("err", err)
				dispatcher.err = err
			}
		}()
		go func() {
			sigResult := <-end

			dispatcher.result = &KeySignResult{
				Msg:       dispatcher.msg,
				Signature: sigResult.Signature,
				KeyId:     dispatcher.keyId,
			}

			dispatcher.done <- struct{}{}
		}()
	}

	dispatcher.baseStart()
	return nil
}

func (dispatcher *SignDispatcher) Done() *promise.Promise[DispatcherResult] {
	return promise.New(func(resolve func(DispatcherResult), reject func(error)) {
		<-dispatcher.done

		if dispatcher.timeout {
			culprits := make([]string, 0)
			for _, p := range dispatcher.party.WaitingFor() {
				culprits = append(culprits, string(p.Key))
			}
			resolve(TimeoutResult{
				tssMgr: dispatcher.tssMgr,

				Culprits:    culprits,
				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,
				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			})
			return
		}

		if dispatcher.tssErr != nil {
			resolve(ErrorResult{
				tssErr: dispatcher.tssErr,

				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,
				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			})
			return
		}

		if dispatcher.err != nil {
			reject(dispatcher.err)
			return
		}

		resolve(*dispatcher.result)
	})
}

type BaseDispatcher struct {
	tssMgr *TssManager

	participants []Participant

	p2pMsg chan btss.Message

	sessionId string

	party btss.Party
	// newParty btss.Party

	keyId   string
	err     error
	tssErr  *btss.Error
	timeout bool
	// partyType string

	done chan struct{}

	keystore datastore.Datastore

	epoch       uint64
	algo        tss_helpers.SigningAlgo
	blockHeight uint64

	started   bool
	startLock sync.Mutex

	lastMsg time.Time
}

func (dispatcher *BaseDispatcher) handleMsgs() {
	go func() {
		for {
			msg := <-dispatcher.p2pMsg

			var commiteeType string
			if msg.IsToOldAndNewCommittees() {
				commiteeType = "both"
			} else if msg.IsToOldCommittee() {
				commiteeType = "old"
			} else if !msg.IsToOldCommittee() {
				commiteeType = "new"
			}

			if msg.IsBroadcast() {
				// bcounter = bcounter + 1
				bytes, _, err := msg.WireBytes()
				if err != nil {
					fmt.Println("sendMsg broadcast err: ", err)
				}
				for _, p := range dispatcher.participants {
					if p.Account == dispatcher.tssMgr.config.Get().HiveUsername {
						continue
					}

					go func() {
						err := dispatcher.tssMgr.SendMsg(dispatcher.sessionId, p, msg.WireMsg().From.Moniker, bytes, true, commiteeType, "")
						if err != nil {
							fmt.Println("SendMsg direct info", err, p.Account, len(bytes))
						}
					}()
				}
			} else {
				for _, to := range msg.GetTo() {
					bytes, _, err := msg.WireBytes()

					if err != nil {
						dispatcher.err = err
					}

					// fmt.Println("", string(to.Id))
					go func() {
						err := dispatcher.tssMgr.SendMsg(dispatcher.sessionId, Participant{
							Account: string(to.Id),
						}, to.Moniker, bytes, false, commiteeType, "")
						if err != nil {
							fmt.Println("SendMsg direct info", err, string(to.Id), len(bytes))
						}
					}()
				}
			}
		}
	}()
}

func (dispatcher *BaseDispatcher) startWait() {
	if !dispatcher.started {
		dispatcher.startLock.Lock()
		dispatcher.startLock.Unlock()
	}
}

func (dispatcher *BaseDispatcher) HandleP2P(input []byte, fromStr string, isBrcst bool, cmt string, cmtFrom string) {
	dispatcher.startWait()
	sortedIds, _, _ := dispatcher.baseInfo()

	var from *btss.PartyID
	for _, p := range sortedIds {
		if p.Id == fromStr {
			from = p
		}
	}

	if from == nil || dispatcher.party == nil {
		return
	}

	// fmt.Println("dispatcher.party", dispatcher.party, from)
	//Filter out any messages to self
	if dispatcher.party.PartyID().Id == from.Id {
		return
	}

	// fmt.Println("dispatcher.party", dispatcher.party, cmt)
	// if cmt == "both" || cmt == "old" {
	// 	fmt.Println("UPDATING OLD")
	// 	fmt.Println("Updating old", from)

	// }
	go func() {
		ok, err := dispatcher.party.UpdateFromBytes(input, from, isBrcst)

		fmt.Println("Update party", ok, len(input), from.Id, time.Now().String())
		if err != nil {
			fmt.Println("UpdateFromBytes", ok, err)
			dispatcher.tssErr = err
		} else {
			dispatcher.lastMsg = time.Now()
		}
	}()
	// if cmt == "both" || cmt == "new" {
	// 	if dispatcher.newParty != nil {
	// 		fmt.Println("UPDATING NEW")
	// 		ok, err := dispatcher.newParty.UpdateFromBytes(input, from, isBrcst)
	// 		if err != nil {
	// 			fmt.Println("UpdateFromBytes", ok, err)
	// 			dispatcher.tssErr = err
	// 		}
	// 	}
	// }
}

func (dsc *BaseDispatcher) SessionId() string {
	return dsc.sessionId
}

func (dsc *BaseDispatcher) KeyId() string {
	return dsc.keyId
}

func (dispatcher *BaseDispatcher) baseStart() {
	timeout := 1 * time.Minute
	dispatcher.lastMsg = time.Now()
	fmt.Printf("[TSS] [BASE] Starting timeout monitor sessionId=%s timeout=%v lastMsg=%v\n",
		dispatcher.sessionId, timeout, dispatcher.lastMsg)
	fmt.Println("baseStart lastMsg", dispatcher.lastMsg)
	go func() {
		for {
			elapsed := time.Since(dispatcher.lastMsg)
			if elapsed > timeout {
				dispatcher.timeout = true
				fmt.Printf("[TSS] [BASE] TIMEOUT: sessionId=%s elapsed=%v timeout=%v lastMsg=%v\n",
					dispatcher.sessionId, elapsed, timeout, dispatcher.lastMsg)
				dispatcher.done <- struct{}{}
				break
			}

			time.Sleep(time.Millisecond)
		}
	}()

	dispatcher.started = true
	dispatcher.startLock.Unlock()
}

func (dispatcher *BaseDispatcher) baseInfo() (btss.SortedPartyIDs, *btss.PartyID, *btss.PeerContext) {
	pIds := make([]*btss.PartyID, 0)
	for idx, p := range dispatcher.participants {
		i := big.NewInt(0)
		i = i.SetBytes([]byte(p.Account))

		pi := btss.NewPartyID(p.Account, dispatcher.sessionId, i)
		dispatcher.participants[idx].PartyId = pi

		pIds = append(pIds, pi)
	}
	sortedPids := btss.SortPartyIDs(pIds)
	p2pCtx := btss.NewPeerContext(sortedPids)

	userId := dispatcher.tssMgr.config.Get().HiveUsername

	var selfId *btss.PartyID
	for _, p := range sortedPids {
		if p.Id == userId {
			selfId = p
			break
		}
	}

	return sortedPids, selfId, p2pCtx
}

type KeyGenDispatcher struct {
	BaseDispatcher

	// secretChan chan tss_helpers.KeygenLocalState

	result *KeyGenResult
	// party      btss.Party

}

func (dispatcher *KeyGenDispatcher) Start() error {
	threshold, err := tss_helpers.GetThreshold(len(dispatcher.participants))
	threshold++

	pl := len(dispatcher.participants)

	if err != nil {
		return err
	}

	_, myParty, p2pCtx := dispatcher.baseInfo()

	if myParty == nil {
		return fmt.Errorf("node not part of keygen committee")
	}

	if dispatcher.algo == tss_helpers.SigningAlgoEcdsa {
		end := make(chan *keyGenSecp256k1.LocalPartySaveData)
		dispatcher.tssMgr.GeneratePreParams()
		preParams := <-dispatcher.tssMgr.preParams
		parameters := btss.NewParameters(btss.S256(), p2pCtx, myParty, pl, threshold)
		dispatcher.party = keyGenSecp256k1.NewLocalParty(parameters, dispatcher.p2pMsg, end, preParams)

		go dispatcher.handleMsgs()
		time.Sleep(15 * time.Second)
		go func() {
			err := dispatcher.party.Start()
			if err != nil {
				fmt.Println("party.Start() err", err)
				dispatcher.err = err
			}
		}()

		go func() {
			savedOutput := <-end

			pk := savedOutput.ECDSAPub

			pubKey := btcec.PublicKey{
				Curve: btss.S256(),
				X:     pk.X(),
				Y:     pk.Y(),
			}
			pubBytes := pubKey.SerializeCompressed()

			fmt.Println("Hex public key", hex.EncodeToString(pubBytes))

			bytes, _ := json.Marshal(savedOutput)

			k := makeKey("key", dispatcher.keyId, int(dispatcher.epoch))
			dispatcher.tssMgr.keyStore.Put(context.Background(), k, bytes)

			dispatcher.result = &KeyGenResult{
				PublicKey:   pubBytes,
				Commitment:  dispatcher.tssMgr.setToCommitment(dispatcher.participants, dispatcher.epoch),
				SavedSecret: bytes,
				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,

				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			}
			dispatcher.done <- struct{}{}
		}()
	} else if dispatcher.algo == tss_helpers.SigningAlgoEddsa {
		end := make(chan *keyGenEddsa.LocalPartySaveData)
		parameters := btss.NewParameters(btss.Edwards(), p2pCtx, myParty, pl, threshold)
		party := keyGenEddsa.NewLocalParty(parameters, dispatcher.p2pMsg, end)

		dispatcher.party = party

		go dispatcher.handleMsgs()
		time.Sleep(15 * time.Second)

		go func() {
			err := party.Start()
			if err != nil {
				dispatcher.err = err
			}
		}()

		go func() {
			savedOutput := <-end

			publicKey := edwards.NewPublicKey(savedOutput.EDDSAPub.X(), savedOutput.EDDSAPub.Y())

			pubBytes := publicKey.SerializeCompressed()

			// fmt.Println("pubHex ed25519", hex.EncodeToString(pubHex))
			bytes, _ := json.Marshal(savedOutput)

			k := makeKey("key", dispatcher.keyId, int(dispatcher.epoch))
			dispatcher.tssMgr.keyStore.Put(context.Background(), k, bytes)

			dispatcher.result = &KeyGenResult{
				PublicKey:   pubBytes,
				Commitment:  dispatcher.tssMgr.setToCommitment(dispatcher.participants, dispatcher.epoch),
				SavedSecret: bytes,
				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,

				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			}
			dispatcher.done <- struct{}{}
		}()
	}
	dispatcher.baseStart()
	return nil
}

func (dispatcher *KeyGenDispatcher) Done() *promise.Promise[DispatcherResult] {
	return promise.New(func(resolve func(DispatcherResult), reject func(error)) {
		<-dispatcher.done

		if dispatcher.timeout {
			culprits := make([]string, 0)
			for _, p := range dispatcher.party.WaitingFor() {
				culprits = append(culprits, string(p.Key))
			}
			resolve(TimeoutResult{
				tssMgr: dispatcher.tssMgr,

				Culprits:    culprits,
				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,
				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			})
			return
		}

		if dispatcher.tssErr != nil {
			resolve(ErrorResult{
				tssErr: dispatcher.tssErr,

				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,
				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			})
			return
		}

		if dispatcher.err != nil {
			reject(dispatcher.err)
			return
		}

		resolve(*dispatcher.result)
	})
}

type DispatcherResult interface {
	Type() DispatcherType
	Serialize() tss_helpers.BaseCommitment
}

type DispatcherType string

const (
	KeyGenResultType  DispatcherType = "keygen_result"
	KeySignResultType DispatcherType = "sign_result"
	ReshareResultType DispatcherType = "reshare_result"
	ErrorType         DispatcherType = "error_result"
	TimeoutType       DispatcherType = "timeout_result"
)

type KeyGenResult struct {
	tssMgr *TssManager

	PublicKey   []byte
	SavedSecret []byte
	SessionId   string
	KeyId       string
	BlockHeight uint64
	Epoch       uint64
	Commitment  string
}

func (KeyGenResult) Type() DispatcherType {
	return KeyGenResultType
}

func (result KeyGenResult) Serialize() tss_helpers.BaseCommitment {
	pubKey := hex.EncodeToString(result.PublicKey)

	return tss_helpers.BaseCommitment{
		Type:        "keygen",
		SessionId:   result.SessionId,
		KeyId:       result.KeyId,
		Commitment:  result.Commitment,
		PublicKey:   &pubKey,
		Metadata:    nil,
		BlockHeight: result.BlockHeight,
		Epoch:       result.Epoch,
	}
}

type KeySignResult struct {
	Msg       []byte
	Signature []byte

	KeyId       string
	SessionId   string
	BlockHeight uint64
	Epoch       uint64
}

func (KeySignResult) Type() DispatcherType {
	return KeySignResultType
}

func (result KeySignResult) Serialize() tss_helpers.BaseCommitment {
	// result.tssMgr.electionDb.GetElectionByHeight()
	return tss_helpers.BaseCommitment{
		Type:        "sign_result",
		SessionId:   result.SessionId,
		KeyId:       result.KeyId,
		Commitment:  "",
		PublicKey:   nil,
		Metadata:    nil,
		BlockHeight: result.BlockHeight,
		Epoch:       result.Epoch,
	}
}

type ReshareResult struct {
	// NewSecret []byte
	// NewSet      []Participant
	Commitment  string
	KeyId       string
	SessionId   string
	NewEpoch    uint64
	BlockHeight uint64
}

func (ReshareResult) Type() DispatcherType {
	return ReshareResultType
}

func (result ReshareResult) Serialize() tss_helpers.BaseCommitment {
	return tss_helpers.BaseCommitment{
		Type:        "reshare",
		SessionId:   result.SessionId,
		KeyId:       result.KeyId,
		Commitment:  result.Commitment,
		PublicKey:   nil,
		Metadata:    nil,
		BlockHeight: result.BlockHeight,
		Epoch:       result.NewEpoch,
	}
}

type ErrorResult struct {
	tssMgr *TssManager `json:"-"`
	err    error
	tssErr *btss.Error

	SessionId   string
	KeyId       string
	BlockHeight uint64
	Epoch       uint64
}

func (ErrorResult) Type() DispatcherType {
	return ErrorType
}

func (eres ErrorResult) Serialize() tss_helpers.BaseCommitment {
	if eres.tssErr != nil {
		blameNodes := make([]string, 0)
		for _, n := range eres.tssErr.Culprits() {
			blameNodes = append(blameNodes, string(n.GetId()))
		}

		fmt.Println(blameNodes)

		err := eres.tssErr.Error()
		// serialized, _ := json.Marshal(x)

		commitment := eres.tssMgr.setToCommitment(utils.Map(blameNodes, func(arg string) Participant {
			return Participant{
				Account: arg,
			}
		}), eres.Epoch)

		return tss_helpers.BaseCommitment{
			Type:       "blame",
			SessionId:  eres.SessionId,
			KeyId:      eres.KeyId,
			Commitment: commitment,
			Metadata: &tss_helpers.CommitmentMetadata{
				Error: &err,
			},
			BlockHeight: eres.BlockHeight,
			Epoch:       eres.Epoch,
		}
	} else {
		err := eres.err.Error()
		return tss_helpers.BaseCommitment{
			Type:       "blame",
			SessionId:  eres.SessionId,
			KeyId:      eres.KeyId,
			Commitment: "",
			Metadata: &tss_helpers.CommitmentMetadata{
				Error: &err,
			},
			BlockHeight: eres.BlockHeight,
			Epoch:       eres.Epoch,
		}
	}
}

type TimeoutResult struct {
	tssMgr *TssManager `json:"-"`

	Culprits    []string `json:"culprits"`
	SessionId   string   `json:"session_id"`
	KeyId       string   `json:"key_id"`
	BlockHeight uint64
	Epoch       uint64 `json:"epoch"`
}

func (TimeoutResult) Type() DispatcherType {
	return TimeoutType
}

func (result TimeoutResult) Serialize() tss_helpers.BaseCommitment {
	commitment := result.tssMgr.setToCommitment(utils.Map(result.Culprits, func(arg string) Participant {
		return Participant{
			Account: arg,
		}
	}), result.Epoch)

	return tss_helpers.BaseCommitment{
		Type:        "blame",
		KeyId:       result.KeyId,
		SessionId:   result.SessionId,
		Commitment:  commitment,
		PublicKey:   nil,
		Metadata:    nil,
		BlockHeight: result.BlockHeight,
		Epoch:       result.Epoch,
	}
}
