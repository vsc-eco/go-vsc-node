package settlement

import (
	"fmt"
	"sync"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/elections"
)

type ElectionReader interface {
	GetElectionByHeight(height uint64) (elections.ElectionResult, error)
}

type Engine struct {
	mu sync.Mutex

	electionDb ElectionReader
	selfID     string

	getSchedule func(slotHeight uint64) []ScheduledLeader
	onBoundary  func(info BoundaryInfo) error

	lastEpoch uint64
	hasEpoch  bool
}

type BoundaryInfo struct {
	CurrentEpoch  uint64
	PreviousEpoch uint64
	BlockHeight   uint64
	Leader        string
	SelfID        string
}

type ScheduledLeader struct {
	Account    string
	SlotHeight uint64
}

func New(
	electionDb ElectionReader,
	selfID string,
	getSchedule func(slotHeight uint64) []ScheduledLeader,
	onBoundary func(info BoundaryInfo) error,
) *Engine {
	return &Engine{
		electionDb:  electionDb,
		selfID:      selfID,
		getSchedule: getSchedule,
		onBoundary:  onBoundary,
		lastEpoch:   0,
		hasEpoch:    false,
	}
}

func (e *Engine) ProcessBlock(blockHeight uint64) error {
	if e == nil || e.electionDb == nil {
		return nil
	}
	electionInfo, err := e.electionDb.GetElectionByHeight(blockHeight)
	if err != nil {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.hasEpoch {
		e.lastEpoch = electionInfo.Epoch
		e.hasEpoch = true
		return nil
	}
	if electionInfo.Epoch == e.lastEpoch {
		return nil
	}

	prev := e.lastEpoch
	e.lastEpoch = electionInfo.Epoch

	if e.onBoundary == nil || e.getSchedule == nil {
		return nil
	}

	schedule := e.getSchedule(blockHeight)
	leader, ok := pickSlotLeader(schedule, blockHeight)
	if !ok {
		return nil
	}
	if e.selfID != "" && leader != e.selfID {
		return nil
	}

	return e.onBoundary(BoundaryInfo{
		CurrentEpoch:  electionInfo.Epoch,
		PreviousEpoch: prev,
		BlockHeight:   blockHeight,
		Leader:        leader,
		SelfID:        e.selfID,
	})
}

func pickSlotLeader(schedule []ScheduledLeader, blockHeight uint64) (string, bool) {
	if len(schedule) == 0 {
		return "", false
	}
	slotLen := common.CONSENSUS_SPECS.SlotLength
	if slotLen == 0 {
		return "", false
	}
	slotStart := blockHeight - (blockHeight % slotLen)
	for _, s := range schedule {
		if s.SlotHeight == slotStart {
			return s.Account, true
		}
	}
	return "", false
}

func BuildDeterministicSettlementTxID(epoch uint64, previousEpoch uint64, leader string) string {
	return fmt.Sprintf("pendulum:settlement:%d:%d:%s", epoch, previousEpoch, leader)
}
