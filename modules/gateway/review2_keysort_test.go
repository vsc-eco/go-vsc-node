package gateway

import (
	"testing"

	"vsc-node/lib/test_utils"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/vsc-eco/hivego"
)

// fakeHiveCreator captures the gateway KeyAuths handed to UpdateAccount so
// the sorted gateway-key order produced by keyRotation is observable.
type fakeHiveCreator struct{ keyAuths [][2]interface{} }

func (f *fakeHiveCreator) UpdateAccount(_ string, owner *hivego.Auths, _ *hivego.Auths, _ *hivego.Auths, _ string, _ string) hivego.HiveOperation {
	f.keyAuths = owner.KeyAuths
	return nil
}
func (f *fakeHiveCreator) CustomJson(_ []string, _ []string, _ string, _ string) hivego.HiveOperation {
	return nil
}
func (f *fakeHiveCreator) Transfer(_ string, _ string, _ string, _ string, _ string) hivego.HiveOperation {
	return nil
}
func (f *fakeHiveCreator) TransferToSavings(_ string, _ string, _ string, _ string, _ string) hivego.HiveOperation {
	return nil
}
func (f *fakeHiveCreator) TransferFromSavings(_ string, _ string, _ string, _ string, _ string, _ int) hivego.HiveOperation {
	return nil
}
func (f *fakeHiveCreator) MakeTransaction(_ []hivego.HiveOperation) hivego.HiveTransaction {
	return hivego.HiveTransaction{}
}
func (f *fakeHiveCreator) PopulateSigningProps(_ *hivego.HiveTransaction, _ []int) error { return nil }
func (f *fakeHiveCreator) Sign(_ hivego.HiveTransaction) (string, error)                 { return "", nil }
func (f *fakeHiveCreator) Broadcast(_ hivego.HiveTransaction) (string, error)            { return "", nil }

// review2 MEDIUM #57 — keyRotation sorted the gateway keys with
// `int(weightMap[a]) - int(weightMap[b])`. uint64→int truncates weights
// >= 2^63 to negative int64, so the comparator orders the highest-weight
// keys (>= 2^63) as if they were the lowest. The gateway multisig key
// set must be ordered identically and correctly on every node, so this
// mis-order is a consensus/correctness defect. Fixed with cmp.Compare on
// the uint64 weights.
//
// Two clean groups make the comparator a *total order* on both arms (so
// the sort is deterministic) but reversed between them:
//   - s1..s4: small weights (1..4)        — int64  1..4
//   - b1..b4: weights near 2^64 (uint max) — int64 -4..-1 after truncation
//
// Cross-compare int(s)-int(b) = (1..4)-(-4..-1) is a small positive (no
// overflow), so baseline ranks the huge-weight b* keys BELOW the tiny s*
// keys. Correct (fix) ascending order: s1..s4 then b1..b4. Assert a
// small-weight key precedes a near-max-weight key.
func TestReview2KeyRotationWeightSort(t *testing.T) {
	const bh = uint64(20) // bh % ACTION_INTERVAL == 0

	// 8 members (keyRotation requires >= 8 keys). Accounts double as
	// gateway keys for easy identification.
	accounts := []string{"s1", "s2", "s3", "s4", "b1", "b2", "b3", "b4"}
	weights := []uint64{
		1, 2, 3, 4, // small — int64 1..4
		^uint64(0) - 3, // b1 = 2^64-4 -> int64 -4
		^uint64(0) - 2, // b2        -> int64 -3
		^uint64(0) - 1, // b3        -> int64 -2
		^uint64(0),     // b4 = 2^64-1 -> int64 -1
	}

	witnessDb := &test_utils.MockWitnessDb{ByAccount: map[string]*witnesses.Witness{}}
	members := make([]elections.ElectionMember, len(accounts))
	for i, acc := range accounts {
		witnessDb.ByAccount[acc] = &witnesses.Witness{Account: acc, GatewayKey: acc}
		members[i] = elections.ElectionMember{Account: acc, Key: acc}
	}

	electionDb := &test_utils.MockElectionDb{
		ElectionsByHeight: map[uint64]elections.ElectionResult{
			bh: {
				ElectionDataInfo: elections.ElectionDataInfo{
					Members: members,
					Weights: weights,
				},
			},
		},
	}

	fake := &fakeHiveCreator{}
	ms := &MultiSig{
		sconf:       systemconfig.MocknetConfig(),
		electionDb:  electionDb,
		witnessDb:   witnessDb,
		hiveCreator: fake,
	}

	if _, err := ms.keyRotation(bh); err != nil {
		t.Fatalf("review2 #57: keyRotation returned error: %v", err)
	}

	idx := func(key string) int {
		for i, ka := range fake.keyAuths {
			if ka[0].(string) == key {
				return i
			}
		}
		return -1
	}
	si, bi := idx("s1"), idx("b1")
	if si < 0 || bi < 0 {
		t.Fatalf("review2 #57: keys missing from rotation auths: s1=%d b1=%d (auths=%v)", si, bi, fake.keyAuths)
	}
	if si > bi {
		t.Fatalf("review2 #57: small-weight key s1 (1) sorted AFTER near-max-weight key b1 (2^64-4): "+
			"s1@%d b1@%d — uint64→int truncation ranked the highest weight lowest", si, bi)
	}
}
