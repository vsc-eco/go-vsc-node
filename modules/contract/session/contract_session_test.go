package contract_session

import (
	"testing"

	"vsc-node/modules/db/vsc/contracts"
)

func TestCallSessionTakePendingClonesAndDeletes(t *testing.T) {
	orig := &TempOutput{
		Cache: map[string][]byte{
			"foo": {1, 2, 3},
		},
		Deletions: map[string]bool{"foo": true},
		Metadata:  contracts.ContractMetadata{CurrentSize: 1, MaxSize: 10},
		Cid:       "cid-1",
	}
	cs := &CallSession{
		pending: map[string]*TempOutput{
			"contract": orig,
		},
	}

	cloned := cs.takePending("contract")
	if cloned == nil {
		t.Fatalf("expected pending output, got nil")
	}
	if len(cs.pending) != 0 {
		t.Fatalf("expected pending map to be empty, got %#v", cs.pending)
	}

	cloned.Cache["foo"][0] = 42
	if orig.Cache["foo"][0] == 42 {
		t.Fatalf("cache slice should have been cloned")
	}
	cloned.Deletions["foo"] = false
	if !orig.Deletions["foo"] {
		t.Fatalf("deletions map should have been cloned")
	}
}

func TestCloneTempOutputsNilWhenEmpty(t *testing.T) {
	if res := cloneTempOutputs(nil); res != nil {
		t.Fatalf("expected nil for nil input, got %#v", res)
	}
	if res := cloneTempOutputs(map[string]*TempOutput{}); res != nil {
		t.Fatalf("expected nil for empty map, got %#v", res)
	}
}

func TestCloneTempOutputsDeepCopy(t *testing.T) {
	src := map[string]*TempOutput{
		"a": {
			Cache: map[string][]byte{"k": {9, 9}},
			Deletions: map[string]bool{
				"k": true,
			},
		},
		"b": {
			Cache: map[string][]byte{"z": {5}},
		},
	}

	cloned := cloneTempOutputs(src)
	if len(cloned) != len(src) {
		t.Fatalf("expected %d entries, got %d", len(src), len(cloned))
	}

	cloned["a"].Cache["k"][0] = 1
	if src["a"].Cache["k"][0] == 1 {
		t.Fatalf("cache slice should have been deep cloned")
	}

	cloned["a"].Deletions["k"] = false
	if !src["a"].Deletions["k"] {
		t.Fatalf("deletions map should have been deep cloned")
	}
}
