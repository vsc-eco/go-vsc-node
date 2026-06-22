package ledgerSystem

type LedgerUpdate struct {
	Id string
	//Block Index
	BlockHeight uint64
	//Block Index: Index of the TX in the block
	BIdx int64
	//Op Index: Index of the operation in the TX
	OpIdx int64

	Owner  string
	Amount int64
	Asset  string
	Memo   string
	//transfer, withdraw, stake, unstake
	Type string
}

type OpLogEvent struct {
	To     string `json:"to" bson:"to"`
	From   string `json:"fr" bson:"from"`
	Amount int64  `json:"am" bson:"amount"`
	Asset  string `json:"as" bson:"asset"`
	Memo   string `json:"mo" bson:"memo"`
	Type   string `json:"ty" bson:"type"`

	//Not parted of compiled state
	Id          string `json:"id" bson:"id"`
	BIdx        int64  `json:"-"  bson:"-"`
	OpIdx       int64  `json:"-"  bson:"-"`
	BlockHeight uint64 `json:"-"  bson:"-"`

	//Fee for instant stake unstake
	// Fee int64 `json:"fee,omitempty"`
	Params map[string]interface{} `json:"params,omitempty"`
}

type WithdrawParams struct {
	Id     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Asset  string `json:"asset"`
	Amount int64  `json:"amount"`
	Memo   string `json:"memo"`

	BIdx        int64  `json:"bidx"`
	OpIdx       int64  `json:"opidx"`
	BlockHeight uint64 `json:"block_height"`
}

type ConsensusParams struct {
	Id            string
	From          string
	To            string
	Amount        int64
	Type          string
	BlockHeight   uint64
	ElectionEpoch uint64
	// Delegated selects per-edge delegation semantics (consensus 0.3.0+). Set by
	// the caller from StateEngine.delegatedStakeActive(blockHeight). When true,
	// stake records a from->to edge and unstake authorizes against the signer's
	// edge + debits the node bond; when false, the legacy hive_consensus-holder
	// path runs unchanged. Carried through into the OpLogEvent so the (pure)
	// record builder stays deterministic.
	Delegated bool
}

// SafetySlashConsensusParams debits HIVE_CONSENSUS principal for provable safety faults.
// The full slash residual lands in the keyless reserve
// (params.ProtocolSlashReserveAccount) — destination change, no longer burned.
// There is NO victim payout: the only slashable faults (double-block-sign,
// invalid-block-proposal) are consensus-integrity faults with no fund victim,
// so the reserve is a retained backstop only.
type SafetySlashConsensusParams struct {
	Account      string
	SlashBps     int
	TxID         string
	BlockHeight  uint64
	EvidenceKind string
	// BurnDelayBlocks: if 0, the residual is written immediately to the keyless
	// reserve (ProtocolSlashReserveAccount) — and is then NOT reversible, since it
	// is committed with no challenge window. If > 0, it sits on
	// ProtocolSlashPendingBurnAccount until block height reaches
	// BlockHeight+BurnDelayBlocks, then FinalizeMaturedSafetySlashBurns promotes it
	// to the reserve (destination change — no longer burned). Name kept
	// ("BurnDelay") for compatibility.
	BurnDelayBlocks uint64
}

// CancelPendingSafetySlashBurnParams identifies a pending burn slice to cancel
// before it matures. The (TxID, EvidenceKind) tuple matches the original
// SafetySlashConsensusParams; if the row has already been finalized (or
// cancelled), the call is a no-op.
type CancelPendingSafetySlashBurnParams struct {
	// TxID of the original slash (the on-chain proof Tx).
	TxID string
	// EvidenceKind of the original slash.
	EvidenceKind string
	// SlashedAccount carried for traceability; the pending row's owner is
	// always params.ProtocolSlashPendingBurnAccount.
	SlashedAccount string
	// BlockHeight at which the cancel is recorded.
	BlockHeight uint64
	// Reason is logged in the cancellation marker for explorers.
	Reason string
}

// ReverseSafetySlashConsensusDebitParams re-credits a previously debited
// HIVE_CONSENSUS bond. The intended use is via the reverse-handler's "both"
// action (cancel the pending residual, then re-credit the bond) to undo a slash
// DURING the challenge window. Once the residual has been committed to the
// keyless reserve (matured, or written immediately when BurnDelayBlocks==0), the
// reverse cap yields 0 headroom — it cannot re-credit committed value, because
// the reserve RETAINS it and re-crediting would mint. The apply path enforces
// this cap; this primitive does not look up the original debit itself.
type ReverseSafetySlashConsensusDebitParams struct {
	// TxID of the original slash.
	TxID string
	// EvidenceKind of the original slash.
	EvidenceKind string
	// Account whose HIVE_CONSENSUS bond is being re-credited.
	Account string
	// Amount in satoshis to re-credit. Caller must enforce that this does
	// not exceed the original debit recorded for (TxID, EvidenceKind, Account)
	// — the ledger primitive does not currently look up the original debit.
	Amount int64
	// BlockHeight at which the credit is recorded.
	BlockHeight uint64
	// Reason is logged in the credit row for explorers.
	Reason string
	// OpInstanceID is a unique identifier for this *invocation* of the
	// reverse primitive (typically the carrying chain-op's tx id). It is
	// folded into the deterministic ledger row Id so two distinct
	// reverses on the same (TxID, EvidenceKind, Account) tuple produce
	// distinct rows and accumulate, while replays of the same chain op
	// upsert. When blank, the primitive falls back to a single per-tuple
	// row (the legacy single-shot behaviour) — which is still correct
	// when there is exactly one reverse ever issued.
	OpInstanceID string
}

type LedgerResult struct {
	Ok  bool
	Msg string
}

type TransferOptions struct {
	//Excluded HBD amount that cannot be sent
	Exclusion int64
}
type CompiledResult struct {
	OpLog []OpLogEvent
}

type StakeOp struct {
	OpLogEvent
	Instant bool
	//If true, if stake/unstake fails, then regular op will occurr
	NoFail bool
}

type Deposit struct {
	Id          string
	OpIdx       int64
	BIdx        int64
	BlockHeight uint64

	Account string
	Amount  int64
	Asset   string
	Memo    string
	From    string
}

// Add more fields as necessary
type DepositParams struct {
	To string `json:"to"`
}

type OplogInjestOptions struct {
	StartHeight uint64
	EndHeight   uint64
}

type ExtraInfo struct {
	BlockHeight uint64
	ActionId    string
}

type LedgerSession interface {
	GetBalance(account string, blockHeight uint64, asset string) int64
	ExecuteTransfer(OpLogEvent OpLogEvent, options ...TransferOptions) LedgerResult
	Withdraw(withdraw WithdrawParams) LedgerResult
	Stake(StakeOp, ...TransferOptions) LedgerResult
	Unstake(StakeOp) LedgerResult
	ConsensusStake(ConsensusParams) LedgerResult
	ConsensusUnstake(ConsensusParams) LedgerResult
	Done() []string
	Revert()
	// Savepoint/RestoreSavepoint support nested rollback for try/catch
	// inter-contract calls: capture a point, then unwind to it on a caught failure.
	Savepoint() LedgerSavepoint
	RestoreSavepoint(LedgerSavepoint)
}

type LedgerSystem interface {
	GetBalance(account string, blockHeight uint64, asset string) int64
	ClaimHBDInterest(lastClaim uint64, blockHeight uint64, amount int64, txId string)
	IndexActions(actionUpdate map[string]interface{}, extraInfo ExtraInfo)
	Deposit(deposit Deposit) string
	IngestOplog(oplog []OpLogEvent, options OplogInjestOptions)
	// PendulumDistribute drains the pendulum:nodes:hbd bucket into a recipient
	// account at settlement time. Paired ledger ops (debit on bucket, credit
	// on recipient) — settlement is the only path that mutates the bucket
	// downward. Per-swap accrual INTO the bucket goes through
	// LedgerSession.ExecuteTransfer from the executing contract account, NOT
	// through this interface.
	PendulumDistribute(toAccount string, amount int64, txID string, blockHeight uint64) LedgerResult
	SafetySlashConsensusBond(p SafetySlashConsensusParams) LedgerResult
	FinalizeMaturedSafetySlashBurns(blockHeight uint64)
	// MigrateDelegationEdgesOnce backfills per-delegator consensus-stake edges
	// from history exactly once (idempotent via a persisted marker), at the
	// consensus-0.3.0 activation. Caller gates on delegatedStakeActive. Returns
	// the number of edges seeded (0 if already migrated / no history).
	MigrateDelegationEdgesOnce(blockHeight uint64) int
	// AllDelegationEdges returns every node's positive consensus-delegation
	// edges as of blockHeight: node -> (delegator -> net HIVE delegated), both
	// in "hive:" form, from a single deterministic ledger scan. ok=false on a
	// transient read error. Drives the share-mode pendulum reward split.
	AllDelegationEdges(blockHeight uint64) (map[string]map[string]int64, bool)
	// CancelPendingSafetySlashBurn cancels a pending burn slice before maturity.
	// Idempotent: if the (TxID, EvidenceKind) row is missing, finalized, or
	// already cancelled, returns Ok=false with a descriptive Msg but does not
	// error. Pair with ReverseSafetySlashConsensusDebit to fully undo a slash.
	CancelPendingSafetySlashBurn(p CancelPendingSafetySlashBurnParams) LedgerResult
	// ReverseSafetySlashConsensusDebit re-credits HIVE_CONSENSUS bond debited
	// by a previous SafetySlashConsensusBond call. Idempotent per
	// (TxID, EvidenceKind, Account) tuple.
	ReverseSafetySlashConsensusDebit(p ReverseSafetySlashConsensusDebitParams) LedgerResult
	PendulumBucketBalance(bucket string, blockHeight uint64) int64
	NewEmptySession(state *LedgerState, startHeight uint64) LedgerSession
	NewEmptyState() *LedgerState
}
