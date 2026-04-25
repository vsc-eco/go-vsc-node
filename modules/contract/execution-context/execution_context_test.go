package contract_execution_context

import (
	"encoding/json"
	"testing"
)

func TestGetEnvMergesPendulumOracle(t *testing.T) {
	ctx := New(Environment{
		ContractId:    "c1",
		ContractOwner: "o1",
		BlockHeight:   1,
		TxId:          "tx1",
		BlockId:       "b1",
		Index:         0,
		OpIndex:       0,
		Timestamp:     "t",
		RequiredAuths: []string{"a"},
		Caller:        "a",
		Sender:        "a",
		PendulumOracle: map[string]interface{}{
			"pendulum.hbd_interest_rate_bps": 2000,
			"pendulum.hbd_interest_rate_ok":  true,
		},
	}, 1000, 100000, 100000, nil, nil, 0)

	envRes := ctx.GetEnv()
	if envRes.IsErr() {
		t.Fatalf("GetEnv: %v", envRes.UnwrapErr())
	}
	envStr := envRes.Unwrap()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(envStr), &m); err != nil {
		t.Fatal(err)
	}
	// json.Unmarshal decodes JSON numbers as float64.
	if int(m["pendulum.hbd_interest_rate_bps"].(float64)) != 2000 {
		t.Fatalf("bps=%v", m["pendulum.hbd_interest_rate_bps"])
	}
	if m["pendulum.hbd_interest_rate_ok"].(bool) != true {
		t.Fatalf("ok=%v", m["pendulum.hbd_interest_rate_ok"])
	}
}

func TestEnvVarPendulumOracle(t *testing.T) {
	ctx := New(Environment{
		ContractId:    "c1",
		ContractOwner: "o1",
		BlockHeight:   1,
		TxId:          "tx1",
		BlockId:       "b1",
		Index:         0,
		OpIndex:       0,
		Timestamp:     "t",
		RequiredAuths: []string{"a"},
		Caller:        "a",
		Sender:        "a",
		PendulumOracle: map[string]interface{}{
			"pendulum.hbd_interest_rate_bps": 1500,
		},
	}, 1000, 100000, 100000, nil, nil, 0)

	ev := ctx.EnvVar("pendulum.hbd_interest_rate_bps")
	if ev.IsErr() {
		t.Fatalf("EnvVar: %v", ev.UnwrapErr())
	}
	v := ev.Unwrap()
	if v != "1500" {
		t.Fatalf("got %q", v)
	}
}

func TestEnvVarPendulumOracleStringifiesArray(t *testing.T) {
	ctx := New(Environment{
		ContractId:    "c1",
		ContractOwner: "o1",
		BlockHeight:   1,
		TxId:          "tx1",
		BlockId:       "b1",
		Index:         0,
		OpIndex:       0,
		Timestamp:     "t",
		RequiredAuths: []string{"a"},
		Caller:        "a",
		Sender:        "a",
		PendulumOracle: map[string]interface{}{
			"pendulum.trusted_witness_group": []string{"alice", "bob"},
		},
	}, 1000, 100000, 100000, nil, nil, 0)

	ev := ctx.EnvVar("pendulum.trusted_witness_group")
	if ev.IsErr() {
		t.Fatalf("EnvVar: %v", ev.UnwrapErr())
	}
	if ev.Unwrap() != "[\"alice\",\"bob\"]" {
		t.Fatalf("unexpected array encoding: %q", ev.Unwrap())
	}
}
