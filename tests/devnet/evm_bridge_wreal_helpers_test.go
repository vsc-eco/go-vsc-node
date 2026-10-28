//go:build evm_devnet && evm_realproof

package devnet

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// M1's check: the devnet's flat POA committee passes; mainnet's stake-weighted
// committee (its election at epoch 2202, read 2026-10-28) and an election
// without a total do not.
func TestWRealM1_CommitteeHeaviest(t *testing.T) {
	flat := &ElectionInfo{Members: []string{"a", "b", "c", "d", "e"}, Weights: []uint64{1, 1, 1, 1, 1}, TotalWeight: 5}
	if _, _, ok := committeeHeaviest(flat); !ok {
		t.Fatal("five seats of weight 1 not flat")
	}
	mainnet := []uint64{2000000, 4000001, 2000100, 2000000, 2103000, 12950000, 2000000, 3001000, 30000000, 2001000, 7700000, 2000000, 2004000, 2002000, 21894398, 32420128}
	el := &ElectionInfo{Weights: mainnet}
	for i, w := range mainnet {
		el.Members = append(el.Members, fmt.Sprintf("m%d", i))
		el.TotalWeight += w
	}
	if who, share, ok := committeeHeaviest(el); ok || who != "m15" || share < 0.24 || share > 0.26 {
		t.Fatalf("mainnet's committee: %s %.3f flat=%v; want m15 at ~25%%, not flat", who, share, ok)
	}
	if _, _, ok := committeeHeaviest(&ElectionInfo{Members: flat.Members, Weights: flat.Weights}); ok {
		t.Fatal("an election without a total weight read as flat")
	}
	if _, _, ok := committeeHeaviest(&ElectionInfo{Members: []string{"a", "b"}, Weights: []uint64{1, 2}, TotalWeight: 3}); ok {
		t.Fatal("weights 1 and 2 read as flat")
	}
}

type errRecorder struct{ msgs []string }

func (r *errRecorder) Errorf(f string, a ...any) { r.msgs = append(r.msgs, fmt.Sprintf(f, a...)) }

// F6's variants: a payload that could not be built is reported NOT RUN and not
// tried; a built one is tried and reports nothing itself.
func TestWRealF6_ForgedVariantNeverSkipsSilently(t *testing.T) {
	r := &errRecorder{}
	tried := ""
	forgedVariant(r, "F6-block", "", errors.New("rpc 429"), func(id, p string) { tried = id })
	if tried != "" || len(r.msgs) != 1 || !strings.Contains(r.msgs[0], "F6-block NOT RUN: rpc 429") {
		t.Fatalf("a payload error: tried %q, reported %q", tried, r.msgs)
	}
	r.msgs = nil
	forgedVariant(r, "F6-to", "{}", nil, func(id, p string) { tried = id + p })
	if tried != "F6-to{}" || len(r.msgs) != 0 {
		t.Fatalf("a built payload: tried %q, reported %q", tried, r.msgs)
	}
}
