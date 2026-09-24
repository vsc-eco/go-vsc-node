package election_proposer

import (
	"testing"

	"vsc-node/modules/db/vsc/witnesses"
)

// Shadow evaluation runs on EVERY election on EVERY node, gate on or off, and it
// verifies a BLS proof-of-possession per candidate. BLS verification is not
// free, so the cost has to be measured rather than assumed: a check that quietly
// added hundreds of milliseconds to election generation would be paid on the
// slot that produces blocks.
func BenchmarkEvaluateKeyAdmission(b *testing.B) {
	t := &testing.T{}
	list := make([]witnesses.Witness, 0, 50)
	for i := 0; i < 50; i++ {
		list = append(list, h6Witness(t, string(rune('a'+i%26))+string(rune('a'+i/26)), byte(i+1), true))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = evaluateKeyAdmission(list)
	}
}
