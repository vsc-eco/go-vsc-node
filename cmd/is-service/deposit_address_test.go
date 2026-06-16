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
//
// Vectors below are P2SH (Dash testnet `8`/`9` prefix, mainnet `7` prefix).
// Pre-existing vectors were P2WSH (`tdash1...`/`dash1...`) bech32 — those
// addresses are unspendable on real Dash because Dash never activated
// SegWit (validateaddress on dashd v23 returns "Invalid address format"
// for bech32 addresses). The redeem-script bytes are unchanged; only
// the on-chain commitment shifts from 32-byte sha256 (P2WSH witness
// program) to 20-byte HASH160 (P2SH script hash).

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
			expected:    "8i92eC73yb2mMgf4BfnG8KA2R1kY1LFgQ3",
		},
		{
			instruction: "deposit_to=hive:tibfox",
			expected:    "8v28WKnneY5WKXpeGCf3BUmaeQP88wyhJ9",
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
			expected:    "7h3njsQDVUe5cnjTgfj9AFtnNbnawFas6m",
		},
		{
			instruction: "deposit_to=hive:tibfox",
			expected:    "7VopAqnSkLjRExJzfUkgx3WAYhnpDapeCx",
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
