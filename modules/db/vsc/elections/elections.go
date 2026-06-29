package elections

import (
	"context"
	"fmt"
	"math"
	"vsc-node/lib/dids"
	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	cbornode "github.com/ipfs/go-ipld-cbor"
	"github.com/polydawn/refmt"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
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

	indexModel := mongo.IndexModel{
		Keys:    bson.D{{Key: "block_height", Value: 1}},
		Options: options.Index().SetUnique(true),
	}

	// create index on block.block_number for faster queries
	err = e.CreateIndexIfNotExist(indexModel)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
	}

	indexModel2 := mongo.IndexModel{
		Keys:    bson.D{{Key: "epoch", Value: 1}},
		Options: options.Index().SetUnique(true),
	}

	// create index on block.block_number for faster queries
	err = e.CreateIndexIfNotExist(indexModel2)
	if err != nil {
		return fmt.Errorf("failed to create index: %w", err)
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
	// ElectionResultRecord has no Settlement field, and refmt's struct
	// unmarshaller is strict about unknown fields — a non-nil Settlement
	// makes CloneAtlased fail with ErrNoSuchField. The settlement is
	// persisted separately (pendulum_settlements), so drop it from the
	// DB-record clone. Receiver is by value; this doesn't affect the caller.
	a.Settlement = nil
	update := ElectionResultRecord{}
	err := refmt.CloneAtlased(a, &update, cbornode.CborAtlas)
	if update.Type == "" {
		update.Type = "initial"
	}
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

		electionRecord := ElectionResultRecord{}
		err := findResult.Decode(&electionRecord)
		if err != nil {
			return nil
		}

		err = refmt.CloneAtlased(electionRecord, &electionResult, cbornode.CborAtlas)
		if err != nil {
			return nil
		}
		return &electionResult
	}
}

func (e *elections) GetPreviousElections(beforeEpoch uint64, limit int) []ElectionResult {
	findQuery := bson.M{
		"epoch": bson.M{
			"$lt": beforeEpoch,
		},
	}
	queryOptions := options.Find()
	queryOptions.SetSort(bson.M{"epoch": -1})
	queryOptions.SetLimit(int64(limit))

	ctx := context.Background()
	cursor, err := e.Find(ctx, findQuery, queryOptions)
	if err != nil {
		return nil
	}
	defer cursor.Close(ctx)

	var results []ElectionResult
	for cursor.Next(ctx) {
		electionRecord := ElectionResultRecord{}
		err := cursor.Decode(&electionRecord)
		if err != nil {
			// Audit (final pass): a decode error means a corrupt election row.
			// Returning the partial set would SILENTLY DROP that election —
			// which, for the bond gate's established-member map, would miss an
			// established member → gate their already-earned stake → a RESET (the
			// one thing the established exception must never do). Fail the whole
			// read instead; the bond gate's `len(prevs)==0 && previousElection!=nil`
			// guard then fail-stops the election attempt (retries next slot).
			return nil
		}
		electionResult := ElectionResult{}
		err = refmt.CloneAtlased(electionRecord, &electionResult, cbornode.CborAtlas)
		if err != nil {
			return nil
		}
		results = append(results, electionResult)
	}
	// Audit (final pass): a mid-cursor getMore/network error makes Next() return
	// false WITHOUT exhausting the result, silently TRUNCATING it. Unlike a
	// decode error this is per-node and non-deterministic — a truncated read on
	// one node would drop an established member there only → cross-node committee
	// /CID divergence (same M-10 class fixed for GetLedgerRange). Surface it
	// (return nil → fail-closed at the callers) instead of returning a partial.
	if err := cursor.Err(); err != nil {
		return nil
	}
	return results
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

		return electionResult, nil
	}
}

// ChainActiveVersionAt returns the coordinated consensus version active at blockHeight
// — the ResultVersion of the most recent election strictly below it ($lt, matching
// GetElectionByHeight) — queried directly against the live DbInstance.
//
// Unlike the GetElectionByHeight method, it does NOT require the elections plugin's
// collection handle to be Init-bound: it binds a fresh handle from the already-connected
// DbInstance. That makes it safe to call during early startup — specifically the reindex
// version-lag gate (reindex.go), which runs before the elections collection plugin is
// initialized. Calling the method there dereferences a nil *mongo.Collection and panics
// the node on every restart (the gate only fires once a prior run recorded a version).
//
// ok=false when no election precedes blockHeight or the query/decode fails.
func ChainActiveVersionAt(d *db.DbInstance, blockHeight uint64) (consensusversion.Version, bool) {
	var rec struct {
		VersionMajor        uint64 `bson:"version_major"`
		ProtocolVersion     uint64 `bson:"protocol_version"`
		VersionNonConsensus uint64 `bson:"version_non_consensus"`
	}
	err := d.Collection("elections").FindOne(
		context.Background(),
		bson.M{"block_height": bson.M{"$lt": blockHeight}},
		options.FindOne().SetSort(bson.M{"block_height": -1}),
	).Decode(&rec)
	if err != nil {
		return consensusversion.Version{}, false
	}
	return consensusversion.Version{
		Major:        rec.VersionMajor,
		Consensus:    rec.ProtocolVersion,
		NonConsensus: rec.VersionNonConsensus,
	}, true
}

// Utility function
func CalculateSigningScore(circuit *dids.BlsCircuit, election ElectionResult) (uint64, uint64) {
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

const MIN_BLOCKS_SINCE_LAST_ELECTION = 1200   // 1 hour
const MAX_BLOCKS_SINCE_LAST_ELECTION = 403200 // 2 weeks

// MinimalRequiredElectionVotes returns the vote-weight threshold to finalize an
// election. For elections under consensus version 0.2.0+, the threshold is the
// fixed BFT-safe 2/3 quorum. For pre-0.2.0 elections, the threshold decays from
// 2/3 down to a bare majority (floor(W/2+1)) over the MAX_BLOCKS_SINCE_LAST_ELECTION
// window, matching the legacy behavior that was in force when those elections were
// proposed (GV-H3 retroactive compatibility).
func MinimalRequiredElectionVotes(blocksSinceLastElection, memberCountOfLastElection uint64, activeVersion consensusversion.Version) uint64 {
	if consensusversion.Version0_2_0Active(activeVersion) {
		return (2*memberCountOfLastElection + 2) / 3
	}
	// Pre-0.2.0: legacy decay to bare majority (GV-H3 retroactive compatibility).
	if blocksSinceLastElection < MIN_BLOCKS_SINCE_LAST_ELECTION {
		return uint64(math.Ceil(float64(memberCountOfLastElection) * 2.0 / 3.0))
	}
	minMembers := int(math.Floor(float64(memberCountOfLastElection)/2 + 1))
	maxMembers := int(math.Ceil(float64(memberCountOfLastElection) * 2.0 / 3.0))
	cappedBlocks := math.Min(float64(blocksSinceLastElection), float64(MAX_BLOCKS_SINCE_LAST_ELECTION))
	drift := (float64(MAX_BLOCKS_SINCE_LAST_ELECTION) - cappedBlocks) / float64(MAX_BLOCKS_SINCE_LAST_ELECTION)
	mappedValue := float64(minMembers) + (float64(maxMembers)-float64(minMembers))*drift
	return uint64(math.Round(mappedValue))
}

// MinimalRequiredConsensusVersionVotes is a fixed 2/3 stake threshold for adopting a proposed
// consensus version. Unlike MinimalRequiredElectionVotes, it does not decay over time — a
// long-pending upgrade must not become adoptable by a small minority.
func MinimalRequiredConsensusVersionVotes(totalWeight uint64) uint64 {
	if totalWeight == 0 {
		return 0
	}
	return uint64(math.Ceil(float64(totalWeight) * 2.0 / 3.0))
}

func init() {
	cbornode.RegisterCborType(ElectionResultRecord{})
	cbornode.RegisterCborType(ElectionResult{})
	cbornode.RegisterCborType(ElectionData{})
	cbornode.RegisterCborType(ElectionMember{})
	cbornode.RegisterCborType(electionHeaderRaw{})
}
