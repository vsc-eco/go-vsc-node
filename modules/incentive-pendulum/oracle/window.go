package oracle

// WitnessProductionWindow is a fixed-width rolling counter of "which witness
// produced each of the last Width L1 (Hive) blocks."
//
// Despite the historical "signature" naming this used to carry, no
// cryptographic signatures are involved — Hive blocks are single-producer,
// and this just records the producer field of each block. Counting per-
// witness production over the window is the pendulum oracle's eligibility
// gate (FeedTrust): a witness must have produced at least
// DefaultMinBlocksProduced of the last Width blocks to count toward the
// trusted price mean, which effectively requires being an actively-rotating
// Hive top-witness.
type WitnessProductionWindow struct {
	Width int
	ring  []map[string]struct{}
}

// NewWitnessProductionWindow creates an empty window. Width should be 100 for the pendulum spec.
func NewWitnessProductionWindow(width int) *WitnessProductionWindow {
	if width < 1 {
		width = 1
	}
	return &WitnessProductionWindow{Width: width}
}

// PushBlock records one block's producer set (deduplicated). In production
// the slice is a single-element list ({block.Witness}) since Hive is single-
// producer-per-block; the set type is retained so a future multi-producer
// chain could be plugged in without rewriting the storage layer. Oldest
// block drops when length > Width.
func (w *WitnessProductionWindow) PushBlock(producers []string) {
	if w == nil {
		return
	}
	m := make(map[string]struct{}, len(producers))
	for _, s := range producers {
		if s == "" {
			continue
		}
		m[s] = struct{}{}
	}
	w.ring = append(w.ring, m)
	if len(w.ring) > w.Width {
		w.ring = w.ring[len(w.ring)-w.Width:]
	}
}

// BlocksProducedBy returns how many stored blocks list this witness as a producer.
func (w *WitnessProductionWindow) BlocksProducedBy(witness string) int {
	if w == nil || witness == "" {
		return 0
	}
	n := 0
	for _, m := range w.ring {
		if _, ok := m[witness]; ok {
			n++
		}
	}
	return n
}

// BlocksRecorded is the number of blocks currently in the window (≤ Width).
func (w *WitnessProductionWindow) BlocksRecorded() int {
	if w == nil {
		return 0
	}
	return len(w.ring)
}
