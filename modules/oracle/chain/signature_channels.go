package chain

import "sync"

type signatureMessage struct {
	sig string
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
