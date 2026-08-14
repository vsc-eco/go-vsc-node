package blockproducer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// maxWireCids bounds how many individual component CIDs are copied into the
// gossip message. The full lists are always summarised by count+digest, so a
// divergence past the cap is still detected — the cap only limits how much
// detail travels over pubsub.
const maxWireCids = 64

// blockComponents captures every input that feeds the block header CID, so a
// CID mismatch can name WHICH component diverged instead of printing two
// opaque header CIDs.
//
// This is diagnostics only. Nothing here is signed, nothing here feeds the
// header CID, and nothing here may ever gate accept/reject — see Diff, which
// is only ever called on a path that has already decided to reject.
type blockComponents struct {
	Prevb      string   // previous block content CID ("" when nil)
	Br         [2]int   // header block range
	MerkleRoot string   // merkle root over the leaves
	BlockCid   string   // CID of the block body object
	Oplog      string   // oplog tx CID ("" when the block carries no oplog)
	Outputs    []string // contract-output tx CIDs
	Leaves     []string // every merkle leaf (input txs + oplog + outputs + gov ops)

	// Populated only by parseComponents: the peer's FULL list sizes and
	// digests, which may describe more entries than the wire cap carried.
	outputsCount  int
	outputsDigest string
	leavesCount   int
	leavesDigest  string
}

// digest returns a stable fingerprint of a CID list so two nodes can compare
// arbitrarily long lists over a bounded-size gossip message.
func digest(ids []string) string {
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func capCids(ids []string) []string {
	if len(ids) <= maxWireCids {
		return ids
	}
	return ids[:maxWireCids]
}

// toMap renders the components for the block gossip message. Keys are additive:
// peers running older code simply ignore them, and this node tolerates their
// absence (see parseComponents).
func (c *blockComponents) toMap() map[string]interface{} {
	if c == nil {
		return nil
	}
	return map[string]interface{}{
		"prevb":         c.Prevb,
		"br":            []int{c.Br[0], c.Br[1]},
		"merkle_root":   c.MerkleRoot,
		"block_cid":     c.BlockCid,
		"oplog":         c.Oplog,
		"outputs":       capCids(c.Outputs),
		"outputs_count": len(c.Outputs),
		"outputs_dig":   digest(c.Outputs),
		"leaves":        capCids(c.Leaves),
		"leaves_count":  len(c.Leaves),
		"leaves_dig":    digest(c.Leaves),
	}
}

// parseComponents reads a peer's components out of a gossip message. It returns
// nil for any shape it does not recognise — an older peer that never sent the
// field, or a malformed one. Callers must treat nil as "no peer detail
// available" and never as evidence of anything.
func parseComponents(v interface{}) *blockComponents {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	str := func(k string) string {
		s, _ := m[k].(string)
		return s
	}
	strs := func(k string) []string {
		raw, _ := m[k].([]interface{})
		out := make([]string, 0, len(raw))
		for _, e := range raw {
			s, _ := e.(string)
			out = append(out, s)
		}
		return out
	}
	num := func(k string) int {
		f, _ := m[k].(float64)
		return int(f)
	}
	c := &blockComponents{
		Prevb:      str("prevb"),
		MerkleRoot: str("merkle_root"),
		BlockCid:   str("block_cid"),
		Oplog:      str("oplog"),
		Outputs:    strs("outputs"),
		Leaves:     strs("leaves"),
	}
	if br, ok := m["br"].([]interface{}); ok && len(br) == 2 {
		a, _ := br[0].(float64)
		b, _ := br[1].(float64)
		c.Br = [2]int{int(a), int(b)}
	}
	// Counts/digests describe the FULL lists, which may exceed what the wire
	// carried. Stash them so Diff compares the whole list, not just the cap.
	c.outputsCount = num("outputs_count")
	c.outputsDigest = str("outputs_dig")
	c.leavesCount = num("leaves_count")
	c.leavesDigest = str("leaves_dig")
	return c
}

// Diff names the components that differ between the locally derived block and
// the producer's. Returns nil when the peer sent no component detail.
func (c *blockComponents) Diff(remote *blockComponents) []string {
	if c == nil || remote == nil {
		return nil
	}
	var out []string
	if c.Prevb != remote.Prevb {
		out = append(out, "prevb")
	}
	if c.Br != remote.Br {
		out = append(out, "br")
	}
	if c.Oplog != remote.Oplog {
		out = append(out, "oplog")
	}
	// Only compare the summarised lists when the peer actually supplied a
	// digest. A peer that sent a partial payload must not be reported as
	// divergent — a misleading diagnostic is worse than a missing one.
	if remote.outputsDigest != "" &&
		(len(c.Outputs) != remote.outputsCount || digest(c.Outputs) != remote.outputsDigest) {
		out = append(out, "contract_outputs")
	}
	if remote.leavesDigest != "" &&
		(len(c.Leaves) != remote.leavesCount || digest(c.Leaves) != remote.leavesDigest) {
		out = append(out, "tx_list")
	}
	if c.MerkleRoot != remote.MerkleRoot {
		out = append(out, "merkle_root")
	}
	if c.BlockCid != remote.BlockCid {
		out = append(out, "block_body")
	}
	return out
}

// summary renders the components as a single compact log field.
func (c *blockComponents) summary() string {
	if c == nil {
		return "<none>"
	}
	prevb := c.Prevb
	if prevb == "" {
		prevb = "<nil>"
	}
	oplog := c.Oplog
	if oplog == "" {
		oplog = "<none>"
	}
	return fmt.Sprintf(
		"prevb=%s br=[%d %d] oplog=%s outputs=%d/%s leaves=%d/%s mr=%s body=%s",
		prevb, c.Br[0], c.Br[1], oplog,
		len(c.Outputs), digest(c.Outputs),
		len(c.Leaves), digest(c.Leaves),
		c.MerkleRoot, c.BlockCid,
	)
}

// outputs and leaves are nil-safe accessors so the mismatch log can render a
// peer's lists without a nil check at every call site.
func (c *blockComponents) outputs() []string {
	if c == nil {
		return nil
	}
	return c.Outputs
}

func (c *blockComponents) leaves() []string {
	if c == nil {
		return nil
	}
	return c.Leaves
}

// joinCids renders up to maxWireCids CIDs for the mismatch log.
func joinCids(ids []string) string {
	if len(ids) == 0 {
		return "<none>"
	}
	return strings.Join(capCids(ids), ",")
}
