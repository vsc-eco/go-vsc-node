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
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
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
	newParticipants    []Participant
	newEpoch           uint64
	prevCommitmentType string
	origOldSize        int // full old committee size before readiness filtering
	origNewSize        int // full new committee size before readiness filtering

	//Filled internally
	newParty btss.Party
	newPids  btss.SortedPartyIDs
	oldPids  btss.SortedPartyIDs // epoch-modified old party IDs (set during Start)
	result   *ReshareResult
}

// verifyResharedKey checks that the reshared public key matches the original.
// extractPub deserializes the stored key data and returns the original (X, Y).
func (dispatcher *ReshareDispatcher) verifyResharedKey(newX, newY *big.Int, algo string, extractPub func([]byte) (*big.Int, *big.Int, error)) error {
	origKeyData, err := dispatcher.keystore.Get(context.Background(), makeKey("key", dispatcher.keyId, int(dispatcher.epoch)))
	if err != nil {
		return nil // no original key to compare against
	}
	origX, origY, err := extractPub(origKeyData)
	if err != nil {
		return nil
	}
	if origX.Cmp(newX) != 0 || origY.Cmp(newY) != 0 {
		log.Error("RESHARE KEY MISMATCH - public key changed, aborting reshare",
			"keyId", dispatcher.keyId, "sessionId", dispatcher.sessionId,
			"algo", algo,
			"origX", fmt.Sprintf("%x", origX),
			"newX", fmt.Sprintf("%x", newX))
		return fmt.Errorf("reshare produced different public key for key %s", dispatcher.keyId)
	}
	return nil
}

func (dispatcher *ReshareDispatcher) Start() error {
	startTime := time.Now()
	log.Info("starting reshare", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "epoch", dispatcher.epoch, "newEpoch", dispatcher.newEpoch, "blockHeight", dispatcher.blockHeight)

	sortedPids, myParty, p2pCtx := dispatcher.baseInfo()

	userId := dispatcher.tssMgr.config.Get().HiveUsername

	// If previous key data came from a reshare, the save data contains epoch-modified
	// party IDs. We must recreate old party IDs with the same epoch modification so
	// tss-lib can match them against the save data.
	if dispatcher.prevCommitmentType == "reshare" {
		oldEpochIdx := dispatcher.epoch
		pIds := make([]*btss.PartyID, 0)
		for idx, p := range dispatcher.participants {
			i := big.NewInt(0)
			i = i.SetBytes([]byte(p.Account))
			if oldEpochIdx != 0 {
				i = i.Mul(i, big.NewInt(int64(oldEpochIdx+1)))
			}
			pi := btss.NewPartyID(p.Account, dispatcher.sessionId, i)
			dispatcher.participants[idx].PartyId = pi
			pIds = append(pIds, pi)
		}
		sortedPids = btss.SortPartyIDs(pIds)
		p2pCtx = btss.NewPeerContext(sortedPids)
		myParty = nil
		for _, p := range sortedPids {
			if p.Id == userId {
				myParty = p
				break
			}
		}
	}

	// Store epoch-modified old party IDs so reshareMsgs() and HandleP2P() can
	// use them without calling baseInfo() (which would overwrite epoch modifications).
	dispatcher.oldPids = sortedPids

	log.Verbose("reshare participant counts", "oldCount", len(sortedPids), "newCount", len(dispatcher.newParticipants), "sessionId", dispatcher.sessionId)

	epochIdx := dispatcher.newEpoch
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

	// Use the ORIGINAL (pre-readiness-filter) committee sizes for threshold.
	// tss-lib Lagrange interpolation requires the threshold matching the polynomial
	// degree from keygen/reshare. Using the filtered subset size produces wrong
	// coefficients and corrupts the key.
	threshold, _ := tss_helpers.GetThreshold(dispatcher.origOldSize)

	newThreshold, _ := tss_helpers.GetThreshold(dispatcher.origNewSize)
	// newThreshold++

	log.Verbose("reshare thresholds calculated", "oldRequired", threshold+1, "oldTotal", len(sortedPids), "oldThreshold", threshold, "newRequired", newThreshold+1, "newTotal", len(dispatcher.newPids), "newThreshold", newThreshold, "sessionId", dispatcher.sessionId)
	//epoch: 5 <-- actual data
	//epoch: 7 <-- likely empty

	// Only load old key data if this node is part of the old committee.
	// New-only participants don't have (or need) the previous epoch's key data.
	var err error
	var savedKeyData []byte
	if myParty != nil {
		savedKeyData, err = dispatcher.keystore.Get(
			context.Background(),
			makeKey("key", dispatcher.keyId, int(dispatcher.epoch)),
		)
		log.Verbose("key retrieval result", "keyId", dispatcher.keyId, "epoch", dispatcher.epoch, "err", err, "dataLen", len(savedKeyData), "sessionId", dispatcher.sessionId)
		if err != nil {
			log.Error("failed to retrieve key data", "keyId", dispatcher.keyId, "epoch", dispatcher.epoch, "err", err, "sessionId", dispatcher.sessionId)
			return err
		}
	} else {
		log.Verbose("new-only participant, skipping key retrieval", "sessionId", dispatcher.sessionId, "userId", userId)
	}

	log.Verbose("party membership", "inOldParty", myParty != nil, "inNewParty", myNewParty != nil, "sessionId", dispatcher.sessionId)

	var partyInitWg sync.WaitGroup
	partyInitWg.Add(2)

	if dispatcher.algo == tss_helpers.SigningAlgoEcdsa {
		keydata := keyGenSecp256k1.LocalPartySaveData{}

		if myParty != nil {
			err = json.Unmarshal(savedKeyData, &keydata)
			if err != nil {
				log.Error("failed to unmarshal key data", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "err", err)
				return err
			}
		}

		end := make(chan *keyGenSecp256k1.LocalPartySaveData)
		endOld := make(chan *keyGenSecp256k1.LocalPartySaveData)

		save := keyGenSecp256k1.NewLocalPartySaveData(len(dispatcher.newPids))

		go dispatcher.reshareMsgs()

		// Wait for sync delay (reduced from 15s to configurable 5s)
		syncDelay := dispatcher.tssMgr.sconf.TssParams().ReshareSyncDelay
		log.Verbose("waiting before starting parties", "syncDelay", syncDelay, "sessionId", dispatcher.sessionId)

		// Check participant readiness before starting (strict: do not start with missing peers)
		ready := dispatcher.waitForParticipantReadiness(sortedPids, dispatcher.newPids, syncDelay)
		if !ready {
			err := fmt.Errorf("insufficient participants ready for reshare (would block or panic)")
			log.Error("insufficient participants ready, skipping reshare", "sessionId", dispatcher.sessionId, "err", err)
			return err
		}

		go func() {
			var initOnce sync.Once
			defer func() {
				initOnce.Do(func() { partyInitWg.Done() })
				if r := recover(); r != nil {
					log.Error("panic recovered in old party start", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "panic", r)
					dispatcher.err = fmt.Errorf("panic in old party start: %v", r)
				}
			}()
			//Check if old party is not nil (ie node is part of old committee)
			if myParty != nil {
				log.Verbose("starting old party", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "partyId", myParty.Id)
				params := btss.NewReSharingParameters(
					btss.S256(),
					p2pCtx,
					newP2pCtx,
					myParty,
					len(sortedPids),
					threshold,
					len(dispatcher.newPids),
					newThreshold,
				)

				dispatcher.party = reshareSecp256k1.NewLocalParty(params, keydata, dispatcher.p2pMsg, endOld)
				initOnce.Do(func() { partyInitWg.Done() })

				err := dispatcher.party.Start()

				if err != nil {
					log.Warn("old party start failed", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "err", err)
					dispatcher.err = err
				} else {
					log.Verbose("old party started successfully", "algo", "ECDSA", "sessionId", dispatcher.sessionId)
				}
			} else {
				initOnce.Do(func() { partyInitWg.Done() })
			}
		}()
		go func() {
			var initOnce sync.Once
			defer func() {
				initOnce.Do(func() { partyInitWg.Done() })
				if r := recover(); r != nil {
					log.Error("panic recovered in new party start", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "panic", r)
					dispatcher.err = fmt.Errorf("panic in new party start: %v", r)
				}
			}()
			//Check if new party is not nil (ie will be in new committee)
			if myNewParty != nil {
				log.Verbose("starting new party", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "partyId", myNewParty.Id)
				newParams := btss.NewReSharingParameters(
					btss.S256(),
					p2pCtx,
					newP2pCtx,
					myNewParty,
					len(sortedPids),
					threshold,
					len(dispatcher.newPids),
					newThreshold,
				)

				dispatcher.newParty = reshareSecp256k1.NewLocalParty(newParams, save, dispatcher.p2pMsg, end)
				initOnce.Do(func() { partyInitWg.Done() })

				err := dispatcher.newParty.Start()

				if err != nil {
					log.Warn("new party start failed", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "err", err)
					dispatcher.err = err
				} else {
					log.Verbose("new party started successfully", "algo", "ECDSA", "sessionId", dispatcher.sessionId)
				}
			} else {
				initOnce.Do(func() { partyInitWg.Done() })
			}
		}()
		go func() {
			select {
			case <-dispatcher.msgCtx.Done():
			case <-endOld:
			}
		}()
		go func() {
			for {
				var reshareResult *keyGenSecp256k1.LocalPartySaveData
				select {
				case <-dispatcher.msgCtx.Done():
					return
				case reshareResult = <-end:
				}
				log.Verbose("reshare result received", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "partyId", dispatcher.newParty.PartyID().Id, "hasPubKey", reshareResult.ECDSAPub != nil)

				if reshareResult.ECDSAPub != nil {
					// Safety check: verify reshared key matches original
					if err := dispatcher.verifyResharedKey(reshareResult.ECDSAPub.X(), reshareResult.ECDSAPub.Y(), "ECDSA", func(data []byte) (*big.Int, *big.Int, error) {
						var save keyGenSecp256k1.LocalPartySaveData
						if err := json.Unmarshal(data, &save); err != nil || save.ECDSAPub == nil {
							return nil, nil, fmt.Errorf("unmarshal failed or nil pub")
						}
						return save.ECDSAPub.X(), save.ECDSAPub.Y(), nil
					}); err != nil {
						dispatcher.err = err
						dispatcher.signalDone()
						return
					}

					duration := time.Since(startTime)
					log.Info("reshare completed successfully", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "duration", duration, "keyId", dispatcher.keyId, "newEpoch", dispatcher.newEpoch)
					dispatcher.tssMgr.metrics.IncrementReshareSuccess()
					dispatcher.tssMgr.metrics.RecordReshareDuration(duration)

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

					dispatcher.signalDone()
					return
				}
			}
		}()
	} else if dispatcher.algo == tss_helpers.SigningAlgoEddsa {
		log.Trace("starting reshare dispatcher", "algo", "EdDSA", "newParticipantCount", len(dispatcher.newParticipants), "newEpoch", dispatcher.newEpoch)

		keydata := keyGenEddsa.LocalPartySaveData{}

		if myParty != nil {
			err = json.Unmarshal(savedKeyData, &keydata)
			if err != nil {
				log.Error("failed to unmarshal key data", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "err", err)
				return err
			}
		}

		end := make(chan *keyGenEddsa.LocalPartySaveData)
		endOld := make(chan *keyGenEddsa.LocalPartySaveData)

		save := keyGenEddsa.NewLocalPartySaveData(len(dispatcher.newPids))

		go dispatcher.reshareMsgs()

		// Wait for sync delay (reduced from 15s to configurable 5s)
		syncDelay := dispatcher.tssMgr.sconf.TssParams().ReshareSyncDelay
		log.Verbose("waiting before starting parties", "algo", "EdDSA", "syncDelay", syncDelay, "sessionId", dispatcher.sessionId)

		// Check participant readiness before starting (strict: do not start with missing peers)
		ready := dispatcher.waitForParticipantReadiness(sortedPids, dispatcher.newPids, syncDelay)
		if !ready {
			err := fmt.Errorf("insufficient participants ready for reshare (would block or panic)")
			log.Error("insufficient participants ready, skipping reshare", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "err", err)
			return err
		}

		go func() {
			var initOnce sync.Once
			defer func() {
				initOnce.Do(func() { partyInitWg.Done() })
				if r := recover(); r != nil {
					log.Error("panic recovered in old party start", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "panic", r)
					dispatcher.err = fmt.Errorf("panic in old party start: %v", r)
				}
			}()
			if myParty != nil {
				params := btss.NewReSharingParameters(btss.Edwards(), p2pCtx, newP2pCtx, myParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)
				dispatcher.party = reshareEddsa.NewLocalParty(params, keydata, dispatcher.p2pMsg, endOld)
				initOnce.Do(func() { partyInitWg.Done() })

				log.Trace("old party created", "algo", "EdDSA", "sessionId", dispatcher.sessionId)

				err := dispatcher.party.Start()

				if err != nil {
					log.Warn("old party start failed", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "err", err)
					dispatcher.err = err
				}
			} else {
				initOnce.Do(func() { partyInitWg.Done() })
			}
		}()

		go func() {
			var initOnce sync.Once
			defer func() {
				initOnce.Do(func() { partyInitWg.Done() })
				if r := recover(); r != nil {
					log.Error("panic recovered in new party start", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "panic", r)
					dispatcher.err = fmt.Errorf("panic in new party start: %v", r)
				}
			}()
			if myNewParty != nil {
				newParams := btss.NewReSharingParameters(btss.Edwards(), p2pCtx, newP2pCtx, myNewParty, len(sortedPids), threshold, len(dispatcher.newPids), newThreshold)

				dispatcher.newParty = reshareEddsa.NewLocalParty(newParams, save, dispatcher.p2pMsg, end)
				initOnce.Do(func() { partyInitWg.Done() })

				log.Trace("new party created", "algo", "EdDSA", "sessionId", dispatcher.sessionId)
				err := dispatcher.newParty.Start()

				if err != nil {
					log.Warn("new party start failed", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "err", err)
					dispatcher.err = err
				}
			} else {
				partyInitWg.Done()
			}
		}()

		go func() {
			select {
			case <-dispatcher.msgCtx.Done():
			case <-endOld:
			}
		}()
		go func() {
			for {
				var reshareResult *keyGenEddsa.LocalPartySaveData
				select {
				case <-dispatcher.msgCtx.Done():
					return
				case reshareResult = <-end:
				}

				log.Verbose("reshare result received", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "partyId", dispatcher.newParty.PartyID().Id)
				if reshareResult.EDDSAPub == nil {
					continue
				}

				// Safety check: verify reshared key matches original
				if err := dispatcher.verifyResharedKey(reshareResult.EDDSAPub.X(), reshareResult.EDDSAPub.Y(), "EdDSA", func(data []byte) (*big.Int, *big.Int, error) {
					var save keyGenEddsa.LocalPartySaveData
					if err := json.Unmarshal(data, &save); err != nil || save.EDDSAPub == nil {
						return nil, nil, fmt.Errorf("unmarshal failed or nil pub")
					}
					return save.EDDSAPub.X(), save.EDDSAPub.Y(), nil
				}); err != nil {
					dispatcher.err = err
					dispatcher.signalDone()
					return
				}

				duration := time.Since(startTime)
				log.Info("reshare completed successfully", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "duration", duration, "keyId", dispatcher.keyId, "newEpoch", dispatcher.newEpoch)
				dispatcher.tssMgr.metrics.IncrementReshareSuccess()
				dispatcher.tssMgr.metrics.RecordReshareDuration(duration)

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

				dispatcher.signalDone()
				return
			}
		}()
	}

	partyInitWg.Wait()
	dispatcher.baseStart()

	return nil
}

func (dispatcher *ReshareDispatcher) Done() *promise.Promise[DispatcherResult] {
	return promise.New(func(resolve func(DispatcherResult), reject func(error)) {
		<-dispatcher.done

		log.Verbose("reshare done called", "sessionId", dispatcher.sessionId, "timeout", dispatcher.timeout, "hasTssErr", dispatcher.tssErr != nil, "hasErr", dispatcher.err != nil)
		if dispatcher.timeout {
			culprits := make(map[string]bool, 0)
			oldCulprits := make([]string, 0)
			newCulprits := make([]string, 0)

			// Check connection status for each culprit to provide context
			culpritContext := make(map[string]string)

			if dispatcher.party != nil {
				for _, p := range dispatcher.party.WaitingFor() {
					culprits[p.Id] = true
					oldCulprits = append(oldCulprits, p.Id)

					// Check if culprit is connected
					witness, err := dispatcher.tssMgr.witnessDb.GetWitnessAtHeight(p.Id, nil)
					if err == nil {
						peerId, err := peer.Decode(witness.PeerId)
						if err == nil {
							host := dispatcher.tssMgr.p2p.Host()
							connState := host.Network().Connectedness(peerId)
							if connState != network.Connected {
								culpritContext[p.Id] = "not_connected"
							} else {
								culpritContext[p.Id] = "connected_but_no_response"
							}
						}
					} else {
						culpritContext[p.Id] = "witness_not_found"
					}
				}
			}
			if dispatcher.newParty != nil {
				for _, p := range dispatcher.newParty.WaitingFor() {
					culprits[p.Id] = true
					newCulprits = append(newCulprits, p.Id)

					// Check if culprit is connected
					if _, exists := culpritContext[p.Id]; !exists {
						witness, err := dispatcher.tssMgr.witnessDb.GetWitnessAtHeight(p.Id, nil)
						if err == nil {
							peerId, err := peer.Decode(witness.PeerId)
							if err == nil {
								host := dispatcher.tssMgr.p2p.Host()
								connState := host.Network().Connectedness(peerId)
								if connState != network.Connected {
									culpritContext[p.Id] = "not_connected"
								} else {
									culpritContext[p.Id] = "connected_but_no_response"
								}
							}
						} else {
							culpritContext[p.Id] = "witness_not_found"
						}
					}
				}
			}

			log.Warn("reshare session timeout", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "oldCulprits", oldCulprits, "newCulprits", newCulprits, "totalCulprits", len(culprits), "contexts", culpritContext)

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
			log.Warn("reshare TSS error", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "err", dispatcher.tssErr)
			dispatcher.tssMgr.metrics.IncrementReshareFailure()
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
			log.Warn("reshare dispatcher error", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "err", dispatcher.err)
			dispatcher.tssMgr.metrics.IncrementReshareFailure()
			reject(dispatcher.err)
			return
		}
		log.Info("reshare done: success", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "newEpoch", dispatcher.newEpoch)
		resolve(*dispatcher.result)
	})
}

func (dispatcher *ReshareDispatcher) HandleP2P(input []byte, fromStr string, isBrcst bool, cmt string, fromCmt string) {
	dispatcher.startWait()

	// Use stored epoch-modified party IDs instead of baseInfo() which would
	// overwrite the epoch modifications set during Start().
	var from *btss.PartyID

	if fromCmt == "old" {
		for _, p := range dispatcher.oldPids {
			if p.Id == fromStr {
				from = p
				break
			}
		}
	} else if fromCmt == "new" {
		for _, p := range dispatcher.newPids {
			if p.Id == fromStr {
				from = p
				break
			}
		}
	}

	if from == nil {
		log.Trace("received message from unknown participant", "sessionId", dispatcher.sessionId, "from", fromStr, "fromCmt", fromCmt)
		return
	}

	log.Trace("received reshare message", "sessionId", dispatcher.sessionId, "from", fromStr, "isBroadcast", isBrcst, "cmt", cmt, "fromCmt", fromCmt, "msgLen", len(input))

	if cmt == "both" || cmt == "old" {
		if dispatcher.party != nil {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Error("panic recovered in old party UpdateFromBytes", "sessionId", dispatcher.sessionId, "from", fromStr, "panic", r)
					}
				}()
				ok, err := dispatcher.party.UpdateFromBytes(input, from, isBrcst)
				if err != nil {
					log.Trace("UpdateFromBytes failed (old party)", "sessionId", dispatcher.sessionId, "from", fromStr, "ok", ok, "err", err)
					dispatcher.tssErr = err
				} else {
					dispatcher.lastMsg = time.Now()
					log.Trace("message processed successfully (old party)", "sessionId", dispatcher.sessionId, "from", fromStr)
				}
			}()
		}
	}
	if cmt == "both" || cmt == "new" {
		if dispatcher.newParty != nil {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						log.Error("panic recovered in new party UpdateFromBytes", "sessionId", dispatcher.sessionId, "from", fromStr, "panic", r)
					}
				}()
				ok, err := dispatcher.newParty.UpdateFromBytes(input, from, isBrcst)

				if err != nil {
					log.Trace("UpdateFromBytes failed (new party)", "sessionId", dispatcher.sessionId, "from", fromStr, "ok", ok, "err", err)
					dispatcher.tssErr = err
				} else {
					dispatcher.lastMsg = time.Now()
					log.Trace("message processed successfully (new party)", "sessionId", dispatcher.sessionId, "from", fromStr)
				}
			}()
		}
	}
}

func (dispatcher *ReshareDispatcher) reshareMsgs() {
	go func() {
		for {
			var msg btss.Message
			select {
			case <-dispatcher.msgCtx.Done():
				return
			case msg = <-dispatcher.p2pMsg:
			}

			var commiteeType string
			if msg.IsToOldAndNewCommittees() {
				commiteeType = "both"
			} else if msg.IsToOldCommittee() {
				commiteeType = "old"
			} else if !msg.IsToOldCommittee() {
				commiteeType = "new"
			}

			var cmtFrom string
			for _, old := range dispatcher.oldPids {
				if slices.Compare(old.GetKey(), msg.GetFrom().GetKey()) == 0 {
					cmtFrom = "old"
					break
				}
			}

			for _, newP := range dispatcher.newPids {
				if slices.Compare(newP.GetKey(), msg.GetFrom().GetKey()) == 0 {
					cmtFrom = "new"
					break
				}
			}

			for _, to := range msg.GetTo() {
				bytes, _, err := msg.WireBytes()

				if err != nil {
					log.Warn("WireBytes failed", "sessionId", dispatcher.sessionId, "to", to.Id, "err", err)
					dispatcher.err = err
				}

				// Deliver self-messages locally — in reshare, old and new parties are
				// separate instances on the same node, so tss-lib's internal auto-apply
				// doesn't cross between them. Bypass P2P (which fails for self) and
				// call HandleP2P directly.
				if to.Id == dispatcher.tssMgr.config.Get().HiveUsername {
					go func(msgBytes []byte) {
						dispatcher.HandleP2P(msgBytes, to.Id, msg.IsBroadcast(), commiteeType, cmtFrom)
					}(bytes)
					continue
				}

				go func(targetId string, msgBytes []byte) {
					sendStart := time.Now()
					err = dispatcher.tssMgr.SendMsg(dispatcher.sessionId, Participant{
						Account: targetId,
					}, "", msgBytes, msg.IsBroadcast(), commiteeType, cmtFrom)
					sendDuration := time.Since(sendStart)
					if err != nil {
						log.Warn("SendMsg failed", "sessionId", dispatcher.sessionId, "to", targetId, "isBroadcast", msg.IsBroadcast(), "msgLen", len(msgBytes), "duration", sendDuration, "err", err)
					} else {
						log.Trace("SendMsg success", "sessionId", dispatcher.sessionId, "to", targetId, "isBroadcast", msg.IsBroadcast(), "msgLen", len(msgBytes), "duration", sendDuration)
					}
				}(to.Id, bytes)
			}
		}
	}()
}

// waitForParticipantReadiness checks if participants are connected before starting reshare
func (dispatcher *ReshareDispatcher) waitForParticipantReadiness(
	oldPids btss.SortedPartyIDs,
	newPids btss.SortedPartyIDs,
	maxWait time.Duration,
) bool {
	startTime := time.Now()
	checkInterval := 500 * time.Millisecond
	maxChecks := int(maxWait / checkInterval)

	allParticipants := make(map[string]bool)
	for _, p := range oldPids {
		allParticipants[p.Id] = true
	}
	for _, p := range newPids {
		allParticipants[p.Id] = true
	}

	connectedCount := 0
	totalCount := len(allParticipants)

	log.Verbose("checking participant readiness", "sessionId", dispatcher.sessionId, "totalParticipants", totalCount)

	selfAccount := dispatcher.tssMgr.config.Get().HiveUsername

	for i := 0; i < maxChecks; i++ {
		connectedCount = 0
		for account := range allParticipants {
			// Self is always "connected" — no libp2p connection to ourselves
			if account == selfAccount {
				connectedCount++
				continue
			}

			witness, err := dispatcher.tssMgr.witnessDb.GetWitnessAtHeight(account, nil)
			if err != nil {
				continue
			}

			peerId, err := peer.Decode(witness.PeerId)
			if err != nil {
				continue
			}

			host := dispatcher.tssMgr.p2p.Host()
			connState := host.Network().Connectedness(peerId)
			if connState == network.Connected {
				connectedCount++
			}
		}

		readinessPercent := float64(connectedCount) / float64(totalCount) * 100.0
		log.Verbose("readiness check", "sessionId", dispatcher.sessionId, "connected", connectedCount, "total", totalCount, "readinessPercent", readinessPercent, "elapsed", time.Since(startTime))

		// Require at least threshold+1 participants to be connected
		threshold, _ := tss_helpers.GetThreshold(totalCount)
		minRequired := threshold + 1

		if connectedCount >= minRequired {
			log.Verbose("sufficient participants ready", "sessionId", dispatcher.sessionId, "connected", connectedCount, "required", minRequired)
			return true
		}

		time.Sleep(checkInterval)
	}

	log.Warn("readiness check timeout", "sessionId", dispatcher.sessionId, "connected", connectedCount, "total", totalCount, "elapsed", time.Since(startTime))
	return false
}

type SignDispatcher struct {
	BaseDispatcher

	msg []byte

	prevCommitmentType string
	origCommitteeSize  int // full committee size before readiness filtering

	result *KeySignResult
}

func (dispatcher *SignDispatcher) Start() error {

	sortedPids, myParty, p2pCtx := dispatcher.baseInfo()

	// If the save data was created by a reshare, the party IDs in the save data
	// have epoch-modified keys. We must recreate party IDs with the same epoch
	// modification so tss-lib can match them against the save data.
	if dispatcher.prevCommitmentType == "reshare" {
		userId := dispatcher.tssMgr.config.Get().HiveUsername
		epochIdx := dispatcher.epoch
		pIds := make([]*btss.PartyID, 0)
		for idx, p := range dispatcher.participants {
			i := big.NewInt(0)
			i = i.SetBytes([]byte(p.Account))
			if epochIdx != 0 {
				i = i.Mul(i, big.NewInt(int64(epochIdx+1)))
			}
			pi := btss.NewPartyID(p.Account, dispatcher.sessionId, i)
			dispatcher.participants[idx].PartyId = pi
			pIds = append(pIds, pi)
		}
		sortedPids = btss.SortPartyIDs(pIds)
		p2pCtx = btss.NewPeerContext(sortedPids)
		myParty = nil
		for _, p := range sortedPids {
			if p.Id == userId {
				myParty = p
				break
			}
		}
	}

	if myParty == nil {
		return fmt.Errorf("node not part of keygen committee")
	}

	log.Trace("sign dispatcher start", "algo", dispatcher.algo, "sessionId", dispatcher.sessionId)
	if dispatcher.algo == tss_helpers.SigningAlgoEcdsa {
		m, err := tss_helpers.MsgToHashInt(dispatcher.msg, tss_helpers.SigningAlgoEcdsa)

		if err != nil {
			log.Trace("sign msg hash error", "err", err)

			return err
		}
		keydata := keyGenSecp256k1.LocalPartySaveData{}
		k := makeKey("key", dispatcher.keyId, int(dispatcher.epoch))

		rawKey, err := dispatcher.keystore.Get(context.Background(), k)

		if err != nil {
			log.Trace("sign key retrieval error", "err", err)

			return err
		}
		json.Unmarshal(rawKey, &keydata)
		// Use original committee size for threshold — not the readiness-filtered subset.
		// The polynomial degree must match keygen/reshare or Lagrange interpolation is wrong.
		threshold, err := tss_helpers.GetThreshold(dispatcher.origCommitteeSize)

		if err != nil {

			log.Trace("sign threshold error", "err", err)
			return nil
		}

		params := btss.NewParameters(btss.S256(), p2pCtx, myParty, len(sortedPids), threshold)
		end := make(chan *common.SignatureData)

		dispatcher.party = keySignSecp256k1.NewLocalParty(m, params, keydata, dispatcher.p2pMsg, end)

		go dispatcher.handleMsgs()
		time.Sleep(dispatcher.tssMgr.sconf.TssParams().ReshareSyncDelay)
		go func() {
			log.Trace("starting sign party", "algo", "ECDSA", "sessionId", dispatcher.sessionId)
			err := dispatcher.party.Start()

			if err != nil {
				log.Warn("sign party start failed", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "err", err)
				dispatcher.err = err
			}
		}()
		go func() {
			var sigResult *common.SignatureData
			select {
			case <-dispatcher.msgCtx.Done():
				return
			case sigResult = <-end:
			}

			log.Trace("sign result received", "algo", "ECDSA", "sessionId", dispatcher.sessionId)

			derSig := btcec.Signature{
				R: new(big.Int).SetBytes(sigResult.R),
				S: new(big.Int).SetBytes(sigResult.S),
			}

			dispatcher.result = &KeySignResult{
				Msg:       dispatcher.msg,
				Signature: derSig.Serialize(),
				KeyId:     dispatcher.keyId,
			}

			dispatcher.signalDone()
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

		// Use original committee size for threshold — not the readiness-filtered subset.
		threshold, err := tss_helpers.GetThreshold(dispatcher.origCommitteeSize)

		if err != nil {
			return nil
		}

		params := btss.NewParameters(btss.Edwards(), p2pCtx, myParty, len(sortedPids), threshold)
		end := make(chan *common.SignatureData)

		dispatcher.party = keySignEddsa.NewLocalParty(m, params, keydata, dispatcher.p2pMsg, end)

		go dispatcher.handleMsgs()
		time.Sleep(dispatcher.tssMgr.sconf.TssParams().ReshareSyncDelay)
		go func() {
			err := dispatcher.party.Start()

			if err != nil {
				log.Warn("sign party start failed", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "err", err)
				dispatcher.err = err
			}
		}()
		go func() {
			var sigResult *common.SignatureData
			select {
			case <-dispatcher.msgCtx.Done():
				return
			case sigResult = <-end:
			}

			dispatcher.result = &KeySignResult{
				Msg:       dispatcher.msg,
				Signature: sigResult.Signature,
				KeyId:     dispatcher.keyId,
			}

			dispatcher.signalDone()
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
				culprits = append(culprits, p.Id)
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

	done       chan struct{}
	msgCtx     context.Context    // cancelled by signalDone to unblock message-loop goroutines
	cancelMsgs context.CancelFunc

	keystore datastore.Datastore

	epoch       uint64
	algo        tss_helpers.SigningAlgo
	blockHeight uint64

	started   bool
	startLock sync.Mutex

	lastMsg   time.Time
	isReshare bool // true when this is a ReshareDispatcher (use longer timeout)

	failedMsgs     []failedMsg
	failedMsgsLock sync.Mutex

	doneSignalled bool // guards against both timeout and completion racing to signal done
	doneMu        sync.Mutex
}

type failedMsg struct {
	sessionId    string
	participant  Participant
	moniker      string
	bytes        []byte
	isBroadcast  bool
	commiteeType string
	cmtFrom      string
	attempts     int
}

func (dispatcher *BaseDispatcher) signalDone() {
	// Stop message-loop goroutines (reshareMsgs, handleMsgs, endOld receivers, retryFailedMsgs, etc.)
	dispatcher.cancelMsgs()

	dispatcher.doneMu.Lock()
	if dispatcher.doneSignalled {
		dispatcher.doneMu.Unlock()
		return
	}
	dispatcher.doneSignalled = true
	dispatcher.doneMu.Unlock()

	dispatcher.done <- struct{}{}
}

func (dispatcher *BaseDispatcher) queueFailedMsg(fm failedMsg) {
	dispatcher.failedMsgsLock.Lock()
	dispatcher.failedMsgs = append(dispatcher.failedMsgs, fm)
	dispatcher.failedMsgsLock.Unlock()
}

func (dispatcher *BaseDispatcher) retryFailedMsgs() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-dispatcher.msgCtx.Done():
			return
		case <-ticker.C:
			dispatcher.failedMsgsLock.Lock()
			toRetry := dispatcher.failedMsgs
			dispatcher.failedMsgs = nil
			dispatcher.failedMsgsLock.Unlock()

			if len(toRetry) == 0 {
				continue
			}

			remaining := make([]failedMsg, 0)
			for _, fm := range toRetry {
				fm.attempts++
				if fm.attempts > 6 {
					log.Trace("giving up on message after retries", "to", fm.participant.Account, "attempts", fm.attempts, "sessionId", fm.sessionId)
					continue
				}
				err := dispatcher.tssMgr.SendMsg(fm.sessionId, fm.participant, fm.moniker,
					fm.bytes, fm.isBroadcast, fm.commiteeType, fm.cmtFrom)
				if err != nil {
					log.Trace("message retry failed", "attempt", fm.attempts, "to", fm.participant.Account, "err", err)
					remaining = append(remaining, fm)
				} else {
					log.Trace("message retry succeeded", "attempt", fm.attempts, "to", fm.participant.Account, "sessionId", fm.sessionId)
				}
			}

			if len(remaining) > 0 {
				dispatcher.failedMsgsLock.Lock()
				dispatcher.failedMsgs = append(dispatcher.failedMsgs, remaining...)
				dispatcher.failedMsgsLock.Unlock()
			}
		}
	}
}

func (dispatcher *BaseDispatcher) handleMsgs() {
	go func() {
		for {
			var msg btss.Message
			select {
			case <-dispatcher.msgCtx.Done():
				return
			case msg = <-dispatcher.p2pMsg:
			}

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
					log.Trace("broadcast WireBytes error", "err", err)
				}
				for _, p := range dispatcher.participants {
					if p.Account == dispatcher.tssMgr.config.Get().HiveUsername {
						continue
					}

					go func() {
						err := dispatcher.tssMgr.SendMsg(
							dispatcher.sessionId,
							p,
							msg.WireMsg().From.Moniker,
							bytes,
							true,
							commiteeType,
							"",
						)
						if err != nil {
							log.Trace("SendMsg failed, queuing for retry", "to", p.Account, "err", err)
							dispatcher.queueFailedMsg(failedMsg{
								sessionId:    dispatcher.sessionId,
								participant:  Participant{Account: p.Account},
								moniker:      msg.WireMsg().From.Moniker,
								bytes:        bytes,
								isBroadcast:  true,
								commiteeType: commiteeType,
							})
						}
					}()
				}
			} else {
				for _, to := range msg.GetTo() {
					bytes, _, err := msg.WireBytes()

					if err != nil {
						dispatcher.err = err
					}

					go func() {
						err := dispatcher.tssMgr.SendMsg(dispatcher.sessionId, Participant{
							Account: string(to.Id),
						}, to.Moniker, bytes, false, commiteeType, "")
						if err != nil {
							log.Trace("SendMsg failed, queuing for retry", "to", string(to.Id), "err", err)
							dispatcher.queueFailedMsg(failedMsg{
								sessionId:    dispatcher.sessionId,
								participant:  Participant{Account: string(to.Id)},
								moniker:      to.Moniker,
								bytes:        bytes,
								isBroadcast:  false,
								commiteeType: commiteeType,
							})
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

	//Filter out any messages to self
	if dispatcher.party.PartyID().Id == from.Id {
		return
	}

	go func() {
		ok, err := dispatcher.party.UpdateFromBytes(input, from, isBrcst)

		log.Trace("update party", "ok", ok, "inputLen", len(input), "from", from.Id)
		if err != nil {
			log.Trace("UpdateFromBytes error", "ok", ok, "err", err)
			dispatcher.tssErr = err
		} else {
			dispatcher.lastMsg = time.Now()
		}
	}()
}

func (dsc *BaseDispatcher) SessionId() string {
	return dsc.sessionId
}

func (dsc *BaseDispatcher) KeyId() string {
	return dsc.keyId
}

func (dispatcher *BaseDispatcher) baseStart() {
	// Use configurable timeout, longer for reshare operations
	var timeout time.Duration
	if dispatcher.isReshare {
		timeout = dispatcher.tssMgr.sconf.TssParams().ReshareTimeout
	} else {
		timeout = dispatcher.tssMgr.sconf.TssParams().DefaultTimeout
	}

	go dispatcher.retryFailedMsgs()

	dispatcher.lastMsg = time.Now()
	log.Trace("starting timeout monitor", "sessionId", dispatcher.sessionId, "timeout", timeout, "lastMsg", dispatcher.lastMsg)
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			select {
			case <-dispatcher.msgCtx.Done():
				// Session completed or was cancelled; stop monitoring
				return
			case <-timer.C:
				elapsed := time.Since(dispatcher.lastMsg)
				if elapsed < timeout {
					// Activity happened since the timer started; reset for remaining time
					timer.Reset(timeout - elapsed)
					continue
				}

				dispatcher.doneMu.Lock()
				if dispatcher.doneSignalled {
					dispatcher.doneMu.Unlock()
					return
				}
				dispatcher.doneSignalled = true
				dispatcher.timeout = true
				dispatcher.doneMu.Unlock()

				log.Warn("session timeout", "sessionId", dispatcher.sessionId, "elapsed", elapsed, "timeout", timeout, "lastMsg", dispatcher.lastMsg)

				dispatcher.cancelMsgs()
				dispatcher.done <- struct{}{}
				return
			}
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

	pl := len(dispatcher.participants)

	if err != nil {
		return err
	}

	_, myParty, p2pCtx := dispatcher.baseInfo()

	if myParty == nil {
		log.Verbose("node not in committee", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId)
		return fmt.Errorf("node not part of keygen committee")
	}

	log.Info("starting DKG", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "epoch", dispatcher.epoch, "participants", len(dispatcher.participants), "algo", dispatcher.algo)

	if dispatcher.algo == tss_helpers.SigningAlgoEcdsa {
		end := make(chan *keyGenSecp256k1.LocalPartySaveData)
		dispatcher.tssMgr.GeneratePreParams()
		preParams := <-dispatcher.tssMgr.preParams
		parameters := btss.NewParameters(btss.S256(), p2pCtx, myParty, pl, threshold)
		dispatcher.party = keyGenSecp256k1.NewLocalParty(parameters, dispatcher.p2pMsg, end, preParams)

		go dispatcher.handleMsgs()
		time.Sleep(dispatcher.tssMgr.sconf.TssParams().ReshareSyncDelay)
		go func() {
			err := dispatcher.party.Start()
			if err != nil {
				log.Error("keygen party start failed", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "err", err)
				dispatcher.err = err
			}
		}()

		go func() {
			var savedOutput *keyGenSecp256k1.LocalPartySaveData
			select {
			case <-dispatcher.msgCtx.Done():
				return
			case savedOutput = <-end:
			}

			pk := savedOutput.ECDSAPub

			pubKey := btcec.PublicKey{
				Curve: btss.S256(),
				X:     pk.X(),
				Y:     pk.Y(),
			}
			pubBytes := pubKey.SerializeCompressed()

			log.Info("DKG complete", "algo", "ECDSA", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "pubKey", hex.EncodeToString(pubBytes))

			bytes, _ := json.Marshal(savedOutput)

			k := makeKey("key", dispatcher.keyId, int(dispatcher.epoch))
			ksErr := dispatcher.tssMgr.keyStore.Put(context.Background(), k, bytes)
			if ksErr != nil {
				log.Info("keystore write failed", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "err", ksErr)
			} else {
				log.Info("keystore write OK", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId)
			}

			dispatcher.result = &KeyGenResult{
				PublicKey:   pubBytes,
				Commitment:  dispatcher.tssMgr.setToCommitment(dispatcher.participants, dispatcher.epoch),
				SavedSecret: bytes,
				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,

				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			}
			dispatcher.signalDone()
		}()
	} else if dispatcher.algo == tss_helpers.SigningAlgoEddsa {
		end := make(chan *keyGenEddsa.LocalPartySaveData)
		parameters := btss.NewParameters(btss.Edwards(), p2pCtx, myParty, pl, threshold)
		party := keyGenEddsa.NewLocalParty(parameters, dispatcher.p2pMsg, end)

		dispatcher.party = party

		go dispatcher.handleMsgs()
		time.Sleep(dispatcher.tssMgr.sconf.TssParams().ReshareSyncDelay)

		go func() {
			err := party.Start()
			if err != nil {
				dispatcher.err = err
			}
		}()

		go func() {
			var savedOutput *keyGenEddsa.LocalPartySaveData
			select {
			case <-dispatcher.msgCtx.Done():
				return
			case savedOutput = <-end:
			}

			publicKey := edwards.NewPublicKey(savedOutput.EDDSAPub.X(), savedOutput.EDDSAPub.Y())

			pubBytes := publicKey.SerializeCompressed()

			log.Info("DKG complete", "algo", "EdDSA", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "pubKey", hex.EncodeToString(pubBytes))

			bytes, _ := json.Marshal(savedOutput)

			k := makeKey("key", dispatcher.keyId, int(dispatcher.epoch))
			ksErr := dispatcher.tssMgr.keyStore.Put(context.Background(), k, bytes)
			if ksErr != nil {
				log.Info("keystore write failed", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "err", ksErr)
			} else {
				log.Info("keystore write OK", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId)
			}

			dispatcher.result = &KeyGenResult{
				PublicKey:   pubBytes,
				Commitment:  dispatcher.tssMgr.setToCommitment(dispatcher.participants, dispatcher.epoch),
				SavedSecret: bytes,
				SessionId:   dispatcher.sessionId,
				KeyId:       dispatcher.keyId,

				BlockHeight: dispatcher.blockHeight,
				Epoch:       dispatcher.epoch,
			}
			dispatcher.signalDone()
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
				culprits = append(culprits, p.Id)
			}
			log.Warn("keygen done: timeout", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "culprits", culprits)
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
			culprits := make([]string, 0)
			for _, n := range dispatcher.tssErr.Culprits() {
				culprits = append(culprits, string(n.GetId()))
			}
			log.Warn("keygen done: TSS error", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "culprits", culprits, "err", dispatcher.tssErr.Error())
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
			log.Warn("keygen done: internal error", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId, "err", dispatcher.err)
			reject(dispatcher.err)
			return
		}

		log.Info("keygen done: success", "sessionId", dispatcher.sessionId, "keyId", dispatcher.keyId)
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

		log.Verbose("blame node culprits", "blameNodes", blameNodes)

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

	// Mark timeout in metadata so we can distinguish from errors
	timeoutErr := "timeout"
	timeoutReason := fmt.Sprintf("Timeout waiting for %d nodes: %v", len(result.Culprits), result.Culprits)

	return tss_helpers.BaseCommitment{
		Type:       "blame",
		KeyId:      result.KeyId,
		SessionId:  result.SessionId,
		Commitment: commitment,
		PublicKey:  nil,
		Metadata: &tss_helpers.CommitmentMetadata{
			Error:  &timeoutErr,
			Reason: &timeoutReason,
		},
		BlockHeight: result.BlockHeight,
		Epoch:       result.Epoch,
	}
}
