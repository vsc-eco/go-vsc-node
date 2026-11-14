package mapper

import (
	"fmt"
	"net/url"
	"strings"
	"vsc-node/lib/dids"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
)

// ValidateInstruction checks that an instruction string is suitable for submission to the mapping bot.
// Rules:
//   - Must be valid URL search params (parseable by url.ParseQuery)
//   - Must contain exactly one of "deposit_to" or "swap_to"
//   - "swap_to" requires a non-empty "swap_asset_out" param
//   - The address value of "deposit_to" or "swap_to" must pass dids.VerifyAddress
//   - "swap_to" requires "refund_to": the sender's hive: account, or the sender's
//     own address on this chain (or its did:pkh DID). A swap that does not complete
//     goes back there; without it the contract can only hold it for a manual
//     refund. A payout on another chain (an EVM withdrawal can fail after the
//     swap and comes back in the output asset) needs a hive: refund_to.
//
// mainnet controls which Bitcoin network is accepted by dids.VerifyAddress.
func ValidateInstruction(instruction string, mainnet bool, chainParams *chaincfg.Params) error {
	params, err := url.ParseQuery(instruction)
	if err != nil {
		return fmt.Errorf("instruction is not valid URL search params: %w", err)
	}

	hasDeposit := params.Has("deposit_to")
	hasSwap := params.Has("swap_to")

	if !hasDeposit && !hasSwap {
		return fmt.Errorf("instruction must contain 'deposit_to' or 'swap_to'")
	}
	if hasDeposit && hasSwap {
		return fmt.Errorf("instruction must contain only one of 'deposit_to' or 'swap_to', not both")
	}

	var addr string
	if hasDeposit {
		addr = params.Get("deposit_to")
	} else {
		addr = params.Get("swap_to")
		if params.Get("swap_asset_out") == "" {
			return fmt.Errorf("'swap_to' requires 'swap_asset_out'")
		}
	}

	if dids.VerifyAddress(addr, mainnet) == "unknown" {
		return fmt.Errorf("invalid VSC address %q", addr)
	}
	// The contract pays a swap only to a user account and holds a deposit for any
	// other swap_to (a contract, a system account) for a manual refund.
	if hasSwap && !strings.HasPrefix(dids.VerifyAddress(addr, mainnet), "user:") {
		return fmt.Errorf("'swap_to' %q is not a user account", addr)
	}
	// The node checks a Hive name by format only, looser than Hive itself: a name
	// Hive refuses (ab.cde) can never own its Magi account, and a payout to it
	// fails at the gateway (NEW-GW-2).
	if strings.HasPrefix(addr, "hive:") && !ValidHiveName(strings.TrimPrefix(addr, "hive:")) {
		return fmt.Errorf("%q is not a name Hive accepts", addr)
	}
	if hasSwap {
		refundTo := params.Get("refund_to")
		if refundTo == "" {
			return fmt.Errorf("'swap_to' requires 'refund_to': the sender's hive: account or its own address on this chain")
		}
		hiveRefund, ok := ValidRefundTo(refundTo, mainnet, chainParams)
		if !ok {
			return fmt.Errorf("invalid refund_to %q: the sender's hive: account or its own address on this chain", refundTo)
		}
		switch strings.ToUpper(strings.TrimSpace(params.Get("destination_chain"))) {
		case "", "MAGI", "HIVE":
		default:
			if !hiveRefund {
				return fmt.Errorf("a payout on %s needs a hive: refund_to", params.Get("destination_chain"))
			}
		}
	}
	return nil
}

// ValidRefundTo reports whether refund_to is usable, and whether it is a hive:
// account (true) rather than an address on this chain (false). It accepts what
// the mapping contract accepts (parseRefundTo).
func ValidRefundTo(refundTo string, mainnet bool, chainParams *chaincfg.Params) (bool, bool) {
	refundTo = strings.TrimSpace(refundTo)
	if strings.HasPrefix(refundTo, "hive:") {
		return true, dids.VerifyAddress(refundTo, mainnet) == "user:hive" &&
			ValidHiveName(strings.TrimPrefix(refundTo, "hive:"))
	}
	if chainParams == nil {
		return false, false
	}
	return false, RefundChainAddress(refundTo, chainParams) != ""
}

// RefundChainAddress is refund_to's address on this chain ("" if it is not one).
func RefundChainAddress(refundTo string, chainParams *chaincfg.Params) string {
	if chainParams == nil {
		return ""
	}
	raw := strings.TrimSpace(refundTo)
	if strings.HasPrefix(raw, "did:pkh:bip122:") {
		raw = raw[strings.LastIndex(raw, ":")+1:]
	}
	a, err := btcutil.DecodeAddress(raw, chainParams)
	if err != nil || !a.IsForNet(chainParams) {
		return ""
	}
	return a.EncodeAddress()
}

// ValidHiveName is Hive's account-name rule: 3 to 16 characters, dot-separated
// segments of at least 3, each starting with a letter, ending with a letter or
// digit, holding only lowercase letters, digits and single hyphens.
func ValidHiveName(name string) bool {
	if len(name) < 3 || len(name) > 16 {
		return false
	}
	for _, seg := range strings.Split(name, ".") {
		if len(seg) < 3 {
			return false
		}
		if seg[0] < 'a' || seg[0] > 'z' {
			return false
		}
		last := seg[len(seg)-1]
		if !((last >= 'a' && last <= 'z') || (last >= '0' && last <= '9')) {
			return false
		}
		for i := 1; i < len(seg); i++ {
			c := seg[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			case c == '-':
				if seg[i-1] == '-' {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}
