package stateEngine

import (
	"errors"
	"slices"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// Key DIDs are always supposed to be normalized
// Note: it might make sense to define address format for regular DIDs.
// However, what we have right now suffices
var SUPPORTED_TYPES = []string{
	"ethereum",
	"hive",
}

func NormalizeAddress(address string, addressType string) (*string, error) {
	if !slices.Contains(SUPPORTED_TYPES, addressType) {
		return nil, errors.New("unsupported address type")
	}

	switch addressType {

	case "ethereum":
		{
			if !strings.HasPrefix(address, "0x") || len(address) != 42 {
				return nil, errors.New("invalid ethereum address")
			}
			hexBytes := common.FromHex(address)
			addr := common.Address{}
			addr.SetBytes(hexBytes)
			eip55Addr := common.AddressEIP55(addr)

			// normalizedAddr := common.
			returnVal := "did:pkh:eip155:1:" + eip55Addr.String()

			return &returnVal, nil
		}
	case "hive":
		{
			if !strings.HasPrefix(address, "hive:") {
				returnVal := address
				return &returnVal, nil
			}
			returnVal := "hive:" + address
			return &returnVal, nil
		}
	}

	return nil, errors.New("unsupported address type")
}
