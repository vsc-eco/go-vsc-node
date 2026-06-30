package state_engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/dids"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/params"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	contract_session "vsc-node/modules/contract/session"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"

	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/transactions"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
	pendulumsettlement "vsc-node/modules/incentive-pendulum/settlement"
	ledgerSystem "vsc-node/modules/ledger-system"
	rcSystem "vsc-node/modules/rc-system"
	transactionpool "vsc-node/modules/transaction-pool"
	wasm_context "vsc-node/modules/wasm/context"

	blocks "github.com/ipfs/go-block-format"

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
func (t TxVscCallContract) ExecuteTx(
	se common_types.StateEngine,
	ledgerSession ledgerSystem.LedgerSession,
	rcSession rcSystem.RcSession,
	callSession *contract_session.CallSession,
	rcPayer string,
) TxResult {
	if t.NetId != se.SystemConfig().NetId() {
		return errorToTxResult(fmt.Errorf("wrong net ID"), 100)
	}

	// review2 LOW #70/#110: the contract-call payload was only UTF-8
	// checked, with no explicit length cap — the node implicitly relied
	// on Hive's ~8KB custom_json limit. Enforce an explicit cap here,
	// before any contract fetch or execution, so the bound holds
	// deterministically on every node regardless of the ingest path.
	if len(t.Payload) > params.MAX_CONTRACT_PAYLOAD_SIZE {
		return errorToTxResult(
			fmt.Errorf("payload exceeds maximum size of %d bytes", params.MAX_CONTRACT_PAYLOAD_SIZE),
			100,
		)
	}

	info, exists := se.GetContractInfo(t.ContractId, t.Self.BlockHeight)

	if !exists {
		return errorToTxResult(fmt.Errorf("contract not found"), 100)
	}

	c, err := cid.Decode(info.Code)
	if err != nil {
		return errorToTxResult(err, 100)
	}

	node, err := se.DataLayer().Get(c, nil)
	if err != nil {
		return errorToTxResult(err, 100)
	}

	code := node.RawData()

	hasMinRCs, availableGas, _ := rcSession.CanConsume(rcPayer, t.Self.BlockHeight, 100)

	if !hasMinRCs {
		return errorToTxResult(fmt.Errorf("minimum RC requirement is not met. RCs available: %d", availableGas), 100)
	}

	gas := min(uint(availableGas), t.RcLimit)

	// Cap gas to prevent overflow when multiplied by CYCLE_GAS_PER_RC
	const maxGas = ^uint(0) / params.CYCLE_GAS_PER_RC
	if gas > maxGas {
		gas = maxGas
	}

	var caller string = t.Caller
	if caller == "" {
		if len(t.Self.RequiredAuths) > 0 {
			caller = t.Self.RequiredAuths[0]
		} else if len(t.Self.RequiredPostingAuths) > 0 {
			caller = t.Self.RequiredPostingAuths[0]
		}
	} else if !slices.Contains(t.Self.RequiredAuths, t.Caller) && !slices.Contains(t.Self.RequiredPostingAuths, t.Caller) {
		return errorToTxResult(fmt.Errorf("caller is not in required_auths or required_posting_auths"), 100)
	}

	w := wasm_runtime_ipc.New()
	w.Init()

	// ensure entrypoint contract is appended to outputs regardless of state access or logs
	callSession.GetContractSession(t.ContractId)

	ctxValue := contract_execution_context.New(contract_execution_context.Environment{
		ContractId:           t.ContractId,
		ContractOwner:        info.Owner,
		BlockHeight:          t.Self.BlockHeight,
		TxId:                 t.Self.TxId,
		BlockId:              t.Self.BlockId,
		Index:                t.Self.Index,
		OpIndex:              t.Self.OpIndex,
		Timestamp:            t.Self.Timestamp,
		RequiredAuths:        t.Self.RequiredAuths,
		RequiredPostingAuths: t.Self.RequiredPostingAuths,
		Caller:               caller,
		Sender:               caller,
		Intents:              t.Intents,
		PendulumOracle:       se.PendulumOracleEnv(),
	}, int64(gas), rcSystem.FreeRcRemaining(rcSession, rcPayer, t.Self.BlockHeight), gas*params.CYCLE_GAS_PER_RC, ledgerSession, callSession, 0,
		contract_execution_context.WithPendulumApplier(se.PendulumApplier()),
		// Gate try/catch inter-contract calls on the chain-active consensus version
		// (deterministic, height-addressable) so the new semantics activate only at
		// a coordinated version floor.
		contract_execution_context.WithTryCatch(
			se.ActiveConsensusVersion(t.Self.BlockHeight).MeetsConsensusMin(consensusversion.TryCatchICCVersion),
		),
	)

	validUtf8 := utf8.Valid(t.Payload)
	if !validUtf8 {
		return errorToTxResult(fmt.Errorf("payload does not parse to a UTF-8 string"), 100)
	}

	payload := string(t.Payload)

	// this will pass in unescaped string to the contract if the payload
	// is a JSON string, and so, in the case of an error (i.e. not a JSON
	// string), `payload` will be untouched and errors can be ignored
	json.Unmarshal([]byte(t.Payload), &payload)

	wasmCtx := context.WithValue(
		context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue),
		wasm_context.WasmExecCodeCtxKey,
		hex.EncodeToString(code),
	)

	res := w.Execute(wasmCtx, gas*params.CYCLE_GAS_PER_RC, t.Action, payload, info.Runtime)

	// Integer ceil-divide: (gas + denom − 1) / denom. uint64 stays well below
	// overflow for any realistic gas value (denom = 100_000). Floored at the
	// minimum 100 RC charge.
	rcUsed := int64((uint64(res.Gas) + params.CYCLE_GAS_PER_RC - 1) / params.CYCLE_GAS_PER_RC)
	if rcUsed < 100 {
		rcUsed = 100
	}

	if res.Error != nil {
		return TxResult{
			Success: false,
			Err:     &res.ErrorCode,
			Ret:     *res.Error,
			RcUsed:  rcUsed,
		}
	}

	return TxResult{
		Success: true,
		Ret:     res.Result,
		RcUsed:  rcUsed,
	}
}

func (tx TxVscCallContract) Type() string {
	return "call"
}

func (tx TxVscCallContract) TxSelf() TxSelf {
	return tx.Self
}

func (tx TxVscCallContract) ToData() map[string]interface{} {
	payload := string(tx.Payload)
	json.Unmarshal(tx.Payload, &payload)
	return map[string]interface{}{
		"contract_id": tx.ContractId,
		"action":      tx.Action,
		"payload":     payload,
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

func (tx TxDeposit) ExecuteTx(
	se common_types.StateEngine,
	ledgerSession ledgerSystem.LedgerSession,
	rcSession rcSystem.RcSession,
	callSession *contract_session.CallSession,
	rcPayer string,
) TxResult {
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

func (tx TxVSCTransfer) ExecuteTx(
	se common_types.StateEngine,
	ledgerSession ledgerSystem.LedgerSession,
	rcSession rcSystem.RcSession,
	callSession *contract_session.CallSession,
	rcPayer string,
) TxResult {
	if tx.NetId != se.SystemConfig().NetId() {
		return errorToTxResult(fmt.Errorf("wrong net ID"), 50)
	}
	if tx.To == "" || tx.From == "" {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	if (!strings.HasPrefix(tx.To, "did:") && !strings.HasPrefix(tx.To, "hive:")) ||
		(!strings.HasPrefix(tx.From, "did:") && !strings.HasPrefix(tx.From, "hive:")) {
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

	amount, err := common.ParseAssetAmount(tx.Amount, tx.Asset)

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

	log.Debug("Transfer", "blockHeight", tx.Self.BlockHeight)

	ledgerResult := ledgerSession.ExecuteTransfer(transferParams)

	log.Debug("Transfer LedgerResult", "result", ledgerResult)

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
func (t *TxVSCWithdraw) ExecuteTx(
	se common_types.StateEngine,
	ledgerSession ledgerSystem.LedgerSession,
	rcSession rcSystem.RcSession,
	callSession *contract_session.CallSession,
	rcPayer string,
) TxResult {
	if t.NetId != se.SystemConfig().NetId() {
		return errorToTxResult(fmt.Errorf("wrong net ID"), 50)
	}
	if t.To == "" {
		//Maybe default to self later?
		return TxResult{
			Success: false,
			Ret:     "Invalid to",
			RcUsed:  50,
		}
	}

	amount, err := common.ParseAssetAmount(t.Amount, t.Asset)

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
	if t.From != "" {
		params.From = t.From
	} else if len(t.Self.RequiredAuths) > 0 {
		params.From = t.Self.RequiredAuths[0]
	}

	//Verifies
	if t.From == "" || !slices.Contains(t.Self.RequiredAuths, t.From) {
		return TxResult{
			Success: false,
			Ret:     "Invalid RequiredAuths",
			RcUsed:  50,
		}
	}

	parameter, _ := json.Marshal(params)
	ledgerResult := ledgerSession.Withdraw(params)

	log.Debug("ExecuteTx Result", "params", params, "result", ledgerResult, "parameterJson", string(parameter))
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

func (t *TxStakeHbd) ExecuteTx(
	se common_types.StateEngine,
	ledgerSession ledgerSystem.LedgerSession,
	rcSession rcSystem.RcSession,
	callSession *contract_session.CallSession,
	rcPayer string,
) TxResult {
	if t.NetId != se.SystemConfig().NetId() {
		return errorToTxResult(fmt.Errorf("wrong net ID"), 50)
	}
	if t.To == "" {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	amount, err := common.ParseAssetAmount(t.Amount, t.Asset)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Errorf("Invalid amount: %w", err).Error(),
			RcUsed:  50,
		}
	}

	params := ledgerSystem.StakeOp{
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
	if t.From != "" {
		params.From = t.From
	} else if len(t.Self.RequiredAuths) > 0 {
		params.From = "hive:" + t.Self.RequiredAuths[0]
	}

	if t.From == "" || !slices.Contains(t.Self.RequiredAuths, t.From) {
		return TxResult{
			Success: false,
			Ret:     "Invalid RequiredAuths",
			RcUsed:  50,
		}
	}
	ledgerResult := ledgerSession.Stake(params)

	log.Debug("Stake LedgerResult", "result", ledgerResult)
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

func (t *TxUnstakeHbd) ExecuteTx(
	se common_types.StateEngine,
	ledgerSession ledgerSystem.LedgerSession,
	rcSession rcSystem.RcSession,
	callSession *contract_session.CallSession,
	rcPayer string,
) TxResult {
	if t.NetId != se.SystemConfig().NetId() {
		return errorToTxResult(fmt.Errorf("wrong net ID"), 50)
	}
	if t.To == "" {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	amount, err := common.ParseAssetAmount(t.Amount, t.Asset)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Errorf("Invalid amount: %w", err).Error(),
			RcUsed:  50,
		}
	}

	params := ledgerSystem.StakeOp{
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
	if t.From != "" {
		params.From = t.From
	} else if len(t.Self.RequiredAuths) > 0 {
		params.From = "hive:" + t.Self.RequiredAuths[0]
	}

	if t.From == "" || !slices.Contains(t.Self.RequiredAuths, t.From) {
		return TxResult{
			Success: false,
			Ret:     "Invalid RequiredAuths",
			RcUsed:  50,
		}
	}
	ledgerResult := ledgerSession.Unstake(params)
	paramsJson, _ := json.Marshal(params)

	log.Debug("Unstake LedgerResult", "result", ledgerResult, "paramsJson", string(paramsJson))
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
	}
}

func (t *TxUnstakeHbd) TxSelf() TxSelf {
	return t.Self
}

func (t *TxUnstakeHbd) Type() string {
	return "unstake_hbd"
}


type TxConsensusStake struct {
	Self TxSelf `json:"-"`

	From   string `json:"from"`
	To     string `json:"to"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	NetId  string `json:"net_id"`
}

func (tx *TxConsensusStake) ExecuteTx(
	se common_types.StateEngine,
	ledgerSession ledgerSystem.LedgerSession,
	rcSession rcSystem.RcSession,
	callSession *contract_session.CallSession,
	rcPayer string,
) TxResult {
	if tx.NetId != se.SystemConfig().NetId() {
		return errorToTxResult(fmt.Errorf("wrong net ID"), 50)
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

	if !strings.HasPrefix(tx.To, "hive:") || !strings.HasPrefix(tx.From, "hive:") {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	// review2 MEDIUM #45: the asset field is part of the signed payload but
	// was never validated; the ledger always stakes "hive", so a tx claiming
	// asset "hbd"/"hbd_savings"/etc was silently treated as a HIVE stake.
	// Reject anything other than the empty default or explicit "hive".
	if tx.Asset != "" && tx.Asset != "hive" {
		return TxResult{
			Success: false,
			Ret:     "Invalid asset",
			RcUsed:  50,
		}
	}

	amount, err := common.ParseAssetAmount(tx.Amount, tx.Asset)

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

	ledgerResult := ledgerSession.ConsensusStake(params)

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

func (tx *TxConsensusUnstake) ExecuteTx(
	se common_types.StateEngine,
	ledgerSession ledgerSystem.LedgerSession,
	rcSession rcSystem.RcSession,
	callSession *contract_session.CallSession,
	rcPayer string,
) TxResult {
	if tx.NetId != se.SystemConfig().NetId() {
		return errorToTxResult(fmt.Errorf("wrong net ID"), 50)
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
	if !strings.HasPrefix(tx.To, "hive:") || !strings.HasPrefix(tx.From, "hive:") {
		return TxResult{
			Success: false,
			Ret:     "Invalid to/from",
			RcUsed:  50,
		}
	}

	// review2 MEDIUM #45: see TxConsensusStake — the asset field was never
	// validated; the ledger always unstakes "hive". Reject anything other
	// than the empty default or explicit "hive".
	if tx.Asset != "" && tx.Asset != "hive" {
		return TxResult{
			Success: false,
			Ret:     "Invalid asset",
			RcUsed:  50,
		}
	}

	amount, err := common.ParseAssetAmount(tx.Amount, tx.Asset)

	if err != nil {
		return TxResult{
			Success: false,
			Ret:     fmt.Errorf("Invalid amount: %w", err).Error(),
			RcUsed:  50,
		}
	}

	// review4 HIGH #96 (fail-stop): the unstake lock epoch comes from an
	// election read. The prior read swallowed the DB error and returned a
	// zero-value epoch, so a transient failure on one node would lock the
	// unstake to epoch 5 (instant unlock — bypassing the 5-epoch hold) while
	// peers locked it correctly: a fork plus a fund bug. Route through the
	// fail-stop reader instead. An infra error blocks until the DB recovers
	// (no per-node divergence). A deterministic "no election yet"
	// (pre-genesis) cleanly refuses the tx — every node agrees it's absent.
	// A genuine epoch-0 election is accepted normally (locks to epoch 5).
	electionResult, found := se.GetElectionInfoOrBlock(tx.Self.BlockHeight)
	if !found {
		return TxResult{
			Success: false,
			Ret:     "election lookup unavailable; retry unstake later",
			RcUsed:  50,
		}
	}

	params := ledgerSystem.ConsensusParams{
		Id:            MakeTxId(tx.Self.TxId, tx.Self.OpIndex),
		From:          tx.From,
		To:            tx.To,
		Amount:        amount,
		BlockHeight:   tx.Self.BlockHeight,
		Type:          "unstake",
		ElectionEpoch: electionResult.Epoch + 5,
	}
	ledgerResult := ledgerSession.ConsensusUnstake(params)

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
	} else if tx.TypeInt == int(common.BlockTypePendulumSettlement) {
		return "pendulum_settlement"
	} else if tx.TypeInt == int(common.BlockTypeSafetySlashReverse) {
		return "safety_slash_reverse"
	} else {
		return "unknown"
	}
}

// getDagOrBlock fetches the DAG node for a CID carried in a VSC block,
// fail-stopping (blocking with capped backoff) on a fetch error instead of
// handing it to a caller that would skip the entry. Every CID processed in the
// block path is anchored in a 2/3-BLS-attested block, so its bytes are
// guaranteed to propagate from the producer: a GetDag error here is a transient
// local I/O fault, never missing or malformed payload. Returning the error so
// the caller skips the entry would let a node with a momentary fault apply a
// different op set than its peers and silently fork its ledger. Blocking until
// the fetch succeeds keeps every honest node on the identical op set — the same
// fail-stop contract as blockingRetry's DB readers. The returned node is always
// non-nil. (A genuinely undecodable payload is deterministic and identical on
// every node, so it would block all nodes alike — a network-wide fail-stop, not
// a fork.)
func getDagOrBlock(da *datalayer.DataLayer, c cid.Cid, what string) *dagCbor.Node {
	var node *dagCbor.Node
	blockingRetry(what, func() error {
		var err error
		node, err = da.GetDag(c)
		if err != nil {
			return err
		}
		if node == nil {
			return fmt.Errorf("GetDag returned nil node for %s", c)
		}
		return nil
	})
	return node
}

// Converts to Contract Output
func (tx *TransactionContainer) AsContractOutput() (*ContractOutput, error) {
	txCid, err := cid.Parse(tx.Id)
	if err != nil {
		return nil, fmt.Errorf("invalid output CID %q: %w", tx.Id, err)
	}
	dag := getDagOrBlock(tx.da, txCid, fmt.Sprintf("GetDag(output %s)", tx.Id))
	bJson, err := dag.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("MarshalJSON failed for output %s: %w", tx.Id, err)
	}
	output := ContractOutput{Id: tx.Id}
	if err := json.Unmarshal(bJson, &output); err != nil {
		return nil, fmt.Errorf("Unmarshal failed for output %s: %w", tx.Id, err)
	}
	return &output, nil
}

// As a regular VSC transaction
func (tx *TransactionContainer) AsTransaction() (*OffchainTransaction, error) {
	txCid, err := cid.Parse(tx.Id)
	if err != nil {
		return nil, fmt.Errorf("invalid tx CID %q: %w", tx.Id, err)
	}
	dag := getDagOrBlock(tx.da, txCid, fmt.Sprintf("GetDag(tx %s)", tx.Id))
	bJson, err := dag.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("MarshalJSON failed for tx %s: %w", tx.Id, err)
	}
	offchainTx := OffchainTransaction{
		TxId: tx.Id,
		Self: tx.Self,
	}
	if err := json.Unmarshal(bJson, &offchainTx); err != nil {
		return nil, fmt.Errorf("Unmarshal failed for tx %s: %w", tx.Id, err)
	}
	return &offchainTx, nil
}

func (tx *TransactionContainer) AsOplog(endBlock uint64) (Oplog, error) {
	txCid, err := cid.Parse(tx.Id)
	if err != nil {
		return Oplog{Self: tx.Self, EndBlock: endBlock}, fmt.Errorf("invalid oplog CID %q: %w", tx.Id, err)
	}
	node := getDagOrBlock(tx.da, txCid, fmt.Sprintf("GetDag(oplog %s)", tx.Id))
	jsonBytes, err := node.MarshalJSON()
	if err != nil {
		return Oplog{Self: tx.Self, EndBlock: endBlock}, fmt.Errorf("MarshalJSON failed for oplog %s: %w", tx.Id, err)
	}
	oplog := Oplog{
		Self:     tx.Self,
		EndBlock: endBlock,
	}
	if err := json.Unmarshal(jsonBytes, &oplog); err != nil {
		return Oplog{Self: tx.Self, EndBlock: endBlock}, fmt.Errorf("Unmarshal failed for oplog %s: %w", tx.Id, err)
	}
	return oplog, nil
}

// AsPendulumSettlement decodes the SettlementRecord pointed at by this
// container's CID. Returns (record, true) on success; (zero, false) only on a
// deterministic decode failure (unparseable CID or malformed payload) that
// every honest node skips alike. A transient DAG fetch fault is a local I/O
// issue, not missing bytes (the carrying block had its CID validated by 2/3 BLS
// aggregation); getDagOrBlock blocks on it rather than dropping the settlement,
// so a momentary fault can't fork this node's ledger.
func (tx *TransactionContainer) AsPendulumSettlement() (pendulumsettlement.SettlementRecord, bool) {
	if tx == nil || tx.da == nil {
		return pendulumsettlement.SettlementRecord{}, false
	}
	txCid, err := cid.Parse(tx.Id)
	if err != nil {
		return pendulumsettlement.SettlementRecord{}, false
	}
	node := getDagOrBlock(tx.da, txCid, fmt.Sprintf("GetDag(pendulum-settlement %s)", tx.Id))
	jsonBytes, err := node.MarshalJSON()
	if err != nil {
		return pendulumsettlement.SettlementRecord{}, false
	}
	var rec pendulumsettlement.SettlementRecord
	if err := json.Unmarshal(jsonBytes, &rec); err != nil {
		return pendulumsettlement.SettlementRecord{}, false
	}
	return rec, true
}

// AsSafetySlashReverse decodes a SafetySlashReverseRecord pointed at by
// this container's CID. Same I/O-vs-malformed-payload story as the other
// As* helpers: deterministic decode failures return false (every node skips
// alike); a transient DAG fetch fault blocks via getDagOrBlock rather than
// skipping, so it can't fork this node's ledger.
func (tx *TransactionContainer) AsSafetySlashReverse() (safetyslash.SafetySlashReverseRecord, bool) {
	if tx == nil || tx.da == nil {
		return safetyslash.SafetySlashReverseRecord{}, false
	}
	txCid, err := cid.Parse(tx.Id)
	if err != nil {
		return safetyslash.SafetySlashReverseRecord{}, false
	}
	node := getDagOrBlock(tx.da, txCid, fmt.Sprintf("GetDag(safetyslash-reverse %s)", tx.Id))
	jsonBytes, err := node.MarshalJSON()
	if err != nil {
		return safetyslash.SafetySlashReverseRecord{}, false
	}
	var rec safetyslash.SafetySlashReverseRecord
	if err := json.Unmarshal(jsonBytes, &rec); err != nil {
		return safetyslash.SafetySlashReverseRecord{}, false
	}
	return rec.Normalize(), true
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
	se.LedgerState.Flush()
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
	se.LedgerSystem.IngestOplog(aoplog, ledgerSystem.OplogInjestOptions{
		EndHeight:   oplog.EndBlock,
		StartHeight: startBlock,
	})

	for _, v := range oplog.Outputs {
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

	didBufs := make([]dids.DID, len(tx.Headers.RequiredAuths))
	for i, auth := range tx.Headers.RequiredAuths {
		didBufs[i], err = dids.Parse(auth)
		if err != nil {
			return false, err
		}
	}

	return common.VerifySignatures(didBufs, blk, txSig.Sigs)
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

func (tx *OffchainTransaction) Ingest(se *StateEngine, vscBlockTxId string, txSelf TxSelf) {
	anchoredHeight := txSelf.BlockHeight
	anchoredIndex := int64(txSelf.Index)
	// anchoredOpIdx := int64(txSelf.OpIndex)

	// data := make(map[string]interface{})
	// An invalid tx (err != nil) is still recorded — with no ops — so it is
	// queryable and reaches a terminal status. It is marked FAILED later via the
	// oplog round-trip driven by ExecuteBatch's TxPacket.Invalid branch, exactly
	// as every other tx's status is finalized. Resolving zero ops here never
	// means "success": empty Ops on an invalid tx ride the FAILED path.
	txs, _ := tx.ToTransaction()

	opTypes := make([]string, 0)
	opTypesM := make(map[string]bool, 0)
	opList := make([]transactions.TransactionOperation, 0)

	for idx, v := range txs {
		// Defense-in-depth: ToTransaction never emits a nil VSCTransaction (it
		// returns an error instead), but guard here too — v.Type() on a nil
		// interface is the panic that halted every node in the devnet PoC.
		if v == nil {
			continue
		}
		op := transactions.TransactionOperation{
			Type: v.Type(),
			Data: v.ToData(),
			Idx:  int64(idx),
		}

		opTypesM[op.Type] = true
		opList = append(opList, op)
	}
	for opType := range opTypesM {
		opTypes = append(opTypes, opType)
	}

	se.txDb.Ingest(transactions.IngestTransactionUpdate{
		Status:         "INCLUDED",
		Id:             tx.TxId,
		AnchoredIndex:  &anchoredIndex,
		AnchoredHeight: &anchoredHeight,
		AnchoredBlock:  &txSelf.BlockId,
		AnchoredId:     &vscBlockTxId,
		Nonce:          tx.Headers.Nonce,
		RcLimit:        tx.Headers.RcLimit,
		RequiredAuths:  tx.Headers.RequiredAuths,
		OpTypes:        opTypes,
		Ops:            opList,
		//Transaction is a VSC transaction
		Type:   "vsc",
		Ledger: make([]ledgerSystem.OpLogEvent, 0),
	})

}

func (tx *OffchainTransaction) TxSelf() TxSelf {
	return tx.Self
}

// ToTransaction resolves the offchain op list into executable VSCTransactions.
// It returns an error the moment any op cannot be executed exactly as submitted
// — an unknown op.Type, or an F4/F21 CBOR decode failure on the (fully
// attacker-controlled) op.Payload. A non-nil error means the ENTIRE transaction
// must fail: callers record it FAILED with no op executed (see TxPacket.Invalid
// and the early-fail branch in ExecuteBatch) rather than partially applying it
// or — the old bug — silently dropping the op and reporting a CONFIRMED no-op.
//
// Determinism: invalidity is a pure function of the content-addressed tx bytes
// (op.Type string match + CBOR decodability), so every node either resolves the
// identical op list or fails the identical tx, producing the same block CID.
func (tx *OffchainTransaction) ToTransaction() ([]VSCTransaction, error) {
	self := tx.TxSelf()
	self.RequiredAuths = tx.Headers.RequiredAuths

	output := make([]VSCTransaction, 0)
	for idx, op := range tx.Tx {
		var vtx VSCTransaction
		switch op.Type {
		case "call":
			callTx := TxVscCallContract{
				Self:  self,
				NetId: tx.Headers.NetId,
			}
			callTx.Self.OpIndex = idx
			if err := transactionpool.DecodeTxCbor(op, &callTx); err != nil {
				return nil, fmt.Errorf("op %d (%q): %w", idx, op.Type, err)
			}

			vtx = callTx
		case "transfer":
			transferTx := TxVSCTransfer{
				Self:  self,
				NetId: tx.Headers.NetId,
			}
			if err := transactionpool.DecodeTxCbor(op, &transferTx); err != nil {
				return nil, fmt.Errorf("op %d (%q): %w", idx, op.Type, err)
			}

			vtx = transferTx
		case "withdraw":
			withdrawTx := TxVSCWithdraw{
				Self:  self,
				NetId: tx.Headers.NetId,
			}
			if err := transactionpool.DecodeTxCbor(op, &withdrawTx); err != nil {
				return nil, fmt.Errorf("op %d (%q): %w", idx, op.Type, err)
			}

			vtx = &withdrawTx
		case "stake_hbd":
			stakeTx := TxStakeHbd{
				Self: self,

				NetId: tx.Headers.NetId,
			}

			if err := transactionpool.DecodeTxCbor(op, &stakeTx); err != nil {
				return nil, fmt.Errorf("op %d (%q): %w", idx, op.Type, err)
			}

			vtx = &stakeTx
		case "unstake_hbd":
			// NOTE: offchain unstake_hbd intentionally still builds TxStakeHbd
			// here (the 0.3.0 behavior — it STAKES instead of releasing). The
			// direction is wrong, but correcting it changes ledger state on a
			// VALID input and would fork live 0.3.0 nodes, so the fix (F14, build
			// TxUnstakeHbd) is deferred to the version-gated 0.4.0 rollout and
			// lives in its own commit.
			stakeTx := TxStakeHbd{
				Self:  self,
				NetId: tx.Headers.NetId,
			}

			if err := transactionpool.DecodeTxCbor(op, &stakeTx); err != nil {
				return nil, fmt.Errorf("op %d (%q): %w", idx, op.Type, err)
			}

			vtx = &stakeTx
		default:
			// F4: an unknown op.Type previously left vtx as a nil VSCTransaction
			// interface, dereferenced (v.Type()) in Ingest/ExecuteBatch and
			// panicking ProcessBlock on every ingesting node (unprivileged,
			// network-wide chain halt, devnet-confirmed). An unknown op cannot be
			// executed as submitted, so fail the entire tx deterministically.
			return nil, fmt.Errorf("op %d: unknown op type %q", idx, op.Type)
		}

		// Defense-in-depth: a known case that forgets to set vtx must still fail
		// the tx, never silently drop the op (a nil here is what halts the chain).
		if vtx == nil {
			return nil, fmt.Errorf("op %d (%q): nil transaction produced", idx, op.Type)
		}
		output = append(output, vtx)
	}

	return output, nil
}

func (tx *OffchainTransaction) Type() string {
	return "offchain"
}

var _ VscTxContainer = &OffchainTransaction{}

type VSCTransaction interface {
	ExecuteTx(
		se common_types.StateEngine,
		ledgerSession ledgerSystem.LedgerSession,
		rcSession rcSystem.RcSession,
		callSession *contract_session.CallSession,
		rcPayer string,
	) TxResult
	TxSelf() TxSelf
	ToData() map[string]interface{}
	Type() string
}

type VscTxContainer interface {
	Type() string //Hive, offchain
	ToTransaction() ([]VSCTransaction, error)
}
