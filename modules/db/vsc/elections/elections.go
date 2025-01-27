package elections

import (
	"context"
	"fmt"
	"math"
	"vsc-node/lib/dids"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type elections struct {
	*db.Collection
}

func New(d *vsc.VscDb) Elections {
	return &elections{db.NewCollection(d.DbInstance, "elections")}
}

func (e *elections) Init() error {
	err := e.Collection.Init()
	if err != nil {
		return err
	}

	return nil
}

func (e *elections) StoreElection(a ElectionResult) {
	ctx := context.Background()
	options := options.FindOneAndUpdate().SetUpsert(true)
	filter := bson.M{
		"epoch": a.Epoch,
	}
	updateQuery := bson.M{
		"$set": bson.M{
			"block_height": a.BlockHeight,
			"data":         a.Data,
			"members":      a.Members,
			"proposer":     a.Proposer,
			"weights":      a.Weights,
			"weight_total": a.WeightTotal,
		},
	}
	result := e.FindOneAndUpdate(ctx, filter, updateQuery, options)
	fmt.Println(result.Err())
}

func (e *elections) GetElection(epoch int) *ElectionResult {
	findQuery := bson.M{
		"epoch": epoch,
	}
	ctx := context.Background()
	findResult := e.FindOne(ctx, findQuery)

	if findResult.Err() != nil {
		return nil
	} else {
		electionResult := ElectionResult{}
		findResult.Decode(&electionResult)
		return &electionResult
	}
}

func (e *elections) GetElectionByHeight(height int) *ElectionResult {
	findQuery := bson.M{
		"block_height": bson.M{
			//Elections activate going forward, not retroactively to the same block
			//Thus $lt is logical
			"$lt": height,
		},
	}
	queryOptions := options.FindOne()
	queryOptions.SetSort(bson.M{
		"block_height": -1,
	})
	ctx := context.Background()
	findResult := e.FindOne(ctx, findQuery, queryOptions)

	if findResult.Err() != nil {
		return nil
	} else {
		electionResult := ElectionResult{}
		findResult.Decode(&electionResult)
		return &electionResult
	}
}

// Utility function
func CalculateSigningScore(circuit dids.BlsCircuit, election ElectionResult) (int64, int64) {
	IncludedDids := circuit.IncludedDIDs()
	BitVector := circuit.RawBitVector()
	WeightTotal := int64(0)
	sum := int64(0)
	if election.Weights == nil {
		sum = sum + int64(len(IncludedDids))
		WeightTotal = int64(len(election.Members))
	} else {
		for idx, weight := range election.Weights {
			if BitVector.Bit(idx) == 1 {
				sum = sum + weight
			}
			WeightTotal = WeightTotal + weight
		}
	}

	return sum, WeightTotal
}

func MinimumSigningScore(lastElectionHeight int64, memberCount int64) {

}

// Probably should be a minimum of 6
const MIN_BLOCKS_SINCE_LAST_ELECTION = 1200   // 1 hour
const MAX_BLOCKS_SINCE_LAST_ELECTION = 403200 // 2 weeks

func MinimalRequiredElectionVotes(blocksSinceLastElection, memberCountOfLastElection int) int {
	if blocksSinceLastElection < MIN_BLOCKS_SINCE_LAST_ELECTION {
		//Return 2/3
		return int(math.Ceil(float64(memberCountOfLastElection) * 2.0 / 3.0))
	}

	// Calculate minimum and maximum members.
	minMembers := int(math.Floor(float64(memberCountOfLastElection)/2 + 1))
	maxMembers := int(math.Ceil(float64(memberCountOfLastElection) * 2.0 / 3.0))

	// Compute drift.
	cappedBlocks := math.Min(float64(blocksSinceLastElection), float64(MAX_BLOCKS_SINCE_LAST_ELECTION))
	drift := (float64(MAX_BLOCKS_SINCE_LAST_ELECTION) - cappedBlocks) / float64(MAX_BLOCKS_SINCE_LAST_ELECTION)

	// Map drift from [0, 1] to [minMembers, maxMembers].
	mappedValue := float64(minMembers) + (float64(maxMembers)-float64(minMembers))*drift

	return int(math.Round(mappedValue))
}

// export const MIN_BLOCKS_SINCE_LAST_ELECTION = 1200 // 1 hour
// export const MAX_BLOCKS_SINCE_LAST_ELECTION = 403200 // 2 weeks

// export function minimalRequiredElectionVotes(blocksSinceLastElection: number, memberCountOfLastElection: number): number {
//     if (blocksSinceLastElection < MIN_BLOCKS_SINCE_LAST_ELECTION) {
//         throw new Error('tried to run election before time slot')
//     }
//     const minMembers = Math.floor((memberCountOfLastElection / 2) + 1) // 1/2 + 1
//     const maxMembers = Math.ceil(memberCountOfLastElection * 2 / 3) // 2/3
//     const drift = (MAX_BLOCKS_SINCE_LAST_ELECTION - Math.min(blocksSinceLastElection, MAX_BLOCKS_SINCE_LAST_ELECTION)) / MAX_BLOCKS_SINCE_LAST_ELECTION;
//     return Math.round(Range.from([0, 1]).map(drift, Range.from([minMembers, maxMembers])));
// }
