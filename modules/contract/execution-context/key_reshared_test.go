package contract_execution_context_test

import (
	"errors"
	"fmt"
	"testing"

	"vsc-node/lib/test_utils"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db"
	tss_db "vsc-node/modules/db/vsc/tss"
)

// failingCommitments fails its first reads, then answers from the mock.
type failingCommitments struct {
	*test_utils.MockTssCommitmentsDb
	failures int
	err      error
}

func (f *failingCommitments) FindCommitmentsSimple(keyId *string, byTypes []string, epoch *uint64, fromBlock *uint64, toBlock *uint64, limit int) ([]tss_db.TssCommitment, error) {
	if f.failures > 0 {
		f.failures--
		return nil, f.err
	}
	return f.MockTssCommitmentsDb.FindCommitmentsSimple(keyId, byTypes, epoch, fromBlock, toBlock, limit)
}

func strPtr(s string) *string { return &s }

// 0.11.0: TssGetKey's fifth field says whether the key has been handed on by a
// reshare since the keygen that created it. The BTC vault contract starts a
// migration only into a key that has (testnet 2026-10-11: funds moved into the
// gen-2 key in the epoch it was made, before any reshare).
func TestTssGetKey_ReportsWhetherTheKeyWasReshared(t *testing.T) {
	const contractId, keyName = "vsc1Vault", "mainv2"
	const fullKey = contractId + "-" + keyName
	const pub, otherPub = "02aa", "03bb"

	run := func(t *testing.T, key tss_db.TssKey, commitments tss_db.TssCommitments, v2, reshared bool) string {
		t.Helper()
		keys := &test_utils.MockTssKeysDb{Keys: map[string]tss_db.TssKey{fullKey: key}}
		ctx := contract_execution_context.New(contract_execution_context.Environment{ContractId: contractId}, 1000, 100000, 100000, nil,
			contract_session.NewCallSession(nil, nil, nil, keys, commitments, 0, nil), 0,
			contract_execution_context.WithVaultRotationV2(v2), contract_execution_context.WithKeyReshared(reshared))
		res := ctx.TssGetKey(keyName)
		if res.IsErr() {
			t.Fatalf("TssGetKey: %v", res.UnwrapErr())
		}
		return res.Unwrap()
	}
	commitments := func(cs ...tss_db.TssCommitment) *test_utils.MockTssCommitmentsDb {
		m := &test_utils.MockTssCommitmentsDb{Commitments: map[string]tss_db.TssCommitment{}}
		for i, c := range cs {
			c.KeyId = fullKey
			m.Commitments[fmt.Sprint(i)] = c
		}
		return m
	}
	keygen := func(epoch uint64, pk string) tss_db.TssCommitment {
		return tss_db.TssCommitment{Type: "keygen", Epoch: epoch, BlockHeight: epoch * 100, PublicKey: strPtr(pk)}
	}
	reshare := func(epoch uint64) tss_db.TssCommitment {
		return tss_db.TssCommitment{Type: "reshare", Epoch: epoch, BlockHeight: epoch * 100}
	}
	key := func(epoch uint64) tss_db.TssKey {
		return tss_db.TssKey{Id: fullKey, Status: "active", PublicKey: pub, Algo: tss_db.EcdsaType, Epoch: epoch, SignatureVerified: true}
	}

	cases := []struct {
		name         string
		key          tss_db.TssKey
		commitments  tss_db.TssCommitments
		v2, reshared bool
		want         string
	}{
		{"before 0.8.0: legacy three fields", key(1369), commitments(keygen(1368, pub), reshare(1369)), false, false, "active,02aa,ecdsa"},
		{"0.8.0 to 0.10.0: four fields", key(1369), commitments(keygen(1368, pub), reshare(1369)), true, false, "active,02aa,ecdsa,1"},
		{"never the fifth without the fourth", key(1369), commitments(keygen(1368, pub), reshare(1369)), false, true, "active,02aa,ecdsa"},
		{"made this epoch, not yet reshared (testnet gen 2 at 7276300)", key(1368), commitments(keygen(1368, pub)), true, true, "active,02aa,ecdsa,1,0"},
		{"reshared once (testnet gen 2 at 7278600)", key(1369), commitments(keygen(1368, pub), reshare(1369)), true, true, "active,02aa,ecdsa,1,1"},
		{"a later keygen for the same id with another key does not count as its creation",
			key(1368), commitments(keygen(1368, pub), keygen(1370, otherPub)), true, true, "active,02aa,ecdsa,1,0"},
		{"no keygen carrying this key: not proven", key(1371), commitments(keygen(1368, otherPub), reshare(1371)), true, true, "active,02aa,ecdsa,1,0"},
		{"no commitments at all", key(1371), commitments(), true, true, "active,02aa,ecdsa,1,0"},
		{"a record that cannot decode reads as not reshared", key(1369),
			&failingCommitments{commitments(keygen(1368, pub)), 1, fmt.Errorf("%w: bad doc", db.ErrDecode)}, true, true, "active,02aa,ecdsa,1,0"},
		{"a transient read error is retried, never answered differently", key(1369),
			&failingCommitments{commitments(keygen(1368, pub), reshare(1369)), 2, errors.New("connection reset")}, true, true, "active,02aa,ecdsa,1,1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(t, tc.key, tc.commitments, tc.v2, tc.reshared); got != tc.want {
				t.Fatalf("TssGetKey = %q, want %q", got, tc.want)
			}
		})
	}
}
