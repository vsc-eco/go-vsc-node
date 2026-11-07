package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// P7 (BTC bot parity: FailedTxStore + POST /retry, http_server.go retryHandler,
// mapper/init.go RetryFailedTx). The bot dead-letters a deposit it will not map
// on its own (a map that failed on chain, no result in time, an oversize
// proof). An operator who has fixed the cause (registered the relayer, funded
// RC, raised a limit) asks the bot to try it again: POST /retry on the health
// listener with "Authorization: Bearer $OPS_API_KEY" and
// {"deposits":["0x<deposit tx hash>", ...]}. The endpoint only queues the
// request; the main loop, which owns the checkpoint, re-scans the deposit's
// block and parks the deposit in WaitingDeposits, where the normal path maps
// it (the contract refuses a deposit it has already credited). Throttled like
// BTC's: one request per retryGlobalThrottle, one per deposit per
// retryTxThrottle. Unset OPS_API_KEY: the endpoint answers 403.

var (
	retryGlobalThrottle = 10 * time.Second
	retryTxThrottle     = 2 * time.Minute
	depositTxPattern    = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)
)

// errRetryThrottled is a request inside a cooldown.
var errRetryThrottled = errors.New("retry throttled")

type deadLetterRetries struct {
	mu         sync.Mutex
	apiKey     string
	queued     map[string]bool // lowercase deposit tx hash
	lastGlobal time.Time
	lastPer    map[string]time.Time
	now        func() time.Time
}

func newDeadLetterRetries(apiKey string) *deadLetterRetries {
	return &deadLetterRetries{apiKey: apiKey, queued: map[string]bool{}, lastPer: map[string]time.Time{}, now: time.Now}
}

// request queues txs for the next tick. It refuses the whole request inside the
// global cooldown (consuming no per-deposit cooldown, as BTC's does), and skips a
// deposit inside its own.
func (r *deadLetterRetries) request(txs []string) (accepted, throttled []string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.lastGlobal.IsZero() && now.Sub(r.lastGlobal) < retryGlobalThrottle {
		return nil, nil, errRetryThrottled
	}
	r.lastGlobal = now
	for _, tx := range txs {
		k := strings.ToLower(tx)
		if t, ok := r.lastPer[k]; ok && now.Sub(t) < retryTxThrottle {
			throttled = append(throttled, tx)
			continue
		}
		r.lastPer[k] = now
		r.queued[k] = true
		accepted = append(accepted, tx)
	}
	return accepted, throttled, nil
}

func (r *deadLetterRetries) drain() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.queued))
	for k := range r.queued {
		out = append(out, k)
	}
	r.queued = map[string]bool{}
	return out
}

// handler serves POST /retry.
func (r *deadLetterRetries) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		writeJSON := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		if r.apiKey == "" {
			writeJSON(http.StatusForbidden, map[string]string{"error": "/retry disabled: set OPS_API_KEY"})
			return
		}
		const prefix = "Bearer "
		auth := req.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) || subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, prefix)), []byte(r.apiKey)) != 1 {
			writeJSON(http.StatusUnauthorized, map[string]string{"error": "invalid or missing API key"})
			return
		}
		req.Body = http.MaxBytesReader(w, req.Body, 1<<20)
		var body struct {
			Deposits []string `json:"deposits"`
		}
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeJSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			writeJSON(http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if len(body.Deposits) == 0 || len(body.Deposits) > 100 {
			writeJSON(http.StatusBadRequest, map[string]string{"error": "deposits must list 1 to 100 deposit tx hashes"})
			return
		}
		for _, d := range body.Deposits {
			if !depositTxPattern.MatchString(d) {
				writeJSON(http.StatusBadRequest, map[string]string{"error": "deposits must be 0x-prefixed 32-byte tx hashes"})
				return
			}
		}
		accepted, throttled, err := r.request(body.Deposits)
		if err != nil {
			writeJSON(http.StatusTooManyRequests, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(http.StatusAccepted, map[string]any{"queued": accepted, "throttled": throttled})
	}
}

// requeueDeadLetters moves the queued dead-lettered deposits, and the ones
// whose map vanished (AutoRetry, I-46, with or without the operator API), back
// to WaitingDeposits. A deposit not in DeadLetters is ignored (logged); a block
// that cannot be re-scanned now keeps its dead letter and the request is put
// back for the next tick.
func requeueDeadLetters(rpc *ethRPC, cp *Checkpoint, cfg EVMBotConfig, r *deadLetterRetries) {
	cp.pruneDeadLetters(time.Now())
	var txs []string
	seen := map[string]bool{}
	cp.mu.Lock()
	for _, dl := range cp.DeadLetters {
		if k := strings.ToLower(dl.DepositTx); dl.AutoRetry && !seen[k] {
			seen[k] = true
			txs = append(txs, dl.DepositTx)
		}
	}
	cp.mu.Unlock()
	if r != nil {
		for _, tx := range r.drain() {
			if k := strings.ToLower(tx); !seen[k] {
				seen[k] = true
				txs = append(txs, tx)
			}
		}
	}
	for _, tx := range txs {
		cp.mu.Lock()
		idx := -1
		for i, dl := range cp.DeadLetters {
			if strings.EqualFold(dl.DepositTx, tx) {
				idx = i
				break
			}
		}
		var block uint64
		if idx >= 0 {
			block = cp.DeadLetters[idx].Block
		}
		cp.mu.Unlock()
		if idx < 0 {
			slog.Warn("P7: retry requested for a deposit that is not dead-lettered; ignored", "deposit", tx)
			continue
		}
		res, err := scanBlockSafe(rpc, block, cfg.scanVaults(), cfg.Tokens)
		if err != nil {
			slog.Warn("P7: cannot re-scan the dead-lettered deposit's block; trying next tick", "deposit", tx, "block", block, "err", err)
			if r != nil { // an AutoRetry dead letter stays and is tried again anyway
				r.mu.Lock()
				r.queued[tx] = true
				r.mu.Unlock()
			}
			continue
		}
		var found []detectedDeposit
		for _, d := range res.Deposits {
			if strings.EqualFold(d.TxHash, tx) {
				found = append(found, d)
			}
		}
		cp.mu.Lock()
		if len(found) == 0 {
			for i := range cp.DeadLetters { // left to the operator: not re-scanned every tick (I-46)
				if strings.EqualFold(cp.DeadLetters[i].DepositTx, tx) {
					cp.DeadLetters[i].AutoRetry = false
				}
			}
			cp.mu.Unlock()
			slog.Warn("P7: the dead-lettered deposit is not in its block on re-scan; kept dead-lettered", "deposit", tx, "block", block)
			continue
		}
		if cp.WaitingDeposits == nil {
			cp.WaitingDeposits = map[uint64][]detectedDeposit{}
		}
		have := map[string]bool{}
		for _, d := range cp.WaitingDeposits[block] {
			have[depositKey(d)] = true
		}
		for _, d := range found {
			if !have[depositKey(d)] {
				cp.WaitingDeposits[block] = append(cp.WaitingDeposits[block], d)
			}
		}
		// the block's earlier "already submitted" marks would skip it again
		if keys := cp.BlockMapped[block]; len(keys) > 0 {
			kept := keys[:0]
			for _, k := range keys {
				if !strings.HasPrefix(k, strings.ToLower(tx)+"#") {
					kept = append(kept, k)
				}
			}
			cp.BlockMapped[block] = kept
		}
		kept := cp.DeadLetters[:0]
		for _, dl := range cp.DeadLetters {
			if !strings.EqualFold(dl.DepositTx, tx) {
				kept = append(kept, dl)
			}
		}
		cp.DeadLetters = kept
		cp.mu.Unlock()
		slog.Info("P7: dead-lettered deposit queued for mapping again", "deposit", tx, "block", block, "deposits", len(found))
	}
}
