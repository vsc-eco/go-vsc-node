package stateEngine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode/utf8"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/common"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	contract_session "vsc-node/modules/contract/session"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"

	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/transactions"
	ledgerSystem "vsc-node/modules/ledger-system"
	rcSystem "vsc-node/modules/rc-system"
	transactionpool "vsc-node/modules/transaction-pool"
	wasm_context "vsc-node/modules/wasm/context"

	blocks "github.com/ipfs/go-block-format"
	"go.mongodb.org/mongo-driver/mongo"

	"github.com/ipfs/go-cid"
	dagCbor "github.com/ipfs/go-ipld-cbor"
	mh "github.com/multiformats/go-multihash"
)

type CustomJson struct {
	Id                   string   `json:"id"`
	RequiredAuths        []string `json:"required_auths"`
	RequiredPostingAuths []string `json:"required_posting_auths"`
	Json                 []byte   `json:"json"`
}

type TxVscCallContract struct {
	Self  TxSelf
	NetId string `json:"net_id"`

	Caller     string             `json:"caller"`
	ContractId string             `json:"contract_id"`
	Action     string             `json:"action"`
	Payload    json.RawMessage    `json:"payload"`
	RcLimit    uint               `json:"rc_limit"`
	Intents    []contracts.Intent `json:"intents"`
}

func errorToTxResult(err error, RCs int64) TxResult {
	return TxResult{
		Success: false,
		Ret:     err.Error(),
		RcUsed:  RCs,
	}
}

// ExecuteTx implements VSCTransaction.
func (t TxVscCallContract) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, contractSession *contract_session.ContractSession, rcPayer string) TxResult {
	if t.NetId != common.NETWORK_ID {
		return errorToTxResult(fmt.Errorf("wrong net ID"), 100)
	}
	info, err := se.contractDb.ContractById(t.ContractId)

	if err == mongo.ErrNoDocuments {
		return errorToTxResult(fmt.Errorf("contract not found"), 100)
	}
	if err != nil {
		return errorToTxResult(err, 100)
	}

	c, err := cid.Decode(info.Code)
	if err != nil {
		return errorToTxResult(err, 100)
	}

	node, err := se.da.Get(c, nil)
	if err != nil {
		return errorToTxResult(err, 100)
	}

	code := node.RawData()

	hasMinRCs, availableGas, _ := rcSession.CanConsume(rcPayer, t.Self.BlockHeight, 100)

	if !hasMinRCs {
		return errorToTxResult(fmt.Errorf("minimum RC requirement is not met. RCs available: %d", availableGas), 100)
	}

	gas := min(uint(availableGas), t.RcLimit)

	ss := contractSession.GetStateStore()

	var caller string = t.Caller

	w := wasm_runtime_ipc.New()
	w.Init()

	metadata := contractSession.GetMetadata()

	fmt.Println("Contract Metadata", metadata)
	ctxValue := contract_execution_context.New(contract_execution_context.Environment{
		ContractId:           t.ContractId,
		BlockHeight:          t.Self.BlockHeight,
		TxId:                 t.Self.TxId,
		BlockId:              t.Self.BlockId,
		Index:                t.Self.Index,
		OpIndex:              t.Self.OpIndex,
		Timestamp:            t.Self.Timestamp,
		RequiredAuths:        t.Self.RequiredAuths,
		RequiredPostingAuths: t.Self.RequiredPostingAuths,
		Caller:               caller,
	}, int64(gas), t.Intents, ledgerSession, ss, metadata)

	validUtf8 := utf8.Valid(t.Payload)
	if !validUtf8 {
		return errorToTxResult(fmt.Errorf("payload does not parse to a UTF-8 string"), 100)
	}

	payload := string(t.Payload)

	// this will pass in unescaped string to the contract if the payload
	// is a JSON string, and so, in the case of an error (i.e. not a JSON
	// string), `payload` will be untouched and errors can be ignored
	json.Unmarshal([]byte(t.Payload), &payload)

	wasmCtx := context.WithValue(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue), wasm_context.WasmExecCodeCtxKey, hex.EncodeToString(code))

	res := w.Execute(wasmCtx, gas*common.CYCLE_GAS_PER_RC, t.Action, payload, info.Runtime)

	if res.Error != nil {
		fmt.Println("WASM execution error:", *res.Error)

		return TxResult{
			Success: false,
			Ret:     *res.Error,
			RcUsed:  10,
		}
	}
	fmt.Println("basicResult:", res)

	contractSession.SetMetadata(ctxValue.InternalStorage())

	// res := result.MapOrElse(
	// 	se.wasm.Execute(gas*common.CYCLE_GAS_PER_RC, t.Action, payload, info.Runtime),
	// 	func(err error) TxResult {
	// 		return errorToTxResult(err, int64(gas))
	// 	},
	// 	func(res wasm_types.WasmResultStruct) TxResult {
	// 		contractSession.SetMetadata(ctx.InternalStorage())
	// 		return TxResult{
	// 			Success: !res.Error,
	// 			Ret:     res.Result,
	// 			RcUsed:  int64(math.Ceil(float64(res.Gas) / common.CYCLE_GAS_PER_RC)),
	// 		}
	// 	},
	// )

	// if !res.Success {
	// 	ctx.Revert()
	// }

	ss.Commit()

	rcUsed := int64(math.Max(math.Ceil(float64(res.Result.Gas)/common.CYCLE_GAS_PER_RC), 100))

	fmt.Println("rcUsed", rcUsed)
	return TxResult{
		Success: true,
		Ret:     res.Result.Result,
		RcUsed:  rcUsed,
	}
}

func (tx TxVscCallContract) Type() string {
	return "call_contract"
}

func (tx TxVscCallContract) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxVscCallContract) ToData() map[string]interface{} {
	return map[string]interface{}{
		"type":        "call",
		"contract_id": tx.ContractId,
		"action":      tx.Action,
		"payload":     tx.Payload,
		"rc_limit":    tx.RcLimit,
		"intents":     tx.Intents,
	}
}

type TxDeposit struct {
	Self   TxSelf
	From   string `json:"from"`
	To     string `json:"to"`
	Amount int64  `json:"amount"`
	Asset  string `json:"asset"`
	Memo   string `json:"memo"`
}

func (tx TxDeposit) Type() string {
	return "deposit"
}

func (tx TxDeposit) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxDeposit) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, contractSession *contract_session.ContractSession, rcPayer string) TxResult {
	return TxResult{
		Success: true,
		RcUsed:  0,
	}
}

func (tx TxDeposit) ToData() map[string]interface{} {
	return map[string]interface{}{
		"from":   tx.From,
		"to":     tx.To,
		"amount": tx.Amount,
		"asset":  tx.Asset,
		"memo":   tx.Memo,
	}
}

func (tx TxDeposit) ToLedger() []ledgerSystem.OpLogEvent {
	return []ledgerSystem.OpLogEvent{
		{
			Id:     tx.Self.TxId,
			From:   tx.From,
			To:     tx.To,
			Amount: tx.Amount,
			Asset:  tx.Asset,
			Memo:   tx.Memo,
			Type:   "deposit",
		},
	}
}

// Costs no RCs as it consumes Hive RCs.
// Only applies if original transaction is Hive
type TxVSCTransfer struct {
	Self  TxSelf
	NetId string `json:"net_id"`

	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	Memo   string `json:"memo"`
}

func (tx TxVSCTransfer) Type() string {
	return "transfer"
}

func (tx TxVSCTransfer) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxVSCTransfer) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, contractSession *contract_session.ContractSession, rcPayer string) TxResult {
	if tx.NetId != common.NETWORK_ID {
		return TxResult{
			Success: false,
			Ret:     "Invalid network id",
			RcUsed:  50,
		}
	}
	if tx.To == "" || tx.From == "" {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	if (!strings.HasPrefix(tx.To, "did:") && !strings.HasPrefix(tx.To, "hive:")) || (!strings.HasPrefix(tx.From, "did:") && !strings.HasPrefix(tx.From, "hive:")) {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	if !slices.Contains(tx.Self.RequiredAuths, tx.From) {
		return TxResult{
			Success: false,
			Ret:     "Invalid RequiredAuths",
			RcUsed:  50,
		}
	}

	amount, err := common.SafeParseHiveFloat(tx.Amount)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Errorf("Invalid amount: %w", err).Error(),
			RcUsed:  50,
		}
	}

	transferParams := ledgerSystem.OpLogEvent{
		Id:          MakeTxId(tx.Self.TxId, tx.Self.OpIndex),
		BIdx:        int64(tx.Self.Index),
		OpIdx:       int64(tx.Self.OpIndex),
		From:        tx.From,
		To:          tx.To,
		Amount:      amount,
		Asset:       tx.Asset,
		Memo:        tx.Memo,
		BlockHeight: tx.Self.BlockHeight,
	}

	se.log.Debug("Transfer - tx.Self.BlockHeight", tx.Self.BlockHeight)

	ledgerResult := ledgerSession.ExecuteTransfer(transferParams)

	se.log.Debug("Transfer LedgerResult", ledgerResult)

	return TxResult{
		Success: ledgerResult.Ok,
		Ret:     ledgerResult.Msg,
		RcUsed:  100,
	}
}

func (tx TxVSCTransfer) ToData() map[string]interface{} {
	return map[string]interface{}{
		"from":   tx.From,
		"to":     tx.To,
		"amount": tx.Amount,
		"asset":  tx.Asset,
		"memo":   tx.Memo,
	}
}

type TxVSCWithdraw struct {
	Self  TxSelf
	NetId string `json:"net_id"`

	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	Memo   string `json:"memo"`
}

func (tx TxVSCWithdraw) Type() string {
	return "withdraw"
}

func (tx TxVSCWithdraw) TxSelf() TxSelf {
	return tx.Self
}

// Development note:
// t.From is a slightly different field from t.Self.RequiredAuths[0]
// It must exist this way for cosigned transaction support.
// Note: this function does the work of translating any and all VSC transactions to the ledger compatible formats
// ledgerExecutor will then do the heavy lifting of executing the input ops
// as LedgerExecutor may be called within other contexts, such as the contract executor
func (t *TxVSCWithdraw) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, contractSession *contract_session.ContractSession, rcPayer string) TxResult {
	if t.NetId != common.NETWORK_ID {
		return TxResult{
			Success: false,
			Ret:     "Invalid network id",
			RcUsed:  50,
		}
	}
	if t.To == "" {
		//Maybe default to self later?
		return TxResult{
			Success: false,
			Ret:     "Invalid to",
			RcUsed:  50,
		}
	}

	amount, err := common.SafeParseHiveFloat(t.Amount)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Errorf("Invalid amount: %w", err).Error(),
			RcUsed:  50,
		}
	}

	params := ledgerSystem.WithdrawParams{
		Id:          MakeTxId(t.Self.TxId, t.Self.OpIndex),
		BIdx:        int64(t.Self.Index),
		OpIdx:       int64(t.Self.OpIndex),
		To:          t.To,
		Asset:       t.Asset,
		Memo:        t.Memo,
		Amount:      amount,
		BlockHeight: t.Self.BlockHeight,
	}
	if t.From == "" {
		params.From = t.Self.RequiredAuths[0]
	} else {
		params.From = t.From
	}

	//Verifies
	if !slices.Contains(t.Self.RequiredAuths, t.From) {
		return TxResult{
			Success: false,
			Ret:     "Invalid RequiredAuths",
			RcUsed:  50,
		}
	}

	parameter, _ := json.Marshal(params)
	ledgerResult := ledgerSession.Withdraw(params)

	se.log.Debug("ExecuteTx Result", params, ledgerResult, string(parameter))
	return TxResult{
		Success: ledgerResult.Ok,
		Ret:     ledgerResult.Msg,
		RcUsed:  200,
	}
}

func (tx *TxVSCWithdraw) ToData() map[string]interface{} {
	return map[string]interface{}{
		"from":   tx.From,
		"to":     tx.To,
		"amount": tx.Amount,
		"asset":  tx.Asset,
		"memo":   tx.Memo,
	}
}

type TxStakeHbd struct {
	Self   TxSelf
	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	Typei  string `json:"type"`

	NetId string `json:"net_id"`
}

func (t *TxStakeHbd) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, contractSession *contract_session.ContractSession, rcPayer string) TxResult {
	if t.NetId != common.NETWORK_ID {
		return TxResult{
			Success: false,
			Ret:     "Invalid network id",
		}
	}
	if t.To == "" || t.From == "" {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	amount, err := common.SafeParseHiveFloat(t.Amount)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Errorf("Invalid amount: %w", err).Error(),
			RcUsed:  50,
		}
	}

	params := StakeOp{
		OpLogEvent: ledgerSystem.OpLogEvent{
			Id:          MakeTxId(t.Self.TxId, t.Self.OpIndex),
			To:          t.To,
			From:        t.From,
			Asset:       t.Asset,
			Amount:      amount,
			Memo:        "",
			BlockHeight: t.Self.BlockHeight,
		},
	}
	if t.From == "" {
		params.From = "hive:" + t.Self.RequiredAuths[0]
	} else {
		params.From = t.From
	}

	if !slices.Contains(t.Self.RequiredAuths, t.From) {
		return TxResult{
			Success: false,
			Ret:     "Invalid RequiredAuths",
			RcUsed:  50,
		}
	}
	ledgerResult := se.LedgerExecutor.Stake(params, ledgerSession)

	se.log.Debug("Stake LedgerResult", ledgerResult)
	return TxResult{
		Success: ledgerResult.Ok,
		Ret:     ledgerResult.Msg,
		RcUsed:  200,
	}
}

func (t *TxStakeHbd) ToData() map[string]interface{} {
	return map[string]interface{}{
		"from":   t.From,
		"to":     t.To,
		"amount": t.Amount,
		"asset":  t.Asset,
		"type":   t.Type(),
	}
}

func (t *TxStakeHbd) TxSelf() TxSelf {
	return t.Self
}

func (t *TxStakeHbd) Type() string {
	return "stake_hbd"
}

type TxUnstakeHbd struct {
	Self   TxSelf
	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	NetId  string `json:"net_id"`
}

func (t *TxUnstakeHbd) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, contractSession *contract_session.ContractSession, rcPayer string) TxResult {
	se.log.Debug("TxUnstakeHbd", t.Self.BlockHeight, t.Self.TxId, t.Self.OpIndex, t.NetId, common.NETWORK_ID)
	if t.NetId != common.NETWORK_ID {
		return TxResult{
			Success: false,
			Ret:     "Invalid network id",
			RcUsed:  50,
		}
	}
	if t.To == "" || t.From == "" {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	amount, err := common.SafeParseHiveFloat(t.Amount)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Errorf("Invalid amount: %w", err).Error(),
			RcUsed:  50,
		}
	}

	params := StakeOp{
		OpLogEvent: ledgerSystem.OpLogEvent{
			Id:          MakeTxId(t.Self.TxId, t.Self.OpIndex),
			To:          t.To,
			From:        t.From,
			Asset:       t.Asset,
			Amount:      amount,
			Memo:        "",
			BlockHeight: t.Self.BlockHeight,
			Type:        "unstake",
		},
	}
	if t.From == "" {
		params.From = "hive:" + t.Self.RequiredAuths[0]
	} else {
		params.From = t.From
	}

	if !slices.Contains(t.Self.RequiredAuths, t.From) {
		return TxResult{
			Success: false,
			Ret:     "Invalid RequiredAuths",
			RcUsed:  50,
		}
	}
	ledgerResult := se.LedgerExecutor.Unstake(params, ledgerSession)
	paramsJson, _ := json.Marshal(params)

	se.log.Debug("Unstake LedgerResult", ledgerResult, string(paramsJson))
	return TxResult{
		Success: ledgerResult.Ok,
		Ret:     ledgerResult.Msg,
		RcUsed:  200,
	}
}

func (t *TxUnstakeHbd) ToData() map[string]interface{} {
	return map[string]interface{}{
		"from":   t.From,
		"to":     t.To,
		"amount": t.Amount,
		"asset":  t.Asset,
		"type":   t.Type(),
	}
}

func (t *TxUnstakeHbd) TxSelf() TxSelf {
	return t.Self
}

func (t *TxUnstakeHbd) Type() string {
	return "unstake_hbd"
}

// type TxUnstakeHbd struct {
// 	Self   TxSelf
// 	From   string `json:"from"`
// 	To     string `json:"to"`
// 	Amount string `json:"amount"`
// 	Asset  string `json:"asset"`
// 	NetId  string `json:"net_id"`
// }

type TxConsensusStake struct {
	Self TxSelf

	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	NetId  string `json:"net_id"`
}

func (tx *TxConsensusStake) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, contractSession *contract_session.ContractSession, rcPayer string) TxResult {
	if tx.NetId != common.NETWORK_ID {
		return TxResult{
			Success: false,
			Ret:     "Invalid network id",
			RcUsed:  50,
		}
	}

	if tx.To == "" || tx.From == "" {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	if !slices.Contains(tx.Self.RequiredAuths, tx.From) {
		return TxResult{
			Success: false,
			Ret:     "Invalid RequiredAuths",
			RcUsed:  50,
		}
	}

	if (strings.HasPrefix(tx.To, "did:") || !strings.HasPrefix(tx.To, "hive:")) || (!strings.HasPrefix(tx.From, "did:") && !strings.HasPrefix(tx.From, "hive:")) {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	amount, err := common.SafeParseHiveFloat(tx.Amount)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Errorf("Invalid amount: %w", err).Error(),
			RcUsed:  50,
		}
	}

	params := ledgerSystem.ConsensusParams{
		Id:          MakeTxId(tx.Self.TxId, tx.Self.OpIndex),
		From:        tx.From,
		To:          tx.To,
		Amount:      amount,
		BlockHeight: tx.Self.BlockHeight,
		Type:        "stake",
	}

	ledgerResult := se.LedgerExecutor.ConsensusStake(params, ledgerSession)

	return TxResult{
		Success: ledgerResult.Ok,
		Ret:     ledgerResult.Msg,
		RcUsed:  100,
	}
}

func (t *TxConsensusStake) ToData() map[string]interface{} {
	return map[string]interface{}{
		"to":     t.To,
		"from":   t.From,
		"amount": t.Amount,
		"asset":  t.Asset,
		"type":   "consensus_stake",
	}
}

func (t *TxConsensusStake) TxSelf() TxSelf {
	return t.Self
}

func (t *TxConsensusStake) Type() string {
	return "consensus_stake"
}

type TxConsensusUnstake struct {
	Self TxSelf

	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	NetId  string `json:"net_id"`
}

func (tx *TxConsensusUnstake) ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, contractSession *contract_session.ContractSession, rcPayer string) TxResult {
	if tx.NetId != common.NETWORK_ID {
		return TxResult{
			Success: false,
			Ret:     "Invalid network id",
			RcUsed:  50,
		}
	}

	if tx.To == "" || tx.From == "" {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}
	if !slices.Contains(tx.Self.RequiredAuths, tx.From) {
		return TxResult{
			Success: false,
			Ret:     "Invalid RequiredAuths",
			RcUsed:  50,
		}
	}
	if (strings.HasPrefix(tx.To, "did:") && !strings.HasPrefix(tx.To, "hive:")) || (strings.HasPrefix(tx.From, "did:") || !strings.HasPrefix(tx.From, "hive:")) {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	amount, err := common.SafeParseHiveFloat(tx.Amount)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Errorf("Invalid amount: %w", err).Error(),
			RcUsed:  50,
		}
	}

	electionResult, _ := se.electionDb.GetElectionByHeight(tx.Self.BlockHeight - 1)

	params := ledgerSystem.ConsensusParams{
		Id:            MakeTxId(tx.Self.TxId, tx.Self.OpIndex),
		From:          tx.From,
		To:            tx.To,
		Amount:        amount,
		BlockHeight:   tx.Self.BlockHeight,
		Type:          "unstake",
		ElectionEpoch: electionResult.Epoch + 5,
	}
	ledgerResult := se.LedgerExecutor.ConsensusUnstake(params, ledgerSession)

	return TxResult{
		Success: ledgerResult.Ok,
		Ret:     ledgerResult.Msg,
		RcUsed:  100,
	}
}

func (t *TxConsensusUnstake) ToData() map[string]interface{} {
	return map[string]interface{}{
		"from":   t.From,
		"to":     t.To,
		"amount": t.Amount,
		"asset":  t.Asset,
		"type":   "consensus_unstake",
	}
}

func (t *TxConsensusUnstake) TxSelf() TxSelf {
	return t.Self
}

func (t *TxConsensusUnstake) Type() string {
	return "consensus_unstake"
}

type TransactionSig struct {
	Type string       `json:"__t"`
	Sigs []common.Sig `json:"sigs"`
}

type TransactionHeader struct {
	NetId         string   `json:"net_id"`
	Nonce         uint64   `json:"nonce"`
	RequiredAuths []string `json:"required_auths" jsonschema:"required"`
	RcLimit       uint64   `json:"rc_limit"`
}

// 0: Object
// 		id: "bafyreiclsfy6wld6otvy5djlvd7cu6ewyxutw26lmppzgka5odofy32liu"
// 		type: 2
// 		contract_id: "vs41q9c3ygynfp6kl86qnlaswuwvam748s5lvugns5schg4hte5vhusnx7sg5u8falrt"
// 	1: Object
// 		id: "bafyreifteiviq2ioxbnjbo6vszjjiaqxob3wdra3ve5x7qjbsmz3hnoxga"
// 		data: "iIzpEWkns0Ov47wnPg0KYYTCnB1YvKCajIXdePEDWoI"
// 		type: 5
// 		chain: "hive"

type TransactionContainer struct {
	Self TxSelf

	da *datalayer.DataLayer

	//Guaranteed fields
	Id      string `json:"string"`
	TypeInt int    `json:"type"`

	Obj      map[string]interface{}
	RawBytes []byte
}

func (tx *TransactionContainer) Type() string {
	if tx.TypeInt == int(common.BlockTypeTransaction) {
		return "transaction"
	} else if tx.TypeInt == int(common.BlockTypeOutput) {
		return "output"
	} else if tx.TypeInt == int(common.BlockTypeAnchor) {
		return "anchor"
	} else if tx.TypeInt == int(common.BlockTypeOplog) {
		return "oplog"
	} else if tx.TypeInt == int(common.BlockTypeRcUpdate) {
		return "rc_update"
	} else {
		return "unknown"
	}
}

// Converts to Contract Output
func (tx *TransactionContainer) AsContractOutput() *ContractOutput {
	output := ContractOutput{
		Id: tx.Id,
	}
	txCid := cid.MustParse(tx.Id)
	dag, _ := tx.da.GetDag(txCid)

	bJson, _ := dag.MarshalJSON()

	// fmt.Println("Marshelled JSON from contract output", string(bJson))
	json.Unmarshal(bJson, &output)

	return &output
}

// As a regular VSC transaction
func (tx *TransactionContainer) AsTransaction() *OffchainTransaction {
	txCid := cid.MustParse(tx.Id)
	dag, _ := tx.da.GetDag(txCid)

	bJson, _ := dag.MarshalJSON()

	offchainTx := OffchainTransaction{
		TxId: tx.Id,
		Self: TxSelf{
			TxId:        tx.Id,
			BlockHeight: tx.Self.BlockHeight,
		},
	}
	json.Unmarshal(bJson, &offchainTx)

	// b64Bytes, _ := base64.StdEncoding.DecodeString(offchainTx.Tx["payload"].(string))

	// node, _ := cbornode.Decode(b64Bytes, mh.SHA2_256, -1)
	// bbytes, _ := node.MarshalJSON()
	// var txPayload map[string]interface{}
	// json.Unmarshal(bbytes, &txPayload)

	// offchainTx.Tx = map[string]interface{}{
	// 	"type":    offchainTx.Tx["op"],
	// 	"payload": txPayload,
	// }

	return &offchainTx
}

// Hive anchor containing merkle root, list of hive txs
// Consider deprecating from protocol
func (tx *TransactionContainer) AsHiveAnchor() {

}

func (tx *TransactionContainer) AsOplog(endBlock uint64) Oplog {
	cid := cid.MustParse(tx.Id)
	node, err := tx.da.GetDag(cid)

	if err != nil {
		panic(err)
	}
	jsonBytes, _ := node.MarshalJSON()
	// fmt.Println("Oplog node", node, string(jsonBytes))

	oplog := Oplog{
		Self: tx.Self,

		EndBlock: endBlock,
	}
	json.Unmarshal(jsonBytes, &oplog)
	// fmt.Println("Oplog decoded", oplog)

	return oplog
}

func (tx *TransactionContainer) Decode(bytes []byte) {

}

type Oplog struct {
	Self TxSelf

	LedgerOps []ledgerSystem.OpLogEvent `json:"ledger"`
	Outputs   []OplogOutputEntry        `json:"outputs"`

	EndBlock uint64 `json:"-"`
}

func (oplog *Oplog) ExecuteTx(se *StateEngine) {
	se.LedgerExecutor.Flush()
	se.Flush()

	aoplog := make([]ledgerSystem.OpLogEvent, 0)
	for _, v := range oplog.LedgerOps {
		v.BlockHeight = oplog.Self.BlockHeight
		aoplog = append(aoplog, v)
	}

	vscBlock, _ := se.vscBlocks.GetBlockByHeight(oplog.EndBlock - 1)

	startBlock := uint64(0)
	if vscBlock != nil {
		//Need to confirm the slot height here
		startBlock = uint64(vscBlock.EndBlock)
	}

	// se.log.Debug("Execute Oplog", oplog.EndBlock)
	se.LedgerExecutor.Ls.IngestOplog(aoplog, OplogInjestOptions{
		EndHeight:   oplog.EndBlock,
		StartHeight: startBlock,
	})

	for i, v := range oplog.Outputs {
		ledgerOps := make([]ledgerSystem.OpLogEvent, 0)
		for _, v2 := range v.LedgerIdx {
			ledgerOps = append(ledgerOps, oplog.LedgerOps[v2])
		}
		status := transactions.TransactionStatusConfirmed
		if !v.Ok {
			status = transactions.TransactionStatusFailed
		}
		se.txDb.SetOutput(transactions.SetResultUpdate{
			Id:     v.Id,
			OpIdx:  i,
			Ledger: &ledgerOps,
			Status: &status,
		})
	}
}

type OffchainTransaction struct {
	Self TxSelf

	TxId string `json:"-"`

	transactionpool.VSCTransactionShell

	// DataType string `json:"__t" jsonschema:"required"`
	// Version  string `json:"__v" jsonschema:"required"`

	// Headers TransactionHeader `json:"headers"`

	//This this can be any kind of object.
	// Tx []transactionpool.VSCTransactionOp `json:"tx"`
}

// Verify signature of vsc transaction
// Note: VSC uses a segratated witness format (segwit) for transaction signatures
// This eliminates malleability issues and grants flexibility
// Thus signatures are separately stored and serialized
// Segwit only applies to transactions generally speaking

// Note: Signatures are verified on a 1:1 pubKey:sig structure
// In other words, signatures must be sorted the same as requiredAuths sort.
// Only applicable for multisig TXs
func (tx *OffchainTransaction) Verify(txSig TransactionSig, nonce int) (bool, error) {
	//Do verification logic using Key DID and Ethereum DID

	blk, err := tx.ToBlock()
	if err != nil {
		return false, err
	}

	return common.VerifySignatures(tx.Headers.RequiredAuths, blk, txSig.Sigs)
}

func (tx *OffchainTransaction) Encode() ([]byte, error) {
	return common.EncodeDagCbor(tx)
}

func (tx *OffchainTransaction) Decode(rawData []byte) error {
	dagNode, _ := dagCbor.Decode(rawData, mh.SHA2_256, -1)

	bytes, err := dagNode.MarshalJSON()
	if err != nil {
		return err
	}
	return json.Unmarshal(bytes, tx)
}

func (tx *OffchainTransaction) ToBlock() (*blocks.BasicBlock, error) {
	encodedBytes, _ := common.EncodeDagCbor(tx)

	prefix := cid.Prefix{
		Version:  1,
		Codec:    cid.DagCBOR,
		MhType:   mh.SHA2_256,
		MhLength: -1,
	}

	cid, err := prefix.Sum(encodedBytes)

	if err != nil {
		return nil, err
	}

	blk, err := blocks.NewBlockWithCid(encodedBytes, cid)
	return blk, err
}

func (tx *OffchainTransaction) Cid() cid.Cid {
	txId := cid.MustParse(tx.TxId)
	return txId
}

func (tx *OffchainTransaction) Ingest(se *StateEngine, txSelf TxSelf) {
	anchoredHeight := txSelf.BlockHeight
	anchoredIndex := int64(txSelf.Index)
	// anchoredOpIdx := int64(txSelf.OpIndex)

	// data := make(map[string]interface{})
	txs := tx.ToTransaction()

	opList := make([]transactions.TransactionOperation, 0)

	for idx, v := range txs {
		op := transactions.TransactionOperation{
			RequiredAuths: txSelf.RequiredAuths,
			Type:          v.Type(),
			Data:          v.ToData(),
			Idx:           int64(idx),
		}

		opList = append(opList, op)
	}

	se.txDb.Ingest(transactions.IngestTransactionUpdate{
		Status:         "INCLUDED",
		Id:             tx.TxId,
		AnchoredIndex:  &anchoredIndex,
		AnchoredHeight: &anchoredHeight,
		AnchoredBlock:  &txSelf.BlockId,
		AnchoredId:     &txSelf.BlockId,
		Nonce:          tx.Headers.Nonce,
		RcLimit:        tx.Headers.RcLimit,
		RequiredAuths:  tx.Headers.RequiredAuths,
		Ops:            opList,
		//Transaction is a VSC transaction
		Type:   "vsc",
		Ledger: make([]ledgerSystem.OpLogEvent, 0),
	})

}

func (tx *OffchainTransaction) TxSelf() TxSelf {
	return tx.Self
}

func (tx *OffchainTransaction) ToTransaction() []VSCTransaction {
	self := tx.TxSelf()
	self.RequiredAuths = tx.Headers.RequiredAuths

	// fmt.Println("stakeTx tx.Tx[type].(string)", tx.Tx["type"].(string))

	output := make([]VSCTransaction, 0)
	for _, op := range tx.Tx {
		var vtx VSCTransaction
		switch op.Type {
		case "call":
			callTx := TxVscCallContract{
				Self:  self,
				NetId: tx.Headers.NetId,
			}
			transactionpool.DecodeTxCbor(op, &callTx)

			vtx = callTx
		case "transfer":
			transferTx := TxVSCTransfer{
				Self:  self,
				NetId: tx.Headers.NetId,
			}
			transactionpool.DecodeTxCbor(op, &transferTx)

			// bbytes, _ := json.Marshal(transferTx)
			// fmt.Println("Decoded transfer tx", string(bbytes), decodeErr)
			vtx = transferTx
		case "withdraw":
			withdrawTx := TxVSCWithdraw{
				Self:  self,
				NetId: tx.Headers.NetId,
			}
			transactionpool.DecodeTxCbor(op, &withdrawTx)

			vtx = &withdrawTx
		case "stake_hbd":
			stakeTx := TxStakeHbd{
				Self: self,

				NetId: tx.Headers.NetId,
			}

			transactionpool.DecodeTxCbor(op, &stakeTx)

			fmt.Println("stakeTx", stakeTx)
			vtx = &stakeTx
		case "unstake_hbd":
			stakeTx := TxStakeHbd{
				Self: self,

				NetId: tx.Headers.NetId,
			}

			transactionpool.DecodeTxCbor(op, &stakeTx)

			fmt.Println("stakeTx", stakeTx)
			vtx = &stakeTx
		}

		output = append(output, vtx)
	}

	return output
}

func (tx *OffchainTransaction) Type() string {

	return "offchain"
}

// var _ VSCTransaction = &TxElectionResult{}

// var _ VSCTransaction = &TxProposeBlock{}

// This would probably be the only one to be considered a tx, since we can apply pulling of balance for deployment
var _ VSCTransaction = &TxCreateContract{}

var _ VscTxContainer = &OffchainTransaction{}

type VSCTransaction interface {
	ExecuteTx(se *StateEngine, ledgerSession *LedgerSession, rcSession *rcSystem.RcSession, contractSession *contract_session.ContractSession, rcPayer string) TxResult
	TxSelf() TxSelf
	ToData() map[string]interface{}
	Type() string
}

type VscTxContainer interface {
	Type() string //Hive, offchain
	ToTransaction() []VSCTransaction
}
