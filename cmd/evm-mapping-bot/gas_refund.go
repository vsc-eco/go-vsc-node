package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// EVM-FEE-1 (testnet 11-12): a withdrawal prepays its L1 gas at its gas limit,
// and the bridge now settles the real cost once the transaction is proven
// mined. confirmSpend settles a block's first transaction at once; for the
// others it records the settlement (gs-{nonce}, listed in "gsl") and
// refundWithdrawalGas settles it with a proof of the previous receipt in the
// same block. This sends those calls: at most gasRefundPerTick a tick, none
// while one for that nonce has no result yet, none while the bridge is paused
// (the contract refuses refunds then, A04). Anyone may send them; a missed one
// only delays the refund, the record stays until it is settled.

const (
	gasRefundEvery   = time.Minute
	gasRefundPerTick = 2
	gasRefundRC      = 12_000
	gasRefundResend  = 5 * time.Minute
)

var (
	lastGasRefundAt time.Time
	gasRefundSent   = map[uint64]time.Time{}
)

type gasSettleRecord struct {
	Block   uint64 `json:"h"`
	TxIndex uint64 `json:"i"`
}

func refundWithdrawalGasIfOpen(ctx context.Context, rpc *ethRPC, gql *vscGraphQL, submitter *l2Submitter, cfg EVMBotConfig) {
	if time.Since(lastGasRefundAt) < gasRefundEvery {
		return
	}
	lastGasRefundAt = time.Now()
	st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{"gsl"})
	if err != nil || st["gsl"] == "" {
		return
	}
	sent := 0
	for _, f := range strings.Split(st["gsl"], "|") {
		if sent >= gasRefundPerTick || ctx.Err() != nil {
			return
		}
		nonce, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			continue
		}
		if at, ok := gasRefundSent[nonce]; ok && time.Since(at) < gasRefundResend {
			continue
		}
		key := "gs-" + f
		rs, err := gql.fetchContractState(ctx, cfg.ContractID, []string{key})
		if err != nil || rs[key] == "" {
			continue
		}
		var rec gasSettleRecord
		if json.Unmarshal([]byte(rs[key]), &rec) != nil {
			continue
		}
		p := map[string]any{"nonce": nonce}
		if rec.TxIndex > 0 {
			bp, err := fetchBlockProofs(ctx, rpc, rec.Block, false)
			if err != nil {
				slog.Warn("EVM-FEE-1: block receipts for a gas refund", "nonce", nonce, "block", rec.Block, "err", err)
				continue
			}
			_, nodes, err := bp.receiptProof(int(rec.TxIndex) - 1)
			if err != nil {
				slog.Warn("EVM-FEE-1: previous receipt proof", "nonce", nonce, "block", rec.Block, "err", err)
				continue
			}
			p["receipt_proof"] = base64.StdEncoding.EncodeToString(bytes.Join(nodes, nil))
		}
		payload, _ := json.Marshal(p)
		tx, err := submitter.callContractL2(ctx, cfg.ContractID, "refundWithdrawalGas", payload, gasRefundRC)
		if err != nil {
			slog.Warn("EVM-FEE-1: refundWithdrawalGas", "nonce", nonce, "err", err)
			continue
		}
		gasRefundSent[nonce] = time.Now()
		sent++
		slog.Info("EVM-FEE-1: refundWithdrawalGas sent", "nonce", nonce, "block", rec.Block, "txIndex", rec.TxIndex, "tx", tx)
	}
}
