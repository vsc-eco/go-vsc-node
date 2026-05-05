package ledgerSystem

import (
	"fmt"
	"slices"
	"time"
	ledger_db "vsc-node/modules/db/vsc/ledger"
)

// blockingLedgerRead runs read() until it returns nil, with capped exponential
// backoff. Fail-stop primitive for the ledger-system spend-check path: a
// balance read that one node completes but another swallows lets the two nodes
// decide a tx outcome differently — a consensus fork — and the nil slice
// pointer GetLedgerRange returns on a Mongo error panics the node if
// dereferenced. Blocking until the DB recovers keeps every honest node either
// computing the identical balance or making no progress, never crashing and
// never forking. Mirrors state-processing.blockingRetry.
func blockingLedgerRead(what string, read func() error) {
	const (
		baseDelay = 100 * time.Millisecond
		maxDelay  = 30 * time.Second
	)
	delay := baseDelay
	for attempt := 1; ; attempt++ {
		if err := read(); err == nil {
			if attempt > 1 {
				log.Error("ledger DB read recovered; resuming", "op", what, "attempts", attempt)
			}
			return
		} else {
			log.Error("ledger DB read failed; blocking until DB recovers (fail-stop)",
				"op", what, "attempt", attempt, "retryIn", delay.String(), "err", err)
		}
		time.Sleep(delay)
		if delay < maxDelay {
			if delay *= 2; delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

// ledgerRangeOrBlock fail-stops on a GetLedgerRange error (GV-H1). The prior
// code discarded the error and ranged over the nil slice pointer the DB returns
// on a Mongo fault, panicking the node mid-spend-check; silently treating the
// error as "no records" would compute a balance from a partial read and fork
// this node from peers. Block until the read succeeds and return a non-nil
// slice.
func (ls *LedgerState) ledgerRangeOrBlock(
	account string,
	start, end uint64,
	asset string,
	opTypes []string,
) []ledger_db.LedgerRecord {
	var out *[]ledger_db.LedgerRecord
	blockingLedgerRead(fmt.Sprintf("GetLedgerRange(%s @%d %s)", account, end, asset), func() error {
		r, err := ls.LedgerDb.GetLedgerRange(account, start, end, asset, ledger_db.LedgerOptions{OpType: opTypes})
		if err != nil {
			return err
		}
		if r == nil {
			return fmt.Errorf("GetLedgerRange returned a nil result without an error")
		}
		out = r
		return nil
	})
	return *out
}

// hiveConsensusLedgerOps are the ONLY four LedgerDb op types that mutate the
// hive_consensus balance: consensus_stake (+), consensus_unstake (−),
// safety_slash_consensus (−, debits a slashed bond) and
// safety_slash_consensus_reverse (+, re-credits a bond on governance reversal
// of an erroneous slash). Single source of truth shared by GetBalance and
// GetConsensusBalanceAt — if a fifth mutator is ever added, extending this
// list updates both reads at once (a silent drift between them would make the
// bond inclusion gate and other hive_consensus consumers disagree about the
// same account at the same height).
var hiveConsensusLedgerOps = []string{
	"consensus_stake",
	"consensus_unstake",
	LedgerTypeSafetySlashConsensus,
	LedgerTypeSafetySlashConsensusReverse,
}

// Used to represent the global ledger state in the execution environment
type LedgerState struct {
	//List of finalized operations such as transfers, withdrawals.
	//Expected to be identical when block is produced and used during block creation
	Oplog []OpLogEvent
	//Virtual ledger is a cache of all balance changes (virtual and non-virtual)
	//Includes deposits, transfers (in-n-out), withdrawals, and stake/unstake operations (future)
	VirtualLedger map[string][]LedgerUpdate

	//Live calculated gateway balances on the fly
	//Use last saved balance as the starting data
	GatewayBalances map[string]uint64

	//Block height of the operation to be processed at
	BlockHeight uint64

	//Potential database state access

	LedgerDb  ledger_db.Ledger
	ActionDb  ledger_db.BridgeActions
	BalanceDb ledger_db.Balances
}

func (state *LedgerState) Validate() {
	// if state.BlockHeight == 0 {
	// 	panic("invalid ledgerState instance: BlockHeight is 0")
	// }
	if state.LedgerDb == nil || state.ActionDb == nil || state.BalanceDb == nil {
		panic("invalid ledgerState instance: LedgerDb is nil")
	}
}

// func (le *LedgerState) AppendLedger(update LedgerUpdate) {
// 	key := update.Owner + "#" + update.Asset
// 	if le.GatewayBalances[key] == 0 {
// 		le.Ls.GetBalance(update.Owner, update.BlockHeight, update.Asset)
// 	}
// }

func (le *LedgerState) Export() struct {
	Oplog []OpLogEvent
} {
	oplogCP := make([]OpLogEvent, len(le.Oplog))
	copy(oplogCP, le.Oplog)

	return struct {
		Oplog []OpLogEvent
	}{
		Oplog: oplogCP,
	}
}

func (state *LedgerState) Flush() {
	state.VirtualLedger = make(map[string][]LedgerUpdate)
	state.Oplog = make([]OpLogEvent, 0)

	//qq: should this be cleared when flushing?
	state.GatewayBalances = make(map[string]uint64)
}

func (state *LedgerState) Compile(bh uint64) *CompiledResult {
	if len(state.Oplog) == 0 {
		return nil
	}
	oplog := make([]OpLogEvent, 0)
	// copy(oplog, le.Oplog)

	for _, v := range state.Oplog {
		//bh should be == slot height
		if v.BlockHeight <= bh {
			oplog = append(oplog, v)
		}
	}

	return &CompiledResult{
		OpLog: oplog,
	}
}

// Original ledger executor
func (state *LedgerState) SnapshotForAccount(account string, blockHeight uint64, asset string) int64 {
	bal := state.GetBalance(account, blockHeight, asset)

	//le.Ls.log.Debug("getBalance le.VirtualLedger["+account+"]", le.VirtualLedger[account], blockHeight)

	for _, v := range state.VirtualLedger[account] {
		//Must be ledger ops with height below or equal to the current block height
		//Current block height ledger ops are recently executed
		if v.Asset == asset {
			bal += v.DeltaFor(account)
		}
	}
	return bal
}

// GetBalance returns the spendable balance of asset for account as of blockHeight:
// the BalanceDb snapshot field for the asset plus the net of every LedgerDb
// record past the snapshot height.
//
// The snapshot itself is always written correctly by state_engine.UpdateBalances
// (it sums every record of the asset minus the protocol meta rows); only the
// incremental delta below the snapshot height is computed here.
func (ls *LedgerState) GetBalance(account string, blockHeight uint64, asset string) int64 {
	if !slices.Contains(assetTypes, asset) {
		return 0
	}

	// GV-H1: fail-stop both DB reads. GetBalanceRecord returning (nil,nil) is a
	// valid "no snapshot yet" state, but a non-nil error must not be swallowed.
	var balRecordPtr *ledger_db.BalanceRecord
	blockingLedgerRead(fmt.Sprintf("GetBalanceRecord(%s @%d)", account, blockHeight), func() error {
		r, err := ls.BalanceDb.GetBalanceRecord(account, blockHeight)
		if err != nil {
			return err
		}
		balRecordPtr = r
		return nil
	})

	var recordHeight uint64
	var balRecord ledger_db.BalanceRecord
	if balRecordPtr == nil {
		recordHeight = 0
	} else {
		balRecord = *balRecordPtr
		recordHeight = balRecord.BlockHeight + 1
	}

	var base int64
	switch asset {
	case "hbd":
		base = balRecord.HBD
	case "hive":
		base = balRecord.Hive
	case "hbd_savings":
		base = balRecord.HBD_SAVINGS
	case "hive_consensus":
		base = balRecord.HIVE_CONSENSUS
	default:
		return 0
	}

	// CRIT-1 fix: sum EVERY ledger record of the asset past the snapshot height
	// (natural signs), excluding only the protocol meta rows
	// (IsProtocolMetaLedgerType — the shared source of truth, identical to the
	// snapshot fold in state_engine.UpdateBalances). The previous per-asset
	// OpType ALLOW-list silently dropped OUTFLOWS — transfer, withdraw, stake
	// (hbd) and transfer, withdraw, consensus_stake (hive) all carry the asset's
	// own `tk` but were never subtracted — so between a debit landing and the
	// next slot snapshot fold the spend check read a stale, overstated balance
	// and admitted a second debit of the same funds (double-spend). Summing all
	// records minus the meta rows makes GetBalance == snapshot + remaining-delta
	// by construction, so the delta and the fold can never drift again.
	//
	// Not consensus-version-gated: this is not a forkable change. GetBalance
	// only feeds block PRODUCTION and the cosigner's re-derivation; finalized
	// ledger state is the producer's oplog applied verbatim by IngestOplog (no
	// balance check), and a block finalizes only on a 2/3 matching-CID quorum
	// (one leader per slot). A divergent balance can therefore stall a slot but
	// can neither fork the chain nor change the replay of an already-finalized
	// block.
	// GV-H1: fail-stop this range read too (was `, _` discarding the error and
	// dereferencing the nil slice GetLedgerRange returns on a Mongo fault —
	// panicking the node mid-spend-check). nil opTypes == no `t` filter == every
	// record of the asset, identical to the prior unfiltered call.
	balAdjust := int64(0)
	for _, v := range ls.ledgerRangeOrBlock(account, recordHeight, blockHeight, asset, nil) {
		if IsProtocolMetaLedgerType(v.Type) {
			continue
		}
		balAdjust += v.DeltaFor(account)
	}
	return base + balAdjust
}

// GetConsensusBalanceAt is the ERROR-AWARE replay read of hive_consensus at
// blockHeight: the BalanceDb snapshot field at-or-before blockHeight plus the
// replay of every hiveConsensusLedgerOps record past the snapshot height —
// the exact value the same way GetBalance computes it, but with every DB error
// PROPAGATED instead of swallowed.
//
// It exists for the bond inclusion gate (audit F4/H-10, z3 m63-6): a seat gate
// that silently treats a read error as 0/stale either evicts an honest member
// on one node's transient DB hiccup (cross-node CID fork) or counts stale
// stake; fail-stop — abort the election attempt and retry next slot — is the
// unique non-divergent option. GetBalance keeps its legacy swallow semantics
// for its existing consumers; consensus-gating callers use this instead.
// (A nil snapshot record is NOT an error: it is the legitimate
// "account has no snapshot row yet" case and replay starts from genesis,
// exactly as GetBalance treats it.)
func (ls *LedgerState) GetConsensusBalanceAt(account string, blockHeight uint64) (int64, error) {
	balRecordPtr, err := ls.BalanceDb.GetBalanceRecord(account, blockHeight)
	if err != nil {
		return 0, fmt.Errorf("consensus balance snapshot read failed for %s at %d: %w", account, blockHeight, err)
	}

	var base int64
	var recordHeight uint64
	if balRecordPtr != nil {
		base = balRecordPtr.HIVE_CONSENSUS
		recordHeight = balRecordPtr.BlockHeight + 1
	}

	ledgerResults, err := ls.LedgerDb.GetLedgerRange(
		account,
		recordHeight,
		blockHeight,
		"hive_consensus",
		ledger_db.LedgerOptions{
			OpType: hiveConsensusLedgerOps,
		},
	)
	if err != nil {
		return 0, fmt.Errorf("consensus ledger range read failed for %s in (%d,%d]: %w", account, recordHeight, blockHeight, err)
	}
	if ledgerResults == nil {
		return 0, fmt.Errorf("consensus ledger range read returned nil for %s in (%d,%d]", account, recordHeight, blockHeight)
	}
	for _, v := range *ledgerResults {
		base += v.Amount
	}
	return base, nil
}

// ConsensusReverseCreditsInRange returns every safety_slash_consensus_reverse
// ledger row for account with start <= block_height <= end (error-aware, like
// GetConsensusBalanceAt). It exists for the bond inclusion gate's reverse-slash
// grandfather (audit X1/H-17): an erroneous safety_slash_consensus debits the
// bond, a governance reversal re-credits it, but the trailing min-over-window
// still reads the slashed-low value at samples that PRE-DATE the reversal — so
// an exonerated node would stay below MinStake (exiled) for up to a full window
// after being cleared (W == the slash burn delay, exactly). The gate uses these
// rows to add the reversed amount back to the pre-reversal samples, undoing the
// erroneous dip without affecting fresh stake / top-ups / unstakes. Dormant
// until safety slashing activates (params.SafetySlashActive /
// SafetySlashActivationHeight) — built defensively so the gate is correct the
// day slashing is turned on.
func (ls *LedgerState) ConsensusReverseCreditsInRange(account string, start, end uint64) ([]ledger_db.LedgerRecord, error) {
	rows, err := ls.LedgerDb.GetLedgerRange(
		account,
		start,
		end,
		"hive_consensus",
		ledger_db.LedgerOptions{
			OpType: []string{LedgerTypeSafetySlashConsensusReverse},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("consensus reverse-credit read failed for %s in [%d,%d]: %w", account, start, end, err)
	}
	if rows == nil {
		return nil, fmt.Errorf("consensus reverse-credit read returned nil for %s in [%d,%d]", account, start, end)
	}
	return *rows, nil
}
