package gateway

import (
	"crypto/sha256"
	"strconv"
	"testing"

	"vsc-node/lib/test_utils"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"

	"github.com/vsc-eco/hivego"
)

// fixtureGatewayKey returns a well-formed Hive STM-prefixed public key
// derived deterministically from seed. Used by tests that need real-shape
// gateway keys post-FUZZ-1 (safeValidateGatewayKey rejects placeholders).
func fixtureGatewayKey(seed string) string {
	priv := sha256.Sum256([]byte(seed))
	kp := hivego.KeyPairFromBytes(priv[:])
	return *kp.GetPublicKeyString()
}

// fakeHiveCreator captures the gateway owner authority handed to UpdateAccount
// so the gateway-key set, per-key weights, and weight_threshold produced by
// keyRotation are observable.
type fakeHiveCreator struct {
	keyAuths [][2]interface{}
	owner    *hivego.Auths
}

func (f *fakeHiveCreator) UpdateAccount(_ string, owner *hivego.Auths, _ *hivego.Auths, _ *hivego.Auths, _ string, _ string) hivego.HiveOperation {
	f.keyAuths = owner.KeyAuths
	f.owner = owner
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
// `int(weightMap[a]) - int(weightMap[b])`. uint64→int truncates stakes
// >= 2^63 to negative int64, so the comparator ordered the highest-stake
// keys (>= 2^63) as if they were the lowest. The gateway multisig key set
// must be selected identically and correctly on every node, so this
// mis-order is a consensus/correctness defect. Fixed with cmp.Compare on
// the uint64 stakes.
//
// With stake-proportional weighting the >40-key cutoff now keeps the LARGEST
// stakers (sort by stake DESC). This test pins both properties at once: with
// more than MAX_GATEWAY_KEYS eligible witnesses, the near-uint64-max-stake
// keys must be RETAINED and the tiny-stake keys DROPPED. Under the old
// truncating comparator the near-max keys would rank lowest and be the ones
// dropped — the inverse of correct — so this catches a regression.
//
// (Final on-wire key order is canonicalized by hivego's serializer, so the
// test asserts membership, not positional order.)
func TestReview2KeyRotationWeightSort(t *testing.T) {
	const bh = uint64(20) // bh % ACTION_INTERVAL == 0

	// 4 whale accounts with stakes near 2^64 (int64-negative if truncated) and
	// 40 small accounts with stakes 1..40 → 44 eligible > MAX_GATEWAY_KEYS (40).
	// Top-40 by stake keeps the 4 whales + the 36 largest small accounts; the 4
	// smallest small accounts (stakes 1..4) are dropped.
	type member struct {
		account string
		stake   uint64
	}
	mems := []member{
		{"b1", ^uint64(0) - 3}, // 2^64-4 -> int64 -4 if truncated
		{"b2", ^uint64(0) - 2},
		{"b3", ^uint64(0) - 1},
		{"b4", ^uint64(0)}, // 2^64-1 -> int64 -1 if truncated
	}
	for i := 1; i <= 40; i++ {
		mems = append(mems, member{account: "s" + strconv.Itoa(i), stake: uint64(i)})
	}

	witnessDb := &test_utils.MockWitnessDb{ByAccount: map[string]*witnesses.Witness{}}
	members := make([]elections.ElectionMember, len(mems))
	weights := make([]uint64, len(mems))
	gatewayKeys := make(map[string]string, len(mems))
	for i, m := range mems {
		gk := fixtureGatewayKey(m.account)
		gatewayKeys[m.account] = gk
		witnessDb.ByAccount[m.account] = &witnesses.Witness{Account: m.account, GatewayKey: gk}
		members[i] = elections.ElectionMember{Account: m.account, Key: m.account}
		weights[i] = m.stake
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

	present := func(key string) bool {
		for _, ka := range fake.keyAuths {
			if ka[0].(string) == key {
				return true
			}
		}
		return false
	}

	if got := len(fake.keyAuths); got != MAX_GATEWAY_KEYS {
		t.Fatalf("review2 #57: expected exactly %d gateway keys after cutoff, got %d", MAX_GATEWAY_KEYS, got)
	}
	// All 4 near-uint64-max-stake whales must survive the cutoff.
	for _, b := range []string{"b1", "b2", "b3", "b4"} {
		if !present(gatewayKeys[b]) {
			t.Fatalf("review2 #57: near-max-stake key %s was DROPPED by the cutoff — "+
				"uint64→int truncation ranked the highest stake lowest", b)
		}
	}
	// The smallest stakers (stakes 1..4) must be the ones dropped.
	for _, s := range []string{"s1", "s2", "s3", "s4"} {
		if present(gatewayKeys[s]) {
			t.Fatalf("review2 #57: smallest-stake key %s should have been dropped by the cutoff", s)
		}
	}
}
