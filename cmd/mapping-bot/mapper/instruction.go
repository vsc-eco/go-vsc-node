package mapper

import (
	"fmt"
	"net/url"
	"vsc-node/lib/dids"
)

// ValidateInstruction checks that an instruction string is suitable for submission to the mapping bot.
// Rules:
//   - Must be valid URL search params (parseable by url.ParseQuery)
//   - Must contain exactly one of "deposit_to" or "swap_to"
//   - "swap_to" requires a non-empty "swap_asset_out" param
//   - The address value of "deposit_to" or "swap_to" must pass dids.VerifyAddress
//
// mainnet controls which Bitcoin network is accepted by dids.VerifyAddress.
func ValidateInstruction(instruction string, mainnet bool) error {
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
	return nil
}
