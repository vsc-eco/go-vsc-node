package stateEngine

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/witnesses"
)

var CONSENSUS_SPECS = struct {
	SlotLength     int64
	EpochLength    int64
	ScheduleLength int64
}{
	EpochLength:    7_200, // 6 hours - length between elections
	SlotLength:     10,    //10 * 3 seconds = 30 seconds - length between blocks
	ScheduleLength: 1_200, // 60 * 20 = 1 hour - length of a schedule before it's recalcualted
}

type Consensus struct {
	witnessDb witnesses.Witnesses
}

func (consensus *Consensus) GetWitnessSchedule(blockHeight int) {

}

func CalculateEpochRound(blockHeight int64) struct {
	NextRoundHeight int64
	PastRoundHeight int64
} {
	modLength := CONSENSUS_SPECS.ScheduleLength * CONSENSUS_SPECS.EpochLength
	mod3 := blockHeight % modLength
	pastRoundHeight := blockHeight - mod3

	return struct {
		NextRoundHeight int64
		PastRoundHeight int64
	}{
		NextRoundHeight: blockHeight + (modLength - mod3),
		PastRoundHeight: pastRoundHeight,
	}
}

func CalculateSlotInfo(blockHeight int64) struct {
	StartHeight int64
	EndHeight   int64
} {
	mod3 := blockHeight % CONSENSUS_SPECS.SlotLength

	pastHeight := blockHeight - mod3

	return struct {
		StartHeight int64
		EndHeight   int64
	}{
		StartHeight: pastHeight,
		EndHeight:   pastHeight + CONSENSUS_SPECS.SlotLength,
	}
}

func CalculateRoundInfo(blockHeight int64) struct {
	StartHeight int64
	EndHeight   int64
} {
	mod3 := blockHeight % CONSENSUS_SPECS.ScheduleLength

	pastHeight := blockHeight - mod3
	return struct {
		StartHeight int64
		EndHeight   int64
	}{
		StartHeight: pastHeight,
		EndHeight:   pastHeight + CONSENSUS_SPECS.ScheduleLength,
	}
}

type Witness struct {
	Account string
	Key     string
}

type WitnessSlot struct {
	Account    string `json:"account"`
	SlotHeight int64  `json:"bn"`
}

func GenerateSchedule(blockHeight int64, witnessList []Witness, seed [32]byte) []WitnessSlot {

	roundInfo := CalculateRoundInfo(blockHeight)

	slots := (roundInfo.EndHeight - roundInfo.StartHeight) / CONSENSUS_SPECS.SlotLength

	schedule := make([]WitnessSlot, 0)
	for slot := int64(0); slot < slots; slot++ {
		selection := witnessList[slot%int64(len(witnessList))]
		schedule = append(schedule, WitnessSlot{
			Account: selection.Account,
		})
	}

	data := binary.BigEndian.Uint64(seed[:])
	rando := rand.New(rand.NewSource(int64(data)))
	rando.Shuffle(len(schedule), func(i, j int) {
		schedule[i], schedule[j] = schedule[j], schedule[i]
	})
	//Apply block numbers after sorting
	for i, slot := range schedule {
		sh := (int64(i) * CONSENSUS_SPECS.SlotLength) + roundInfo.StartHeight

		slot.SlotHeight = sh
		schedule[i] = slot
	}

	return schedule
}

func CalculateSlotLeader(blockHeight int64, witnessList []Witness, seed [32]byte) *WitnessSlot {
	schedule := GenerateSchedule(blockHeight, witnessList, seed)
	slotInfo := CalculateSlotInfo(blockHeight)

	//We could probably calculate the index value faster than iterating through witness list.
	//But who cares! It's a max of 120 entries with current config
	var selectedSlot *WitnessSlot
	for _, slot := range schedule {
		if slot.SlotHeight == slotInfo.StartHeight {
			selectedSlot = &slot
			break
		}
	}

	fmt.Println("selectedSlot", selectedSlot)

	return selectedSlot
}

// Streamer designed to handle consensus processing
type ConsensusStreamer struct {
	Cache []any
}

func (consensus *ConsensusStreamer) StreamFunc(block hive_blocks.HiveBlock, extraInfo ProcessExtraInfo) {
	epochInfo := CalculateEpochRound(int64(block.BlockNumber))

	fmt.Println("EpochInfo", epochInfo)
	for _, tx := range block.Transactions {
		fmt.Println(tx)
		for opIdx, op := range tx.Operations {
			fmt.Println(opIdx, op)
			// opIdx
		}
	}
}

//Block A - Anchor
//Block B - TX?
//Block C - ??

//Anchor
// --> Execution results
// --> Newly included TXs
//Processed TX in Block B with updated executions results

//Slot length is 5 blocks
//...Previous 5 blocks happened, and now we are on 6th block
// 6th, it's missed until the 7th
// 6 and 7th have been processed before the slot finalized
// 1 - 5 processed --> some state

// Currently: it's streamed directly from the DB
// And processed immediately
// That does not respect slot intervals (yet)

// Stream from DB
// - We cache all transactions within our slot interval

// 0 - 5 = slot 1
// 6 - 10
// process 0 - 10

// Slot 1 finalized in block 7
// 0 - 5, not 6 or part of 7
//anchor at block 7 + block 6 and 7 on top
//Hive block pipeline [1...5, 6, "Anchor block" at 7] --> correct state
// [1...5, Anchor block, 6, 7]
//0...5 hive txs, 6, Anchor block at 7.
//[Hive TXs, VSC, Hive TXs]

// [0...5, Stop executing at 5, 6, 7, anchor block]
