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
	PendulumBucketBalance(bucket string, blockHeight uint64) int64
	NewEmptySession(state *LedgerState, startHeight uint64) LedgerSession
	NewEmptyState() *LedgerState
}
