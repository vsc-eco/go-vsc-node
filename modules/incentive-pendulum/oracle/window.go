package oracle

// WitnessSignatureWindow counts, for each witness, how many of the last Width blocks
// included that witness as a signer (plan: Magi incentive pendulum oracle eligibility).
type WitnessSignatureWindow struct {
	Width int
	ring  []map[string]struct{}
}

// NewWitnessSignatureWindow creates an empty window. Width should be 100 for the pendulum spec.
func NewWitnessSignatureWindow(width int) *WitnessSignatureWindow {
	if width < 1 {
		width = 1
	}
	return &WitnessSignatureWindow{Width: width}
}

// PushBlock records one block's signer set (deduplicated). Oldest block drops when length > Width.
func (w *WitnessSignatureWindow) PushBlock(signers []string) {
	if w == nil {
		return
	}
	m := make(map[string]struct{}, len(signers))
	for _, s := range signers {
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

// SignatureCount returns how many stored blocks include this witness.
func (w *WitnessSignatureWindow) SignatureCount(witness string) int {
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
func (w *WitnessSignatureWindow) BlocksRecorded() int {
	if w == nil {
		return 0
	}
	return len(w.ring)
}
