package islock_attestation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// SanitizeRPCURLForLog reduces a dashd RPC URL to "<scheme>://<host>"
// so embedded credentials (basic-auth userinfo, query-string tokens,
// fragments, paths that happen to encode secrets) don't leak via log
// lines + log-shipping pipelines.
//
// Audit history:
//   - R16-CORR-dashd-rpc-url-logged-raw-may-leak-credentials added the
//     initial sanitiser as `url.Parse + u.User=nil + u.String()`.
//   - R17-SEC-sanitizeRPCURL-leaks-on-parse-edge-cases (HIGH) found
//     three concrete bypasses in that first cut:
//     1. Query strings (`?token=secret`) were preserved verbatim.
//     2. url.Parse errors fell through to `return raw`, so
//     malformed inputs (backslash, control bytes, mismatched
//     IPv6 brackets) emitted the ORIGINAL value with creds
//     intact.
//     3. Inputs that parsed successfully but had empty
//     scheme/host (opaque `user:pass@host` form) still rendered
//     the userinfo via u.String().
//
// Behaviour now matches the sister-helper cmd/is-service/
// orchestrator.go:sanitizeURLForLogWithFlag (used for the L2
// GraphQL URL — Round-7/8 audits R7-OP-01-logleak + R8-SEC-01 /
// R8-DESIGN-DOC-01 closed the same gaps there):
//
//   - empty input → empty output
//   - url.Parse failure → "<redacted: unparseable URL>"
//     (NEVER the raw input)
//   - missing scheme OR host (e.g. "user:pass@host" opaque form,
//     "junk_no_url") → "<redacted: missing scheme>"
//   - well-formed URL → "<scheme>://<host>" (host preserves port +
//     IPv6 brackets; userinfo + query + fragment + path dropped)
func SanitizeRPCURLForLog(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<redacted: unparseable URL>"
	}
	if u.Scheme == "" || u.Host == "" {
		return "<redacted: missing scheme>"
	}
	return u.Scheme + "://" + u.Host
}

// DashdPoller is the validator-side bridge between dashd's RPC and the
// in-memory IsLockMemory cache that islock-attestation.Service consults
// when deciding whether to sign a request. It polls dashd's mempool
// every PollInterval, fetches getrawtransaction for each new txid, and
// — when the tx is IS-locked (or the AcceptUnlocked devnet bypass is
// active) — calls memory.Observe.
//
// Production usage: AcceptUnlocked=false; the poller only records
// txids whose `instantlock` field is true per dashd v23's
// getrawtransaction-verbose output.
//
// Devnet usage: AcceptUnlocked=true; regtest dashd has no LLMQ so
// instantlock is never set. The poller records every observed tx —
// matching the test bypass the IS-service-side dashd watcher uses.
type DashdPoller struct {
	client       *dashdRPCClient
	memory       *IsLockMemory
	pollInterval time.Duration
	// acceptUnlocked is the devnet bypass that records txs even when
	// dashd reports instantlock=false (regtest has no LLMQ).
	acceptUnlocked bool
	// seen txids in mempool — avoids re-fetching getrawtransaction
	// every tick. Capped at 50K with FIFO trim.
	seenMu sync.Mutex
	seen   map[string]struct{}
	seenQ  []string
}

// NewDashdPoller constructs a poller. memory must be the same
// IsLockMemory instance the Service was constructed with so the
// poll-observed txids feed the gossip-handler's Lookup path.
func NewDashdPoller(
	rpcURL, rpcUser, rpcPass string,
	memory *IsLockMemory,
	pollInterval time.Duration,
	acceptUnlocked bool,
) *DashdPoller {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	return &DashdPoller{
		client:         newDashdRPCClient(rpcURL, rpcUser, rpcPass),
		memory:         memory,
		pollInterval:   pollInterval,
		acceptUnlocked: acceptUnlocked,
		seen:           make(map[string]struct{}, 1024),
		seenQ:          make([]string, 0, 1024),
	}
}

// Run blocks until ctx is cancelled. Polls mempool every
// pollInterval. Transient RPC errors are logged + retried on the next
// tick — never fatal.
func (p *DashdPoller) Run(ctx context.Context) error {
	slog.Info("islock-attestation dashd poller starting",
		"acceptUnlocked", p.acceptUnlocked, "pollInterval", p.pollInterval)
	t := time.NewTicker(p.pollInterval)
	defer t.Stop()
	consecutiveFails := 0
	const escalateAfter = 5
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			err := p.tick(ctx)
			if err != nil {
				consecutiveFails++
				// Audit R15-LOG-dashd-poller-no-rpc-url: include the
				// dashd RPC URL so multi-validator deploys with shared
				// dashd infra can identify the failing endpoint without
				// SSH'ing into every host. The URL is operator-config,
				// not user-credentialed, so logging it raw is fine.
				if consecutiveFails == escalateAfter {
					slog.Error("islock-attestation dashd poller: repeated failures — validator will sign nothing",
						"consecutiveFails", consecutiveFails, "rpc", SanitizeRPCURLForLog(p.client.url), "err", err)
				} else if consecutiveFails > escalateAfter && consecutiveFails%30 == 0 {
					slog.Error("islock-attestation dashd poller still failing",
						"consecutiveFails", consecutiveFails, "rpc", SanitizeRPCURLForLog(p.client.url), "err", err)
				}
			} else if consecutiveFails > 0 {
				slog.Info("islock-attestation dashd poller recovered",
					"previousConsecutiveFails", consecutiveFails, "rpc", SanitizeRPCURLForLog(p.client.url))
				consecutiveFails = 0
			}
		}
	}
}

func (p *DashdPoller) tick(ctx context.Context) error {
	mempool, err := p.client.GetRawMempool(ctx)
	if err != nil {
		return fmt.Errorf("getrawmempool: %w", err)
	}
	p.seenMu.Lock()
	defer p.seenMu.Unlock()
	for _, txid := range mempool {
		if _, dup := p.seen[txid]; dup {
			continue
		}
		// Mark seen BEFORE fetching so a failed fetch doesn't loop
		// forever on the same tx within ONE tick. Errored fetches +
		// !instantlock txs unmark themselves below so the next tick
		// retries.
		p.seen[txid] = struct{}{}
		p.seenQ = append(p.seenQ, txid)
		txCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		tx, err := p.client.GetRawTransaction(txCtx, txid)
		cancel()
		if err != nil {
			// Audit R15-CORR-dashd-poller-rpc-error-stranded: a
			// transient RPC error (3s timeout, dashd-not-yet-indexed,
			// JSON decode failure, RPC 500) used to leave the txid
			// in `seen` permanently — until 50K-FIFO trim, which on
			// a quiet chain is effectively never. On low-volume
			// mainnet IS-locked Dash traffic, one timeout on a
			// fresh tx caused the validator to silently refuse to
			// attest forever. Roll back the pre-mark so the next
			// tick retries (same shape as the !instantlock branch
			// below).
			delete(p.seen, txid)
			if len(p.seenQ) > 0 && p.seenQ[len(p.seenQ)-1] == txid {
				p.seenQ = p.seenQ[:len(p.seenQ)-1]
			}
			continue
		}
		if !tx.InstantLock && !p.acceptUnlocked {
			// Not yet locked + production-mode — don't observe.
			// Don't mark as permanently-seen; let it re-fetch next
			// tick when instantlock may have flipped.
			delete(p.seen, txid)
			// Also drop the trailing append we just made.
			if len(p.seenQ) > 0 && p.seenQ[len(p.seenQ)-1] == txid {
				p.seenQ = p.seenQ[:len(p.seenQ)-1]
			}
			continue
		}
		// rawTxHash = sha256d(rawTxBytes) per Bitcoin convention.
		rawBytes, err := hex.DecodeString(tx.Hex)
		if err != nil {
			// Same retry path as the RPC error above: a malformed
			// hex blob (very unlikely, but defensive) shouldn't
			// strand the txid forever.
			delete(p.seen, txid)
			if len(p.seenQ) > 0 && p.seenQ[len(p.seenQ)-1] == txid {
				p.seenQ = p.seenQ[:len(p.seenQ)-1]
			}
			continue
		}
		first := sha256.Sum256(rawBytes)
		rawTxHash := sha256.Sum256(first[:])
		p.memory.Observe(txid, tx.Hex, rawTxHash[:])
	}
	// FIFO trim the seen set.
	const maxSeen = 50_000
	for len(p.seenQ) > maxSeen {
		head := p.seenQ[0]
		p.seenQ = p.seenQ[1:]
		delete(p.seen, head)
	}
	return nil
}

// ===== minimal dashd RPC client (subset of cmd/is-service's, duplicated
// here so modules/ doesn't depend on cmd/) =====

type dashdRPCClient struct {
	url      string
	user     string
	password string
	http     *http.Client
}

func newDashdRPCClient(url, user, password string) *dashdRPCClient {
	return &dashdRPCClient{
		url:      url,
		user:     user,
		password: password,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *dashdRPCClient) call(ctx context.Context, method string, params []any, out any) error {
	body, _ := json.Marshal(rpcRequest{JSONRPC: "1.0", ID: "islock", Method: method, Params: params})
	req, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Audit L3 (LOW): bound the dashd RPC response to prevent
	// unbounded-ReadAll slow-loris / OOM. 32 MiB covers any legitimate
	// dashd response.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	var r rpcResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("rpc response not JSON: %w (raw=%s)", err, string(raw))
	}
	if r.Error != nil {
		return fmt.Errorf("dashd rpc %s: %s (code %d)", method, r.Error.Message, r.Error.Code)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (c *dashdRPCClient) GetRawMempool(ctx context.Context) ([]string, error) {
	var out []string
	return out, c.call(ctx, "getrawmempool", []any{}, &out)
}

// rawTransactionResult matches the subset of dashd's
// getrawtransaction verbose output that the poller consumes.
type rawTransactionResult struct {
	TxID        string `json:"txid"`
	Hex         string `json:"hex"`
	InstantLock bool   `json:"instantlock"`
}

func (c *dashdRPCClient) GetRawTransaction(ctx context.Context, txid string) (*rawTransactionResult, error) {
	var r rawTransactionResult
	if err := c.call(ctx, "getrawtransaction", []any{txid, 1}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
