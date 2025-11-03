package rc_system

type RcSession interface {
	//Returns: Did consume (bool), amount consumed (int64)
	Consume(account string, blockHeight uint64, rcAmt int64) (bool, int64)
	//Returns: Did consume (bool), remaining (int64), amount consumed (int64)
	CanConsume(account string, blockHeight uint64, rcAmt int64) (bool, int64, int64)
	Revert()
	Done() RcMapResult
}

type RcMapResult struct {
	RcMap map[string]int64
}
