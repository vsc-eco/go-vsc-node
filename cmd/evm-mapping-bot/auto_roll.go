package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrlp "github.com/ethereum/go-ethereum/rlp"
)

// AUTO-ROLL (owner 11-22 "ok do the bot"): the vault sends in nonce order, so a
// withdrawal that cannot mine (signed during a gas spike, its fee cap now below
// the base fee) holds every later withdrawal and payout behind it. THORChain's
// bifrost does not leave it to an operator (bifrost/pkg/chainclients/evm/
// unstuck.go): once a signed outbound has waited its signing period and is still
// pending, it is cancelled with a 0-value self-transfer at the same nonce and a
// higher gas price, and the outbound is sent again.
//
// rollWithdrawal is that cancel. Anyone may call it once the verified L1 chain
// is WithdrawalRollWindow blocks past the withdrawal; the bot now does, with
// THORChain's guards:
//   - only the head withdrawal (the bridge refuses any other), only past the
//     window (the bridge checks it too);
//   - not when it already mined: the vault's L1 nonce is past it, and its
//     confirmation settles it;
//   - not when it can still mine: its fee cap is at least today's base fee.
//     Rolling it then would only make it cost twice (a payout is sent again
//     and pays its gas again; an own withdrawal comes back to the Magi balance
//     and has to be made again);
//   - a dry run first: the bridge's own refusals (paused, cooldown since the
//     last roll, the key no longer holds the vault) cost no RC.
// Whichever transaction mines settles the nonce (confirmSpend proves which one,
// handlers.go): a roll cannot pay twice. A rolled payout is sent again to the
// same address (resendPayout); the bot's broadcast of a re-signed nonce (B1)
// sends the roll. If the roll is itself priced out later, it is rolled again
// after the bridge's cooldown, at that day's price (THORChain re-runs unstuck).

const autoRollEvery = time.Minute

var lastAutoRollAt time.Time

// headL1 is what the auto-roll reads on Ethereum: the vault's mined nonce and
// the latest base fee.
type headL1 struct {
	nonce   func(ctx context.Context, address string) (uint64, error)
	baseFee func(ctx context.Context) (*big.Int, error)
}

// rollStuckHead rolls the head withdrawal when it cannot mine (see above).
func rollStuckHead(ctx context.Context, gql *vscGraphQL, submitter *l2Submitter, cfg EVMBotConfig, l1 *headL1) {
	if cfg.Paused || l1 == nil || time.Since(lastAutoRollAt) < autoRollEvery {
		return
	}
	lastAutoRollAt = time.Now()
	st, err := gql.fetchContractState(ctx, cfg.ContractID, []string{"n", "np"})
	if err != nil {
		return
	}
	n, _ := strconv.ParseUint(st["n"], 10, 64)
	np, _ := strconv.ParseUint(st["np"], 10, 64)
	if np <= n {
		return // nothing in flight
	}
	key := "d-" + strconv.FormatUint(n, 10)
	ds, err := gql.fetchContractState(ctx, cfg.ContractID, []string{key})
	if err != nil {
		return
	}
	ps := parsePendingSpend(n, ds[key])
	if ps == nil {
		return // B-7 reports an unreadable head
	}
	tip, err := fetchContractLastHeight(ctx, gql, cfg.ContractID)
	if err != nil || tip < ps.BlockHeight+withdrawalRollWindow {
		return
	}
	mined, err := l1.nonce(ctx, ps.VaultAtQueue)
	if err != nil {
		slog.Warn("auto-roll: cannot read the vault's L1 nonce; not rolling", "nonce", n, "vault", ps.VaultAtQueue, "err", err)
		return
	}
	if mined > n {
		return // it (or an earlier roll) mined: its confirmation settles it
	}
	feeCap := unsignedFeeCap(ps.UnsignedTxHex)
	base, err := l1.baseFee(ctx)
	if feeCap == nil || err != nil || base == nil {
		slog.Warn("auto-roll: cannot price the head withdrawal against the base fee; not rolling", "nonce", n, "err", err)
		return
	}
	if feeCap.Cmp(base) >= 0 {
		return // it can mine at today's gas: it is waiting for a block, not stuck
	}
	payload := fmt.Sprintf(`{"nonce":%d}`, n)
	sim, simErr := gql.simulateContractCall(ctx, submitter.did.String(), cfg.ContractID, "rollWithdrawal", payload)
	if simErr != nil || !sim.Success {
		reason := sim.ErrMsg
		if simErr != nil {
			reason = simErr.Error()
		}
		slog.Info("auto-roll: the bridge does not take a roll of the stuck head now; trying again later", "nonce", n, "reason", reason)
		return
	}
	tx, err := submitter.callContractL2(ctx, cfg.ContractID, "rollWithdrawal", json.RawMessage(payload), max(uint64(sim.RcUsed)*2, 1))
	if err != nil {
		slog.Warn("auto-roll: rollWithdrawal did not go out; trying again later", "nonce", n, "err", err)
		return
	}
	slog.Warn("auto-roll: rolled the stuck head withdrawal (it cannot mine at today's gas; every later withdrawal waited behind it)",
		"nonce", n, "asset", ps.Asset, "amount", ps.Amount, "to", ps.To, "fee_cap", feeCap, "base_fee", base,
		"queued_at_l1", ps.BlockHeight, "l1_tip", tip, "tx", tx)
}

// unsignedFeeCap is an unsigned EIP-1559 transaction's max fee per gas (nil
// when it is not one).
func unsignedFeeCap(unsignedHex string) *big.Int {
	raw := hexToBytes(unsignedHex)
	if len(raw) < 2 || raw[0] != types.DynamicFeeTxType {
		return nil
	}
	var tx struct {
		ChainID    *big.Int
		Nonce      uint64
		GasTipCap  *big.Int
		GasFeeCap  *big.Int
		Gas        uint64
		To         *common.Address `rlp:"nil"`
		Value      *big.Int
		Data       []byte
		AccessList types.AccessList
	}
	if gethrlp.DecodeBytes(raw[1:], &tx) != nil || tx.GasFeeCap == nil {
		return nil
	}
	return tx.GasFeeCap
}

// l1For is the auto-roll's view of Ethereum through the bot's RPC.
func l1For(rpc *ethRPC) *headL1 {
	if rpc == nil {
		return nil
	}
	return &headL1{
		nonce: func(ctx context.Context, address string) (uint64, error) {
			res, err := rpc.callCtx(ctx, "eth_getTransactionCount", `"`+address+`","latest"`)
			if err != nil {
				return 0, err
			}
			var q string
			if err := json.Unmarshal(res, &q); err != nil {
				return 0, err
			}
			return strconv.ParseUint(strings.TrimPrefix(q, "0x"), 16, 64)
		},
		baseFee: func(ctx context.Context) (*big.Int, error) {
			res, err := rpc.callCtx(ctx, "eth_getBlockByNumber", `"latest",false`)
			if err != nil {
				return nil, err
			}
			var b struct {
				BaseFee string `json:"baseFeePerGas"`
			}
			if err := json.Unmarshal(res, &b); err != nil {
				return nil, err
			}
			v, ok := new(big.Int).SetString(strings.TrimPrefix(b.BaseFee, "0x"), 16)
			if !ok {
				return nil, fmt.Errorf("latest block has no base fee (%q)", b.BaseFee)
			}
			return v, nil
		},
	}
}
