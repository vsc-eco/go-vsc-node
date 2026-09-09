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

type signatureChannels struct {
	rwLock  *sync.RWMutex
	chanMap map[string]chan signatureMessage
	// VR2-19: signers already enqueued for a session, so a repeat is rejected
	// BEFORE it occupies one of the 8 buffer slots. Without this, a witness
	// rapid-firing duplicates could fill the buffer faster than the single
	// collection goroutine drains it (each drain does a BLS verify), pushing
	// GENUINE signatures from other witnesses into errChannelFull — a liveness
	// grief distinct from the weight double-count, and reachable by one node.
	seen map[string]map[string]bool
}

func makeSignatureChannels() *signatureChannels {
	rwLock := &sync.RWMutex{}
	chanMap := make(map[string]chan signatureMessage)
	seen := make(map[string]map[string]bool)
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
	s.seen[sessionID] = make(map[string]bool)

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

	// VR2-19: drop a repeat from a signer already queued for this session before
	// it consumes a buffer slot. The collection loop credits each signer once
	// regardless; rejecting here additionally stops one witness from crowding
	// honest signatures out of the buffer.
	if msg.BlsDid != "" {
		if s.seen[sessionID][msg.BlsDid] {
			return errDuplicateSig
		}
	}

	select {
	case c <- msg:
		if msg.BlsDid != "" {
			if s.seen[sessionID] == nil {
				s.seen[sessionID] = make(map[string]bool)
			}
			s.seen[sessionID][msg.BlsDid] = true
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
	s.seen = make(map[string]map[string]bool)
}
