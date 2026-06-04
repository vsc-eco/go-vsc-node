package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	islock "vsc-node/modules/islock-attestation"
)

// Audit R17-SEC-sanitizeRPCURL-leaks-on-parse-edge-cases (HIGH) +
// R17-OPS-sanitize-rpc-url-helper-duplicated (INFO): the local
// islock.SanitizeRPCURLForLog used to be a byte-for-byte copy of
// islock.SanitizeRPCURLForLog with the same parse-error + query-
// string + opaque-userinfo bugs. Now there's exactly one
// implementation — call the exported one in modules/islock-
// attestation so a future bug-fix lands once.

// DashdRPCClient is a minimal RPC client for the few methods we need:
//   - getrawtransaction <txid> 1 — returns tx details including
//     `instantlock` flag (Dash-specific extension)
//   - getrawmempool — list of txids currently in mempool
//
// HTTP RPC polling was chosen over ZMQ for ops simplicity: no extra
// dependency (pure-Go net/http), no extra port to expose on the
// dashd side, no PUB/SUB filter config. The IS service's overall
// latency budget (~10-15s end-to-end per /session/start) absorbs a
// 1-2s RPC poll cycle with no UX impact.
//
// Validators use the same HTTP RPC poll model — see
// modules/islock-attestation/dashd_poller.go. Audit R17-CONS-zmq-
// comment-survives-in-dashd-watcher retired the prior "ZMQ vs RPC
// trade-off + validators will use ZMQ eventually" wording.
type DashdRPCClient struct {
	url      string
	user     string
	password string
	client   *http.Client
}

// NewDashdRPCClient constructs a client. URL must include scheme +
// host:port, e.g. "http://vsc-dashd-testnet:9998".
func NewDashdRPCClient(url, user, password string) *DashdRPCClient {
	return &DashdRPCClient{
		url:      url,
		user:     user,
		password: password,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

type rpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
	ID      int    `json:"id"`
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	ID     int             `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("dashd rpc error %d: %s", e.Code, e.Message)
}

func (c *DashdRPCClient) call(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(rpcReq{
		JSONRPC: "1.0",
		Method:  method,
		Params:  params,
		ID:      1,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var r rpcResp
	if err := json.Unmarshal(respBody, &r); err != nil {
		return fmt.Errorf("decode response: %w (body=%s)", err, string(respBody))
	}
	if r.Error != nil {
		return r.Error
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// RawTransactionResult is the subset of dashd's getrawtransaction verbose
// response that we consume. The InstantLock field is Dash-specific and
// indicates the LLMQ has signed the IS-lock for this tx.
type RawTransactionResult struct {
	TxID        string `json:"txid"`
	Hex         string `json:"hex"`
	InstantLock bool   `json:"instantlock"`
	// Vout carries the output set so the watcher can pre-match watched
	// addresses BEFORE firing onObserved. Without this filter every
	// session-watching the watcher would mass-transition on every
	// IS-locked tx in the mempool.
	Vout []RawTxOutput `json:"vout"`
}

// RawTxOutput is the minimal projection of dashd's verbose vout entry
// we need: the addresses script pays to. Other fields (value, n, type)
// exist on the wire but we don't consume them — Vout is purely for the
// per-tx watcher fan-out filter.
type RawTxOutput struct {
	ScriptPubKey RawTxScriptPubKey `json:"scriptPubKey"`
}

type RawTxScriptPubKey struct {
	Addresses []string `json:"addresses"`
	// Some Dash forks ALSO surface an `address` (singular) field for
	// single-address scripts. Accept both for robustness.
	Address string `json:"address"`
}

// payeeAddresses returns the union of address-strings across all vout
// entries (de-duped, order preserved by first sighting). Used by the
// poll loop to filter the watched-address fan-out.
func (r *RawTransactionResult) payeeAddresses() []string {
	if r == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(r.Vout))
	out := make([]string, 0, len(r.Vout))
	for _, vo := range r.Vout {
		if vo.ScriptPubKey.Address != "" {
			if _, dup := seen[vo.ScriptPubKey.Address]; !dup {
				seen[vo.ScriptPubKey.Address] = struct{}{}
				out = append(out, vo.ScriptPubKey.Address)
			}
		}
		for _, a := range vo.ScriptPubKey.Addresses {
			if _, dup := seen[a]; !dup {
				seen[a] = struct{}{}
				out = append(out, a)
			}
		}
	}
	return out
}

// GetRawTransaction fetches a tx's verbose details. Returns nil + a
// well-typed not-found error if the tx isn't on the node yet. Caller
// should retry with backoff in that case.
func (c *DashdRPCClient) GetRawTransaction(ctx context.Context, txid string) (*RawTransactionResult, error) {
	var r RawTransactionResult
	if err := c.call(ctx, "getrawtransaction", []any{txid, 1}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRawMempool returns the list of txids in mempool. Used by the
// watcher to scan for IS-locks affecting our watched addresses.
func (c *DashdRPCClient) GetRawMempool(ctx context.Context) ([]string, error) {
	var ids []string
	if err := c.call(ctx, "getrawmempool", []any{}, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

// ===== watcher =====

// DashdWatcher polls dashd for IS-lock observations on a set of
// per-session deposit addresses. When it sees an IS-lock for one of our
// addresses, it invokes onObserved with the session's sid, the dashd
// txid, and the raw tx hex.
//
// Per-session lifetime: a session's deposit address is registered with
// the watcher when POST /session/start succeeds and unregistered when
// the session reaches a terminal state.
//
// Watcher is concurrent-safe. Start runs the poll loop in a goroutine
// until Stop is called or the context is cancelled.
type DashdWatcher struct {
	client *DashdRPCClient
	mu     sync.RWMutex
	// addr → session info (sid + onObserved cb)
	watched map[string]watchedAddress
	// pollInterval controls the dashd polling cadence. Default 2s.
	pollInterval time.Duration
	// healthMu protects the consecutiveFails / lastErr / lastErrAt
	// fields exposed via Health() for /healthz.
	healthMu         sync.Mutex
	consecutiveFails int
	lastErr          error
	lastErrAt        time.Time
	// bypassISLockCheck: when true, the poll loop treats every observed
	// tx as IS-locked regardless of dashd's `instantlock` field.
	// **TEST-ONLY** — set via -testBypassDashdISLock=true, gated by
	// args.go to -network=devnet. Used by tests/devnet's IS-login E2E
	// because regtest dashd has no LLMQ to produce real IS-locks.
	bypassISLockCheck bool
}

type watchedAddress struct {
	sid        string
	onObserved func(sid, txid, rawTxHex string)
}

func NewDashdWatcher(client *DashdRPCClient) *DashdWatcher {
	return &DashdWatcher{
		client:       client,
		watched:      make(map[string]watchedAddress),
		pollInterval: 2 * time.Second,
	}
}

// SetBypassISLockCheck toggles the test-only IS-lock bypass. See the
// `bypassISLockCheck` field comment. Should only be called from main
// when -testBypassDashdISLock=true (which args.go gates to
// -network=devnet).
func (w *DashdWatcher) SetBypassISLockCheck(bypass bool) {
	w.bypassISLockCheck = bypass
}

// Watch registers an address for IS-lock observation.
func (w *DashdWatcher) Watch(addr, sid string, onObserved func(sid, txid, rawTxHex string)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watched[addr] = watchedAddress{sid: sid, onObserved: onObserved}
}

// Unwatch removes a registered address.
func (w *DashdWatcher) Unwatch(addr string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.watched, addr)
}

// Run starts the poll loop. Blocks until ctx is cancelled.
//
// Strategy:
//  1. Every pollInterval seconds, fetch the mempool.
//  2. For each new mempool txid, fetch getrawtransaction.
//  3. Inspect outputs — does any pay to one of our watched addresses?
//  4. If yes AND instantlock==true, fire onObserved.
//  5. Track recently-seen txids to avoid double-firing.
//
// This is O(mempool-size + watch-set-size) per poll. For testnet/dev
// load it's negligible. Production hardening can shard or add filters
// before this becomes a bottleneck.
func (w *DashdWatcher) Run(ctx context.Context) error {
	seen := make(map[string]bool)
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	// consecutiveFailures: escalate poll-error logging once we've
	// missed enough cycles that the watcher is effectively offline
	// (audit `dashd-rpc-errors-only-at-debug-level`).
	consecutiveFailures := 0
	const escalateAfter = 5

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := w.poll(ctx, seen); err != nil {
				consecutiveFailures++
				// Audit R16-OPS-is-service-dashd-watcher-no-rpc-url +
				// R16-CONS-dashd-watcher-rpc-url-still-missing: mirror
				// the validator-side dashd_poller fix (commit 1ec40c77)
				// so multi-dashd deploys can identify the failing
				// endpoint from a single log line.
				if consecutiveFailures == escalateAfter {
					slog.Error("dashd watcher: poll has failed repeatedly — sessions will stall in WAITING_FOR_IS",
						"consecutiveFailures", consecutiveFailures, "rpc", islock.SanitizeRPCURLForLog(w.client.url), "err", err)
				} else if consecutiveFailures > escalateAfter && consecutiveFailures%30 == 0 {
					// Once we've escalated, sample every ~30 polls
					// (~1 min at default cadence) so the log isn't flooded.
					slog.Error("dashd watcher still failing",
						"consecutiveFailures", consecutiveFailures, "rpc", islock.SanitizeRPCURLForLog(w.client.url), "err", err)
				} else {
					slog.Debug("dashd watcher poll error",
						"consecutiveFailures", consecutiveFailures, "rpc", islock.SanitizeRPCURLForLog(w.client.url), "err", err)
				}
				w.healthMu.Lock()
				w.lastErr = err
				w.lastErrAt = time.Now()
				w.consecutiveFails = consecutiveFailures
				w.healthMu.Unlock()
				// Don't return — transient errors are normal during
				// dashd restarts/network blips. The /healthz handler
				// surfaces the degraded state.
			} else if consecutiveFailures > 0 {
				slog.Info("dashd watcher recovered",
					"previousConsecutiveFailures", consecutiveFailures, "rpc", islock.SanitizeRPCURLForLog(w.client.url))
				consecutiveFailures = 0
				w.healthMu.Lock()
				w.lastErr = nil
				w.consecutiveFails = 0
				w.healthMu.Unlock()
			}
		}
	}
}

// Health returns the watcher's last-error + consecutive-failure count
// for /healthz probes.
func (w *DashdWatcher) Health() (consecutiveFails int, lastErr error, lastErrAt time.Time) {
	w.healthMu.Lock()
	defer w.healthMu.Unlock()
	return w.consecutiveFails, w.lastErr, w.lastErrAt
}

func (w *DashdWatcher) poll(ctx context.Context, seen map[string]bool) error {
	// Snapshot watched addresses under read-lock for the duration of one poll.
	w.mu.RLock()
	if len(w.watched) == 0 {
		w.mu.RUnlock()
		return nil // nothing to do
	}
	addrs := make(map[string]watchedAddress, len(w.watched))
	for k, v := range w.watched {
		addrs[k] = v
	}
	w.mu.RUnlock()

	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	mempool, err := w.client.GetRawMempool(pollCtx)
	if err != nil {
		return err
	}

	for _, txid := range mempool {
		if seen[txid] {
			continue
		}
		txCtx, txCancel := context.WithTimeout(ctx, 3*time.Second)
		tx, err := w.client.GetRawTransaction(txCtx, txid)
		txCancel()
		if err != nil {
			continue // skip; might be in mempool but not yet fully indexed
		}
		if !tx.InstantLock && !w.bypassISLockCheck {
			// Not yet locked — don't mark seen so we re-check on next poll.
			// bypassISLockCheck=true (devnet test-only) short-circuits this
			// check; see field doc.
			continue
		}
		seen[txid] = true
		// Per-output address match: only fire onObserved for sessions
		// whose deposit address actually appears in tx.vout. Without
		// this filter every IS-locked tx fans out to every watched
		// session, mass-transitioning unrelated sessions to IS_OBSERVED
		// with the wrong txid — see audit finding
		// `watcher-fires-onobserved-cross-product`.
		payees := tx.payeeAddresses()
		if len(payees) == 0 {
			// dashd's vout didn't decode (legacy multisig, op_return,
			// older dashd lacking scriptPubKey.addresses). Skip — we
			// can't safely fan-out without knowing who got paid.
			slog.Debug("dashd tx has no decodable output addresses; skipping fan-out",
				"txid", txid)
			continue
		}
		for _, payee := range payees {
			wa, ok := addrs[payee]
			if !ok {
				continue
			}
			wa.onObserved(wa.sid, txid, tx.Hex)
		}
	}

	// Trim "seen" once it gets large — keep last 10K observations.
	if len(seen) > 10_000 {
		// Simple: drop everything older than some threshold. Since we
		// don't track timestamps here, just clear. The cost is one
		// duplicate fire per session if a tx is re-seen post-clear,
		// which is harmless (idempotency at the contract layer).
		for k := range seen {
			delete(seen, k)
		}
	}

	return nil
}
