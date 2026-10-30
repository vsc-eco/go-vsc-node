package chain

import (
	"errors"
	"sync"
)

var (
	errChannelExists  = errors.New("channel exists")
	errInvalidSession = errors.New("invalid session")
	errChannelFull    = errors.New("channel full")
	errDuplicateSig   = errors.New("duplicate signature for session")
)

// signatureMessage carries a witness's BLS signature response through
// the internal channel system.
type signatureMessage struct {
	Signature string `json:"signature"` // base64 BLS signature
	Account   string `json:"account"`   // signer's hive account
	BlsDid    string `json:"bls_did"`   // signer's BLS DID
}

// seenKey identifies one exact (claimed-DID, signature) pair within a session. The intake
// dedup keys on this pair - NOT on the DID alone.
//
// NC-1: the bls_did in a relayed signatureResponse is unverified, self-declared JSON; it is
// only authenticated later, when collectChainSignatures' AddAndVerify checks the signature
// against that DID. Keying the intake dedup on the claimed DID alone let one keyless peer
// pre-send a junk message carrying a victim's DID and permanently mark that DID "seen", so
// the victim's GENUINE signature was then refused as errDuplicateSig - one junk message per
// committee DID blocked the whole relay and no BTC headers were ever signed.
//
// A BLS signature is deterministic and unforgeable: a junk message carries a DIFFERENT
// signature than the genuine one, so the two key to different entries and the genuine
// message still enqueues. Keying on the pair still drops a true identical repeat (VR2-19's
// buffer-flood case - a witness rapid-firing copies of its own response). The authoritative
// per-signer WEIGHT dedup stays in collectChainSignatures, keyed on the VERIFIED DID after
// AddAndVerify, so a forged message is never credited.
type seenKey struct {
	did string
	sig string
}

type signatureChannels struct {
	rwLock  *sync.RWMutex
	chanMap map[string]chan signatureMessage
	// VR2-19: (DID, signature) pairs already enqueued for a session, so an identical
	// repeat is rejected BEFORE it occupies one of the 8 buffer slots. Without this a
	// witness rapid-firing duplicates could fill the buffer faster than the single
	// collection goroutine drains it (each drain does a BLS verify), pushing GENUINE
	// signatures from other witnesses into errChannelFull.
	seen map[string]map[seenKey]bool
}

func makeSignatureChannels() *signatureChannels {
	rwLock := &sync.RWMutex{}
	chanMap := make(map[string]chan signatureMessage)
	seen := make(map[string]map[seenKey]bool)
	return &signatureChannels{rwLock: rwLock, chanMap: chanMap, seen: seen}
}

func (s *signatureChannels) makeSession(
	sessionID string,
) (<-chan signatureMessage, error) {
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	_, ok := s.chanMap[sessionID]
	if ok {
		return nil, errChannelExists
	}

	s.chanMap[sessionID] = make(chan signatureMessage, 8)
	s.seen[sessionID] = make(map[seenKey]bool)

	return s.chanMap[sessionID], nil
}

func (s *signatureChannels) receiveSignature(
	sessionID string,
	msg signatureMessage,
) error {
	// A write lock: the duplicate check and the enqueue must be atomic, or two
	// concurrent copies of the same signature both pass the check.
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	c, ok := s.chanMap[sessionID]
	if !ok {
		return errInvalidSession
	}

	// VR2-19 / NC-1: drop an identical (DID, signature) repeat before it consumes a buffer
	// slot, but only key on the pair - never the DID alone - so an unverified DID claim
	// cannot pre-block a genuine distinct signature. A message with no signature carries
	// nothing to dedup on (and the collector skips it anyway), so it is never deduped here.
	key := seenKey{did: msg.BlsDid, sig: msg.Signature}
	if msg.Signature != "" {
		if s.seen[sessionID][key] {
			return errDuplicateSig
		}
	}

	select {
	case c <- msg:
		if msg.Signature != "" {
			if s.seen[sessionID] == nil {
				s.seen[sessionID] = make(map[seenKey]bool)
			}
			s.seen[sessionID][key] = true
		}
		return nil
	default:
		return errChannelFull
	}
}

// clearSession closes and removes a single session channel.
func (s *signatureChannels) clearSession(sessionID string) {
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	if ch, ok := s.chanMap[sessionID]; ok {
		close(ch)
		delete(s.chanMap, sessionID)
	}
	delete(s.seen, sessionID)
}

func (s *signatureChannels) clearMap() {
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	for k := range s.chanMap {
		close(s.chanMap[k])
	}
	s.chanMap = make(map[string]chan signatureMessage)
	s.seen = make(map[string]map[seenKey]bool)
}
