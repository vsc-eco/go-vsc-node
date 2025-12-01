package rc_system

import (
	"strings"
	"vsc-node/modules/common/params"
	rcDb "vsc-node/modules/db/vsc/rcs"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/chebyrash/promise"
)

type RcSystem struct {
	RcDb         rcDb.RcDb
	LedgerSystem ledgerSystem.LedgerSystem
}

// Returns the amount of RCs that are frozen for the given account at the given block height
func (rcs *RcSystem) GetFrozenAmt(account string, blockHeight uint64) int64 {
	rcRecord, _ := rcs.RcDb.GetRecord(account, blockHeight)

	diff := blockHeight - rcRecord.BlockHeight

	amtRet := int64(diff * uint64(rcRecord.Amount) / params.RC_RETURN_PERIOD)

	if amtRet > rcRecord.Amount {
		amtRet = rcRecord.Amount
	}

	return rcRecord.Amount - amtRet
}

func (rcs *RcSystem) GetAvailableRCs(account string, blockHeight uint64) int64 {
	balAmt := rcs.LedgerSystem.GetBalance(account, blockHeight, "hbd")

	if strings.HasPrefix(account, "hive:") {
		//Give the user 5 HBD worth of RCs by default
		//If user is Hive account
		balAmt = balAmt + params.RC_HIVE_FREE_AMOUNT
	}

	frozeAmt := rcs.GetFrozenAmt(account, blockHeight)

	return balAmt - frozeAmt
}

func (rcs *RcSystem) NewSession(ledgerSession ledgerSystem.LedgerSession) RcSession {
	return &rcSession{
		ledgerSession: ledgerSession,
		rcSystem:      rcs,

		rcMap: make(map[string]int64),
	}
}

func (rc *RcSystem) Init() error {
	return nil
}

func (rc *RcSystem) Start() *promise.Promise[any] {
	return promise.New(func(resolve func(interface{}), reject func(error)) {
		resolve(nil)
	})
}

func (rc *RcSystem) Stop() error {
	return nil
}

func New(rcDb rcDb.RcDb, ledgerSystem ledgerSystem.LedgerSystem) *RcSystem {
	return &RcSystem{
		RcDb:         rcDb,
		LedgerSystem: ledgerSystem,
	}
}

type rcSession struct {
	ledgerSession ledgerSystem.LedgerSession
	rcSystem      *RcSystem

	rcMap map[string]int64
}

func (rss *rcSession) Consume(account string, blockHeight uint64, rcAmt int64) (bool, int64) {
	canConsume, _, _ := rss.CanConsume(account, blockHeight, rcAmt)

	if canConsume {
		rss.rcMap[account] = rss.rcMap[account] + rcAmt
		return true, rcAmt
	} else {
		return false, 0
	}
}

func (rss *rcSession) CanConsume(account string, blockHeight uint64, rcAmt int64) (bool, int64, int64) {
	balAmt := rss.ledgerSession.GetBalance(account, blockHeight, "hbd")

	if strings.HasPrefix(account, "hive:") {
		//Give the user 5 HBD worth of RCs by default
		//If user is Hive account
		balAmt = balAmt + 5_000
	}

	frozeAmt := rss.rcSystem.GetFrozenAmt(account, blockHeight)

	//fmt.Println("rcAmt", balAmt, frozeAmt, rcAmt)
	totalAmt := balAmt - frozeAmt
	if totalAmt < rcAmt {
		return false, 0, rcAmt
	} else {
		return true, totalAmt - rcAmt, rcAmt
	}
}

func (rss *rcSession) Revert() {
	rss.rcMap = make(map[string]int64)
}

func (rss *rcSession) Done() RcMapResult {
	return RcMapResult{
		RcMap: rss.rcMap,
	}
}
