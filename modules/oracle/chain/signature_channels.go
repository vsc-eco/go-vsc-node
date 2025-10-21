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

type signatureChannels struct {
	rwLock  *sync.RWMutex
	chanMap map[string]chan chainOracleWitnessMessage
}

func makeSignatureChannels() *signatureChannels {
	rwLock := &sync.RWMutex{}
	chanMap := make(map[string]chan chainOracleWitnessMessage)
	return &signatureChannels{rwLock, chanMap}
}

func (s *signatureChannels) makeSession(
	sessionID string,
) (<-chan chainOracleWitnessMessage, error) {
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	_, ok := s.chanMap[sessionID]
	if ok {
		return nil, errChannelExists
	}

	s.chanMap[sessionID] = make(chan chainOracleWitnessMessage, 8)

	return s.chanMap[sessionID], nil
}

func (s *signatureChannels) removeSession(sessionID string) error {
	s.rwLock.Lock()
	defer s.rwLock.Unlock()

	c, ok := s.chanMap[sessionID]
	if ok {
		return errInvalidSession
	}

	close(c)
	delete(s.chanMap, sessionID)

	return nil
}

func (s *signatureChannels) receiveSignature(
	sessionID string,
	msg chainOracleWitnessMessage,
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
