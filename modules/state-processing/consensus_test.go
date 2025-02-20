package stateEngine_test

import (
	"fmt"
	"testing"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

func TestConsensusEpoch(t *testing.T) {
	slotInfo := stateEngine.CalculateSlotInfo(7200)

	fmt.Println(slotInfo)

	assert.Equal(t, slotInfo.StartHeight, int64(7200))
	assert.Equal(t, slotInfo.EndHeight, int64(7210))

	epochInfo := stateEngine.CalculateEpochRound(0)

	roundInfo := stateEngine.CalculateSlotInfo(0)

	fmt.Println(epochInfo, roundInfo)
	bbbytes := []byte("sadfaseqrwefwvfwrwdddzxcvasdfasd")
	var bytes32 [32]byte
	copy(bytes32[:], bbbytes)
	witnessList := []stateEngine.Witness{
		{
			Account: "vaultec",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "geo52rey",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "arcange.vsc",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "avalonreport",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "diyhub",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "asgarth.vsc",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "dalz.vsc",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "possibly.vsc",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "v4vapp.vsc",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "manu-node",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "mahdiyari.vsc",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "town-hall",
			Key:     "did:key:bls123f12354134",
		},
		{
			Account: "engrave.vsc",
			Key:     "did:key:bls123f12354134",
		},
	}

	stateEngine.GenerateSchedule(0, witnessList, bytes32)
	stateEngine.CalculateSlotLeader(20, witnessList, bytes32)

}
