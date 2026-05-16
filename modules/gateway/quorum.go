package gateway

// gatewayWeightThreshold returns the owner-auth weight threshold for the
// gateway multisig account: a strict 2/3 supermajority of totalWeight.
//
// review2 HIGH #29: this was previously int(totalWeight * 2 / 3), i.e.
// floor(2N/3). For 10 keys that yields 6, letting 6-of-10 signers move
// funds when a 2/3 supermajority should require 7. ceil(2N/3) is the correct
// threshold; ceil(a/b) == (a + b - 1) / b, so ceil(2N/3) == (2N + 2) / 3.
func gatewayWeightThreshold(totalWeight int) int {
	if totalWeight <= 0 {
		return 0
	}
	return (totalWeight*2 + 2) / 3
}
