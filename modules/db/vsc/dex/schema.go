package dex

import "time"

// PoolStatus represents the status of a liquidity pool
type PoolStatus string

const (
	PoolStatusLocked   PoolStatus = "locked"
	PoolStatusUnlocked PoolStatus = "unlocked"
)

// BridgeDirection represents the direction of a bridge action
type BridgeDirection string

const (
	BridgeDirectionCreate   BridgeDirection = "create"
	BridgeDirectionTransfer BridgeDirection = "transfer"
	BridgeDirectionWithdraw BridgeDirection = "withdraw"
)

// BridgeActionStatus represents the status of a bridge action
type BridgeActionStatus string

const (
	BridgeActionStatusPending    BridgeActionStatus = "pending"
	BridgeActionStatusProcessing BridgeActionStatus = "processing"
	BridgeActionStatusComplete   BridgeActionStatus = "complete"
	BridgeActionStatusFailed     BridgeActionStatus = "failed"
)

// PoolStatus methods
func (ps PoolStatus) IsValid() bool {
	return ps == PoolStatusLocked || ps == PoolStatusUnlocked
}

func (ps PoolStatus) String() string {
	return string(ps)
}

// BridgeDirection methods
func (bd BridgeDirection) IsValid() bool {
	return bd == BridgeDirectionCreate ||
		bd == BridgeDirectionTransfer ||
		bd == BridgeDirectionWithdraw
}

func (bd BridgeDirection) String() string {
	return string(bd)
}

// BridgeActionStatus methods
func (bs BridgeActionStatus) IsValid() bool {
	return bs == BridgeActionStatusPending ||
		bs == BridgeActionStatusProcessing ||
		bs == BridgeActionStatusComplete ||
		bs == BridgeActionStatusFailed
}

func (bs BridgeActionStatus) String() string {
	return string(bs)
}

type TokenMetadata struct {
	Symbol      string    `bson:"symbol" json:"symbol"`
	Decimals    uint8     `bson:"decimals" json:"decimals"`
	ContractId  *string   `bson:"contract_id,omitempty" json:"contract_id,omitempty"`
	Description string    `bson:"description" json:"description"`
	CreatedAt   time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time `bson:"updated_at" json:"updated_at"`
}

type PoolInfo struct {
	ContractId     string     `bson:"contract_id" json:"contract_id"`
	Asset0         string     `bson:"asset0" json:"asset0"`
	Asset1         string     `bson:"asset1" json:"asset1"`
	Creator        string     `bson:"creator" json:"creator"`
	CreationHeight uint64     `bson:"creation_height" json:"creation_height"`
	LockedLPAmount uint64     `bson:"locked_lp_amount" json:"locked_lp_amount"`
	BondingMetric  uint64     `bson:"bonding_metric" json:"bonding_metric"`
	BondingTarget  uint64     `bson:"bonding_target" json:"bonding_target"`
	Status         PoolStatus `bson:"status" json:"status"` // Use PoolStatus type
	// Cross-chain support
	TargetChains []string          `bson:"target_chains" json:"target_chains"` // ["ethereum", "polygon", etc.]
	PairAccounts map[string]string `bson:"pair_accounts" json:"pair_accounts"` // chain -> account mapping
	CreatedAt    time.Time         `bson:"created_at" json:"created_at"`
	UpdatedAt    time.Time         `bson:"updated_at" json:"updated_at"`
}

type DexParams struct {
	PoolCreationFee       int64     `bson:"pool_creation_fee" json:"pool_creation_fee"`
	BondingCurveThreshold uint64    `bson:"bonding_curve_threshold" json:"bonding_curve_threshold"`
	UpdatedAt             time.Time `bson:"updated_at" json:"updated_at"`
}

type SwapParams struct {
	Sender         string  `json:"sender"`
	AmountIn       int64   `json:"amount_in"`
	AssetIn        string  `json:"asset_in"`
	AssetOut       string  `json:"asset_out"`
	MinAmountOut   int64   `json:"min_amount_out"`
	MaxSlippage    uint64  `json:"max_slippage"`     // basis points
	MiddleOutRatio float64 `json:"middle_out_ratio"` // 0.0-1.0, portion of slippage for HBD output check
	Beneficiary    string  `json:"beneficiary"`      // for referrals
	RefBps         uint64  `json:"ref_bps"`
}

type SwapResult struct {
	AmountOut    int64    `json:"amount_out"`
	HbdAmount    int64    `json:"hbd_amount"` // intermediate HBD amount
	Fee0         int64    `json:"fee0"`       // fee from first swap
	Fee1         int64    `json:"fee1"`       // fee from second swap
	Route        []string `json:"route"`      // chains used in swap ["hive", "ethereum"]
	Success      bool     `json:"success"`
	ErrorMessage string   `json:"error_message,omitempty"`
}

type PairBridgeAction struct {
	Id          string                 `bson:"id"`
	Status      BridgeActionStatus     `bson:"status"`    // Use BridgeActionStatus type
	PairId      string                 `bson:"pair_id"`   // "hbd-hive"
	Chain       string                 `bson:"chain"`     // "ethereum", "polygon", etc.
	Direction   BridgeDirection        `bson:"direction"` // Use BridgeDirection type
	Asset       string                 `bson:"asset"`
	Amount      int64                  `bson:"amount"`
	From        string                 `bson:"from,omitempty"`
	To          string                 `bson:"to,omitempty"`
	Memo        string                 `bson:"memo,omitempty"`
	BlockHeight uint64                 `bson:"block_height"`
	TxId        string                 `bson:"tx_id,omitempty"`
	Params      map[string]interface{} `bson:"params,omitempty"`
	CreatedAt   time.Time              `bson:"created_at"`
	UpdatedAt   time.Time              `bson:"updated_at"`
}
