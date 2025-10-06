package chain

import (
	"errors"
	"sync"
)

var errChannelExists = errors.New("channel exists")

type signatureMessage struct {
	signature string
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

func (s *signatureChannels) clearMap() {
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	for k := range s.chanMap {
		close(s.chanMap[k])
	}
	s.chanMap = make(map[string]chan signatureMessage)
}
