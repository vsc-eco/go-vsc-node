//go:build evm_devnet

// FS18 (GAP-7): the fleet as testnet will run right after the merge: some
// nodes on develop's previous build (techcoderx's test1/2/3 on cf4d77d5 until
// they update), the rest on this tree, the network at consensus 0.13, with the
// EVM bridge bound. A full BOTH cycle with BTC, ETH and USDC must run with no
// fork: CG-1 changes only a node-local sign gate and a local (not hashed)
// consensus_state field, and SELF-INJECTION-1 is gated on 0.14, so the old and
// new nodes must agree block for block. FS12 covered a 0.12 fleet only.
//
// BB_OLD_CODE_DIR: a clean go-vsc-node checkout of cf4d77d5.
//
// Run: BB_OLD_CODE_DIR=/home/clauderfly/evm-audit/gvn go test -tags evm_devnet -run TestBothBridges_FS18 ./tests/devnet/ -timeout 400m -v
package devnet

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBothBridges_FS18_MixedFleetTestnet(t *testing.T) {
	bbSkipShort(t)
	old := os.Getenv("BB_OLD_CODE_DIR")
	if old == "" {
		t.Skip("set BB_OLD_CODE_DIR to a clean go-vsc-node checkout of cf4d77d5 (testnet's lagging nodes)")
	}
	b := newBothEnv(t, 390*time.Minute, bbWeatherLevel("none"), true, func(c *Config) {
		c.FastBlocks = false
		c.RealCadence = true
		c.OldCodeSourceDir = old
		c.OldCodeNodes = []int{4, 5}
		c.OldCodeSysconfig = true
	})
	base := requireConverged(t, b.ctx, b.d, b.d.cfg.Nodes, 0, 10*time.Minute)
	b.note("mixed fleet (4-5 on %s) converged at block %d", filepath.Base(old), base)
	b.runCycle(cycleHooks{settled: func() {
		after := requireConverged(t, b.ctx, b.d, b.d.cfg.Nodes, base+20, 10*time.Minute)
		b.note("mixed fleet still converged at block %d after the cycle", after)
		t.Logf("CASE FS18 PASS: nodes 4-5 on %s, 1-3 on this tree, consensus 0.13: full BTC/ETH/USDC cycle, no fork (blocks %d..%d compared)",
			filepath.Base(old), base, after)
	}})
}
