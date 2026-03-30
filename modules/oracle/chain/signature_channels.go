package chain

import (
	"errors"
	"sync"
)

var (
	errChannelExists  = errors.New("channel exists")
	errInvalidSession = errors.New("invalid session")
	errChannelFull    = errors.New("channel full")
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
}

func makeSignatureChannels() *signatureChannels {
	rwLock := &sync.RWMutex{}
	chanMap := make(map[string]chan signatureMessage)
	return &signatureChannels{rwLock, chanMap}
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

	return s.chanMap[sessionID], nil
}

func (s *signatureChannels) receiveSignature(
	sessionID string,
	msg signatureMessage,
) error {
	s.rwLock.RLock()
	defer s.rwLock.RUnlock()

	c, ok := s.chanMap[sessionID]
	if !ok {
		return errInvalidSession
	}

	select {
	case c <- msg:
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
}

func (s *signatureChannels) clearMap() {
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	for k := range s.chanMap {
		close(s.chanMap[k])
	}
	s.chanMap = make(map[string]chan signatureMessage)
}
