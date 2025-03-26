// Code generated by github.com/99designs/gqlgen, DO NOT EDIT.

package gqlgen

import (
	"fmt"
	"io"
	"strconv"
	"vsc-node/modules/db/vsc/contracts"
)

type BalanceAccessCondition interface {
	IsBalanceAccessCondition()
	GetType() *BalanceAccessConditionType
	GetValue() *string
}

type BalanceController interface {
	IsBalanceController()
	GetType() *BalanceControllerType
	GetAuthority() *string
	GetConditions() []BalanceAccessCondition
}

type BlockRef interface {
	IsBlockRef()
	GetBlockRef() *string
	GetIncludedBlock() *int
}

type DepositDrain interface {
	IsDepositDrain()
	GetDepositID() *string
	GetAmount() *float64
	GetToken() *string
	GetOwner() *string
}

type AccountInfoResult struct {
	RcMax     *int `json:"rc_max,omitempty"`
	RcCurrent *int `json:"rc_current,omitempty"`
}

type AccountNonceResult struct {
	Nonce *int `json:"nonce,omitempty"`
}

type AnchorProducer struct {
	NextSlot *string `json:"nextSlot,omitempty"`
}

type Auth struct {
	Value string `json:"value"`
}

type Contract struct {
	ID         *string `json:"id,omitempty"`
	Code       *string `json:"code,omitempty"`
	CreationTs *string `json:"creation_ts,omitempty"`
}

type ContractDiff struct {
	Diff                    *string `json:"diff,omitempty"`
	PreviousContractStateID string  `json:"previousContractStateId"`
}

type FindContractOutputFilter struct {
	ByInput    *string `json:"byInput,omitempty"`
	ByOutput   *string `json:"byOutput,omitempty"`
	ByContract *string `json:"byContract,omitempty"`
	Limit      *int    `json:"limit,omitempty"`
}

type FindContractOutputResult struct {
	Outputs []*contracts.ContractOutput `json:"outputs,omitempty"`
}

type FindContractResult struct {
	Status *string `json:"status,omitempty"`
}

type FindTransactionFilter struct {
	ByID         *string `json:"byId,omitempty"`
	ByAccount    *string `json:"byAccount,omitempty"`
	ByContract   *string `json:"byContract,omitempty"`
	ByStatus     *string `json:"byStatus,omitempty"`
	ByOpCategory *string `json:"byOpCategory,omitempty"`
	ByAction     *string `json:"byAction,omitempty"`
	Limit        *int    `json:"limit,omitempty"`
}

type FindTransactionResult struct {
	Txs []*Transaction `json:"txs,omitempty"`
}

type Gas struct {
	Io *int `json:"IO,omitempty"`
}

type GetBalanceResult struct {
	Account     *string           `json:"account,omitempty"`
	BlockHeight *int              `json:"block_height,omitempty"`
	Tokens      *GetBalanceTokens `json:"tokens,omitempty"`
}

type GetBalanceTokens struct {
	Hbd  *float64 `json:"HBD,omitempty"`
	Hive *float64 `json:"HIVE,omitempty"`
}

type Headers struct {
	Nonce *int `json:"nonce,omitempty"`
}

type HiveKeys struct {
	Posting *string `json:"posting,omitempty"`
	Active  *string `json:"active,omitempty"`
	Owner   *string `json:"owner,omitempty"`
}

type JSONPatchOp struct {
	Op    *string `json:"op,omitempty"`
	Path  *string `json:"path,omitempty"`
	Value *string `json:"value,omitempty"`
}

type LedgerOp struct {
	ID          string  `json:"id"`
	Amount      int     `json:"amount"`
	BlockHeight int     `json:"block_height"`
	Idx         float64 `json:"idx"`
	From        *string `json:"from,omitempty"`
	Memo        *string `json:"memo,omitempty"`
	Owner       string  `json:"owner"`
	T           string  `json:"t"`
	Tk          string  `json:"tk"`
	Status      string  `json:"status"`
}

type LedgerResults struct {
	Txs []LedgerOp `json:"txs,omitempty"`
}

type LedgerTxFilter struct {
	ByToFrom *string `json:"byToFrom,omitempty"`
	ByTxID   *string `json:"byTxId,omitempty"`
	Offset   *int    `json:"offset,omitempty"`
	Limit    *int    `json:"limit,omitempty"`
}

type LocalNodeInfo struct {
	PeerID *string `json:"peer_id,omitempty"`
	Did    *string `json:"did,omitempty"`
}

type Mutation struct {
}

type Query struct {
}

type TestResult struct {
	CurrentNumber *int `json:"currentNumber,omitempty"`
}

type Transaction struct {
	ID              string             `json:"id"`
	Status          string             `json:"status"`
	Headers         *Headers           `json:"headers,omitempty"`
	RequiredAuths   []Auth             `json:"required_auths,omitempty"`
	Data            *TransactionData   `json:"data,omitempty"`
	SigHash         *string            `json:"sig_hash,omitempty"`
	Src             *string            `json:"src,omitempty"`
	FirstSeen       *string            `json:"first_seen,omitempty"`
	Local           *bool              `json:"local,omitempty"`
	Accessible      *bool              `json:"accessible,omitempty"`
	AnchoredBlock   *string            `json:"anchored_block,omitempty"`
	AnchoredHeight  *int               `json:"anchored_height,omitempty"`
	AnchoredID      *string            `json:"anchored_id,omitempty"`
	AnchoredIndex   *int               `json:"anchored_index,omitempty"`
	AnchoredOpIndex *int               `json:"anchored_op_index,omitempty"`
	Output          *TransactionOutput `json:"output,omitempty"`
}

type TransactionData struct {
	Op         string  `json:"op"`
	Action     *string `json:"action,omitempty"`
	Payload    *string `json:"payload,omitempty"`
	ContractID *string `json:"contract_id,omitempty"`
}

type TransactionOutput struct {
	Index *int    `json:"index,omitempty"`
	ID    *string `json:"id,omitempty"`
}

type TransactionSubmitResult struct {
	ID *string `json:"id,omitempty"`
}

type BalanceAccessConditionType string

const (
	BalanceAccessConditionTypeTime     BalanceAccessConditionType = "TIME"
	BalanceAccessConditionTypeHash     BalanceAccessConditionType = "HASH"
	BalanceAccessConditionTypeWithdraw BalanceAccessConditionType = "WITHDRAW"
)

var AllBalanceAccessConditionType = []BalanceAccessConditionType{
	BalanceAccessConditionTypeTime,
	BalanceAccessConditionTypeHash,
	BalanceAccessConditionTypeWithdraw,
}

func (e BalanceAccessConditionType) IsValid() bool {
	switch e {
	case BalanceAccessConditionTypeTime, BalanceAccessConditionTypeHash, BalanceAccessConditionTypeWithdraw:
		return true
	}
	return false
}

func (e BalanceAccessConditionType) String() string {
	return string(e)
}

func (e *BalanceAccessConditionType) UnmarshalGQL(v any) error {
	str, ok := v.(string)
	if !ok {
		return fmt.Errorf("enums must be strings")
	}

	*e = BalanceAccessConditionType(str)
	if !e.IsValid() {
		return fmt.Errorf("%s is not a valid BalanceAccessConditionType", str)
	}
	return nil
}

func (e BalanceAccessConditionType) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, strconv.Quote(e.String()))
}

type BalanceControllerType string

const (
	BalanceControllerTypeHive     BalanceControllerType = "HIVE"
	BalanceControllerTypeDid      BalanceControllerType = "DID"
	BalanceControllerTypeContract BalanceControllerType = "CONTRACT"
)

var AllBalanceControllerType = []BalanceControllerType{
	BalanceControllerTypeHive,
	BalanceControllerTypeDid,
	BalanceControllerTypeContract,
}

func (e BalanceControllerType) IsValid() bool {
	switch e {
	case BalanceControllerTypeHive, BalanceControllerTypeDid, BalanceControllerTypeContract:
		return true
	}
	return false
}

func (e BalanceControllerType) String() string {
	return string(e)
}

func (e *BalanceControllerType) UnmarshalGQL(v any) error {
	str, ok := v.(string)
	if !ok {
		return fmt.Errorf("enums must be strings")
	}

	*e = BalanceControllerType(str)
	if !e.IsValid() {
		return fmt.Errorf("%s is not a valid BalanceControllerType", str)
	}
	return nil
}

func (e BalanceControllerType) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, strconv.Quote(e.String()))
}

type TransactionStatus string

const (
	TransactionStatusUnconfirmed TransactionStatus = "UNCONFIRMED"
	TransactionStatusConfirmed   TransactionStatus = "CONFIRMED"
	TransactionStatusFailed      TransactionStatus = "FAILED"
	TransactionStatusIncluded    TransactionStatus = "INCLUDED"
	TransactionStatusProcessed   TransactionStatus = "PROCESSED"
)

var AllTransactionStatus = []TransactionStatus{
	TransactionStatusUnconfirmed,
	TransactionStatusConfirmed,
	TransactionStatusFailed,
	TransactionStatusIncluded,
	TransactionStatusProcessed,
}

func (e TransactionStatus) IsValid() bool {
	switch e {
	case TransactionStatusUnconfirmed, TransactionStatusConfirmed, TransactionStatusFailed, TransactionStatusIncluded, TransactionStatusProcessed:
		return true
	}
	return false
}

func (e TransactionStatus) String() string {
	return string(e)
}

func (e *TransactionStatus) UnmarshalGQL(v any) error {
	str, ok := v.(string)
	if !ok {
		return fmt.Errorf("enums must be strings")
	}

	*e = TransactionStatus(str)
	if !e.IsValid() {
		return fmt.Errorf("%s is not a valid TransactionStatus", str)
	}
	return nil
}

func (e TransactionStatus) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, strconv.Quote(e.String()))
}

type TransactionType string

const (
	TransactionTypeNull    TransactionType = "NULL"
	TransactionTypeInput   TransactionType = "INPUT"
	TransactionTypeOutput  TransactionType = "OUTPUT"
	TransactionTypeVirtual TransactionType = "VIRTUAL"
	TransactionTypeCore    TransactionType = "CORE"
)

var AllTransactionType = []TransactionType{
	TransactionTypeNull,
	TransactionTypeInput,
	TransactionTypeOutput,
	TransactionTypeVirtual,
	TransactionTypeCore,
}

func (e TransactionType) IsValid() bool {
	switch e {
	case TransactionTypeNull, TransactionTypeInput, TransactionTypeOutput, TransactionTypeVirtual, TransactionTypeCore:
		return true
	}
	return false
}

func (e TransactionType) String() string {
	return string(e)
}

func (e *TransactionType) UnmarshalGQL(v any) error {
	str, ok := v.(string)
	if !ok {
		return fmt.Errorf("enums must be strings")
	}

	*e = TransactionType(str)
	if !e.IsValid() {
		return fmt.Errorf("%s is not a valid TransactionType", str)
	}
	return nil
}

func (e TransactionType) MarshalGQL(w io.Writer) {
	fmt.Fprint(w, strconv.Quote(e.String()))
}
