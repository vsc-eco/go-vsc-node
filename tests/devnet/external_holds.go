package devnet

import "sync"

// External processes the pacemaker cannot probe (a bridge bot, a prover) hold a
// FastBlocks devnet at real cadence while they work, so chain time never jumps
// ahead of them: every async path that acts on the chain must gate the
// pacemaker, and these run outside the nodes' quiescence probe.
var externalHolds = struct {
	sync.Mutex
	m map[*Devnet][]externalHold
}{m: map[*Devnet][]externalHold{}}

type externalHold struct {
	name string
	busy func() bool
}

// AddExternalHold makes busy a reason to hold d at real cadence.
func (d *Devnet) AddExternalHold(name string, busy func() bool) {
	externalHolds.Lock()
	defer externalHolds.Unlock()
	externalHolds.m[d] = append(externalHolds.m[d], externalHold{name: name, busy: busy})
}

// externalHold names the first registered external process that is busy, or "".
func (d *Devnet) externalHold() string {
	externalHolds.Lock()
	hs := externalHolds.m[d]
	externalHolds.Unlock()
	for _, h := range hs {
		if h.busy() {
			return h.name + " busy"
		}
	}
	return ""
}
