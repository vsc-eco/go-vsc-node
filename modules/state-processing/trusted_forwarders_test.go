package state_engine

import (
	"reflect"
	"testing"
)

func TestMergeTrustedForwarders_NoSources(t *testing.T) {
	if got := mergeTrustedForwarders(nil, nil, nil); got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestMergeTrustedForwarders_SysconfigOnly(t *testing.T) {
	got := mergeTrustedForwarders([]string{"contract:A", "contract:B"}, nil, nil)
	want := []string{"contract:A", "contract:B"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeTrustedForwarders_ContractOnly(t *testing.T) {
	got := mergeTrustedForwarders(nil, []string{"contract:A", "contract:B"}, nil)
	want := []string{"contract:A", "contract:B"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeTrustedForwarders_UnionDedup(t *testing.T) {
	// sysconfig has A,B; contract has B,C → union is A,B,C (no dup B).
	// Order preserves sysconfig-first then contract additions.
	got := mergeTrustedForwarders(
		[]string{"contract:A", "contract:B"},
		[]string{"contract:B", "contract:C"},
		nil,
	)
	want := []string{"contract:A", "contract:B", "contract:C"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeTrustedForwarders_RevokeSubtractsFromUnion(t *testing.T) {
	got := mergeTrustedForwarders(
		[]string{"contract:A", "contract:B"},
		[]string{"contract:C"},
		[]string{"contract:B"},
	)
	want := []string{"contract:A", "contract:C"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeTrustedForwarders_RevokeCannotAdd(t *testing.T) {
	// Revoked entry not in union → no-op. The local kill-switch is
	// subtract-only by construction; this verifies the encoding.
	got := mergeTrustedForwarders(
		[]string{"contract:A"},
		nil,
		[]string{"contract:Z"},
	)
	want := []string{"contract:A"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeTrustedForwarders_RevokeBothSysconfigAndContract(t *testing.T) {
	// A revoked entry that comes from BOTH sysconfig + contract is
	// still revoked (the entry is in the union exactly once after
	// dedup, and revocation removes it).
	got := mergeTrustedForwarders(
		[]string{"contract:A", "contract:B"},
		[]string{"contract:B", "contract:C"},
		[]string{"contract:B"},
	)
	want := []string{"contract:A", "contract:C"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMergeTrustedForwarders_AllRevoked(t *testing.T) {
	got := mergeTrustedForwarders(
		[]string{"contract:A"},
		[]string{"contract:B"},
		[]string{"contract:A", "contract:B"},
	)
	if len(got) != 0 {
		t.Errorf("expected empty after total revoke, got %v", got)
	}
}
