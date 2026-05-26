package state_engine

import (
	"fmt"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/consensus_state"
)

// ConsensusLine identifies a coordinated consensus lane (major.consensus).
type ConsensusLine struct {
	Major     uint64
	Consensus uint64
}

func (l ConsensusLine) Key() string {
	return fmt.Sprintf("%d.%d", l.Major, l.Consensus)
}

// ConsensusExecutor is a versioned consensus implementation hook point.
// Phase-1 scaffold: concrete executors will be wired in follow-up upgrades.
type ConsensusExecutor interface {
	Name() string
	Line() ConsensusLine
}

type passthroughConsensusExecutor struct {
	line    ConsensusLine
	execName string
}

func (p *passthroughConsensusExecutor) Name() string {
	if p.execName != "" {
		return p.execName
	}
	return "passthrough"
}
func (p *passthroughConsensusExecutor) Line() ConsensusLine { return p.line }

type ConsensusRuntime struct {
	executors map[string]ConsensusExecutor
	fallback  ConsensusExecutor
}

func NewConsensusRuntime() ConsensusRuntime {
	fallback := &passthroughConsensusExecutor{line: ConsensusLine{}, execName: "passthrough"}
	return ConsensusRuntime{
		executors: map[string]ConsensusExecutor{
			fallback.Line().Key(): fallback,
		},
		fallback: fallback,
	}
}

func (cr *ConsensusRuntime) Has(line ConsensusLine) bool {
	if cr.executors == nil {
		return false
	}
	_, ok := cr.executors[line.Key()]
	return ok
}

func (cr *ConsensusRuntime) Register(exec ConsensusExecutor) {
	if cr.executors == nil {
		cr.executors = map[string]ConsensusExecutor{}
	}
	cr.executors[exec.Line().Key()] = exec
}

func (cr *ConsensusRuntime) Resolve(line ConsensusLine) ConsensusExecutor {
	if cr.executors != nil {
		if exec, ok := cr.executors[line.Key()]; ok {
			return exec
		}
	}
	return cr.fallback
}

func lineFromVersion(v consensusversion.Version) ConsensusLine {
	return ConsensusLine{Major: v.Major, Consensus: v.Consensus}
}

// ConsensusActivation reports the pending scheduled version switch (for API visibility).
func (se *StateEngine) ConsensusActivation() *consensus_state.ScheduledActivation {
	return se.scheduledActivation()
}

// ActiveConsensusLine returns the coordinated major.consensus line for a block height,
// resolved purely from the on-chain election. It is a pure function of blockHeight, so it is
// correct under replay/resync and selects the executor deterministically.
func (se *StateEngine) ActiveConsensusLine(blockHeight uint64) ConsensusLine {
	return lineFromVersion(se.ActiveConsensusVersion(blockHeight))
}

func (se *StateEngine) ActiveConsensusExecutor(blockHeight uint64) ConsensusExecutor {
	line := se.ActiveConsensusLine(blockHeight)
	if !se.consensusRuntime.Has(line) {
		return &passthroughConsensusExecutor{
			line:    line,
			execName: "passthrough-unregistered",
		}
	}
	return se.consensusRuntime.Resolve(line)
}

func (se *StateEngine) RegisterConsensusExecutor(exec ConsensusExecutor) {
	se.consensusRuntime.Register(exec)
}

