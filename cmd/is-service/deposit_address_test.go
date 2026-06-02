package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Parity vectors captured from utxo-mapping/dash-mapping-contract/contract/mapping/export.go:DepositAddress
// using the actual contract code. If our reimplementation drifts from the contract,
// addresses won't round-trip and these tests catch it immediately.
//
// To regenerate after a deliberate algorithm change, run dash-mapping-contract
// against the same (primaryPubKey, backupPubKey, instruction, params) inputs and
// update the expected addresses below.

const (
	// Test bridge pubkeys (same ones the existing gen-address tool uses).
	parityPrimaryPubKey = "037252c3e934177fdcc14e3b3dbf295378fce11305ca513e9f2651dc2839e3be1a"
	parityBackupPubKey  = "0242f9da15eae56fe6aca65136738905c0afdb2c4edf379e107b3b00b98c7fc9f0"
)

func TestDepositAddress_ParityWithContract_Testnet(t *testing.T) {
	cases := []struct {
		instruction string
		expected    string
	}{
		{
			instruction: "op=auth;sid=test1",
			expected:    "tdash1q8d8rp9gk8mljvlt5tdpg5296h94vawzvtf5hkvl2rg0jwyjqlhrsx6u4sz",
		},
		{
			instruction: "deposit_to=hive:tibfox",
			expected:    "tdash1qmjaexgarq8ckt3mjeas0um0vu04cxznydtns935ldysntajruryq5e8pjy",
		},
	}
	for _, c := range cases {
		t.Run(c.instruction, func(t *testing.T) {
			got, _, err := DepositAddress(parityPrimaryPubKey, parityBackupPubKey, c.instruction, dashTestNetParams())
			require.NoError(t, err)
			assert.Equal(t, c.expected, got,
				"derivation drifted from dash-mapping-contract! instruction=%q", c.instruction)
		})
	}
}

func TestDepositAddress_ParityWithContract_Mainnet(t *testing.T) {
	cases := []struct {
		instruction string
		expected    string
	}{
		{
			instruction: "op=auth;sid=test1",
			expected:    "dash1qtd9samgpvaqsp8g35wfnpcrqg5ffp3m4825mavqg9mldzat7rx6qrr9m32",
		},
		{
			instruction: "deposit_to=hive:tibfox",
			expected:    "dash1q8ukr0pf4z6rataxe9ly0fqqhsp9t2nst2aggxmwdwpzl9dffq20sd22qju",
		},
	}
	for _, c := range cases {
		t.Run(c.instruction, func(t *testing.T) {
			got, _, err := DepositAddress(parityPrimaryPubKey, parityBackupPubKey, c.instruction, dashMainNetParams())
			require.NoError(t, err)
			assert.Equal(t, c.expected, got,
				"derivation drifted from dash-mapping-contract! instruction=%q", c.instruction)
		})
	}
}

func TestDepositAddress_InstructionChangesAddress(t *testing.T) {
	addrA, _, err := DepositAddress(parityPrimaryPubKey, parityBackupPubKey, "op=auth;sid=A", dashTestNetParams())
	require.NoError(t, err)
	addrB, _, err := DepositAddress(parityPrimaryPubKey, parityBackupPubKey, "op=auth;sid=B", dashTestNetParams())
	require.NoError(t, err)
	assert.NotEqual(t, addrA, addrB,
		"different instructions must produce different addresses — this is the foundational property of the per-op-unique-address design")
}

func TestDepositAddress_RejectsMalformedPubkey(t *testing.T) {
	_, _, err := DepositAddress("not-hex", parityBackupPubKey, "op=auth", dashTestNetParams())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "primary")

	_, _, err = DepositAddress(parityPrimaryPubKey, "not-hex", "op=auth", dashTestNetParams())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "backup")

	// 32-byte (one short)
	_, _, err = DepositAddress("037252c3e934177fdcc14e3b3dbf295378fce11305ca513e9f2651dc2839e3be", parityBackupPubKey, "op=auth", dashTestNetParams())
	assert.Error(t, err)
}

func TestDepositAddress_TestnetVsMainnetAddrsDiffer(t *testing.T) {
	tn, _, err := DepositAddress(parityPrimaryPubKey, parityBackupPubKey, "op=auth;sid=x", dashTestNetParams())
	require.NoError(t, err)
	mn, _, err := DepositAddress(parityPrimaryPubKey, parityBackupPubKey, "op=auth;sid=x", dashMainNetParams())
	require.NoError(t, err)
	assert.NotEqual(t, tn, mn, "mainnet and testnet addresses must differ for the same instruction")
}
