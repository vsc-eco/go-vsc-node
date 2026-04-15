package dids

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/btcsuite/btcd/btcutil/base58"
	blocks "github.com/ipfs/go-block-format"
)

// hivePattern validates Hive account names.
// Must be 3–16 characters, lowercase letters/digits/hyphens, start and end with a letter or digit.
var hivePattern = regexp.MustCompile(`^[a-z][0-9a-z\-]*[0-9a-z](\.[a-z][0-9a-z\-]*[0-9a-z])*$`)

// ===== DIDs =====

type DID interface {
	String() string
	Verify(data blocks.Block, sig string) (bool, error)
}

// ===== provider interface (can be passed around later, depending on how DIDs want to be used) =====

type Provider[V any] interface {
	Sign(data V) (string, error)
	Type() string
}

func Parse(did string, includeBLS ...bool) (DID, error) {
	errs := []error{}

	if len(includeBLS) > 0 && includeBLS[0] {
		res, err := ParseBlsDID(did)
		if err == nil {
			return res, nil
		}
		errs = append(errs, err)
	}

	var res DID

	res, err := ParseKeyDID(did)
	if err == nil {
		return res, nil
	}
	errs = append(errs, err)

	res, err = ParseEthDID(did)
	if err == nil {
		return res, nil
	}
	errs = append(errs, err)

	res, err = ParseBtcDID(did)
	if err == nil {
		return res, nil
	}
	errs = append(errs, err)

	return nil, errors.Join(errs...)
}

// VerifyAddress checks the type of a Magi address string and returns a type label:
//
//	"user:evm"  — Ethereum address (did:pkh:eip155:1:)
//	"user:hive" — Hive account (hive:<username>)
//	"user:btc"  — Bitcoin address — mainnet CAIP-2 ID accepted when mainnet=true,
//	              testnet3 CAIP-2 ID accepted when mainnet=false; the wrong chain returns "unknown"
//	"key"       — did:key multikey
//	"contract"  — VSC contract ID (contract:vsc1…)
//	"system"    — system address (system:<name>)
//	"unknown"   — unrecognised, malformed, or wrong-network BTC address
//
// mainnet controls which Bitcoin CAIP-2 chain ID is valid: true for Magi mainnet
// (Bitcoin mainnet addresses only), false for testnet/devnet (Bitcoin testnet3 addresses only).
func VerifyAddress(addr string, mainnet bool) string {
	switch {
	case strings.HasPrefix(addr, EthDIDPrefix):
		if _, err := ParseEthDID(addr); err != nil {
			return "unknown"
		}
		return "user:evm"

	case strings.HasPrefix(addr, KeyDIDPrefix):
		if _, err := ParseKeyDID(addr); err != nil {
			return "unknown"
		}
		return "key"

	case strings.HasPrefix(addr, BtcDIDPrefix):
		if !mainnet {
			return "unknown"
		}
		if _, err := ParseBtcDID(addr); err != nil {
			return "unknown"
		}
		return "user:btc"

	case strings.HasPrefix(addr, BtcTestnetDIDPrefix):
		if mainnet {
			return "unknown"
		}
		if _, err := ParseBtcTestnetDID(addr); err != nil {
			return "unknown"
		}
		return "user:btc"

	case strings.HasPrefix(addr, "hive:"):
		username := strings.TrimPrefix(addr, "hive:")
		if !hivePattern.MatchString(username) || len(username) < 3 || len(username) >= 17 {
			return "unknown"
		}
		return "user:hive"

	case strings.HasPrefix(addr, "contract:"):
		contractId := strings.TrimPrefix(addr, "contract:")
		if !strings.HasPrefix(contractId, "vsc1") || len(contractId) != 38 {
			return "unknown"
		}
		_, ver, err := base58.CheckDecode(contractId[4:])
		if err != nil || ver != 0x1a {
			return "unknown"
		}
		return "contract"

	case strings.HasPrefix(addr, "system:"):
		if len(strings.TrimPrefix(addr, "system:")) == 0 {
			return "unknown"
		}
		return "system"

	default:
		return "unknown"
	}
}

func ParseMany(dids []string, includeBLS ...bool) ([]DID, error) {
	res := make([]DID, len(dids))
	var err error

	for i, did := range dids {
		res[i], err = Parse(did, includeBLS...)
		if err != nil {
			return nil, fmt.Errorf("could not parse did %d: %w", i, err)
		}
	}

	return res, nil
}

func VerifyMany(dids []DID, blk blocks.Block, sigs []string) (bool, []bool, error) {
	if len(dids) != len(sigs) {
		return false, nil, fmt.Errorf("len(dids) != len(sigs)")
	}

	res := true
	results := make([]bool, len(dids))
	for i, did := range dids {
		sig := sigs[i]
		verified, err := did.Verify(blk, sig)
		if err != nil {
			return false, nil, err
		}

		if !verified {
			res = false
		}

		results[i] = verified
	}

	return res, results, nil
}
