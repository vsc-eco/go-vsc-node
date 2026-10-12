package tss

import (
	"fmt"
	"strings"
	"testing"
)

// realistic vsc.tss_sign packet entry: contract:vsc1...-main key id, 32-byte
// msg hash (64 hex), ~71-byte DER sig (~142 hex).
func mkEntry(i int) map[string]any {
	return map[string]any{
		"key_id": fmt.Sprintf("contract:vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d-main-%d", i),
		"msg":    strings.Repeat("ab", 32),
		"sig":    strings.Repeat("cd", 71),
	}
}

func TestChunkSigPacket_EmptyAndSingle(t *testing.T) {
	if got := chunkSigPacket(nil, maxTssSignPacketBytes); got != nil {
		t.Fatalf("empty packet must yield nil, got %v", got)
	}
	one := []map[string]any{mkEntry(0)}
	chunks := chunkSigPacket(one, maxTssSignPacketBytes)
	if len(chunks) != 1 || len(chunks[0]) != 1 {
		t.Fatalf("single entry must be one chunk of one, got %d chunks", len(chunks))
	}
}

// The core NT-1 guarantee: no chunk's marshaled {"packet":[...]} exceeds the
// Hive custom_json limit, and every entry is preserved exactly once, in order.
func TestChunkSigPacket_NeverExceedsLimit_PreservesAll(t *testing.T) {
	for _, n := range []int{1, 10, 28, 30, 100, 500} {
		packet := make([]map[string]any, n)
		for i := range packet {
			packet[i] = mkEntry(i)
		}
		chunks := chunkSigPacket(packet, maxTssSignPacketBytes)

		// 1) every chunk fits under the limit (a multi-entry chunk never
		//    overflows; a lone entry is allowed to be its own chunk).
		total := 0
		var flat []map[string]any
		for ci, c := range chunks {
			if len(c) == 0 {
				t.Fatalf("n=%d: empty chunk %d", n, ci)
			}
			if sz := packetJSONLen(c); sz > maxTssSignPacketBytes && len(c) > 1 {
				t.Fatalf("n=%d: chunk %d has %d entries = %d bytes > %d", n, ci, len(c), sz, maxTssSignPacketBytes)
			}
			total += len(c)
			flat = append(flat, c...)
		}
		// 2) no entry lost or duplicated.
		if total != n {
			t.Fatalf("n=%d: chunks cover %d entries, want %d", n, total, total)
		}
		// 3) order preserved (key_id carries the index).
		for i := range packet {
			if flat[i]["key_id"] != packet[i]["key_id"] {
				t.Fatalf("n=%d: order broken at %d: %v vs %v", n, i, flat[i]["key_id"], packet[i]["key_id"])
			}
		}
		// 4) the stall case: a 30-sig batch (the reported overflow) must split
		//    into >1 chunk rather than one oversized op.
		if n == 30 && len(chunks) < 2 {
			t.Fatalf("n=30 must split into >=2 chunks (the overflow case), got %d", len(chunks))
		}
	}
}
