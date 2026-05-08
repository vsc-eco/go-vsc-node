package settlement

import (
	"strings"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/lib/vsclog"
)

var log = vsclog.Module("pendulum-settlement")

// BalanceRecordReader is the narrow read surface this package needs from
// ledgerDb.Balances. Pulled out so tests can stub without standing up Mongo.
type BalanceRecordReader interface {
	GetBalanceRecord(account string, blockHeight uint64) (*ledgerDb.BalanceRecord, error)
}

// ReadCommitteeBonds returns the per-account HIVE_CONSENSUS bond at
// blockHeight, reading directly from BalanceRecord.HIVE_CONSENSUS instead of
// via LedgerSystem.GetBalance("hive_consensus").
//
// Why direct: GetBalance applies an op-type filter ({"unstake","deposit"})
// that does not include the consensus_stake op, so it silently returns 0 for
// freshly-staked HIVE on some code paths. The settlement leader cannot tolerate
// that — under-counted bonds would skew the post-slash distribution.
//
// Accounts in the returned map are normalized to "hive:account" form so the
// caller can correlate with slash payloads (which also use that form).
// Accounts with zero bond are omitted so callers can iterate the map and
// only see the subset that actually contributes to T_post.
func ReadCommitteeBonds(reader BalanceRecordReader, members []string, blockHeight uint64) map[string]int64 {
	if reader == nil || len(members) == 0 {
		return nil
	}
	out := make(map[string]int64, len(members))
	for _, m := range members {
		acct := normalizeHiveAccount(m)
		if acct == "" {
			continue
		}
		rec, err := reader.GetBalanceRecord(acct, blockHeight)
		if err != nil {
			// Transient ledger-DB error: drop this member from the bonds
			// map, same liveness contract as any other per-signer outage
			// (the BLS gate handles divergent CIDs by simply not signing).
			// Logged so operators can see when their node is repeatedly
			// dropping out of settlement composition.
			log.Warn("bond read failed; member dropped from settlement",
				"account", acct, "block_height", blockHeight, "err", err)
			continue
		}
		if rec == nil {
			continue
		}
		if rec.HIVE_CONSENSUS <= 0 {
			continue
		}
		out[acct] = rec.HIVE_CONSENSUS
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
