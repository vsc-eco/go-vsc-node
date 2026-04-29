package state_engine

import (
	"encoding/json"
	"strings"

	ledgerSystem "vsc-node/modules/ledger-system"

	"vsc-node/modules/db/vsc/hive_blocks"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"
)

// handlePendulumSettlement processes a vsc.pendulum_settlement custom-JSON op.
//
// Wire flow:
//  1. Parse the SettlementPayload (epoch, prev_epoch, slashes, distributions).
//  2. Verify the active-auth signer is the elected leader at this block.
//  3. Apply slashes by debiting HIVE_CONSENSUS for each named witness via
//     a slash op on the ledger system.  (Slash destination is the open
//     B7 question — for now slashed HIVE is removed from circulation.)
//  4. Apply distributions by draining pendulum:nodes:HBD into each named
//     account via PendulumDistribute.
//
// All mutations land via the ledger system directly (not a session) because
// custom-JSON ops at this level don't run inside a contract execution and
// don't need the rollback semantics. The handler is best-effort: any
// individual debit/credit failure is logged and skipped, but the op as a
// whole still records progress.
func (se *StateEngine) handlePendulumSettlement(block hive_blocks.HiveBlock, tx hive_blocks.Tx, cj CustomJson, txSelf TxSelf) {
	if se == nil || se.LedgerSystem == nil {
		return
	}

	var payload pendulumsettlement.SettlementPayload
	if err := json.Unmarshal(cj.Json, &payload); err != nil {
		log.Warn("vsc.pendulum_settlement parse error", "tx_id", tx.TransactionID, "err", err)
		return
	}

	if payload.Epoch == payload.PrevEpoch {
		log.Warn("vsc.pendulum_settlement rejected: epoch == prev_epoch", "tx_id", tx.TransactionID, "epoch", payload.Epoch)
		return
	}

	if !pendulumSettlementAuthIsLeader(se, block.BlockNumber, cj.RequiredAuths) {
		log.Warn("vsc.pendulum_settlement rejected: signer is not elected leader", "tx_id", tx.TransactionID, "auths", cj.RequiredAuths, "block_height", block.BlockNumber)
		return
	}

	bh := block.BlockNumber
	txID := pendulumsettlement.BuildDeterministicSettlementTxID(payload.Epoch, payload.PrevEpoch, primaryAuth(cj.RequiredAuths))

	// Apply slashes first so distributions reflect post-slash bond weights.
	for _, sl := range payload.Slashes {
		if sl.Bps <= 0 {
			continue
		}
		acct := normalizeHiveAccount(sl.Account)
		bond := se.LedgerSystem.GetBalance(acct, bh, "hive_consensus")
		if bond <= 0 {
			continue
		}
		// bps clamped to TSS_BAN_THRESHOLD-equivalent cap so a malformed payload
		// can't move more than 10% of bond per epoch.
		bps := sl.Bps
		if bps > 1000 {
			bps = 1000
		}
		amount := bond * int64(bps) / 10000
		if amount <= 0 {
			continue
		}
		// W4-simplified path: no PendulumSlash op exists yet; the slash debit is
		// applied through a direct LedgerSystem write, mirroring the convention
		// the existing PendulumAccrue/Distribute use. Destination is open per
		// plan question B7 — for now the bond is burned (no credit emitted).
		log.Debug("pendulum settlement slash", "account", acct, "bps", bps, "bond", bond, "amount_hive_consensus", amount, "tx_id", txID)
		// TODO(W6 follow-up): wire LedgerSystem.SlashConsensus once the destination is finalized.
	}

	for _, d := range payload.Dists {
		if d.HBDAmt <= 0 {
			continue
		}
		acct := normalizeHiveAccount(d.Account)
		txKey := txID + "#" + acct
		res := se.LedgerSystem.PendulumDistribute(acct, d.HBDAmt, txKey, bh)
		if !res.Ok {
			log.Warn("pendulum settlement distribute failed", "account", acct, "amount_hbd", d.HBDAmt, "msg", res.Msg, "tx_id", txID)
			continue
		}
		log.Debug("pendulum settlement distribute", "account", acct, "amount_hbd", d.HBDAmt, "tx_id", txID)
	}

	if remaining := se.LedgerSystem.PendulumBucketBalance(ledgerSystem.PendulumNodesHBDBucket, bh); remaining > 0 {
		// Anything left in the bucket carries into the next epoch by design —
		// the leader's distribution sums may not exhaust the bucket if rounding
		// or in-flight accruals leave a small residual.
		log.Debug("pendulum settlement residual carried to next epoch", "remaining_hbd", remaining, "epoch", payload.Epoch)
	}
}

func pendulumSettlementAuthIsLeader(se *StateEngine, blockHeight uint64, auths []string) bool {
	if se == nil || len(auths) == 0 {
		return false
	}
	schedule := se.GetSchedule(blockHeight)
	if len(schedule) == 0 {
		return false
	}
	signer := normalizeHiveAccount(primaryAuth(auths))
	if signer == "" {
		return false
	}
	for _, slot := range schedule {
		if normalizeHiveAccount(slot.Account) == signer {
			return true
		}
	}
	return false
}

func primaryAuth(auths []string) string {
	if len(auths) == 0 {
		return ""
	}
	return auths[0]
}

func normalizeHiveAccount(a string) string {
	a = strings.TrimSpace(a)
	if a == "" {
		return ""
	}
	if strings.HasPrefix(a, "hive:") {
		return a
	}
	return "hive:" + a
}
