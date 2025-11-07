package logstream

import (
	"sync"
)

// ContractLog is a single emitted log from a contract execution.
type ContractLog struct {
	BlockHeight     uint64
	TxHash          string
	ContractAddress string
	Log             string
	Timestamp       string
}

// LogFilterInternal restricts logs by minimum block or contract addresses.
type LogFilterInternal struct {
	FromBlock         *uint64
	ContractAddresses map[string]struct{}
}

// logSubscriber represents a subscription including only filtered logs.
type logSubscriber struct {
	Id     int
	Filter LogFilterInternal
	Ch     chan ContractLog
}

// LogStream manages active subscribers and dispatches contract logs to them.
type LogStream struct {
	mu          sync.Mutex
	nextID      int
	subscribers map[int]*logSubscriber
}

// NewLogStream returns an initialized LogStream.
func NewLogStream() *LogStream {
	return &LogStream{
		subscribers: make(map[int]*logSubscriber),
	}
}

// Publish dispatches a log to all matching subscribers.
func (ls *LogStream) Publish(log ContractLog) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	// fmt.Printf("[logstream] Publish called: height=%d, addr=%s, tx=%s, log=%s\n",
	//	log.BlockHeight, log.ContractAddress, log.TxHash, log.Log)

	for _, sub := range ls.subscribers {
		// fmt.Printf("[logstream] checking subscriber %d\n", sub.Id)
		if sub.Filter.FromBlock != nil && log.BlockHeight < *sub.Filter.FromBlock {
			continue
		}
		if len(sub.Filter.ContractAddresses) > 0 {
			if _, ok := sub.Filter.ContractAddresses[log.ContractAddress]; !ok {
				continue
			}
		}
		select {
		case sub.Ch <- log:
			// fmt.Printf("[logstream] subscriber %d received log\n", sub.Id)
		default: // subscribers log channel is full (they are not reading fat enuugh)
			// fmt.Printf("[logstream] subscriber %d channel full, dropped log!\n", sub.Id)
		}
	}
}

// Subscribe registers and returns a new subscriber for the given filter.
func (ls *LogStream) Subscribe(filter LogFilterInternal) *logSubscriber {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	id := ls.nextID
	ls.nextID++
	sub := &logSubscriber{
		Id:     id,
		Filter: filter,
		Ch:     make(chan ContractLog, 128),
	}
	ls.subscribers[id] = sub
	// fmt.Println("new subscriber: ", id)
	return sub
}

// Unsubscribe removes a subscriber and closes its channel.
func (ls *LogStream) Unsubscribe(sub *logSubscriber) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	delete(ls.subscribers, sub.Id)
	// fmt.Println("closed subscriber: ", sub.Id)
	close(sub.Ch)
}
