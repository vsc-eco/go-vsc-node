package blockproducer

import (
	"encoding/json"
	"testing"
)

// wireRoundTrip pushes components through the exact path a real gossip message
// takes: toMap -> json.Marshal -> json.Unmarshal into map[string]interface{}
// -> parseComponents. Testing Diff on a hand-built struct would pass even if
// the wire encoding dropped every field, so the round trip is the part that
// actually matters.
func wireRoundTrip(t *testing.T, c *blockComponents) *blockComponents {
	t.Helper()
	msg := map[string]interface{}{"components": c.toMap()}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := parseComponents(decoded["components"])
	if got == nil {
		t.Fatal("parseComponents returned nil after a valid round trip")
	}
	return got
}

func baseComponents() *blockComponents {
	return &blockComponents{
		Prevb:      "bafyPREV",
		Br:         [2]int{100, 200},
		MerkleRoot: "bafyMERKLE",
		BlockCid:   "bafyBODY",
		Oplog:      "bafyOPLOG",
		Outputs:    []string{"bafyOUT1", "bafyOUT2"},
		Leaves:     []string{"bafyTX1", "bafyOPLOG", "bafyOUT1", "bafyOUT2"},
	}
}

// TestWireRoundTripPreservesEveryField is the guard that the diagnostic
// survives pubsub at all.
func TestWireRoundTripPreservesEveryField(t *testing.T) {
	local := baseComponents()
	remote := wireRoundTrip(t, local)

	if remote.Prevb != local.Prevb {
		t.Errorf("prevb lost: got %q want %q", remote.Prevb, local.Prevb)
	}
	if remote.Br != local.Br {
		t.Errorf("br lost: got %v want %v", remote.Br, local.Br)
	}
	if remote.Oplog != local.Oplog {
		t.Errorf("oplog lost: got %q want %q", remote.Oplog, local.Oplog)
	}
	if remote.MerkleRoot != local.MerkleRoot || remote.BlockCid != local.BlockCid {
		t.Errorf("merkle/body lost: %q %q", remote.MerkleRoot, remote.BlockCid)
	}
	if remote.outputsCount != len(local.Outputs) || remote.outputsDigest != digest(local.Outputs) {
		t.Errorf("outputs summary lost: count=%d dig=%q", remote.outputsCount, remote.outputsDigest)
	}
	if remote.leavesCount != len(local.Leaves) || remote.leavesDigest != digest(local.Leaves) {
		t.Errorf("leaves summary lost: count=%d dig=%q", remote.leavesCount, remote.leavesDigest)
	}

	// Identical components must report no divergence.
	if d := local.Diff(remote); len(d) != 0 {
		t.Errorf("identical components reported divergent: %v", d)
	}
}

// TestDiffNamesDivergentComponent covers the shapes an operator will actually
// hit. Each case mutates ONE input and asserts the diagnostic names it.
func TestDiffNamesDivergentComponent(t *testing.T) {
	contains := func(hay []string, needle string) bool {
		for _, h := range hay {
			if h == needle {
				return true
			}
		}
		return false
	}

	cases := []struct {
		name    string
		mutate  func(*blockComponents)
		wantHas []string
		wantNot []string
	}{
		{
			// The mainnet shape: divergent chain history. prevb feeds the
			// block body, so both are expected; the merkle root is built only
			// from the leaves and must NOT be flagged.
			name: "divergent_history_prevb",
			mutate: func(c *blockComponents) {
				c.Prevb = "bafyOTHERPREV"
				c.BlockCid = "bafyOTHERBODY"
			},
			wantHas: []string{"prevb", "block_body"},
			wantNot: []string{"oplog", "merkle_root", "contract_outputs"},
		},
		{
			name: "divergent_block_range",
			mutate: func(c *blockComponents) {
				c.Br = [2]int{100, 201}
			},
			wantHas: []string{"br"},
			wantNot: []string{"prevb", "oplog"},
		},
		{
			// A ledger divergence surfaces as a different oplog CID, which is
			// also a merkle leaf.
			name: "divergent_oplog",
			mutate: func(c *blockComponents) {
				c.Oplog = "bafyOTHEROPLOG"
				c.Leaves = []string{"bafyTX1", "bafyOTHEROPLOG", "bafyOUT1", "bafyOUT2"}
				c.MerkleRoot = "bafyOTHERMERKLE"
				c.BlockCid = "bafyOTHERBODY"
			},
			wantHas: []string{"oplog", "tx_list", "merkle_root", "block_body"},
			wantNot: []string{"prevb", "br", "contract_outputs"},
		},
		{
			name: "divergent_contract_output",
			mutate: func(c *blockComponents) {
				c.Outputs = []string{"bafyOUT1", "bafyOTHEROUT"}
				c.Leaves = []string{"bafyTX1", "bafyOPLOG", "bafyOUT1", "bafyOTHEROUT"}
				c.MerkleRoot = "bafyOTHERMERKLE"
				c.BlockCid = "bafyOTHERBODY"
			},
			wantHas: []string{"contract_outputs", "tx_list"},
			wantNot: []string{"prevb", "br", "oplog"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := baseComponents()
			producer := baseComponents()
			tc.mutate(producer)

			remote := wireRoundTrip(t, producer)
			got := local.Diff(remote)

			for _, want := range tc.wantHas {
				if !contains(got, want) {
					t.Errorf("expected %q in divergent set, got %v", want, got)
				}
			}
			for _, notWant := range tc.wantNot {
				if contains(got, notWant) {
					t.Errorf("did NOT expect %q in divergent set, got %v", notWant, got)
				}
			}
			t.Logf("%s -> divergent=%v", tc.name, got)
		})
	}
}

// TestDiffTolerantOfOlderPeer proves the diagnostic degrades safely: a peer
// running code without the components field must not crash the signer or be
// reported as divergent.
func TestDiffTolerantOfOlderPeer(t *testing.T) {
	local := baseComponents()

	// An older producer's message simply has no "components" key.
	oldMsg := map[string]interface{}{
		"producer":  "someone",
		"block_cid": "bafyHEADER",
	}
	remote := parseComponents(oldMsg["components"])
	if remote != nil {
		t.Fatalf("expected nil for absent components, got %+v", remote)
	}
	if d := local.Diff(remote); d != nil {
		t.Errorf("expected nil diff against older peer, got %v", d)
	}
	// summary() and the list accessors must be nil-safe — they are called
	// unconditionally on the mismatch log line.
	if s := remote.summary(); s != "<none>" {
		t.Errorf("nil summary: %q", s)
	}
	if j := joinCids(remote.outputs()); j != "<none>" {
		t.Errorf("nil outputs: %q", j)
	}
	if j := joinCids(remote.leaves()); j != "<none>" {
		t.Errorf("nil leaves: %q", j)
	}
}

// TestDiffIgnoresPartialPeerPayload guards the guard: a peer that supplied no
// digests must not be reported as divergent on those lists.
func TestDiffIgnoresPartialPeerPayload(t *testing.T) {
	local := baseComponents()
	partial := parseComponents(map[string]interface{}{
		"prevb": "bafyPREV",
		"br":    []interface{}{float64(100), float64(200)},
		"oplog": "bafyOPLOG",
		// no merkle_root/block_cid/digests at all
	})
	if partial == nil {
		t.Fatal("expected non-nil for a partial map")
	}
	got := local.Diff(partial)
	for _, name := range []string{"contract_outputs", "tx_list"} {
		for _, g := range got {
			if g == name {
				t.Errorf("partial payload wrongly reported %q divergent (got %v)", name, got)
			}
		}
	}
	t.Logf("partial peer -> divergent=%v", got)
}

// TestDigestDetectsBeyondWireCap proves a divergence past the wire cap is still
// caught: the cap truncates the copied list but the digest covers all of it.
func TestDigestDetectsBeyondWireCap(t *testing.T) {
	mk := func(n int, mutateAt int) *blockComponents {
		c := &blockComponents{Prevb: "p", MerkleRoot: "m", BlockCid: "b", Oplog: "o"}
		for i := 0; i < n; i++ {
			id := "leaf" + string(rune('A'+i%26)) + string(rune('0'+i/26))
			if i == mutateAt {
				id = "MUTATED"
			}
			c.Leaves = append(c.Leaves, id)
		}
		return c
	}
	const n = maxWireCids + 40
	local := mk(n, -1)
	producer := mk(n, n-1) // differs only in the LAST leaf, well past the cap

	remote := wireRoundTrip(t, producer)
	if len(remote.Leaves) != maxWireCids {
		t.Fatalf("expected wire list capped at %d, got %d", maxWireCids, len(remote.Leaves))
	}
	if remote.leavesCount != n {
		t.Errorf("expected full count %d on the wire, got %d", n, remote.leavesCount)
	}
	got := local.Diff(remote)
	found := false
	for _, g := range got {
		if g == "tx_list" {
			found = true
		}
	}
	if !found {
		t.Errorf("divergence past the wire cap went undetected: %v", got)
	}
	t.Logf("beyond-cap divergence detected: %v (wire carried %d of %d leaves)",
		got, len(remote.Leaves), remote.leavesCount)
}
