package tss

import "encoding/json"

// NT-1: the leader broadcasts every freshly produced signature in a SINGLE
// vsc.tss_sign custom_json op ({"packet":[...]}). Hive rejects a custom_json
// whose json field exceeds 8192 bytes, and a DER ECDSA sig is ~70-72 bytes
// (~144 hex chars) plus key_id/msg, so ~28 signatures overflow the cap. The
// op then never lands, the requests stay "unsigned", and every TSS_SIGN_INTERVAL
// re-attempts the same oversized op — the whole batch (a large withdrawal or a
// migration sweep) stalls permanently.
//
// The state engine (state_engine.go vsc.tss_sign handler) iterates packet[]
// and marks each request signed INDEPENDENTLY, so emitting N smaller ops yields
// the identical on-chain result as one big op. chunkSigPacket splits the packet
// so each op's marshaled {"packet":[...]} stays under the limit; the caller
// broadcasts one op per chunk. Leader-only emission — no consensus divergence.

// maxTssSignPacketBytes caps the marshaled {"packet":[...]} json well under
// Hive's 8192-byte custom_json limit, leaving headroom for op/tx envelope.
const maxTssSignPacketBytes = 7800

// packetJSONLen returns the byte length of the marshaled {"packet": entries}
// object — exactly what goes in the custom_json json field.
func packetJSONLen(entries []map[string]any) int {
	b, err := json.Marshal(map[string]any{"packet": entries})
	if err != nil {
		// Marshal of these simple string maps does not fail; treat as oversized
		// so the caller errs on the side of a smaller chunk.
		return maxTssSignPacketBytes + 1
	}
	return len(b)
}

// chunkSigPacket greedily groups packet entries so each group's marshaled
// {"packet":[...]} is <= maxBytes. Entry order is preserved. A single entry
// that alone exceeds maxBytes (should not happen for one sig) is still returned
// in a chunk of its own so it is never silently dropped; the caller logs and
// lets Hive reject only that one op rather than stalling the whole batch.
func chunkSigPacket(packet []map[string]any, maxBytes int) [][]map[string]any {
	if len(packet) == 0 {
		return nil
	}
	chunks := make([][]map[string]any, 0, 1)
	current := make([]map[string]any, 0, len(packet))
	for _, entry := range packet {
		candidate := append(current, entry)
		if len(current) > 0 && packetJSONLen(candidate) > maxBytes {
			// Adding entry overflows: flush current, start fresh with entry.
			chunks = append(chunks, current)
			current = []map[string]any{entry}
			continue
		}
		current = candidate
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}
