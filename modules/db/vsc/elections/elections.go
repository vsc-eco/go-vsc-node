package elections

import (
	"context"
	"math"
	"vsc-node/lib/dids"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/polydawn/refmt"
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

func (e *elections) StoreElection(a ElectionResult) error {

	totalWeight := uint64(0)
	if len(a.Weights) > 0 {
		for _, weight := range a.Weights {
			totalWeight = totalWeight + weight
		}
	} else {
		totalWeight = uint64(len(a.Members))
	}
	a.TotalWeight = totalWeight
	ctx := context.Background()
	options := options.Update().SetUpsert(true)
	filter := bson.M{
		"epoch": a.Epoch,
	}
	update := ElectionResultRecord{}
	err := refmt.CloneAtlased(a, &update, cbornode.CborAtlas)
	if err != nil {
		return err
	}
	updateQuery := bson.M{
		"$set": update,
	}
	_, err = e.UpdateOne(ctx, filter, updateQuery, options)
	return err
}

func (e *elections) GetElection(epoch uint64) *ElectionResult {
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

func (e *elections) GetElectionByHeight(height uint64) (ElectionResult, error) {
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
		return ElectionResult{}, findResult.Err()
	} else {
		electionRecord := ElectionResultRecord{}
		err := findResult.Decode(&electionRecord)
		if err != nil {
			return ElectionResult{}, err
		}

		electionResult := ElectionResult{}
		err = refmt.CloneAtlased(electionRecord, &electionResult, cbornode.CborAtlas)
		if err != nil {
			return electionResult, err
		}

		// electionResult := ElectionResult{
		// 	electionCommonInfo{
		// 		Epoch: electionRecord.Epoch,
		// 		NetId: electionRecord.NetId,
		// 	},
		// 	electionHeaderInfo{
		// 		Data: electionRecord.Data,
		// 	},
		// 	electionDataInfo{
		// 		Members: electionRecord.Members,
		// 	},
		// }
		return electionResult, nil
	}
}

// Utility function
func CalculateSigningScore(circuit dids.BlsCircuit, election ElectionResult) (uint64, uint64) {
	IncludedDids := circuit.IncludedDIDs()
	BitVector := circuit.RawBitVector()
	WeightTotal := uint64(0)
	sum := uint64(0)
	if election.Weights == nil {
		sum = sum + uint64(len(IncludedDids))
		WeightTotal = uint64(len(election.Members))
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

func MinimalRequiredElectionVotes(blocksSinceLastElection, memberCountOfLastElection uint64) uint64 {
	if blocksSinceLastElection < MIN_BLOCKS_SINCE_LAST_ELECTION {
		//Return 2/3
		return uint64(math.Ceil(float64(memberCountOfLastElection) * 2.0 / 3.0))
	}

	// Calculate minimum and maximum members.
	minMembers := int(math.Floor(float64(memberCountOfLastElection)/2 + 1))
	maxMembers := int(math.Ceil(float64(memberCountOfLastElection) * 2.0 / 3.0))

	// Compute drift.
	cappedBlocks := math.Min(float64(blocksSinceLastElection), float64(MAX_BLOCKS_SINCE_LAST_ELECTION))
	drift := (float64(MAX_BLOCKS_SINCE_LAST_ELECTION) - cappedBlocks) / float64(MAX_BLOCKS_SINCE_LAST_ELECTION)

	// Map drift from [0, 1] to [minMembers, maxMembers].
	mappedValue := float64(minMembers) + (float64(maxMembers)-float64(minMembers))*drift

	return uint64(math.Round(mappedValue))
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

func init() {
	cbornode.RegisterCborType(ElectionResultRecord{})
	cbornode.RegisterCborType(ElectionResult{})
	cbornode.RegisterCborType(ElectionData{})
	cbornode.RegisterCborType(ElectionMember{})
	cbornode.RegisterCborType(electionHeaderRaw{})
}
