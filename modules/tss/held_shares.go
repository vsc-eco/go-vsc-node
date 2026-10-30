package tss

import (
	"context"
	"encoding/base64"
	"math/big"
	"strconv"
	"time"
)

// THORChain's bifrost checks at start-up that it holds the share of its current
// vault and, when it does not, restores it from the encrypted backup it put on
// chain at keygen (bifrost/tss/recover_keyshares.go). Magi keeps no such backup,
// so only the check is copied. A node listed as a holder of a key whose share it
// cannot load fails every session of that key, and the key has one usable holder
// fewer than its record shows. Its operator has to know while the share can still
// be restored from a backup of the tss-keys directory.

// missingShareRelog is how often a missing share is reported again.
const missingShareRelog = time.Hour

// missingShare is a key whose newest keygen/reshare commitment lists this node
// as a holder, but whose share at that epoch this node cannot load.
type missingShare struct {
	KeyId string
	Epoch uint64
	Err   error
}

// heldSharesMissing lists every active key this node is a listed holder of whose
// share it cannot load. Read only: it changes no party list, readiness or
// commitment.
func (tssMgr *TssManager) heldSharesMissing(bh uint64) []missingShare {
	if tssMgr.keyStore == nil || tssMgr.tssKeys == nil || tssMgr.tssCommitments == nil || tssMgr.electionDb == nil {
		return nil
	}
	self := tssMgr.config.Get().HiveUsername
	election, err := tssMgr.electionDb.GetElectionByHeight(bh)
	if err != nil {
		return nil
	}
	// Every active, unexpired key made up to and including this epoch.
	keys, err := tssMgr.tssKeys.FindEpochKeys(election.Epoch + 1)
	if err != nil {
		return nil
	}
	encKey, encErr := tssMgr.keystoreEncryptionKey()

	missing := make([]missingShare, 0)
	for _, key := range keys {
		commitment, err := tssMgr.tssCommitments.GetCommitmentByHeight(key.Id, bh+1, "keygen", "reshare")
		if err != nil {
			continue
		}
		commitElection := tssMgr.electionDb.GetElection(commitment.Epoch)
		if commitElection == nil {
			continue
		}
		bits, err := base64.RawURLEncoding.DecodeString(commitment.Commitment)
		if err != nil {
			continue
		}
		bv := new(big.Int).SetBytes(bits)
		listed := false
		for idx, member := range commitElection.Members {
			if member.Account == self && bv.Bit(idx) == 1 {
				listed = true
				break
			}
		}
		if !listed {
			continue
		}
		if encErr != nil {
			missing = append(missing, missingShare{KeyId: key.Id, Epoch: commitment.Epoch, Err: encErr})
			continue
		}
		k := makeKey("key", key.Id, int(commitment.Epoch))
		if _, err := keystoreGetDecrypted(context.Background(), tssMgr.keyStore, k, encKey); err != nil {
			missing = append(missing, missingShare{KeyId: key.Id, Epoch: commitment.Epoch, Err: err})
		}
	}
	return missing
}

// reportMissingShares logs each missing share, again at most once an hour. It
// runs in its own goroutine and must never take the node down.
func (tssMgr *TssManager) reportMissingShares(bh uint64) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic recovered in reportMissingShares", "panic", r)
		}
	}()
	for _, m := range tssMgr.heldSharesMissing(bh) {
		if !tssMgr.missingShareDue(m.KeyId+"@"+strconv.FormatUint(m.Epoch, 10), time.Now()) {
			continue
		}
		log.Error("this node is a listed holder of a TSS key but cannot load its share; "+
			"restore the tss-keys directory (and the identity it was encrypted with) from a backup",
			"keyId", m.KeyId, "epoch", m.Epoch, "err", m.Err)
	}
}

// missingShareDue reports whether the missing share id should be logged now.
func (tssMgr *TssManager) missingShareDue(id string, now time.Time) bool {
	tssMgr.missingShareMu.Lock()
	defer tssMgr.missingShareMu.Unlock()
	if tssMgr.missingShareLogged == nil {
		tssMgr.missingShareLogged = make(map[string]time.Time)
	}
	if last, ok := tssMgr.missingShareLogged[id]; ok && now.Sub(last) < missingShareRelog {
		return false
	}
	tssMgr.missingShareLogged[id] = now
	return true
}
