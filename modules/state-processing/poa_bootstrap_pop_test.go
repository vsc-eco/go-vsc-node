package state_engine

import (
	"errors"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/db/vsc/witnesses"

	ethBls "github.com/protolambda/bls12-381-util"
	"github.com/vsc-eco/hivego"
	"go.mongodb.org/mongo-driver/mongo"
)

// provenWitnessRecord builds the witness record of a HEALTHY operator: a
// consensus BLS key and a gateway secp256k1 key, each with a valid
// proof-of-possession bound to the account. This is what a node running the
// current binary announces (modules/announcements builds both), so it is the
// correct default for every fixture that is not specifically about a broken key.
//
// Deterministic from the account name so a test can rebuild the same record.
func provenWitnessRecord(account string) witnesses.Witness {
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(len(account)*7 + i)
	}
	if len(account) > 0 {
		seed[0] = account[0]
	}
	priv := dids.BlsPrivKey{}
	priv.Deserialize(&seed)
	pub, err := ethBls.SkToPk(&priv)
	if err != nil {
		panic(err)
	}
	did, err := dids.NewBlsDID(pub)
	if err != nil {
		panic(err)
	}
	consPoP, err := dids.GenerateBlsPoP(&priv, account)
	if err != nil {
		panic(err)
	}
	kp := hivego.KeyPairFromBytes(seed[:])
	gwPoP, err := dids.GenerateGatewayKeyPoP(kp, account)
	if err != nil {
		panic(err)
	}
	return witnesses.Witness{
		Account: account,
		Enabled: true,
		Height:  1,
		DidKeys: []witnesses.PostingJsonKeys{
			{CryptoType: "bls", Type: "consensus", Key: string(did), PoP: consPoP},
		},
		GatewayKey:    *kp.GetPublicKeyString(),
		GatewayKeyPoP: gwPoP,
	}
}

// Guards the helper itself: if it ever stopped producing valid proofs, every
// test relying on it would pass or fail for reasons unrelated to its subject.
func TestProvenWitnessRecordActuallyProves(t *testing.T) {
	w := provenWitnessRecord("alice")
	if err := w.VerifyConsensusPoP(); err != nil {
		t.Fatalf("consensus PoP invalid: %v", err)
	}
	if err := w.VerifyGatewayKeyPoP(); err != nil {
		t.Fatalf("gateway PoP invalid: %v", err)
	}
	other := provenWitnessRecord("bob")
	if other.GatewayKey == w.GatewayKey {
		t.Fatal("two accounts share a gateway key — the helper is not per-account")
	}
}

// ★ THE GAP D15 PROVED, CLOSED. With H-6 off, gateway-key proof-of-possession is
// not evaluated during election at all, and bootstrap then writes whatever was
// elected into a registry with no delete path. The result is not one bad epoch:
// it is a permanent member of the electorate holding a share of the ceil(2/3)
// veto over every future admission.
func TestBootstrapExcludesMembersThatCannotProveTheirKeys(t *testing.T) {
	se, seats, wits := poaEnv(t, 7)
	wits.breakGatewayPoP("mallory")

	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol", "mallory"), belowPoa(9), 100)

	if _, ok, _ := seats.GetSeat("mallory"); ok {
		t.Fatal("mallory was seeded despite failing gateway-key proof-of-possession. Seats are " +
			"append-only with no delete path and no vote can remove one, so this is a permanent " +
			"validator that never demonstrated control of the key it announced.")
	}
	for _, acct := range []string{"alice", "bob", "carol"} {
		if _, ok, _ := seats.GetSeat(acct); !ok {
			t.Fatalf("%s proves both keys but was not seeded — the filter is dropping healthy operators", acct)
		}
	}
	if len(seats.seats) != 3 {
		t.Fatalf("registry has %d seats, want 3", len(seats.seats))
	}
}

// A missing consensus PoP is rejected on the same footing as an invalid one.
// Tolerating absence would make the check bypassable by simply not providing a
// proof, which is no check at all.
func TestBootstrapRejectsAMissingConsensusPoP(t *testing.T) {
	se, seats, wits := poaEnv(t, 7)
	wits.breakConsensusPoP("mallory")

	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol", "mallory"), belowPoa(9), 100)

	if _, ok, _ := seats.GetSeat("mallory"); ok {
		t.Fatal("a member with no consensus-key proof-of-possession was enshrined permanently")
	}
	if len(seats.seats) != 3 {
		t.Fatalf("registry has %d seats, want 3", len(seats.seats))
	}
}

// ★ THE REFUSAL, and it is the failure state to understand before activating.
// If too few of the incumbent committee can prove their keys, bootstrap seeds
// NOTHING rather than founding the permanent operator set on a set too small to
// widen itself. The chain is unaffected — the seat gate stays inert and
// candidacy continues exactly as before — but POA does not activate.
func TestBootstrapRefusesWhenTooFewMembersCanProveTheirKeys(t *testing.T) {
	se, seats, wits := poaEnv(t, 7)
	// MocknetConfig MinMembers is 3; leave only 2 provable.
	wits.breakGatewayPoP("carol")
	wits.breakGatewayPoP("dave")

	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol", "dave"), belowPoa(9), 100)

	if len(seats.seats) != 0 {
		t.Fatalf("registry has %d seats, want 0. Seeding a committee below MinMembers founds POA "+
			"permanently on a degraded set whose survivors then hold a 2/3 veto over ever widening "+
			"it again, and there is no un-seed.", len(seats.seats))
	}
}

// The positive control for the refusal: with every operator provable, the same
// committee seeds normally. Without this, the refusal test above would pass just
// as well if bootstrap had stopped working altogether.
func TestBootstrapSeedsWhenEveryMemberProvesItsKeys(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol", "dave"), belowPoa(9), 100)
	if len(seats.seats) != 4 {
		t.Fatalf("registry has %d seats, want 4 — every member proves both keys", len(seats.seats))
	}
}

// absentWitnesses returns mongo.ErrNoDocuments for one account — the ordinary
// result when an account has no announcement below the queried height.
type absentWitnesses struct {
	*fakeWitnesses
	absent string
}

func (a *absentWitnesses) GetWitnessAtHeight(account string, bh *uint64) (*witnesses.Witness, error) {
	if account == a.absent {
		return nil, mongo.ErrNoDocuments
	}
	return a.fakeWitnesses.GetWitnessAtHeight(account, bh)
}

// ★ THE WEDGE. blockingRetry returns only on a nil error, and GetWitnessAtHeight
// signals "no record here" with mongo.ErrNoDocuments rather than a nil witness.
// Feeding that straight into blockingRetry spins forever on the first member
// whose record is simply absent — no election, no progress, no recovery, and a
// log line every 30 seconds insisting the DB has not come back. Absence is a
// determinate answer and must be read as one.
func TestBootstrapDoesNotWedgeOnAMissingWitnessRecord(t *testing.T) {
	se, seats, wits := poaEnv(t, 7)
	se.witnessDb = &absentWitnesses{fakeWitnesses: wits, absent: "carol"}

	done := make(chan struct{})
	go func() {
		se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol", "dave"), belowPoa(9), 100)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("bootstrap BLOCKED on a member with no witness record. mongo.ErrNoDocuments was " +
			"treated as a transient failure, so the node retries a determinate absence forever and " +
			"can never process another block.")
	}

	if _, ok, _ := seats.GetSeat("carol"); ok {
		t.Fatal("carol has no witness record and therefore no proof of possession, but was seeded anyway")
	}
	if len(seats.seats) != 3 {
		t.Fatalf("registry has %d seats, want 3 (alice, bob, dave)", len(seats.seats))
	}
}

// The other half: a genuinely transient read MUST still be retried, or a DB blip
// silently drops a legitimate operator from a registry with no delete path.
type flakyWitnesses struct {
	*fakeWitnesses
	failFor int
}

func (f *flakyWitnesses) GetWitnessAtHeight(account string, bh *uint64) (*witnesses.Witness, error) {
	if f.failFor > 0 {
		f.failFor--
		return nil, errors.New("connection reset by peer")
	}
	return f.fakeWitnesses.GetWitnessAtHeight(account, bh)
}

func TestBootstrapRetriesATransientWitnessRead(t *testing.T) {
	se, seats, wits := poaEnv(t, 7)
	se.witnessDb = &flakyWitnesses{fakeWitnesses: wits, failFor: 3}

	done := make(chan struct{})
	go func() {
		se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), belowPoa(9), 100)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("bootstrap never completed after a transient witness read failure")
	}

	if len(seats.seats) != 3 {
		t.Fatalf("registry has %d seats, want 3. A transient DB error dropped a legitimate operator "+
			"from a registry that has no delete path — the drop is permanent and the error was not.",
			len(seats.seats))
	}
}
