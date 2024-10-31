package schema

type JSON map[string]interface{}

type JsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value JSON   `json:"value"`
}

// this represents the smart contract
type Contract struct {
	ID         string `json:"id"`
	Code       string `json:"code"`
	CreationTS string `json:"creation_ts"`
}

type TransactionStatus string

const (
	TxStatusUnconfirmed TransactionStatus = "UNCONFIRMED"
	TxStatusConfirmed   TransactionStatus = "CONFIRMED"
	TxStatusFailed      TransactionStatus = "FAILED"
	TxStatusIncluded    TransactionStatus = "INCLUDED"
	TxStatusProcessed   TransactionStatus = "PROCESSED"
)

type TransactionsType string

const (
	TxTypeNull    TransactionsType = "NULL"
	TxTypeInput   TransactionsType = "INPUT"
	TxTypeOuput   TransactionsType = "OUTPUT"
	TxTypeVirtual TransactionsType = "VIRTUAL"
	TxTypeCore    TransactionsType = "CORE"
)

type Transaction struct {
	ID              string             `json:"id"`
	Status          string             `json:"status"`
	Headers         *Headers           `json:"headers, omitemptyy"`
	RequiredAuths   []Auth             `json:required_auths"`
	Data            *TransactionData   `json: "data, omitempty"`
	SigHash         string             `json: "sig_hash, omitempty"`
	Src             string             `json: "src, omitempty"`
	FirstSeen       string             `json:"first_seen,omitempty"`
	Local           bool               `json:"local,omitempty"`
	Accessible      bool               `json:"accessible, omitempty"`
	AnchoredBlock   string             `json:"anchored_black, omitempty"`
	AnchoredHeight  int                `json:"anchored_height, omitempty"`
	AnchoredID      int                `json:"anchored_id, omitempty"`
	AnchoredIndex   int                `json:"anchored_index, omitempty"`
	AnchoredOpIndex int                `json:"anchored_op_index, omitempty"`
	Output          *TransactionOutput `json:"output, omitempty"`
}

type Headers struct {
	Nonce int `json:"nonce"`
}

// it represents the authentication information
type Auth struct {
	Value string `json: "value"`
}

type TransactionData struct {
	Op         string `json:"op"`
	Action     string `json:"action, omitempty"`
	Payload    JSON   `json: "payload, omitempty"`
	ContractID string `json:"contract_id, omitempty"`
}

// Transaction output
type TransactionOutput struct {
	Index int    `json:"index"`
	Id    string `json:"id"`
}

// output of contract execution
type ContractOutput struct {
	ID             string   `json: "id"`
	AnchorBlock    string   `json: "anchor_block, omitempty"`
	AnchoredHeight string   `json: "anchored_height, omitempty"`
	AnchoredID     string   `json: "anchored_id, omitempty"`
	AnchoredIndex  string   `json: "anchored_index, omitempty"`
	ContractID     string   `json: "contract_id, omitempty"`
	Gas            *Gas     `json: "gas, omitempty"`
	Inputs         []string `json:"inputs"`
	Results        []JSON   `json:"results"`
	SideEffects    JSON     `json: "side_effects, omitempty"`
	StateMarket    string   `json: "state_market, omitempty"`
}

// Gas represents gas usage information
type Gas struct {
	IO int `json:"io"`
}

// Contract Diff represents difference int
type ContractDiff struct {
	Diff                    JSON   `json:"diff"`
	PreviousContractStateID string `json: "previous_contract_state_id`
}

type ContractState struct {
	ID          string `json: "id"`
	State       string `json: "state. omitempty"`
	StateQuery  string `json: "state_query, omitempty"`
	StateKeys   string `json: "state_keys, omitempty"`
	StateMerkle string `json: "state_merkle, omitempty"`
}

type FindContractsResult struct {
	Status string `json:"status"`
}

type TransactionSubmitResult struct {
	Id string `json:"id"`
}

type AccountNonceResult struct {
	Nonce int `json:"nonce"`
}

type AccountInfoResult struct {
	RCMAX     int `json:"rc_max"`
	RCCurrent int `json:"rc_current"`
}

type LocalNodeInfo struct {
	PeerID string `json:"peer_id"`
	DID    string `json:"did"`
}

type HiveKeys struct {
	posting string `json:"posting"`
	active  string `json:"active"`
	owner   string `json:"owner"`
}

type WitnessNode struct {
	Account     string   `json: "account"`
	IpfsPeerID  string   `json: "ipfs_peer_id"`
	LastSigned  int      `json:"last_signed"`
	NetID       string   `json:"net_id"`
	VersionID   string   `json:"version_id"`
	SigningKeys HiveKeys `json:"signing_keys"`
}

type BalanceControllerType string

const (
	BalanceControllerHive     BalanceControllerType = "HIVE"
	BalanceControllerDID      BalanceControllerType = "DID"
	BalanceControllerContract BalanceControllerType = "CONTRACT"
)

type BalanceAccessConditionType string

const (
	BalanceAccessConditionTime      BalanceAccessConditionType = "TIME"
	BalanceAccecssConditionHash     BalanceAccessConditionType = "HASH"
	BalanceAccecssConditionWithdraw BalanceAccessConditionType = "WITHDRAW"
)

type DepositDrain struct {
	DepositID string  `json:"deposit_id"`
	Amount    float64 `json:"amount"`
	Token     float64 `json:"token"`
	Owner     float64 `json:"owner"`
}

type BlockRef struct {
	BlockRef      string `json:"block_ref"`
	IncludedBlock string `json:"included_block"`
}

type GetBalanceTokens struct {
	HBD  float64 `json:"hbd"`
	HIVE float64 `json:"hive"`
}

type GetBalanceResult struct {
	Account     string `json: "account"`
	BlockHeight int    `json: "block_height"`

	Tokens GetBalanceTokens `json: "tokens"`
}

type FindTransactionsResult struct {
	Txs []Transaction `json:"txs"`
}

type FindContractOutputResult struct {
	Outputs []ContractOutput `json:"outputs"`
}

//TODO: I have a doubt here
//ts version of schema
// type AnchorProducer {
//     nextSlot(account: String): JSON
// }

type AnchorProducer interface {
	NextSlot(account string) map[string]interface{}
}

type LedgerOp struct {
	ID          string  `json: "id"`
	Amount      int     `json: "amount"`
	BlockHeight int     `json: "block_height`
	Idx         float64 `json:"idx"`
	From        string  `json:"from, omitempty"`
	Memo        string  `json:"memo, omitempty"`
	Owner       string  `json:"owner"`
	T           string  `json:"t"`
	Tk          string  `json:"tk"`
	Status      string  `json:"status"`
}

type LedgerResults struct {
	Txs []LedgerOp `json:"txs"`
}

type LedgerTxFilter struct {
	ByToFrom string `json:"by_to_from,omitempty"`
	ByTxID   string `json:"by_tx_id,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type FindTransactionFilter struct {
	ByID         string `json:"by_id,omitempty"`
	ByAccount    string `json:"by_account,omitempty"`
	ByContract   string `json:"by_contract,omitempty"`
	ByStatus     string `json:"by_status,omitempty"`
	ByOpCategory string `json:"by_op_category,omitempty"`
	ByAction     string `json:"by_action,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type FindContractOutputFilter struct {
	ByInput    string `json:"by_input,omitempty"`
	ByOutput   string `json:"by_output,omitempty"`
	ByContract string `json:"by_contract,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

// Query Operations

type Query struct {
	ContractStateDiff    func(id string) ContractDiff
	ContractState        func(id string) ContractState
	FindTransaction      func(filter FindTransactionFilter, decodedFilter JSON) FindTransactionsResult
	FindContractOutput   func(filter FindContractOutputFilter, decodedFilter JSON) FindContractOutputResult
	FindLedgerTXs        func(filter LedgerTxFilter) LedgerResults
	GetAccountBalance    func(account string) GetBalanceResult
	SubmitTransactionV1  func(tx string, sig string) TransactionSubmitResult
	GetAccountNonce      func(keyGroup []string) AccountNonceResult
	LocalNodeInfo        func() LocalNodeInfo
	WitnessNodes         func(height int) []WitnessNode
	ActiveWitnessNodes   func() JSON
	WitnessSchedule      func(height int) JSON
	NextWitnessSlot      func(self bool) JSON
	WitnessActiveScore   func(height int) JSON
	MockGenerateElection func() JSON
	AnchorProducer       func() AnchorProducer
}
