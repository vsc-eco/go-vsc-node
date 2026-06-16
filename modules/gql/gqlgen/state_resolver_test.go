package gqlgen

import (
	"context"
	"testing"

	"vsc-node/lib/test_utils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetStateByKeys_EmptyStateMerkleReturnsNilValues regression-tests
// the resolver's behaviour for contracts that haven't produced a state
// output yet. Before the fix, the resolver passed an empty StateMerkle
// straight to cid.Parse and surfaced the misleading
//
//	"gql errors from magi-1: invalid cid: cid too short"
//
// — making every test that polled a fresh contract's state look like a
// CID-decoding bug. The actual cause was MockContractStateDb /
// production contractState.GetLastOutput returning a zero-valued
// ContractOutput (with StateMerkle="") and a nil error when the
// contract has never produced an output — see
// modules/oracle/chain/chain_relay.go:381-387 for the documented
// "fresh contract" semantics this resolver previously didn't honour.
//
// Expected behaviour: return a result map with every requested key
// mapped to nil and no error. Same shape as the existing per-key
// "key not present in databin" branch.
func TestGetStateByKeys_EmptyStateMerkleReturnsNilValues(t *testing.T) {
	// Mock state db with NO outputs ingested. GetLastOutput returns
	// the zero-valued ContractOutput{StateMerkle:""} + nil error.
	mockState := &test_utils.MockContractStateDb{}

	resolver := &Resolver{
		ContractsState: mockState,
	}

	ctx := context.Background()
	keys := []string{"forwarder", "vs-0", "any-key"}

	result, err := resolver.Query().GetStateByKeys(ctx, "vsc1FreshContractWithNoStateYet", keys, nil)

	require.NoError(t, err, "fresh contract should not surface a CID decode error")
	require.NotNil(t, result, "fresh contract should return a map, not nil")
	assert.Len(t, result, len(keys), "result should contain every requested key")
	for _, k := range keys {
		v, present := result[k]
		assert.True(t, present, "key %q should be present in the result map", k)
		assert.Nil(t, v, "key %q value should be nil for a fresh contract", k)
	}
}

// TestGetStateByKeys_EmptyStateMerkle_HexEncoding mirrors the above
// but with encoding="hex" — both call shapes share the same resolver
// path, the encoding switch only applies to non-nil values returned
// from databin.Get. Confirming both call shapes here keeps the fix
// from regressing on either input combination.
func TestGetStateByKeys_EmptyStateMerkle_HexEncoding(t *testing.T) {
	mockState := &test_utils.MockContractStateDb{}
	resolver := &Resolver{ContractsState: mockState}
	ctx := context.Background()
	keys := []string{"a-hbd-did:pkh:bip122:00000bafbc94add76cb75e2ec9289483:y1example"}

	hex := "hex"
	result, err := resolver.Query().GetStateByKeys(ctx, "vsc1FreshContract", keys, &hex)
	require.NoError(t, err)
	assert.Nil(t, result[keys[0]])
}
