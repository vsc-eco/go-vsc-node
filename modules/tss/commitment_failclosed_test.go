package tss

import (
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/elections"

	"github.com/stretchr/testify/require"
)

// 18h regression.
//
// setToCommitment used to read the election with GetElection, which swallows
// ANY read error -- a transient Mongo blip included -- into a nil
// *ElectionResult, and then dereference it as electionData.Members. Two of its
// callers run on bare goroutines (the ECDSA and EdDSA reshare-result handlers in
// dispatcher.go, the only two of six in that file that had no recover()), so
// the nil deref did not fail one session: it terminated the process.
//
// There are two distinct hazards and these tests pin both:
//   - the panic itself, now an error; and
//   - the silent-wrong-answer, which is the subtler one -- a commitment encoded
//     against a MISSING election is an empty or under-populated bitset, and it
//     is consensus-visible data the node BLS-signs. Callers that can decline to
//     publish must do so rather than vouch for it.

// nilElectionDb returns (nil, nil): no error, no election. The Elections
// interface permits it, so the nil check must stand on its own rather than
// relying on a non-nil error to imply a non-nil result.
type nilElectionDb struct {
	*test_utils.MockElectionDb
}

func (nilElectionDb) GetElectionStrict(epoch uint64) (*elections.ElectionResult, error) {
	return nil, nil
}

func TestTSS18h_SetToCommitmentErrorsInsteadOfPanicking(t *testing.T) {
	present := uint64(7)
	mock := &test_utils.MockElectionDb{Elections: map[uint64]*elections.ElectionResult{
		present: makeElection(present, []string{"alice", "bob", "carol"}),
	}}

	cases := []struct {
		name  string
		mgr   *TssManager
		epoch uint64
	}{
		// What a transient read failure looks like through the interface.
		{"election absent", &TssManager{electionDb: mock}, 999},
		// What the interface allows but the old code assumed away.
		{"db returns (nil, nil)", &TssManager{electionDb: nilElectionDb{mock}}, present},
	}

	participants := []Participant{{Account: "alice"}, {Account: "carol"}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				commitment string
				err        error
			)
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("setToCommitment PANICKED (18h not fixed): %v", r)
					}
				}()
				commitment, err = tc.mgr.setToCommitment(participants, tc.epoch)
			}()

			require.Error(t, err, "an unusable election must be reported, not encoded")
			require.Empty(t, commitment, "no commitment may be returned alongside the error")
			t.Logf("reported cleanly: %v", err)
		})
	}
}

// The control: a present election still encodes exactly as before.
func TestTSS18h_SetToCommitmentStillEncodesWhenElectionPresent(t *testing.T) {
	epoch := uint64(7)
	mgr := &TssManager{electionDb: &test_utils.MockElectionDb{
		Elections: map[uint64]*elections.ElectionResult{
			epoch: makeElection(epoch, []string{"alice", "bob", "carol"}),
		},
	}}

	commitment, err := mgr.setToCommitment([]Participant{{Account: "alice"}, {Account: "carol"}}, epoch)
	require.NoError(t, err)
	require.NotEmpty(t, commitment)

	// alice (bit 0) and carol (bit 2) set, bob (bit 1) clear.
	decoded := decodeBlameBitset(commitment, mgr.electionDb.GetElection(epoch).Members)
	require.ElementsMatch(t, []string{"alice", "carol"}, decoded)
}

// storeReshareResult must fail CLOSED when the commitment cannot be computed:
// the share is already written to the keystore at that point, but the node must
// not publish a result, because publishing a wrong commitment is worse than
// publishing none -- a missing result retries the session, a bad commitment is
// vouched for.
func TestTSS18h_StoreReshareResultFailsClosedOnUnusableElection(t *testing.T) {
	mgr := shareStoreManager(t, true /*writable*/, true /*encKeyOK*/)

	d := &ReshareDispatcher{
		BaseDispatcher: shareStoreBase(t, mgr, "reshare-2600-0-vsc1Vault-mainv2"),
		newParticipants: []Participant{
			{Account: "magi.test1"}, {Account: "magi.test2"}, {Account: "magi.test3"},
		},
		// shareStoreManager only stocks shareStoreEpoch, so this epoch is absent.
		newEpoch:           shareStoreEpoch + 500,
		prevCommitmentType: "keygen",
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("storeReshareResult PANICKED (18h not fixed): %v", r)
			}
		}()
		d.storeReshareResult("ECDSA", []byte(`{"stub":true}`))
	}()

	require.Nil(t, d.result, "no result may be published without a trustworthy commitment")
	require.Error(t, d.shareErr, "the failure must be recorded so Done() rejects")

	// And the dispatcher must not be left hanging: before the fix the panic
	// skipped signalDone() entirely, so waiters blocked forever.
	select {
	case <-d.done:
	default:
		t.Fatalf("signalDone() was not called; Done() would block forever")
	}
}

// The same dispatcher, with its election present, still publishes a result --
// so the fail-closed branch above is not just rejecting everything.
func TestTSS18h_StoreReshareResultStillSucceedsWhenElectionPresent(t *testing.T) {
	mgr := shareStoreManager(t, true, true)

	d := &ReshareDispatcher{
		BaseDispatcher: shareStoreBase(t, mgr, "reshare-2600-0-vsc1Vault-mainv2"),
		newParticipants: []Participant{
			{Account: "magi.test1"}, {Account: "magi.test3"},
		},
		newEpoch:           shareStoreEpoch,
		prevCommitmentType: "keygen",
	}

	d.storeReshareResult("ECDSA", []byte(`{"stub":true}`))

	require.NoError(t, d.shareErr)
	require.NotNil(t, d.result, "a usable election must still yield a published result")
	require.NotEmpty(t, d.result.Commitment)
	require.Equal(t, shareStoreEpoch, int(d.result.NewEpoch))
}

// 18h, second half: with Serialize able to report, the two blame shapes now
// DECLINE to publish instead of degrading to an empty bitset.
//
// The empty-bitset fallback was safe in direction — it accuses nobody — but it
// was still a BLS-signed commitment asserting something the node could not
// compute. The caller now skips the result and the session is retried.
func TestTSS18h_BlameSerializeDeclinesWithoutAUsableElection(t *testing.T) {
	mgr := &TssManager{electionDb: electionErrDb{&test_utils.MockElectionDb{}}}

	t.Run("TimeoutResult", func(t *testing.T) {
		r := TimeoutResult{
			tssMgr:   mgr,
			Culprits: []string{"magi.test2"},
			KeyId:    "vsc1Vault-mainv2",
			Epoch:    4,
		}
		_, err := r.Serialize()
		require.Error(t, err, "a timeout blame must not be published when its election is unreadable")
		require.Contains(t, err.Error(), "timeout commitment")
	})

	// And the control: a readable election still serializes a blame normally,
	// so this is not refusing everything.
	t.Run("a usable election still serializes", func(t *testing.T) {
		epoch := uint64(4)
		ok := &TssManager{electionDb: &test_utils.MockElectionDb{
			Elections: map[uint64]*elections.ElectionResult{
				epoch: makeElection(epoch, []string{"magi.test1", "magi.test2", "magi.test3"}),
			},
		}}
		r := TimeoutResult{tssMgr: ok, Culprits: []string{"magi.test2"}, KeyId: "k", Epoch: epoch}
		c, err := r.Serialize()
		require.NoError(t, err)
		require.Equal(t, "blame", c.Type)
		require.NotEmpty(t, c.Commitment, "the bitset must actually name the culprit")
	})
}
