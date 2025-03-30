package rcSystem

import "vsc-node/modules/common"

func CalculateFrozenBal(start, end uint64, initialBal int64) int64 {

	diff := start - end
	amtRet := int64(diff * uint64(initialBal) / common.RC_RETURN_PERIOD)

	if amtRet > initialBal {
		amtRet = initialBal
	}

	return initialBal - amtRet
}
