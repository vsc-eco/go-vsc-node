package contract_execution_context

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----- IsTrustedForwarder gating -----

func TestIsTrustedForwarder_DefaultFalse(t *testing.T) {
	// No WithTrustedForwarders option => no list => never trusted.
	ctx := New(Environment{
		ContractId: "vsc1abcSelfXX",
		Caller:     "hive:tester",
		Sender:     "hive:tester",
	}, 1000, 100000, 100000, nil, nil, 0)
	assert.False(t, ctx.IsTrustedForwarder(),
		"a context with no trusted-forwarder list must default to not-trusted")
}

func TestIsTrustedForwarder_SelfInList(t *testing.T) {
	ctx := New(Environment{
		ContractId: "vsc1abcSelfXX",
		Caller:     "hive:tester",
		Sender:     "hive:tester",
	}, 1000, 100000, 100000, nil, nil, 0,
		WithTrustedForwarders([]string{"contract:vsc1abcSelfXX", "contract:vsc1OtherForw"}),
	)
	assert.True(t, ctx.IsTrustedForwarder(),
		"contract with own id in TrustedForwarders must be trusted")
}

func TestIsTrustedForwarder_SelfNotInList(t *testing.T) {
	ctx := New(Environment{
		ContractId: "vsc1NotInListXX",
		Caller:     "hive:tester",
		Sender:     "hive:tester",
	}, 1000, 100000, 100000, nil, nil, 0,
		WithTrustedForwarders([]string{"contract:vsc1OtherForw"}),
	)
	assert.False(t, ctx.IsTrustedForwarder(),
		"contract with own id NOT in TrustedForwarders must NOT be trusted")
}

func TestIsTrustedForwarder_EmptyListReturnsFalse(t *testing.T) {
	// Passing an explicit empty list — still disabled.
	ctx := New(Environment{
		ContractId: "vsc1abcSelfXX",
	}, 1000, 100000, 100000, nil, nil, 0,
		WithTrustedForwarders([]string{}),
	)
	assert.False(t, ctx.IsTrustedForwarder())
}

// ----- CallAs gating (without invoking the WASM runtime) -----

func TestCallAs_RejectsNonForwarder(t *testing.T) {
	ctx := New(Environment{
		ContractId: "vsc1RandomCtr",
		Caller:     "hive:tester",
		Sender:     "hive:tester",
	}, 1000, 100000, 100000, nil, nil, 0,
		WithTrustedForwarders([]string{"contract:vsc1OtherForw"}),
	)

	res := ctx.CallAs("vsc1Target", "method", "{}", "{}", "did:pkh:bip122:00000ffd590b1485b3caadc19b22e637:Xexample")
	require.True(t, res.IsErr(), "non-forwarder must get permission-denied error")
	errText := res.UnwrapErr().Error()
	assert.Contains(t, errText, "TrustedForwarders",
		"error must mention the trusted-forwarders check; got: %s", errText)
}

func TestCallAs_RejectsEmptyEffectiveCaller(t *testing.T) {
	ctx := New(Environment{
		ContractId: "vsc1Forwarder",
	}, 1000, 100000, 100000, nil, nil, 0,
		WithTrustedForwarders([]string{"contract:vsc1Forwarder"}),
	)
	// We're a trusted forwarder, but the effectiveCallerDID is empty.
	res := ctx.CallAs("vsc1Target", "method", "{}", "{}", "")
	require.True(t, res.IsErr())
	errText := res.UnwrapErr().Error()
	assert.Contains(t, strings.ToLower(errText), "effective caller",
		"error must mention the empty effective caller; got: %s", errText)
}

func TestCallAs_NoForwardersConfigured(t *testing.T) {
	// Even if a contract claims to be a forwarder, with no WithTrustedForwarders
	// option the gate is effectively disabled.
	ctx := New(Environment{
		ContractId: "vsc1WouldBeForwarder",
	}, 1000, 100000, 100000, nil, nil, 0)

	res := ctx.CallAs("vsc1Target", "method", "{}", "{}", "did:example")
	require.True(t, res.IsErr())
	assert.Contains(t, res.UnwrapErr().Error(), "TrustedForwarders")
}

// ----- EffectiveCaller env var exposure -----

func TestEnvVar_EffectiveCallerFallsBackToCaller(t *testing.T) {
	ctx := New(Environment{
		ContractId: "c1",
		Caller:     "hive:alice",
		Sender:     "hive:alice",
		// EffectiveCaller unset => env returns Caller.
	}, 1000, 100000, 100000, nil, nil, 0)

	res := ctx.EnvVar("msg.effective_caller")
	require.False(t, res.IsErr())
	assert.Equal(t, "hive:alice", res.Unwrap(),
		"with no EffectiveCaller override, msg.effective_caller must fall back to Caller")
}

func TestEnvVar_EffectiveCallerReturnsOverride(t *testing.T) {
	overrideDID := "did:pkh:bip122:00000ffd590b1485b3caadc19b22e637:Xj9kExampleDashAddress"
	ctx := New(Environment{
		ContractId:      "c1",
		Caller:          "contract:vsc1Forwarder",
		Sender:          "hive:alice",
		EffectiveCaller: overrideDID,
	}, 1000, 100000, 100000, nil, nil, 0)

	res := ctx.EnvVar("msg.effective_caller")
	require.False(t, res.IsErr())
	assert.Equal(t, overrideDID, res.Unwrap(),
		"with EffectiveCaller set, msg.effective_caller must return the override")
}

func TestEnvVar_CallerUnchangedByOverride(t *testing.T) {
	// EffectiveCaller override must NOT bleed into msg.caller. They are
	// independent values.
	ctx := New(Environment{
		ContractId:      "c1",
		Caller:          "contract:vsc1Forwarder",
		Sender:          "hive:alice",
		EffectiveCaller: "did:pkh:bip122:00000ffd590b1485b3caadc19b22e637:Xj9kAddr",
	}, 1000, 100000, 100000, nil, nil, 0)

	res := ctx.EnvVar("msg.caller")
	require.False(t, res.IsErr())
	assert.Equal(t, "contract:vsc1Forwarder", res.Unwrap(),
		"msg.caller must remain the literal caller even when EffectiveCaller is set")
}

// ----- GetEnv includes effective_caller key -----

func TestGetEnv_IncludesEffectiveCaller(t *testing.T) {
	overrideDID := "did:pkh:bip122:00000ffd590b1485b3caadc19b22e637:Xj9kSomeDashAddr"
	ctx := New(Environment{
		ContractId:      "c1",
		ContractOwner:   "o1",
		BlockHeight:     1,
		TxId:            "tx1",
		BlockId:         "b1",
		Timestamp:       "t",
		RequiredAuths:   []string{"hive:tester"},
		Caller:          "contract:vsc1Forwarder",
		Sender:          "hive:tester",
		EffectiveCaller: overrideDID,
	}, 1000, 100000, 100000, nil, nil, 0)

	envRes := ctx.GetEnv()
	require.False(t, envRes.IsErr())

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(envRes.Unwrap()), &m))

	assert.Equal(t, overrideDID, m["msg.effective_caller"],
		"GetEnv must surface msg.effective_caller for contracts that read env keys via JSON")
	assert.Equal(t, "contract:vsc1Forwarder", m["msg.caller"],
		"msg.caller must be independent of msg.effective_caller")
}

func TestGetEnv_EffectiveCallerFallsBackInJSON(t *testing.T) {
	ctx := New(Environment{
		ContractId:    "c1",
		ContractOwner: "o1",
		BlockHeight:   1,
		TxId:          "tx1",
		BlockId:       "b1",
		Timestamp:     "t",
		RequiredAuths: []string{"hive:tester"},
		Caller:        "hive:tester",
		Sender:        "hive:tester",
		// No EffectiveCaller — should fall back to Caller in JSON output too.
	}, 1000, 100000, 100000, nil, nil, 0)

	envRes := ctx.GetEnv()
	require.False(t, envRes.IsErr())

	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(envRes.Unwrap()), &m))

	assert.Equal(t, "hive:tester", m["msg.effective_caller"],
		"msg.effective_caller must fall back to Caller in GetEnv when override is empty")
}

// ----- WithTrustedForwarders returns defensive copy -----

func TestWithTrustedForwarders_DefensiveCopy(t *testing.T) {
	source := []string{"contract:vsc1Forwarder"}
	ctx := New(Environment{
		ContractId: "vsc1Forwarder",
	}, 1000, 100000, 100000, nil, nil, 0,
		WithTrustedForwarders(source),
	)
	require.True(t, ctx.IsTrustedForwarder())

	// Mutate the caller's slice — must not affect ctx.
	source[0] = "contract:vsc1tampered"
	assert.True(t, ctx.IsTrustedForwarder(),
		"mutating the source slice after construction must not break trusted-forwarder check")
}
