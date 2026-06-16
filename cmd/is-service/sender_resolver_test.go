package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveSenderAddress_RejectsBadHex(t *testing.T) {
	_, err := resolveSenderAddress("not hex!!!", dashTestNetParams())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not hex")
}

func TestResolveSenderAddress_RejectsTruncatedTx(t *testing.T) {
	_, err := resolveSenderAddress("deadbeef", dashTestNetParams())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestResolveSenderAddress_RejectsEmpty(t *testing.T) {
	_, err := resolveSenderAddress("", dashTestNetParams())
	assert.Error(t, err)
}

// Sender resolution from a real Dash tx is exercised via the
// dash-mapping-contract's TestResolveSenderDashDID — they share the
// same btcsuite/btcd/txscript primitives, so we don't duplicate
// fixture-heavy address-recovery tests here. This file only covers
// the IS-service-side error paths to confirm onISLockObserved's
// best-effort behaviour doesn't panic on bad input.
