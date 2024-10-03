package dids_test

import (
	"encoding/json"
	"testing"
	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/stretchr/testify/assert"
)

func TestNewEthDID(t *testing.T) {
	ethAddr := "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"
	did := dids.NewEthDID(ethAddr)

	expectedDID := dids.EthDIDPrefix + ethAddr
	assert.Equal(t, expectedDID, did.String())
}

func TestEthDIDIdentifier(t *testing.T) {
	ethAddr := "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"
	did := dids.NewEthDID(ethAddr)

	assert.Equal(t, ethAddr, did.Identifier())
}

func TestTypedDataComplexPrimitives(t *testing.T) {
	type Metadata struct {
		Info     [32]byte
		Epoch    uint64
		Hashtags []string
		Ratings  [8]uint16
	}

	type User struct {
		Name        string
		Wallet      string
		Balance     int64
		Healthy     bool
		ID          uint64
		Favorites   [3]byte
		Permissions []bool
		Nicknames   []string
	}

	type Transaction struct {
		From     User
		Alt      Metadata
		To       User
		Contents string
		Frac     float64
		Money    uint64
		Sigs     [65]byte
	}

	// showcasing all the complex primitives that can be used in a typed data struct
	tx := Transaction{
		From: User{
			Name:        "bob",
			Wallet:      "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
			Balance:     -22,
			Healthy:     true,
			ID:          123123123,
			Favorites:   [3]byte{0x0a, 0x0b, 0x0c},
			Permissions: []bool{false, false, true},
			Nicknames:   []string{"rob", "robert", "bobbert"},
		},
		To: User{
			Name:        "alice",
			Wallet:      "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
			Balance:     100,
			Healthy:     false,
			ID:          67890,
			Favorites:   [3]byte{0x04, 0x05, 0x06},
			Permissions: []bool{false, false, true},
			Nicknames:   []string{"alicia", "aly", "allie"},
		},
		Contents: "hello world! this is some exciting text",
		Money:    123123123123,
		Alt: Metadata{
			Info:     [32]byte{0xa, 0xb, 0xc, 0xd},
			Epoch:    1727930344,
			Hashtags: []string{"wolves", "puppies", "dogs"},
			Ratings:  [8]uint16{2, 4, 3, 2, 1, 5, 0},
		},
		Frac: 3.14159265358979,
		Sigs: [65]byte{0xab, 0xba, 0xab, 0xba},
	}

	// function that allows for custom handling of float64 values
	// since EIP-712 only supports integers, we need to decide to either:
	//
	// - throw error if there's a float value
	// - scale the float value to an integer (perhaps x10^18 as seems standard)
	// - round/ceil/floor the float value to an integer
	floatHandler := func(f float64) (*math.HexOrDecimal256, error) {
		scaledInt := int64(f)
		return math.NewHexOrDecimal256(scaledInt), nil
	}

	// convert the struct to EIP-712 typed data
	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", "1", "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC", "0x1231231231231231231231231231231231231231231231231231231231231231", math.NewHexOrDecimal256(1), tx, "tx_container_v0", floatHandler)
	assert.Nil(t, err)

	typedDataJSON, err := json.MarshalIndent(typedData, "", "  ")
	assert.Nil(t, err)

	correctJson := `{
  "types": {
    "EIP712Domain": [
      {
        "name": "name",
        "type": "string"
      },
      {
        "name": "version",
        "type": "string"
      },
      {
        "name": "chainId",
        "type": "uint256"
      },
      {
        "name": "verifyingContract",
        "type": "address"
      },
      {
        "name": "salt",
        "type": "bytes32"
      }
    ],
    "Metadata": [
      {
        "name": "info",
        "type": "bytes32"
      },
      {
        "name": "epoch",
        "type": "uint256"
      },
      {
        "name": "hashtags",
        "type": "string[]"
      },
      {
        "name": "ratings",
        "type": "uint256[]"
      }
    ],
    "User": [
      {
        "name": "name",
        "type": "string"
      },
      {
        "name": "wallet",
        "type": "string"
      },
      {
        "name": "balance",
        "type": "int256"
      },
      {
        "name": "healthy",
        "type": "bool"
      },
      {
        "name": "id",
        "type": "uint256"
      },
      {
        "name": "favorites",
        "type": "bytes3"
      },
      {
        "name": "permissions",
        "type": "bool[]"
      },
      {
        "name": "nicknames",
        "type": "string[]"
      }
    ],
    "tx_container_v0": [
      {
        "name": "from",
        "type": "User"
      },
      {
        "name": "alt",
        "type": "Metadata"
      },
      {
        "name": "to",
        "type": "User"
      },
      {
        "name": "contents",
        "type": "string"
      },
      {
        "name": "frac",
        "type": "uint256"
      },
      {
        "name": "money",
        "type": "uint256"
      },
      {
        "name": "sigs",
        "type": "bytes"
      }
    ]
  },
  "primaryType": "tx_container_v0",
  "domain": {
    "name": "vsc.network",
    "version": "1",
    "chainId": "0x1",
    "verifyingContract": "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
    "salt": "0x1231231231231231231231231231231231231231231231231231231231231231"
  },
  "message": {
    "alt": {
      "epoch": "0x66fe1fe8",
      "hashtags": [
        "wolves",
        "puppies",
        "dogs"
      ],
      "info": "0x0a0b0c0d00000000000000000000000000000000000000000000000000000000",
      "ratings": [
        "0x2",
        "0x4",
        "0x3",
        "0x2",
        "0x1",
        "0x5",
        "0x0",
        "0x0"
      ]
    },
    "contents": "hello world! this is some exciting text",
    "frac": "0x3",
    "from": {
      "balance": "0x-16",
      "favorites": "0x0a0b0c",
      "id": "0x756b5b3",
      "healthy": true,
      "name": "bob",
      "nicknames": [
        "rob",
        "robert",
        "bobbert"
      ],
      "permissions": [
        false,
        false,
        true
      ],
      "wallet": "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"
    },
    "money": "0x1caab5c3b3",
    "sigs": "0xabbaabba00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
    "to": {
      "balance": "0x64",
      "favorites": "0x040506",
      "id": "0x10932",
      "healthy": false,
      "name": "alice",
      "nicknames": [
        "alicia",
        "aly",
        "allie"
      ],
      "permissions": [
        false,
        false,
        true
      ],
      "wallet": "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"
    }
  }
}`

	assert.JSONEq(t, correctJson, string(typedDataJSON))
}
