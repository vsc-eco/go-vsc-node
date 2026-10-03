package poaseats_test

import (
	"testing"

	"vsc-node/modules/db/vsc/poaseats"

	"go.mongodb.org/mongo-driver/bson"
)

// ★ THE REGRESSION FOR THE WORST BUG IN THIS BUILD.
//
// The unique index on ubo_id is SPARSE. A Mongo sparse index only exempts
// documents where the field is ABSENT — an explicitly-stored empty string is
// indexed like any other value. Bootstrap seats carry no vetted owner, so if
// UboId marshals as ubo_id:"" they all collide: seat #1 inserts, #2..N fail with
// duplicate-key errors, and the registry ends up holding ONE seat.
//
// That is not a cosmetic loss. A one-seat registry is NOT empty, so the seat
// gate's inert-while-empty guard does not fire; the gate then deletes every
// other candidate, the committee falls below MinMembers, and elections stop.
// It is the epoch-1699 committee-starvation halt, reintroduced by the very
// mechanism written to prevent it.
//
// No mock can catch this — it is a property of the bson tag and the storage
// engine, not of application logic. So it is tested at the marshalling layer,
// which needs no database.
func TestEmptyUboIsOmittedFromTheStoredDocument(t *testing.T) {
	raw, err := bson.Marshal(poaseats.Seat{
		Account:          "alice",
		AdmittedHeight:   100,
		Bootstrap:        true,
		LastSeatedHeight: 100,
		// UboId deliberately empty: this is exactly what bootstrap writes.
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, present := doc["ubo_id"]; present {
		t.Fatalf("ubo_id is present (%#v) on a bootstrap seat — a SPARSE unique index does not exempt an explicit empty string, so every bootstrap seat after the first would fail with a duplicate-key error and the committee would be starved to one seat", v)
	}
}

// A real owner id must of course still be stored, or the per-owner cap has
// nothing to bind on.
func TestNonEmptyUboIsStored(t *testing.T) {
	raw, err := bson.Marshal(poaseats.Seat{
		Account: "alice", UboId: "UBO-7f3c", AdmittedHeight: 100,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["ubo_id"] != "UBO-7f3c" {
		t.Fatalf("ubo_id = %#v, want UBO-7f3c — the one-seat-per-owner cap cannot bind on an unstored owner", doc["ubo_id"])
	}
}

// Account is the registry's primary key and must ALWAYS be stored, even though
// an empty one is refused at the application layer: an absent key field would
// make the unique index meaningless.
func TestAccountAndAdmittedHeightAreAlwaysStored(t *testing.T) {
	raw, _ := bson.Marshal(poaseats.Seat{Account: "alice", AdmittedHeight: 100})
	var doc bson.M
	_ = bson.Unmarshal(raw, &doc)
	if _, ok := doc["account"]; !ok {
		t.Fatal("account is absent from the stored document — the unique index has nothing to bind on")
	}
	if _, ok := doc["admitted_height"]; !ok {
		t.Fatal("admitted_height is absent — height-addressed reads would not see the seat at any height")
	}
}

// Seats are written through governance.NormalizeAccount (which lowercases) on
// the admission path and read through this helper on the election-gate and
// maintenance paths. If the two disagree on case, a seat stored as "alice" is
// looked up as "Alice" and silently matches nothing — excluding an admitted
// operator from the committee, and recording a seated operator as EXITED, which
// arms the collateral halt against an honest node.
func TestNormalizeAccountIsCaseFolding(t *testing.T) {
	for _, in := range []string{"Alice", "ALICE", "hive:Alice", "  hive:ALICE  "} {
		if got := poaseats.NormalizeAccount(in); got != "alice" {
			t.Fatalf("NormalizeAccount(%q) = %q, want alice — a case mismatch between the write and read paths silently empties a committee", in, got)
		}
	}
}

// ★ The mirror image of the ubo_id regression, and just as dangerous.
//
// last_seated_height and exit_height are QUERIED BY VALUE: SetSeating filters
// last_seated_height $lte, SetExit filters exit_height == 0 and
// last_seated_height $gt 0. A MongoDB value query does not match a document
// where the field is ABSENT. So if either carried bson omitempty, a freshly
// admitted seat (both values 0, therefore omitted) would match neither filter:
// SetSeating would never fire, the seat would never be recorded as seated, and
// SetExit would never arm the collateral halt — silently, with every in-memory
// test still green because a Go map has no notion of an absent field.
func TestQueriedHeightFieldsAreAlwaysStoredEvenWhenZero(t *testing.T) {
	raw, err := bson.Marshal(poaseats.Seat{Account: "alice", AdmittedHeight: 100})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"last_seated_height", "exit_height"} {
		v, present := doc[field]
		if !present {
			t.Fatalf("%s is absent on a fresh seat — it is filtered BY VALUE, and a Mongo value query never matches an absent field, so the write guarding on it would silently never fire", field)
		}
		if n, ok := v.(int64); !ok || n != 0 {
			t.Fatalf("%s = %#v, want an explicit 0", field, v)
		}
	}
}

// The write path (governance.NormalizeAccount) lowercases BEFORE stripping the
// prefix. This helper must do the same, or the two disagree on precisely the
// mixed-case inputs that case-handling exists for: stripping first is
// case-sensitive, so "HIVE:alice" would keep its prefix here and normalise to
// "hive:alice" while the write path produced "alice" — a silent membership
// mismatch, which in the election path means an excluded operator and in the
// maintenance path means a wrongly-armed collateral halt.
func TestNormalizeAccountMatchesTheWritePathOnMixedCasePrefixes(t *testing.T) {
	for _, in := range []string{"HIVE:alice", "Hive:Alice", "hive:ALICE", "  HIVE:ALICE  "} {
		if got := poaseats.NormalizeAccount(in); got != "alice" {
			t.Fatalf("NormalizeAccount(%q) = %q, want alice — the read path disagrees with the write path on a mixed-case prefix", in, got)
		}
	}
}
