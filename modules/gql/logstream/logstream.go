package logstream

import (
	"fmt"
	"sync"
)

// ContractLog represents a single log entry emitted by a contract execution.
type ContractLog struct {
	BlockHeight     uint64
	TxHash          string
	ContractAddress string
	Log             string
	Timestamp       string
}

// LogFilterInternal defines optional filters for a log subscription.
// A subscriber can limit logs by starting block or by specific contract addresses.
type LogFilterInternal struct {
	FromBlock         *uint64
	ContractAddresses map[string]struct{}
}

// logSubscriber represents an active subscription and its internal state.
type logSubscriber struct {
	Id     int
	Filter LogFilterInternal
	Ch     chan ContractLog
}

// LogStream manages subscribers and broadcasts logs to them as they come in.
type LogStream struct {
	mu          sync.Mutex
	nextID      int
	subscribers map[int]*logSubscriber
	latestBlock uint64 // latest block height published
}

// NewLogStream creates and returns a new LogStream instance.
func NewLogStream() *LogStream {
	return &LogStream{
		subscribers: make(map[int]*logSubscriber),
	}
}

// Publish sends a new log to all subscribers whose filters match it.
// If a subscriber's channel is full, the log is dropped to avoid blocking.
func (ls *LogStream) Publish(log ContractLog) {
	ls.mu.Lock()
	if log.BlockHeight > ls.latestBlock {
		ls.latestBlock = log.BlockHeight
	}
	defer ls.mu.Unlock()

	fmt.Printf("[logstream] Publish called: height=%d, addr=%s, tx=%s, log=%s\n",
		log.BlockHeight, log.ContractAddress, log.TxHash, log.Log)

	for idx, sub := range ls.subscribers {
		fmt.Printf("[logstream] checking subscriber %d: filter.FromBlock=%v filter.Contracts=%v\n",
			idx, sub.Filter.FromBlock, sub.Filter.ContractAddresses)

		// Skip if below fromBlock
		if sub.Filter.FromBlock != nil && log.BlockHeight < *sub.Filter.FromBlock {
			fmt.Printf("[logstream] subscriber %d skipped: block %d < fromBlock %d\n",
				idx, log.BlockHeight, *sub.Filter.FromBlock)
			continue
		}
		// Skip if contract doesn't match filter
		if len(sub.Filter.ContractAddresses) > 0 {
			if _, ok := sub.Filter.ContractAddresses[log.ContractAddress]; !ok {
				fmt.Printf("[logstream] subscriber %d skipped: addr %s not in filter\n",
					idx, log.ContractAddress)
				continue
			}
		}

		// Forward the log to the subscriber
		fmt.Printf("[logstream] subscriber %d accepted log, sending...\n", idx)
		select {
		case sub.Ch <- log:
			fmt.Printf("[logstream] subscriber %d received log\n", idx)
		default:
			fmt.Printf("[logstream] subscriber %d channel full, dropped log!\n", idx)
		}
	}
}

// Subscribe registers a new subscriber for the given filter
// and returns the subscription instance.
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

// Replay loads historical logs from a given range using the provided
// logsSource function, and publishes them as if they were new.
//
// Example usage:
//
//	err := logStream.Replay(1, 100, db.LoadLogsInRange)
//
// The logsSource function must return all logs in the range [from, to].
func (ls *LogStream) Replay(fromBlock, toBlock uint64, logsSource func(uint64, uint64) ([]ContractLog, error)) error {
	fmt.Printf("[logstream] replaying logs from %d to %d\n", fromBlock, toBlock)

	logs, err := logsSource(fromBlock, toBlock)
	if err != nil {
		return fmt.Errorf("logstream replay: failed to load logs: %w", err)
	}

	for i, l := range logs {
		fmt.Printf("[logstream] replay [%d/%d]: block=%d addr=%s tx=%s\n",
			i+1, len(logs), l.BlockHeight, l.ContractAddress, l.TxHash)
		ls.Publish(l)
	}

	fmt.Printf("[logstream] replay complete: %d logs published\n", len(logs))
	return nil
}

// CurrentHeight returns the most recent block height that has been published.
func (ls *LogStream) CurrentHeight() uint64 {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	return ls.latestBlock
}
