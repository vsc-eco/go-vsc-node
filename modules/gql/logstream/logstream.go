package logstream

import (
	"fmt"
	"sync"
)

// ContractLog represents a single log emitted by a contract.
type ContractLog struct {
	BlockHeight     uint64
	TxID            string
	ContractAddress string
	Log             string
	Timestamp       string
}

// LogFilterInternal defines filters for a subscription.
// It can limit logs by block height or contract address.
type LogFilterInternal struct {
	FromBlock         *uint64
	ContractAddresses map[string]struct{}
}

// logSubscriber holds subscription state for one client.
type logSubscriber struct {
	Id     int
	Filter LogFilterInternal
	Ch     chan ContractLog
}

// LogStream manages log subscribers and broadcasts logs to them.
type LogStream struct {
	mu          sync.Mutex
	nextID      int
	subscribers map[int]*logSubscriber
	latestBlock uint64 // last published block height
}

// NewLogStream returns a new LogStream.
func NewLogStream() *LogStream {
	return &LogStream{
		subscribers: make(map[int]*logSubscriber),
	}
}

// Publish sends a log to all matching subscribers.
// Drops logs if a subscriber channel is full to avoid blocking.
func (ls *LogStream) Publish(log ContractLog) {
	ls.mu.Lock()
	if log.BlockHeight > ls.latestBlock {
		ls.latestBlock = log.BlockHeight
	}
	defer ls.mu.Unlock()

	fmt.Printf("[logstream] Publish called: height=%d, addr=%s, tx=%s, log=%s\n",
		log.BlockHeight, log.ContractAddress, log.TxID, log.Log)

	for idx, sub := range ls.subscribers {
		fmt.Printf("[logstream] checking subscriber %d: filter.FromBlock=%v filter.Contracts=%v\n",
			idx, sub.Filter.FromBlock, sub.Filter.ContractAddresses)

		// Skip blocks below FromBlock.
		if sub.Filter.FromBlock != nil && log.BlockHeight < *sub.Filter.FromBlock {
			continue
		}

		// Skip unmatched contract addresses.
		if len(sub.Filter.ContractAddresses) > 0 {
			if _, ok := sub.Filter.ContractAddresses[log.ContractAddress]; !ok {
				continue
			}
		}

		// Send log or drop if channel is full.
		select {
		case sub.Ch <- log:
			fmt.Printf("[logstream] subscriber %d received log\n", idx)
		default:
			fmt.Printf("[logstream] subscriber %d channel full, dropped log!\n", idx)
		}
	}
}

// Subscribe registers a new subscriber for given filters.
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
	return sub
}

// Unsubscribe removes a subscriber and closes its channel.
func (ls *LogStream) Unsubscribe(sub *logSubscriber) {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	delete(ls.subscribers, sub.Id)
	close(sub.Ch)
}

// Replay loads historical logs via logsSource and publishes them.
// The logsSource must call the callback once for each log in [from, to].
//
// Example:
//
//	ls.Replay(100, 200, db.StreamLogsInRange)
func (ls *LogStream) Replay(fromBlock, toBlock uint64, logsSource func(uint64, uint64, func(ContractLog) error) error) error {
	fmt.Printf("[logstream] replaying logs from %d to %d\n", fromBlock, toBlock)

	count := 0
	err := logsSource(fromBlock, toBlock, func(l ContractLog) error {
		count++
		fmt.Printf("[logstream] replay [%d]: block=%d addr=%s tx=%s\n",
			count, l.BlockHeight, l.ContractAddress, l.TxID)
		ls.Publish(l)
		return nil
	})
	if err != nil {
		return fmt.Errorf("logstream replay: failed to load logs: %w", err)
	}

	fmt.Printf("[logstream] replay complete: %d logs published\n", count)
	return nil
}

// CurrentHeight returns the latest published block height.
func (ls *LogStream) CurrentHeight() uint64 {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.latestBlock
}
