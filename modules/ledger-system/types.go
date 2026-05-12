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
	BIdx        int64  `json:"-" bson:"-"`
	OpIdx       int64  `json:"-" bson:"-"`
	BlockHeight uint64 `json:"-" bson:"-"`

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
}

// SlashRestitutionPayment credits liquid HIVE to a victim from a safety slash.
type SlashRestitutionPayment struct {
	ClaimID       string
	VictimAccount string
	Amount        int64
}

// SlashRestitutionClaim is a FIFO queue entry: remaining LossHive still owed to VictimAccount.
type SlashRestitutionClaim struct {
	ClaimID       string
	VictimAccount string
	LossHive      int64
}

// SlashRestitutionAllocator splits slashed liquid HIVE between victims and protocol burn.
// Implementations must return payments and burnAmt with Sum(payments.Amount)+burnAmt == slashAmt.
type SlashRestitutionAllocator interface {
	AllocateHive(slashAmt int64, blockHeight uint64, txID, evidenceKind, slashedAccount string) (payments []SlashRestitutionPayment, burnAmt int64)
}

// SafetySlashConsensusParams debits HIVE_CONSENSUS principal for provable safety faults.
// Liquid HIVE is credited via Restitution (FIFO victims) and/or protocol burn (see params.ProtocolSlashBurnAccount).
// When Restitution is nil, the full slash is recorded as burn (nothing held for the DAO).
type SafetySlashConsensusParams struct {
	Account      string
	SlashBps     int
	TxID         string
	BlockHeight  uint64
	EvidenceKind string
	Restitution  SlashRestitutionAllocator
	// BurnDelayBlocks: if 0, the burn slice is written immediately to ProtocolSlashBurnAccount.
	// If > 0, it sits on ProtocolSlashPendingBurnAccount until block height reaches
	// BlockHeight+BurnDelayBlocks (see FinalizeMaturedSafetySlashBurns). Restitution is always immediate.
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
// HIVE_CONSENSUS bond. Use after CancelPendingSafetySlashBurnParams to fully
// undo a slash, or alone when a finalized burn has already been processed but
// governance still wants to make the validator whole from another source.
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
}

// EnqueueRestitutionClaimParams persists a restitution claim as a ledger row
// on params.ProtocolSlashRestitutionClaimsAccount so future
// SafetySlashConsensusBond calls allocate liquid HIVE FIFO from the queue
// rather than burning the entire slash. The on-ledger queue is the
// consensus-safe replacement for the in-memory MemoryRestitutionQueue:
// every node sees the same claims after replay because the rows are part
// of the deterministic ledger.
//
// All claims are submitted via the vsc.restitution_claim block-content tx,
// which the witness committee signs with a 2/3 BLS aggregate. Per-claim
// validation (slash row exists, amount within slash residual, optional
// victim_tx_id verification) happens in the chain-op handler before this
// primitive is called.
type EnqueueRestitutionClaimParams struct {
	// ClaimID is the deterministic identifier; replays upsert by this ID.
	// The chain-op handler builds it from the carrying block + tx index so
	// retries within the same block are stable.
	ClaimID string
	// VictimAccount is the recipient credited when the queue allocates.
	// Stored normalized ("hive:<name>").
	VictimAccount string
	// SlashTxID is the original safety_slash_consensus row this claim is
	// scoped to. Mainly explanatory; the allocator does not enforce
	// per-slash quotas in this version.
	SlashTxID string
	// LossHive is the amount in satoshi-HIVE the claim is asking to be
	// restituted from future slashes. Must be > 0.
	LossHive int64
	// BlockHeight at which the claim was enqueued (the carrying block).
	BlockHeight uint64
	// TxID of the carrying block-content tx (for traceability + idempotency
	// id derivation).
	TxID string
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
	// CancelPendingSafetySlashBurn cancels a pending burn slice before maturity.
	// Idempotent: if the (TxID, EvidenceKind) row is missing, finalized, or
	// already cancelled, returns Ok=false with a descriptive Msg but does not
	// error. Pair with ReverseSafetySlashConsensusDebit to fully undo a slash.
	CancelPendingSafetySlashBurn(p CancelPendingSafetySlashBurnParams) LedgerResult
	// ReverseSafetySlashConsensusDebit re-credits HIVE_CONSENSUS bond debited
	// by a previous SafetySlashConsensusBond call. Idempotent per
	// (TxID, EvidenceKind, Account) tuple.
	ReverseSafetySlashConsensusDebit(p ReverseSafetySlashConsensusDebitParams) LedgerResult
	// EnqueueRestitutionClaim persists a victim's restitution claim as a
	// ledger row on params.ProtocolSlashRestitutionClaimsAccount. The row
	// becomes part of the consensus-safe FIFO queue read by
	// OnLedgerRestitutionAllocator the next time a slash fires. Idempotent
	// per ClaimID — replays upsert the same row.
	EnqueueRestitutionClaim(p EnqueueRestitutionClaimParams) LedgerResult
	PendulumBucketBalance(bucket string, blockHeight uint64) int64
	NewEmptySession(state *LedgerState, startHeight uint64) LedgerSession
	NewEmptyState() *LedgerState
}
