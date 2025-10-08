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

type signatureMessage struct {
	// base64 encoded string of 96 bytes is 128
	Signature string `json:"signature,omitempty" validate:"base64,required,len=128"`
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

func (s *signatureChannels) clearMap() {
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	for k := range s.chanMap {
		close(s.chanMap[k])
	}
	s.chanMap = make(map[string]chan signatureMessage)
}
