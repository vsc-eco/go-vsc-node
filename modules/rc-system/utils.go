package rc_system

import "vsc-node/modules/common/params"

func CalculateFrozenBal(start, end uint64, initialBal int64) int64 {

	diff := end - start
	amtRet := int64(diff * uint64(initialBal) / params.RC_RETURN_PERIOD)

	if amtRet > initialBal {
		amtRet = initialBal
	}

	return initialBal - amtRet
}
