package ledgerSystem

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type ledgerSession struct {
	state *LedgerState

	oplog     []OpLogEvent
	ledgerOps []LedgerUpdate
	balances  map[string]*int64
	idCache   map[string]int

	StartHeight uint64
}

func (session *ledgerSession) Done() []string {
	// oplog := make([]OpLogEvent, len(lss.oplog))
	// copy(oplog, lss.oplog)
	ledgerIds := make([]string, 0)
	for _, v := range session.oplog {
		ledgerIds = append(ledgerIds, v.Id)
	}

	session.state.Oplog = append(session.state.Oplog, session.oplog...)
	for _, op := range session.ledgerOps {
		// lss.le.Ls.log.Debug("LedgerSession.Done adding LedgerResult", op)
		session.state.VirtualLedger[op.Owner] = append(session.state.VirtualLedger[op.Owner], op)
	}
	session.balances = make(map[string]*int64)
	session.oplog = make([]OpLogEvent, 0)
	session.ledgerOps = make([]LedgerUpdate, 0)

	return ledgerIds
}

func (lss *ledgerSession) Revert() {
	lss.oplog = make([]OpLogEvent, 0)
	lss.ledgerOps = make([]LedgerUpdate, 0)
	lss.balances = make(map[string]*int64)
}

// Appends an Oplog with no validation
func (session *ledgerSession) AppendOplog(event OpLogEvent) {
	session.state.Validate()
	//Maybe this should be calculated upon indexing rather than before
	id2 := event.Id
	if session.idCache[id2] > 0 {
		event.Id = id2 + ":" + strconv.Itoa(session.idCache[id2])
	}
	// lss.le.Ls.log.Debug("AppendOplog event ID", event, lss.idCache[id2])
	session.idCache[id2]++

	// lss.le
	result := ExecuteOplog([]OpLogEvent{event}, session.StartHeight, event.BlockHeight)

	for _, v := range result.ledgerRecords {
		session.AppendLedger(v)
	}

	session.oplog = append(session.oplog, event)
}

func (lss *ledgerSession) Transfer() {
	//pass to LE
}

// Appends an ledger with no validation
func (session *ledgerSession) AppendLedger(event LedgerUpdate) {
	session.state.Validate()
	// lss.le.Ls.log.Debug("LedgerSession.AppendLedger GetBalance")
	bal := session.GetBalance(event.Owner, event.BlockHeight, event.Asset)
	session.setBalance(event.Owner, event.Asset, bal+event.Amount)

	session.ledgerOps = append(session.ledgerOps, event)
}

func (session *ledgerSession) GetBalance(account string, blockHeight uint64, asset string) int64 {
	session.state.Validate()
	if session.balances[session.key(account, asset)] == nil {
		bal := session.state.SnapshotForAccount(account, blockHeight, asset)
		session.balances[session.key(account, asset)] = &bal
	}

	return *session.balances[session.key(account, asset)]
}

func (lss *ledgerSession) setBalance(account string, asset string, amount int64) {
	lss.balances[lss.key(account, asset)] = &amount
}

func (lss *ledgerSession) key(account, asset string) string {
	return account + "#" + asset
}

func (ledgerSession *ledgerSession) Withdraw(withdraw WithdrawParams) LedgerResult {
	// le := ledgerSession.le
	if withdraw.Amount <= 0 {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	if !slices.Contains([]string{"hive", "hbd"}, withdraw.Asset) {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid asset",
		}
	}

	var dest string
	matchedHive, _ := regexp.MatchString(HIVE_REGEX, withdraw.To)

	if matchedHive && len(withdraw.To) >= 3 && len(withdraw.To) < 17 {
		dest = `hive:` + withdraw.To
	} else if strings.HasPrefix(withdraw.To, "hive:") {
		//No nothing. It's parsed correctly
		splitHive := strings.Split(withdraw.To, ":")[1]
		matchedHive, _ := regexp.MatchString(HIVE_REGEX, splitHive)
		if matchedHive && len(splitHive) >= 3 && len(splitHive) < 17 {
			dest = withdraw.To
		} else {
			return LedgerResult{
				Ok:  false,
				Msg: "invalid destination",
			}
		}
	} else {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid destination",
		}
	}

	balAmt := ledgerSession.GetBalance(withdraw.From, withdraw.BlockHeight, withdraw.Asset)

	// le.Ls.log.Debug("Withdraw - balAmt", balAmt, withdraw.Id)

	if balAmt < withdraw.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	ledgerSession.AppendOplog(OpLogEvent{
		Id:     withdraw.Id,
		From:   withdraw.From,
		To:     dest,
		Amount: withdraw.Amount,
		Asset:  withdraw.Asset,
		Memo:   withdraw.Memo,
		Type:   "withdraw",

		BIdx:  withdraw.BIdx,
		OpIdx: withdraw.OpIdx,
	})

	return LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

func (ledgerSession *ledgerSession) ConsensusStake(params ConsensusParams) LedgerResult {

	if params.Amount <= 0 {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	// if !slices.Contains(assetTypes, withdraw.Asset) {
	// 	return LedgerResult{
	// 		Ok:  false,
	// 		Msg: "Invalid asset",
	// 	}
	// }

	balAmt := ledgerSession.GetBalance(params.From, params.BlockHeight, "hive")

	if balAmt < params.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	ledgerSession.AppendOplog(OpLogEvent{
		Id:          params.Id,
		From:        params.From,
		To:          params.To,
		BlockHeight: params.BlockHeight,

		Amount: params.Amount,
		Asset:  "hive",
		Type:   "consensus_stake",
	})

	return LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

func (ledgerSession *ledgerSession) ConsensusUnstake(params ConsensusParams) LedgerResult {
	if params.Amount <= 0 {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	balAmt := ledgerSession.GetBalance(params.From, params.BlockHeight, "hive_consensus")

	if balAmt < params.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	ledgerSession.AppendOplog(OpLogEvent{
		Id:          params.Id,
		To:          params.To,
		From:        params.From,
		BlockHeight: params.BlockHeight,

		Amount: params.Amount,
		Asset:  "hive",
		Type:   "consensus_unstake",

		Params: map[string]interface{}{
			"epoch": params.ElectionEpoch,
		},
	})

	return LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

// HP_MIN_STAKE is the minimum amount of hive_consensus required to opt into HP staking (50,000 HIVE = 50_000_000 in ledger units)
const HP_MIN_STAKE = int64(50_000_000)

// CONSENSUS_MIN_STAKE is the minimum total stake (consensus + hp) to remain election-eligible.
// Matches params.MAINNET_CONSENSUS_MINIMUM (2,000 HIVE = 2,000,000 in ledger units).
const CONSENSUS_MIN_STAKE = int64(2_000_000)

func (ledgerSession *ledgerSession) OptInHP(params HPStakeParams) LedgerResult {
	if params.Amount <= 0 {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	if params.Amount < HP_MIN_STAKE {
		return LedgerResult{
			Ok:  false,
			Msg: "below minimum: 50,000 HIVE required for HP staking",
		}
	}

	consensusBal := ledgerSession.GetBalance(params.From, params.BlockHeight, "hive_consensus")

	if consensusBal < params.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	// NOTE: No separate MinStake check needed after conversion because:
	// 1. The election system (election-proposer.go) sums HIVE_CONSENSUS + HIVE_HP for eligibility
	// 2. Moving consensus → hp doesn't reduce total stake, just shifts it between asset types
	// 3. The consensus balance check above already prevents moving more than available

	ledgerSession.AppendOplog(OpLogEvent{
		Id:          params.Id,
		From:        params.From,
		To:          params.To,
		BlockHeight: params.BlockHeight,
		Amount:      params.Amount,
		Asset:       "hive",
		Type:        "hp_stake",
		Params: map[string]interface{}{
			"hive_account": params.HiveAccount,
		},
	})

	return LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

// HP_UNSTAKE_ENABLED controls whether opt-out is allowed. Set to false until
// Phase 5 (fill_vesting_withdraw virtual op handler) is implemented.
// Without Phase 5, opt-out debits hive_hp but has no mechanism to credit
// hive_consensus back from L1 power-down installments — funds would be lost.
const HP_UNSTAKE_ENABLED = false

func (ledgerSession *ledgerSession) OptOutHP(params HPStakeParams) LedgerResult {
	if !HP_UNSTAKE_ENABLED {
		return LedgerResult{
			Ok:  false,
			Msg: "hp unstake not yet enabled: power-down handler pending",
		}
	}

	if params.Amount <= 0 {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	hpBal := ledgerSession.GetBalance(params.From, params.BlockHeight, "hive_hp")

	if hpBal < params.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	ledgerSession.AppendOplog(OpLogEvent{
		Id:          params.Id,
		From:        params.From,
		To:          params.To,
		BlockHeight: params.BlockHeight,
		Amount:      params.Amount,
		Asset:       "hive",
		Type:        "hp_unstake",
		Params: map[string]interface{}{
			"epoch":        params.ElectionEpoch,
			"hive_account": params.HiveAccount,
		},
	})

	return LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

// ConfirmHP converts pending_hp -> hive_hp after gateway confirms L1 power-up.
// Timeout rollback: if pending_hp is not confirmed within HP_CONFIRM_TIMEOUT blocks,
// the pending_hp should be rolled back to hive_consensus (handled by a separate scheduled check).
const HP_CONFIRM_TIMEOUT = uint64(1200) // ~1 hour of Hive blocks

func (ledgerSession *ledgerSession) ConfirmHP(params HPStakeParams) LedgerResult {
	if params.Amount <= 0 {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	pendingBal := ledgerSession.GetBalance(params.From, params.BlockHeight, "pending_hp")

	if pendingBal < params.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "insufficient pending_hp balance",
		}
	}

	ledgerSession.AppendOplog(OpLogEvent{
		Id:          params.Id,
		From:        params.From,
		To:          params.From,
		BlockHeight: params.BlockHeight,
		Amount:      params.Amount,
		Asset:       "hive",
		Type:        "hp_confirm",
		Params: map[string]interface{}{
			"hive_account": params.HiveAccount,
		},
	})

	return LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

func (ledgerSession *ledgerSession) ExecuteTransfer(opLogEvent OpLogEvent, options ...TransferOptions) LedgerResult {
	// le := ledgerSession.le
	//Check if the from account has enough balance
	exclusion := int64(0)

	if len(options) > 0 {
		options[0].Exclusion = exclusion
	}

	if opLogEvent.Amount <= 0 {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}
	if opLogEvent.To == opLogEvent.From {
		return LedgerResult{
			Ok:  false,
			Msg: "cannot send to self",
		}
	}
	if strings.HasPrefix(opLogEvent.To, "system:") {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid destination",
		}
	}
	if !slices.Contains(transferableAssetTypes, opLogEvent.Asset) {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid asset",
		}
	}
	fromBal := ledgerSession.GetBalance(opLogEvent.From, opLogEvent.BlockHeight, opLogEvent.Asset)

	// le.Ls.log.Debug("Transfer - balAmt", fromBal, "bh="+strconv.Itoa(int(opLogEvent.BlockHeight)))
	// le.Ls.log.Debug("ledgerSession.StartHeight", ledgerSession.StartHeight, "OpLogEvent.BlockHeight", opLogEvent.BlockHeight)

	if (fromBal - exclusion) < opLogEvent.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}
	opLogEvent.Type = "transfer"

	ledgerSession.AppendOplog(opLogEvent)

	return LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

func (ledgerSession *ledgerSession) Stake(stakeOp StakeOp, options ...TransferOptions) LedgerResult {

	exclusion := int64(0)

	if len(options) > 0 {
		options[0].Exclusion = exclusion
	}

	//Cannot stake less than 0.002 HBD
	//As there is a 0.001 HBD fee for instant staking
	if stakeOp.Amount <= 0 || (stakeOp.Instant && stakeOp.Amount < 2) {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	if stakeOp.Asset != "hbd" {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid asset",
		}
	}

	fromBal := ledgerSession.GetBalance(stakeOp.From, stakeOp.BlockHeight, "hbd")
	// fromBal := le.SnapshotForAccount(stakeOp.From, stakeOp.BlockHeight, "hbd")

	// le.Ls.log.Debug("Stake - balAmt", fromBal, stakeOp.Id)

	if (exclusion + fromBal) < stakeOp.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	ledgerSession.AppendLedger(LedgerUpdate{
		Id:     stakeOp.Id,
		OpIdx:  0,
		Owner:  stakeOp.From,
		Amount: -stakeOp.Amount,
		Asset:  stakeOp.Asset,
		Type:   "stake",
		Memo:   stakeOp.Memo,
	})

	// le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], LedgerUpdate{
	// 	Id:     stakeOp.Id,
	// 	OpIdx:  0,
	// 	Owner:  stakeOp.From,
	// 	Amount: -stakeOp.Amount,
	// 	Asset:  stakeOp.Asset,
	// 	Type:   "stake",
	// 	Memo:   stakeOp.Memo,
	// })

	//VSC:
	// - BlockHeight
	// - BkIndex
	// - OpIndex
	// - LIdx

	// fee := int64(0)

	// if stakeOp.Instant {
	// 	fee = stakeOp.Amount * HBD_INSTANT_FEE / 100
	// 	if fee == 0 {
	// 		//Minimum of
	// 		fee = HBD_INSTANT_MIN
	// 	}
	// 	withdrawAmount := stakeOp.Amount - fee

	// 	ledgerSession.AppendLedger(LedgerUpdate{
	// 		Id:     stakeOp.Id + "#fee",
	// 		OpIdx:  0,
	// 		Owner:  HBD_FEE_RECEIVER,
	// 		Amount: fee,
	// 		Asset:  "hbd",
	// 		Type:   "fee",
	// 		Memo:   "HBD_INSTANT_FEE",
	// 	})
	// 	le.VirtualLedger[HBD_FEE_RECEIVER] = append(le.VirtualLedger[HBD_FEE_RECEIVER], LedgerUpdate{
	// 		Id:     stakeOp.Id + "#fee",
	// 		OpIdx:  0,
	// 		Owner:  HBD_FEE_RECEIVER,
	// 		Amount: fee,
	// 		Asset:  "hbd",
	// 		Type:   "fee",
	// 		Memo:   "HBD_INSTANT_FEE",
	// 	})
	// 	le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], LedgerUpdate{
	// 		Id:          stakeOp.Id,
	// 		BlockHeight: stakeOp.BlockHeight,
	// 		OpIdx:       0,
	// 		Owner:       stakeOp.To,
	// 		Amount:      withdrawAmount,
	// 		Asset:       "hbd_savings",
	// 		Type:        "stake",
	// 		Memo:        stakeOp.Memo,
	// 	})
	// }

	ledgerSession.AppendOplog(OpLogEvent{
		Id: stakeOp.Id,
		// Index: 1,

		From:   stakeOp.From,
		To:     stakeOp.To,
		Asset:  stakeOp.Asset,
		Amount: stakeOp.Amount,
		Memo:   stakeOp.Memo,
		Type:   "stake",

		// Params: map[string]interface{}{
		// 	"instant": stakeOp.Instant,
		// 	"fee":     fee,
		// },
	})

	return LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}

func (ledgerSession *ledgerSession) Unstake(stakeOp StakeOp) LedgerResult {
	//Cannot unstake less than 0.002 HBD
	//As there is a 0.001 HBD fee for instant staking
	if stakeOp.Amount <= 0 || (stakeOp.Instant && stakeOp.Amount < 2) {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid amount",
		}
	}

	if stakeOp.Asset != "hbd" && stakeOp.Asset != "hbd_savings" {
		return LedgerResult{
			Ok:  false,
			Msg: "invalid asset",
		}
	}

	fromBal := ledgerSession.GetBalance(stakeOp.From, stakeOp.BlockHeight, "hbd_savings")

	// le.Ls.log.Debug("Unstake - balAmt", fromBal, stakeOp.Amount)
	if fromBal < stakeOp.Amount {
		return LedgerResult{
			Ok:  false,
			Msg: "insufficient balance",
		}
	}

	// fee := int64(0)

	ledgerSession.AppendLedger(LedgerUpdate{
		Id:     stakeOp.Id,
		OpIdx:  0,
		Owner:  stakeOp.From,
		Amount: -stakeOp.Amount,
		Asset:  stakeOp.Asset,
		Type:   "stake",
		Memo:   stakeOp.Memo,
	})
	// le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], )

	// if stakeOp.Instant {
	// 	//withdrawAmount < currently available "safe" unstaked balance
	// 	//enter in: some of kind of state tracking for "safe" unstaked balance

	// 	fee = stakeOp.Amount * HBD_INSTANT_FEE / 100

	// 	if fee == 0 {
	// 		//Minimum of
	// 		fee = HBD_INSTANT_MIN
	// 	}
	// 	withdrawAmount := stakeOp.Amount - fee

	// 	le.VirtualLedger[HBD_FEE_RECEIVER] = append(le.VirtualLedger[HBD_FEE_RECEIVER], LedgerUpdate{
	// 		Id:          stakeOp.Id + "#fee",
	// 		BlockHeight: stakeOp.BlockHeight,
	// 		OpIdx:       0,

	// 		Owner:  HBD_FEE_RECEIVER,
	// 		Amount: fee,
	// 		Asset:  "hbd",
	// 		Type:   "fee",
	// 		Memo:   "HBD_INSTANT_FEE",
	// 	})

	// 	le.VirtualLedger[stakeOp.From] = append(le.VirtualLedger[stakeOp.From], LedgerUpdate{
	// 		Id:          stakeOp.Id + "-out",
	// 		BlockHeight: stakeOp.BlockHeight,
	// 		OpIdx:       0,

	// 		Owner:  stakeOp.To,
	// 		Amount: withdrawAmount,
	// 		Asset:  "hbd",
	// 		Type:   "stake",
	// 		Memo:   stakeOp.Memo,
	// 	})
	// }

	ledgerSession.AppendOplog(OpLogEvent{
		Id: stakeOp.Id,
		// Index: 1,

		From:   stakeOp.From,
		To:     stakeOp.To,
		Asset:  stakeOp.Asset,
		Amount: stakeOp.Amount,
		Memo:   stakeOp.Memo,
		Type:   "unstake",

		// Params: map[string]interface{}{
		// 	"instant": stakeOp.Instant,
		// 	"fee":     fee,
		// },
	})

	return LedgerResult{
		Ok:  true,
		Msg: "success",
	}
}
