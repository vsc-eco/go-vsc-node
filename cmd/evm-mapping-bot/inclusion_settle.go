package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"vsc-node/modules/common/params"
)

// CONFIRM-RECEIPT-CAP-1: confirmSpend carries the withdrawal's transaction and
// its receipt, and the receipt carries the recipient's logs. An ETH withdrawal
// to a contract that logs KBs made the confirm larger than the node's contract
// payload cap, so it could never be sent and every later withdrawal waited.
// The bridge now closes an ETH withdrawal on its inclusion proof when the
// confirm comes without the receipt, as the BTC bridge closes a payout, and
// keeps a record (wic-{block}-{index}) that settleWithdrawalReceipt settles
// from the receipt: a reverted withdrawal is refunded, and the unused gas goes
// back as confirmSpend would have sent it. This sends those settlements: at
// most receiptSettlePerTick a tick, none while one for that record has no
// result yet, none while the bridge is paused. Each is dry-run first: one the
// bridge refuses (a purged generation, a header it waits for) is not sent and
// waits a growing backoff, outside the per-tick count, so it never holds up
// the others. A reverted receipt has no logs and always fits; a receipt too
// large to settle is left (its withdrawer forgoes the unused gas). Anyone may
// send them.

const (
	receiptSettleEvery   = time.Minute
	receiptSettlePerTick = 2
	receiptSettleResend  = 5 * time.Minute
	// receiptSettleForget: a noted close whose record never appeared (the
	// close failed, or a receipt confirm landed instead) is forgotten after this.
	receiptSettleForget = time.Hour
	// receiptSettleCheckFor: a withdrawal closed without this bot's confirm is
	// checked for an inclusion close for this long at most.
	receiptSettleCheckFor = 7 * 24 * time.Hour
	closedChecksPerTick   = 5
	// closedCheckNullFor: a check whose every hash the RPC does not know is
	// kept this long (a mined withdrawal is found again), then dropped.
	closedCheckNullFor = time.Hour
	// A settlement the bridge refuses waits receiptRefusedBackoff, doubling per
	// refusal, up to receiptRefusedMaxBackoff.
	receiptRefusedBackoff    = 5 * time.Minute
	receiptRefusedMaxBackoff = 6 * time.Hour
)

var (
	lastReceiptSettleAt time.Time
	receiptSettleSent   = map[string]time.Time{}
)

// receiptSettle is a withdrawal closed (or being closed) on its inclusion
// proof, where it mined.
type receiptSettle struct {
	Block   uint64 `json:"h"`
	TxIndex uint64 `json:"i"`
	Nonce   uint64 `json:"n"`
	NotedAt int64  `json:"t"`
	// Refusals / NextAt: the bridge's dry run refused the settlement this many
	// times; the next try is not before NextAt.
	Refusals int   `json:"r,omitempty"`
	NextAt   int64 `json:"next,omitempty"`
}

func inclusionCloseKey(block, txIndex uint64) string {
	return "wic-" + strconv.FormatUint(block, 10) + "-" + strconv.FormatUint(txIndex, 10)
}

func noteReceiptSettle(cp *Checkpoint, block, txIndex, nonce uint64) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.ReceiptSettles == nil {
		cp.ReceiptSettles = map[string]receiptSettle{}
	}
	cp.ReceiptSettles[inclusionCloseKey(block, txIndex)] = receiptSettle{Block: block, TxIndex: txIndex, Nonce: nonce, NotedAt: time.Now().Unix()}
}

// closedCheck is a withdrawal the bridge closed without this bot seeing its
// own confirm land: every hash it was broadcast under, to find where it mined.
type closedCheck struct {
	Hashes []string `json:"x"`
	Nonce  uint64   `json:"n"`
	Since  int64    `json:"t"`
}

// noteClosedCheckLocked queues stx for checkClosedWithdrawals; cp.mu is held.
// Keyed by its first hash: nonces restart with each generation.
func (cp *Checkpoint) noteClosedCheckLocked(stx SentTx) {
	hashes := append([]string{stx.TxHash}, stx.PrevTxHashes...)
	if len(hashes) == 0 || hashes[0] == "" {
		return
	}
	if cp.ClosedChecks == nil {
		cp.ClosedChecks = map[string]closedCheck{}
	}
	if _, ok := cp.ClosedChecks[hashes[0]]; ok {
		return
	}
	cp.ClosedChecks[hashes[0]] = closedCheck{Hashes: hashes, Nonce: stx.Nonce, Since: time.Now().Unix()}
}

// checkClosedWithdrawals finds where each queued withdrawal mined and notes an
// inclusion close of it for settlement. A check whose RPC or GraphQL read
// fails stays for the next tick (a closed revert must not be lost), until
// receiptSettleCheckFor; one that never mined goes.
func checkClosedWithdrawals(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, cp *Checkpoint, cfg EVMBotConfig) {
	cp.mu.Lock()
	checks := make([]closedCheck, 0, len(cp.ClosedChecks))
	for _, c := range cp.ClosedChecks {
		checks = append(checks, c)
	}
	cp.mu.Unlock()
	sort.Slice(checks, func(a, b int) bool { return checks[a].Nonce < checks[b].Nonce })
	for i, c := range checks {
		if i >= closedChecksPerTick || ctx.Err() != nil {
			return
		}
		if done := checkClosedWithdrawal(ctx, rpc, gql, cp, cfg, c); done || time.Since(time.Unix(c.Since, 0)) > receiptSettleCheckFor {
			cp.mu.Lock()
			delete(cp.ClosedChecks, c.Hashes[0])
			cp.mu.Unlock()
		}
	}
}

// checkClosedWithdrawal answers whether the check of c is done.
func checkClosedWithdrawal(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, cp *Checkpoint, cfg EVMBotConfig, c closedCheck) bool {
	for _, h := range c.Hashes {
		if h == "" {
			continue
		}
		// One call, not getReceipt's retries: those pause seconds on every
		// unknown hash (callOne answers null as an error) and would hold the
		// tick. Null: not mined, or not known to this RPC.
		data, err := rpc.callCtx(ctx, "eth_getTransactionReceipt", fmt.Sprintf(`"%s"`, h))
		if err != nil && strings.Contains(err.Error(), "null result from") {
			continue
		}
		if err != nil {
			return false
		}
		if data == nil || string(data) == "null" {
			continue
		}
		var r struct {
			BlockNumber      string `json:"blockNumber"`
			TransactionIndex string `json:"transactionIndex"`
		}
		if json.Unmarshal(data, &r) != nil || r.BlockNumber == "" {
			return true
		}
		block, idx := hexToUint64(r.BlockNumber), hexToUint64(r.TransactionIndex)
		key := inclusionCloseKey(block, idx)
		st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{key})
		if err != nil {
			return false
		}
		if st[key] != "" {
			noteReceiptSettle(cp, block, idx, c.Nonce)
			slog.Info("CONFIRM-RECEIPT-CAP-1: withdrawal closed on its inclusion proof by someone else; its receipt will be settled",
				"nonce", c.Nonce, "block", block, "txIndex", idx)
		}
		return true
	}
	// No hash known to the RPC: keep it a while, a mined one is found again.
	return time.Since(time.Unix(c.Since, 0)) > closedCheckNullFor
}

// withoutReceiptProof is the confirmSpend payload without its receipt proof.
func withoutReceiptProof(payload json.RawMessage) (json.RawMessage, error) {
	var req confirmSpendRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, err
	}
	req.ReceiptProof = ""
	return json.Marshal(req)
}

func forgetReceiptSettle(cp *Checkpoint, key string) {
	cp.mu.Lock()
	delete(cp.ReceiptSettles, key)
	cp.mu.Unlock()
	delete(receiptSettleSent, key)
}

func settleWithdrawalReceipts(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, submitter *l2Submitter, cp *Checkpoint, cfg EVMBotConfig) {
	if time.Since(lastReceiptSettleAt) < receiptSettleEvery {
		return
	}
	lastReceiptSettleAt = time.Now()
	checkClosedWithdrawals(ctx, rpc, gql, cp, cfg)
	cp.mu.Lock()
	refs := make([]receiptSettle, 0, len(cp.ReceiptSettles))
	for _, r := range cp.ReceiptSettles {
		refs = append(refs, r)
	}
	cp.mu.Unlock()
	sort.Slice(refs, func(a, b int) bool {
		if refs[a].Block != refs[b].Block {
			return refs[a].Block < refs[b].Block
		}
		return refs[a].TxIndex < refs[b].TxIndex
	})
	sent := 0
	for _, r := range refs {
		if sent >= receiptSettlePerTick || ctx.Err() != nil {
			return
		}
		if r.NextAt != 0 && time.Now().Unix() < r.NextAt {
			continue
		}
		key := inclusionCloseKey(r.Block, r.TxIndex)
		st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{key})
		if err != nil {
			continue
		}
		if st[key] == "" {
			// Settled, or never closed on inclusion. A close just sent may not
			// have executed yet: keep it a while.
			if _, sentOnce := receiptSettleSent[key]; sentOnce || time.Since(time.Unix(r.NotedAt, 0)) > receiptSettleForget {
				forgetReceiptSettle(cp, key)
			}
			continue
		}
		if at, ok := receiptSettleSent[key]; ok && time.Since(at) < receiptSettleResend {
			continue
		}
		bp, err := fetchBlockProofs(ctx, rpc, r.Block, false)
		if err != nil {
			slog.Warn("CONFIRM-RECEIPT-CAP-1: block receipts for a settlement", "block", r.Block, "err", err)
			continue
		}
		_, nodes, err := bp.receiptProof(int(r.TxIndex))
		if err != nil {
			slog.Warn("CONFIRM-RECEIPT-CAP-1: receipt proof", "block", r.Block, "txIndex", r.TxIndex, "err", err)
			continue
		}
		payload, _ := json.Marshal(map[string]any{"block_height": r.Block, "tx_index": r.TxIndex,
			"receipt_proof": base64.StdEncoding.EncodeToString(bytes.Join(nodes, nil))})
		if n := nodePayloadSize(payload); n > params.MAX_CONTRACT_PAYLOAD_SIZE {
			slog.Warn("CONFIRM-RECEIPT-CAP-1: the receipt is too large to settle; the withdrawal stays paid without its unused-gas refund",
				"nonce", r.Nonce, "block", r.Block, "txIndex", r.TxIndex, "payload_size", n)
			forgetReceiptSettle(cp, key)
			continue
		}
		// Dry-run it first: a settlement the bridge refuses is not sent (it would
		// fail on chain and be sent again every receiptSettleResend).
		sim, err := gql.simulateContractCall(ctx, submitter.did.String(), cfg.ContractID, "settleWithdrawalReceipt", string(payload))
		if err != nil {
			slog.Warn("CONFIRM-RECEIPT-CAP-1: no dry run for a settlement; it waits", "nonce", r.Nonce, "block", r.Block, "txIndex", r.TxIndex, "err", err)
			continue
		}
		if !sim.Success {
			refusedReceiptSettle(cp, key, r, sim)
			continue
		}
		tx, err := submitter.callContractL2(ctx, cfg.ContractID, "settleWithdrawalReceipt", payload, max(uint64(sim.RcUsed)*2, 1))
		if err != nil {
			slog.Warn("CONFIRM-RECEIPT-CAP-1: settleWithdrawalReceipt", "nonce", r.Nonce, "block", r.Block, "txIndex", r.TxIndex, "err", err)
			continue
		}
		receiptSettleSent[key] = time.Now()
		sent++
		slog.Info("CONFIRM-RECEIPT-CAP-1: settleWithdrawalReceipt sent", "nonce", r.Nonce, "block", r.Block, "txIndex", r.TxIndex, "tx", tx)
	}
}

// refusedReceiptSettle backs a refused settlement off (persisted), asks for the
// block's header when the bridge waits for it, and says why, loudly.
func refusedReceiptSettle(cp *Checkpoint, key string, r receiptSettle, sim simulatedCall) {
	r.Refusals++
	wait := receiptRefusedBackoff << min(r.Refusals-1, 10)
	if wait > receiptRefusedMaxBackoff {
		wait = receiptRefusedMaxBackoff
	}
	r.NextAt = time.Now().Add(wait).Unix()
	cp.mu.Lock()
	if _, ok := cp.ReceiptSettles[key]; ok {
		cp.ReceiptSettles[key] = r
	}
	cp.mu.Unlock()
	if strings.Contains(sim.Err+" "+sim.ErrMsg, "block header not found") {
		noteHeaderNeed(cp, r.Block)
	}
	slog.Error("CONFIRM-RECEIPT-CAP-1: the bridge refuses this settlement; it waits and is tried again",
		"nonce", r.Nonce, "block", r.Block, "txIndex", r.TxIndex, "err", sim.Err, "msg", sim.ErrMsg,
		"refusals", r.Refusals, "next", time.Unix(r.NextAt, 0).Format(time.RFC3339))
}
