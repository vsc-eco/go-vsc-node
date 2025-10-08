package tss

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	// "github.com/btcsuite/btcd/btcec"

	tss_helpers "vsc-node/modules/tss/helpers"

	"github.com/bnb-chain/tss-lib/v2/common"
	keyGenSecp256k1 "github.com/bnb-chain/tss-lib/v2/ecdsa/keygen"
	reshareSecp256k1 "github.com/bnb-chain/tss-lib/v2/ecdsa/resharing"
	keySignSecp256k1 "github.com/bnb-chain/tss-lib/v2/ecdsa/signing"
	keyGenEddsa "github.com/bnb-chain/tss-lib/v2/eddsa/keygen"
	reshareEddsa "github.com/bnb-chain/tss-lib/v2/eddsa/resharing"
	keySignEddsa "github.com/bnb-chain/tss-lib/v2/eddsa/signing"
	"github.com/bnb-chain/tss-lib/v2/tss"
	btss "github.com/bnb-chain/tss-lib/v2/tss"
	"github.com/chebyrash/promise"
	"github.com/davidlazar/go-crypto/encoding/base32"
	"github.com/decred/dcrd/dcrec/edwards/v2"
	"github.com/eager7/dogd/btcec"
	"github.com/ipfs/go-datastore"
)

func makeKey(t string, id string) datastore.Key {
	k1 := base32.EncodeToString([]byte(t + "-" + base64.RawURLEncoding.EncodeToString([]byte(id))))
	k := datastore.NewKey(strings.ToUpper(k1))

	return k
}

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
	HandleP2P(msg []byte, from string, isBrcst bool, cmt string)
	Done() *promise.Promise[DispatcherResult]
}

type ReshareDispatcher struct {
	BaseDispatcher
	Algo tss_helpers.SigningAlgo

	result          *ReshareResult
	newParticipants []Participant
	newParty        btss.Party
	newPids         btss.SortedPartyIDs
	epoch           uint64
}

func (dispatcher *ReshareDispatcher) Start() error {
	sortedPids, myParty, p2pCtx := dispatcher.baseInfo()

	userId := dispatcher.tssMgr.config.Get().HiveUsername

	epochIdx := dispatcher.epoch % 3
	newPids := make([]*btss.PartyID, 0)
	for idx, p := range dispatcher.newParticipants {
		i := big.NewInt(0)
		i = i.SetBytes([]byte(p.Account))

		//modify the key value to trick tss-lib into thinking the ID is different
		if epochIdx != 0 {
			fmt.Println("epochIdx", epochIdx+1)
			i = i.Mul(i, big.NewInt(int64(epochIdx+1)))
		}

		// fmt.Println("bytes", i)

		pi := btss.NewPartyID(p.Account, dispatcher.sessionId+"-new", i)
		dispatcher.newParticipants[idx].PartyId = pi

		fmt.Println("pi", pi)
		newPids = append(newPids, pi)
	}

	var myNewParty *btss.PartyID
	dispatcher.newPids = btss.SortPartyIDs(newPids)
	fmt.Println("dispatcher.newPids", dispatcher.newPids, dispatcher.participants)
	for _, p := range dispatcher.newPids {
		if p.Id == userId {
			myNewParty = p
		}
	}
	fmt.Println("newSortedPids", newPids, "sortedPids", sortedPids, "myNewParty", myNewParty)

	newP2pCtx := btss.NewPeerContext(dispatcher.newPids)
	threshold, _ := tss_helpers.GetThreshold(len(sortedPids))
	threshold++ //Add one

	newThreshold, _ := tss_helpers.GetThreshold(len(dispatcher.newPids))
	// newThreshold++

	fmt.Println("newSortedPids", len(dispatcher.newPids), newThreshold, threshold)
	savedKeyData, err := dispatcher.keystore.Get(context.Background(), makeKey("key", dispatcher.keyId))

	fmt.Println("mem", err, len(savedKeyData))
	if err != nil {
		return err
	}

	if dispatcher.Algo == tss_helpers.SigningAlgoSecp256k1 {
		keydata := keyGenSecp256k1.LocalPartySaveData{}

		err = json.Unmarshal(savedKeyData, &keydata)

		fmt.Println("err", err)
		if err != nil {
			return err
		}

		params := tss.NewReSharingParameters(tss.S256(), p2pCtx, newP2pCtx, myParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)
		end := make(chan *keyGenSecp256k1.LocalPartySaveData)
		// endNew := make(chan *keyGenEddsa.LocalPartySaveData)

		dispatcher.party = reshareSecp256k1.NewLocalParty(params, keydata, dispatcher.p2pMsg, end)

		save := keyGenSecp256k1.NewLocalPartySaveData(len(dispatcher.newPids))

		fmt.Println("savedData", save)

		newParams := tss.NewReSharingParameters(tss.S256(), p2pCtx, newP2pCtx, myNewParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)

		dispatcher.newParty = reshareSecp256k1.NewLocalParty(newParams, save, dispatcher.p2pMsg, end)

		go dispatcher.reshareMsgs()
		go func() {
			err := dispatcher.party.Start()

			if err != nil {
				fmt.Println("err", err)
				dispatcher.err = err
			}
		}()
		go func() {
			err := dispatcher.newParty.Start()

			if err != nil {
				fmt.Println("err", err)
				dispatcher.err = err
			}
		}()
		go func() {
			for {
				reshareResult := <-end

				fmt.Println("ECDSA reshareResult", reshareResult, reshareResult.ECDSAPub != nil)
				if reshareResult.ECDSAPub != nil {

					keydata, _ := json.Marshal(reshareResult)

					k := makeKey("key", dispatcher.keyId)
					dispatcher.keystore.Put(context.Background(), k, keydata)

					dispatcher.result = &ReshareResult{
						NewSet: []Participant{},
					}
					dispatcher.done <- struct{}{}
				}
			}
		}()
	} else if dispatcher.Algo == tss_helpers.SigningAlgoEd25519 {
		keydata := keyGenEddsa.LocalPartySaveData{}

		err = json.Unmarshal(savedKeyData, &keydata)

		fmt.Println("err", err)
		if err != nil {
			return err
		}

		params := tss.NewReSharingParameters(tss.Edwards(), p2pCtx, newP2pCtx, myParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)
		end := make(chan *keyGenEddsa.LocalPartySaveData)
		// endNew := make(chan *keyGenEddsa.LocalPartySaveData)

		dispatcher.party = reshareEddsa.NewLocalParty(params, keydata, dispatcher.p2pMsg, end)

		save := keyGenEddsa.NewLocalPartySaveData(len(dispatcher.newPids))

		fmt.Println("savedData", save)

		newParams := tss.NewReSharingParameters(tss.Edwards(), p2pCtx, newP2pCtx, myNewParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)

		dispatcher.newParty = reshareEddsa.NewLocalParty(newParams, save, dispatcher.p2pMsg, end)

		go dispatcher.reshareMsgs()
		go func() {
			err := dispatcher.party.Start()

			if err != nil {
				fmt.Println("err", err)
				dispatcher.err = err
			}
		}()

		go func() {
			err := dispatcher.newParty.Start()

			if err != nil {
				fmt.Println("newParty.start()err", err)
				dispatcher.err = err
			}
		}()
		go func() {
			for {
				reshareResult := <-end

				fmt.Println("pre-check reshareResult", reshareResult, dispatcher.newParty.PartyID().Id)
				if reshareResult.EDDSAPub == nil {
					continue
				}

				keydata, _ := json.Marshal(reshareResult)

				k := makeKey("key", dispatcher.keyId)
				dispatcher.keystore.Put(context.Background(), k, keydata)

				publicKey := edwards.NewPublicKey(reshareResult.EDDSAPub.X(), reshareResult.EDDSAPub.Y())

				pubHex := publicKey.SerializeCompressed()

				fmt.Println("pubHex ed25519", hex.EncodeToString(pubHex))
				fmt.Println("DONE reshare", reshareResult, reshareResult.EDDSAPub)
				// dispatcher.signature = sigResult.Signature

				dispatcher.result = &ReshareResult{
					NewSecret: keydata,
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

		fmt.Println("OKAYISH", dispatcher.timeout, dispatcher.tssErr, dispatcher.err)
		if dispatcher.timeout {
			oldCulprits := make([]string, 0)
			for _, p := range dispatcher.party.WaitingFor() {
				oldCulprits = append(oldCulprits, p.Id)
			}
			newCulprits := make([]string, 0)
			for _, p := range dispatcher.newParty.WaitingFor() {
				newCulprits = append(newCulprits, p.Id)
			}
			a, _, _ := dispatcher.baseInfo()

			fmt.Println("oldCulprits, newCulprits", oldCulprits, newCulprits, dispatcher.party.PartyID().Id)
			fmt.Println(dispatcher.newParticipants, dispatcher.newPids, "partyList", a)

			resolve(TimeoutResult{
				Culprits: append(oldCulprits, newCulprits...),
			})
			return
		}

		if dispatcher.tssErr != nil {
			resolve(ErrorResult{
				tssErr: dispatcher.tssErr,
			})
			return
		}

		if dispatcher.err != nil {

			fmt.Println("dispatcher.err", dispatcher.err)
			reject(dispatcher.err)
			return
		}
		fmt.Println("dispatcher.result", dispatcher.result)
		resolve(*dispatcher.result)
	})
}

func (dispatcher *ReshareDispatcher) HandleP2P(input []byte, fromStr string, isBrcst bool, cmt string) {
	sortedIds, _, _ := dispatcher.baseInfo()

	// msg, _ := tss.ParseWireMessage()

	var from *btss.PartyID
	for _, p := range sortedIds {
		if p.Id == fromStr {
			from = p
			break
		}
	}

	fmt.Println("from", from)

	var newFrom *btss.PartyID
	for _, p := range dispatcher.newParticipants {
		if p.PartyId.Id == fromStr {
			newFrom = p.PartyId
			break
		}
	}

	newFrom.GetId()

	fmt.Println("dispatcher.party", dispatcher.party.PartyID(), cmt)
	if cmt == "both" || cmt == "old" {
		fmt.Println("UPDATING OLD", from.Id, from.Index, newFrom.Index, dispatcher.party)

		ok, err := dispatcher.party.UpdateFromBytes(input, from, isBrcst)
		if err != nil {
			fmt.Println("UpdateFromBytes", ok, err)
			dispatcher.tssErr = err
		}
	}
	if cmt == "both" || cmt == "new" {
		if dispatcher.newParty != nil {
			fmt.Println("UPDATING NEW", from.Id, from.Index, newFrom.Index, dispatcher.party)
			ok, err := dispatcher.newParty.UpdateFromBytes(input, newFrom, isBrcst)

			if err != nil {
				fmt.Println("UpdateFromBytes", ok, err)
				dispatcher.tssErr = err
			}
		}
	}
}

func (dispatcher *ReshareDispatcher) reshareMsgs() {
	go func() {
		for {
			msg := <-dispatcher.p2pMsg

			fmt.Println("msg", msg.IsToOldCommittee(), msg.IsToOldAndNewCommittees())
			var commiteeType string
			if msg.IsToOldAndNewCommittees() {
				commiteeType = "both"
			} else if msg.IsToOldCommittee() {
				commiteeType = "old"
			} else if !msg.IsToOldCommittee() {
				commiteeType = "new"
			}

			for _, to := range msg.GetTo() {
				bytes, _, err := msg.WireBytes()

				if err != nil {
					dispatcher.err = err
				}

				fmt.Println("Sending direct to", string(to.Id), err)
				err = dispatcher.tssMgr.SendMsg(dispatcher.sessionId, Participant{
					Account: to.Id,
				}, to.Moniker, bytes, msg.IsBroadcast(), commiteeType)
				if err != nil {
					fmt.Println("SendMsg direct err", err)
					dispatcher.err = err
				}
			}
			// }
		}
	}()
}

type SignDispatcher struct {
	BaseDispatcher

	Algo tss_helpers.SigningAlgo

	savedKeyData []byte
	msg          []byte

	signature []byte

	result *KeySignResult
}

func (dispatcher *SignDispatcher) Start() error {

	sortedPids, myParty, p2pCtx := dispatcher.baseInfo()

	if dispatcher.Algo == tss_helpers.SigningAlgoSecp256k1 {
		m, err := tss_helpers.MsgToHashInt(dispatcher.msg, tss_helpers.SigningAlgoSecp256k1)

		if err != nil {
			return err
		}
		keydata := keyGenSecp256k1.LocalPartySaveData{}

		json.Unmarshal(dispatcher.savedKeyData, &keydata)
		threshold, err := tss_helpers.GetThreshold(len(sortedPids))
		threshold++ //Add one

		if err != nil {
			return nil
		}

		params := tss.NewParameters(tss.S256(), p2pCtx, myParty, len(sortedPids), threshold)
		end := make(chan *common.SignatureData, 0)

		dispatcher.party = keySignSecp256k1.NewLocalParty(m, params, keydata, dispatcher.p2pMsg, end)

		go dispatcher.handleMsgs()
		go func() {
			err := dispatcher.party.Start()

			if err != nil {
				fmt.Println("err", err)
			}
		}()
		go func() {
			sigResult := <-end
			// fmt.Println("sigResult", sigResult.GetSignature())

			dispatcher.signature = sigResult.Signature

			dispatcher.done <- struct{}{}
		}()

	} else if dispatcher.Algo == tss_helpers.SigningAlgoEd25519 {
		m, err := tss_helpers.MsgToHashInt(dispatcher.msg, tss_helpers.SigningAlgoEd25519)

		if err != nil {
			return err
		}
		keydata := keyGenEddsa.LocalPartySaveData{}

		k := makeKey("key", dispatcher.keyId)

		fmt.Println("key", dispatcher.keyId, string(k.Bytes()))

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

		params := tss.NewParameters(tss.Edwards(), p2pCtx, myParty, len(sortedPids), threshold)
		end := make(chan *common.SignatureData)

		dispatcher.party = keySignEddsa.NewLocalParty(m, params, keydata, dispatcher.p2pMsg, end)

		go dispatcher.handleMsgs()
		go func() {
			err := dispatcher.party.Start()

			if err != nil {
				fmt.Println("err", err)
			}
		}()
		go func() {
			sigResult := <-end

			dispatcher.signature = sigResult.Signature

			dispatcher.result = &KeySignResult{
				Msg:       dispatcher.msg,
				Signature: sigResult.Signature,
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

		fmt.Println("DONE")

		if dispatcher.timeout {
			culprits := make([]string, 0)
			for _, p := range dispatcher.party.WaitingFor() {
				culprits = append(culprits, string(p.Key))
			}
			resolve(TimeoutResult{
				Culprits: culprits,
			})
			return
		}

		if dispatcher.tssErr != nil {
			resolve(ErrorResult{
				tssErr: dispatcher.tssErr,
			})
			return
		}

		if dispatcher.err != nil {
			reject(dispatcher.err)
			return
		}

		resolve(KeySignResult{
			Msg:       dispatcher.msg,
			Signature: dispatcher.signature,
		})
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
}

func (dispatcher *BaseDispatcher) handleMsgs() {
	go func() {
		for {
			msg := <-dispatcher.p2pMsg

			fmt.Println("msg", msg.IsToOldCommittee(), msg.IsToOldAndNewCommittees())
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
						fmt.Println("SHOCKING")
						continue
					}

					err := dispatcher.tssMgr.SendMsg(dispatcher.sessionId, p, msg.WireMsg().From.Moniker, bytes, true, commiteeType)
					if err != nil {
						fmt.Println("SendMsg err", err)
						dispatcher.err = err
					}
				}
			} else {
				for _, to := range msg.GetTo() {
					bytes, _, err := msg.WireBytes()

					if err != nil {
						dispatcher.err = err
					}

					fmt.Println("Sending direct to", string(to.Id))
					err = dispatcher.tssMgr.SendMsg(dispatcher.sessionId, Participant{
						Account: string(to.Id),
					}, to.Moniker, bytes, false, commiteeType)
					if err != nil {
						fmt.Println("SendMsg direct err", err)
						dispatcher.err = err
					}
				}
			}
		}
	}()
}

func (dispatcher *BaseDispatcher) HandleP2P(input []byte, fromStr string, isBrcst bool, cmt string) {
	sortedIds, _, _ := dispatcher.baseInfo()

	var from *btss.PartyID
	for _, p := range sortedIds {
		if p.Id == fromStr {
			from = p
		}
	}

	fmt.Println("from", from)

	//Filter out any messages to self
	if dispatcher.party.PartyID().Id == from.Id {
		return
	}

	// fmt.Println("dispatcher.party", dispatcher.party, cmt)
	// if cmt == "both" || cmt == "old" {
	// 	fmt.Println("UPDATING OLD")
	// 	fmt.Println("Updating old", from)

	// }
	ok, err := dispatcher.party.UpdateFromBytes(input, from, isBrcst)
	if err != nil {
		fmt.Println("UpdateFromBytes", ok, err)
		dispatcher.tssErr = err
	}
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
	go func() {
		time.Sleep(1 * time.Minute)
		dispatcher.timeout = true

		dispatcher.done <- struct{}{}
	}()
}

func (dispatcher *BaseDispatcher) baseInfo() (btss.SortedPartyIDs, *btss.PartyID, *btss.PeerContext) {

	pIds := make([]*btss.PartyID, 0)
	for _, p := range dispatcher.participants {
		i := big.NewInt(0)
		i = i.SetBytes([]byte(p.Account))

		pi := btss.NewPartyID(p.Account, dispatcher.sessionId, i)

		pIds = append(pIds, pi)
	}
	sortedPids := btss.SortPartyIDs(pIds)
	p2pCtx := tss.NewPeerContext(sortedPids)

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

	Algo tss_helpers.SigningAlgo

	secretChan chan tss_helpers.KeygenLocalState

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

	_, selfId, p2pCtx := dispatcher.baseInfo()

	if dispatcher.Algo == tss_helpers.SigningAlgoSecp256k1 {
		end := make(chan *keyGenSecp256k1.LocalPartySaveData)
		preParams := <-dispatcher.tssMgr.preParams
		parameters := btss.NewParameters(btss.S256(), p2pCtx, selfId, pl, threshold)
		party := keyGenSecp256k1.NewLocalParty(parameters, dispatcher.p2pMsg, end, preParams)

		dispatcher.party = party

		go dispatcher.handleMsgs()
		go func() {

			err := party.Start()
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
			hexPubKey := pubKey.SerializeCompressed()

			fmt.Println("Hex public key", hex.EncodeToString(hexPubKey))

			bytes, _ := json.Marshal(savedOutput)

			fmt.Println("localSecrets", string(bytes), len(bytes))

			dispatcher.result = &KeyGenResult{
				PublicKey:   hexPubKey,
				SavedSecret: bytes,
			}
			dispatcher.done <- struct{}{}
		}()
	} else if dispatcher.Algo == tss_helpers.SigningAlgoEd25519 {
		end := make(chan *keyGenEddsa.LocalPartySaveData)
		parameters := btss.NewParameters(btss.Edwards(), p2pCtx, selfId, pl, threshold)
		party := keyGenEddsa.NewLocalParty(parameters, dispatcher.p2pMsg, end)

		dispatcher.party = party

		go dispatcher.handleMsgs()
		go func() {
			err := party.Start()
			if err != nil {
				dispatcher.err = err
			}
		}()

		go func() {
			savedOutput := <-end

			publicKey := edwards.NewPublicKey(savedOutput.EDDSAPub.X(), savedOutput.EDDSAPub.Y())

			pubHex := publicKey.SerializeCompressed()

			fmt.Println("pubHex ed25519", hex.EncodeToString(pubHex))
			bytes, _ := json.Marshal(savedOutput)

			dispatcher.result = &KeyGenResult{
				PublicKey:   pubHex,
				SavedSecret: bytes,
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
				Culprits: culprits,
			})
			return
		}

		if dispatcher.tssErr != nil {
			resolve(ErrorResult{
				tssErr: dispatcher.tssErr,
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
	Serialize() SignableResult
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
	PublicKey   []byte
	SavedSecret []byte
}

func (KeyGenResult) Type() DispatcherType {
	return KeyGenResultType
}

func (result KeyGenResult) Serialize() SignableResult {
	return SignableResult{
		Type: "keygen_result",
		Data: hex.EncodeToString(result.PublicKey),
	}
}

type KeySignResult struct {
	Msg       []byte
	Signature []byte
}

func (KeySignResult) Type() DispatcherType {
	return KeySignResultType
}

func (result KeySignResult) Serialize() SignableResult {

	return SignableResult{
		Type: "sign_result",
		Data: hex.EncodeToString(result.Signature),
	}
}

type ReshareResult struct {
	NewSecret []byte
	NewSet    []Participant
	KeyId     string
}

func (ReshareResult) Type() DispatcherType {
	return ReshareResultType
}

func (result ReshareResult) Serialize() SignableResult {
	return SignableResult{
		Type: "reshare_result",
		Data: "complete",
	}
}

type ErrorResult struct {
	err    error
	tssErr *btss.Error
}

func (ErrorResult) Type() DispatcherType {
	return ErrorType
}

func (eres ErrorResult) Serialize() SignableResult {

	if eres.tssErr != nil {
		blameNodes := make([]string, 0)
		for _, n := range eres.tssErr.Culprits() {
			blameNodes = append(blameNodes, string(n.GetKey()))
		}
		x := map[string]interface{}{
			"culprits": blameNodes,
			"msg":      eres.tssErr.Error(),
		}
		serialized, _ := json.Marshal(x)
		return SignableResult{
			Type: "blame",
			Data: string(serialized),
		}
	} else {
		return SignableResult{
			Type: "error",
			Data: eres.err.Error(),
		}
	}
}

type TimeoutResult struct {
	Culprits []string `json:"culprits"`
}

func (TimeoutResult) Type() DispatcherType {
	return TimeoutType
}

func (result TimeoutResult) Serialize() SignableResult {
	s, _ := json.Marshal(result)
	return SignableResult{
		Type: "blame_timeout",
		Data: string(s),
	}
}

type SignableResult struct {
	Type      string `json:"type"`
	SessionId string `json:"session_id"`
	Data      string `json:"data"`
}
