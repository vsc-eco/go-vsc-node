package elections

import (
	"context"
	"fmt"
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
			"members": a.Members,
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
		"height": bson.M{
			//Elections activate going forward, not retroactively to the same block
			//Thus $lt is logical
			"$lt": height,
		},
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
