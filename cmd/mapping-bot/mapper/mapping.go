package mapper

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/wire"
)

// pausedSeen latches the paused state the map loop last reported, so a pause is
// logged once, not every block.
var (
	pausedMu   sync.Mutex
	pausedSeen bool
)

// notePaused records the contract's pause flag and reports whether it just became
// paused (true only on the transition, for a single warning).
func notePaused(paused bool) bool {
	pausedMu.Lock()
	defer pausedMu.Unlock()
	was := pausedSeen
	pausedSeen = paused
	return paused && !was
}

// dropHeightDiff is set per-chain via ChainConfig.DropHeightDiff

// HandleMap processes a single block for mapping transactions.
// Returns true if the block was processed, false if skipped (e.g., contract not ready).
func (b *Bot) HandleMap(
	blockBytes []byte,
	blockHeight uint64,
) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	lastContractHeightStr, err := b.gql().FetchLastHeight(ctx)
	if err != nil {
		b.L.Error("error fetching contract's last block height", "err", err)
		return false
	}
	lastContractHeight, err := strconv.ParseUint(lastContractHeightStr, 10, 32)
	if err != nil {
		b.L.Error("response received for last contract height is not an integer", "value", lastContractHeightStr)
		return false
	}

	if lastContractHeight < b.contractHeightNeededFor(blockHeight) {
		b.L.Info("delaying processing, block not yet deep enough in the contract",
			"blockHeight", blockHeight, "contractHeight", lastContractHeight,
			"confirmationsRequired", b.Chain.ConfirmationsRequired)
		return false
	}

	// BOT-PAUSE-1: while the owner has paused the contract, every map is refused on
	// chain, and a map refused on chain is final for its proof (recorded for /retry,
	// block advanced below), so a deposit seen during a pause would stay uncredited
	// until an operator retried it by hand. Hold the block instead and map it after
	// the unpause. A flag that cannot be read holds the block too, like the height
	// read above.
	if paused, err := b.gql().FetchPaused(ctx); err != nil {
		b.L.Error("cannot read the contract's pause flag; block kept for retry", "err", err, "blockHeight", blockHeight)
		return false
	} else if paused {
		if notePaused(true) {
			b.L.Warn("contract is paused: deposits are held and mapped after the unpause", "blockHeight", blockHeight)
		}
		return false
	}
	notePaused(false)

	foundTxs, err := b.ParseBlock(ctx, blockBytes, blockHeight)
	if err != nil {
		b.L.Error("error parsing block", "err", err)
		return false
	}

	jsonMessages := make([]json.RawMessage, len(foundTxs))
	for i, tx := range foundTxs {
		jsonBytes, err := json.Marshal(tx)
		if err != nil {
			b.L.Error("could not marshal transaction", "blockHeight", tx.TxData.BlockHeight, "txIndex", tx.TxData.TxIndex)
			return false
		}
		jsonMessages[i] = json.RawMessage(jsonBytes)
	}
	// BOT-MAP-SKIP-1 (testnet 2026-10-06): a map that failed before landing (RC
	// pre-flight, network, node) used to be logged and the block skipped for good,
	// so that deposit was never credited. Keep the block and retry it next cycle.
	// A map the contract REFUSED on chain (ErrTxFailed) is final for this proof and
	// already recorded for /retry, so it does not hold the block. Re-sending a map
	// that did land is refused by the contract (the deposit is already observed).
	//
	// One pre-landing refusal is final too: the node takes no L2 tx above its size
	// limit (transaction-pool MAX_TX_SIZE, 16,384 bytes), and a map carries the whole
	// deposit tx, so a deposit inside a large Bitcoin tx (an exchange's batched
	// withdrawal) can never be mapped. Holding its block would stall every later
	// deposit behind it; it is recorded for the operator instead.
	retryBlock := false
	for i, tx := range jsonMessages {
		if _, err := b.callWithRetry(ctx, tx, "map", broadcastRetryAttempts); err == nil {
			b.recordDeposits(ctx, foundTxs[i].Instructions)
		} else {
			b.L.Error("map call failed", "err", err)
			switch {
			case errors.Is(err, ErrTxFailed):
			case isTooBigForPool(err):
				btcTxId := txIDFromRawTxHex(foundTxs[i].TxData.RawTxHex)
				b.L.Error("deposit tx is too large for a map call and cannot be credited; recorded for the operator",
					"btcTxId", btcTxId, "blockHeight", blockHeight, "error", err)
				b.recordFailedTx(ctx, "map-too-large-"+btcTxId, "map", tx)
			default:
				retryBlock = true
			}
		}
	}
	if retryBlock {
		b.L.Warn("a map did not land for a transient reason; block kept for retry", "blockHeight", blockHeight)
		return false
	}

	advanced, err := b.stateDB().AdvanceBlockHeightIfCurrent(ctx, blockHeight, blockHeight+1)
	if err != nil {
		b.L.Error("error advancing last block height", "err", err, "blockHeight", blockHeight)
		return false
	}
	if !advanced {
		// Another bot instance likely advanced the height first.
		b.L.Info("block height already advanced by another instance", "blockHeight", blockHeight)
	}

	b.setLastBlock(blockHeight)
	return true
}

// HandleExistingTxs checks for existing txs for newly registered addresses.
// This is a best-effort scan — the main loop's block-by-block processing
// is the primary detection mechanism. The lookback window is chain-specific
// (b.Chain.HistoricalTxLookback), sized to roughly one week of chain time.
func (b *Bot) HandleExistingTxs(chainAddress string) {
	b.L.Debug("checking existing txs for new address", "address", chainAddress)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	tipHeight, err := b.Chain.Client.GetTipHeight()
	if err != nil {
		b.L.Error("failed to fetch chain tip height for historical tx scan", "err", err)
		return
	}
	lookback := b.Chain.HistoricalTxLookback
	var minHeight uint64
	if tipHeight >= lookback {
		minHeight = tipHeight - lookback
	}

	entries, err := b.Chain.Client.GetAddressTxs(chainAddress)
	if err != nil {
		b.L.Error("failed to fetch address tx history", "address", chainAddress, "err", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	blockTxIDs := make(map[string]map[string]uint64)
	for _, entry := range entries {
		if !entry.Confirmed {
			continue
		}

		details, err := b.Chain.Client.GetTxDetails(entry.TxID)
		if err != nil {
			b.L.Warn("failed to fetch tx confirmation details", "txid", entry.TxID, "err", err)
			continue
		}
		if !details.Confirmed || details.BlockHash == "" {
			continue
		}
		if details.BlockHeight < minHeight {
			continue
		}
		if _, ok := blockTxIDs[details.BlockHash]; !ok {
			blockTxIDs[details.BlockHash] = make(map[string]uint64)
		}
		blockTxIDs[details.BlockHash][entry.TxID] = details.BlockHeight
	}

	for blockHash, wantedTxIDs := range blockTxIDs {
		var blockHeight uint64
		for _, h := range wantedTxIDs {
			blockHeight = h
			break
		}
		blockBytes, err := b.Chain.Client.GetRawBlock(blockHash)
		if err != nil {
			b.L.Warn("failed to fetch raw block for historical tx scan", "blockHash", blockHash, "err", err)
			continue
		}
		foundTxs, err := b.ParseBlock(ctx, blockBytes, blockHeight)
		if err != nil {
			b.L.Warn("failed to parse historical block", "blockHash", blockHash, "height", blockHeight, "err", err)
			continue
		}

		// Only map historical txs that were present in the address history.
		for _, tx := range foundTxs {
			txID := txIDFromRawTxHex(tx.TxData.RawTxHex)
			if _, ok := wantedTxIDs[txID]; !ok {
				continue
			}

			jsonBytes, err := json.Marshal(tx)
			if err != nil {
				b.L.Warn("could not marshal historical transaction", "err", err)
				continue
			}
			if _, err := b.callWithRetry(ctx, json.RawMessage(jsonBytes), "map", broadcastRetryAttempts); err != nil {
				b.L.Error("historical map call failed", "err", err)
			} else {
				b.recordDeposits(ctx, tx.Instructions)
			}
		}
	}
}

func txIDFromRawTxHex(rawTxHex string) string {
	rawTx, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return ""
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(rawTx)); err != nil {
		return ""
	}
	return tx.TxID()
}

// isTooBigForPool reports an L2 tx above the transaction-pool size limit, from EITHER
// source (P2): the bot's own pre-flight (errTxTooLarge, caught before submit) or the
// node's refusal (transaction-pool.go: "transaction size too big N > MAX_TX_SIZE").
// An oversize map deposit (e.g. inside an exchange's batched withdrawal) can never be
// credited, so the map handler must recognise both and record it for the operator rather
// than hold the deposit's block forever.
func isTooBigForPool(err error) bool {
	return err != nil && (errors.Is(err, errTxTooLarge) || strings.Contains(err.Error(), "transaction size too big"))
}

// contractHeightNeededFor is the contract header height at which a deposit or
// spend mined in block h is deep enough for the contract to act on it.
//
// BOT-MAP-DEPTH-1 (testnet 2026-10-06): the bot mapped a block as soon as the
// contract held it (depth 0), the v2 contract refused the deposit as "not
// confirmed deeply enough yet", and the bot moved on to the next block anyway,
// so the deposit was never credited. Waiting for the depth makes the one attempt
// the bot makes per block land. Same rule for confirmSpend (BOT-CONF-1).
//
// The contract measures depth as tip - minedHeight (requireConfirmationDepth), so
// it acts once tip >= minedHeight + MinConfirmations. Waiting one block less was
// refused on testnet ("block 5156755 sits 1 below a tip of 5156756, and 2 is
// required") and cost a failed call per spend.
func (b *Bot) contractHeightNeededFor(h uint64) uint64 {
	if b.Chain == nil {
		return h
	}
	return h + b.Chain.ConfirmationsRequired
}
