package dex

import (
	"context"
	"fmt"
	"time"
	"vsc-node/lib/logger"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type DexDb interface {
	// Token registry operations
	GetTokenMetadata(symbol string) (*TokenMetadata, error)
	SetTokenMetadata(token *TokenMetadata) error
	GetAllTokens() ([]TokenMetadata, error)

	// Pool registry operations
	GetPoolInfo(contractId string) (*PoolInfo, error)
	SetPoolInfo(pool *PoolInfo) error
	GetPoolsByAsset(asset string) ([]PoolInfo, error)
	GetAllPools() ([]PoolInfo, error)
	UpdateBondingMetric(contractId string, newMetric uint64) error

	// DEX parameters
	GetDexParams() (*DexParams, error)
	SetDexParams(params *DexParams) error

	// Cross-chain bridge actions for DEX pairs
	StorePairBridgeAction(action *PairBridgeAction) error
	GetPendingPairBridgeActions(chain string) ([]PairBridgeAction, error)
	UpdatePairBridgeActionStatus(id string, status string) error
}

type dexDb struct {
	tokenRegistry     *mongo.Collection
	poolRegistry      *mongo.Collection
	dexParams         *mongo.Collection
	pairBridgeActions *mongo.Collection
	log               logger.Logger
}

func NewDexDb(vscDb *vsc.VscDb, log logger.Logger) DexDb {
	return &dexDb{
		tokenRegistry:     vscDb.Collection("token_registry"),
		poolRegistry:      vscDb.Collection("pool_registry"),
		dexParams:         vscDb.Collection("dex_params"),
		pairBridgeActions: vscDb.Collection("pair_bridge_actions"),
		log:               log,
	}
}

// Token registry operations
func (d *dexDb) GetTokenMetadata(symbol string) (*TokenMetadata, error) {
	var token TokenMetadata
	err := d.tokenRegistry.FindOne(context.Background(), bson.M{"symbol": symbol}).Decode(&token)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("token not found: %s", symbol)
		}
		return nil, err
	}
	return &token, nil
}

func (d *dexDb) SetTokenMetadata(token *TokenMetadata) error {
	now := time.Now()
	token.UpdatedAt = now
	if token.CreatedAt.IsZero() {
		token.CreatedAt = now
	}

	opts := options.Replace().SetUpsert(true)
	_, err := d.tokenRegistry.ReplaceOne(
		context.Background(),
		bson.M{"symbol": token.Symbol},
		token,
		opts,
	)
	return err
}

func (d *dexDb) GetAllTokens() ([]TokenMetadata, error) {
	cursor, err := d.tokenRegistry.Find(context.Background(), bson.M{})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	var tokens []TokenMetadata
	if err = cursor.All(context.Background(), &tokens); err != nil {
		return nil, err
	}
	return tokens, nil
}

// Pool registry operations
func (d *dexDb) GetPoolInfo(contractId string) (*PoolInfo, error) {
	var pool PoolInfo
	err := d.poolRegistry.FindOne(context.Background(), bson.M{"contract_id": contractId}).Decode(&pool)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, fmt.Errorf("pool not found: %s", contractId)
		}
		return nil, err
	}
	return &pool, nil
}

func (d *dexDb) SetPoolInfo(pool *PoolInfo) error {
	now := time.Now()
	pool.UpdatedAt = now
	if pool.CreatedAt.IsZero() {
		pool.CreatedAt = now
	}

	opts := options.Replace().SetUpsert(true)
	_, err := d.poolRegistry.ReplaceOne(
		context.Background(),
		bson.M{"contract_id": pool.ContractId},
		pool,
		opts,
	)
	return err
}

func (d *dexDb) GetPoolsByAsset(asset string) ([]PoolInfo, error) {
	cursor, err := d.poolRegistry.Find(context.Background(), bson.M{
		"$or": []bson.M{
			{"asset0": asset},
			{"asset1": asset},
		},
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	var pools []PoolInfo
	if err = cursor.All(context.Background(), &pools); err != nil {
		return nil, err
	}
	return pools, nil
}

func (d *dexDb) GetAllPools() ([]PoolInfo, error) {
	cursor, err := d.poolRegistry.Find(context.Background(), bson.M{})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	var pools []PoolInfo
	if err = cursor.All(context.Background(), &pools); err != nil {
		return nil, err
	}
	return pools, nil
}

func (d *dexDb) UpdateBondingMetric(contractId string, newMetric uint64) error {
	update := bson.M{
		"$set": bson.M{
			"bonding_metric": newMetric,
			"updated_at":     time.Now(),
		},
	}

	// Check if bonding target is met
	var pool PoolInfo
	err := d.poolRegistry.FindOne(context.Background(), bson.M{"contract_id": contractId}).Decode(&pool)
	if err != nil {
		return err
	}

	if newMetric >= pool.BondingTarget && pool.Status == "locked" {
		update["$set"].(bson.M)["status"] = "unlocked"
	}

	_, err = d.poolRegistry.UpdateOne(
		context.Background(),
		bson.M{"contract_id": contractId},
		update,
	)
	return err
}

// DEX parameters
func (d *dexDb) GetDexParams() (*DexParams, error) {
	var params DexParams
	err := d.dexParams.FindOne(context.Background(), bson.M{}).Decode(&params)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			// Return default parameters if none exist
			return &DexParams{
				PoolCreationFee:       50_000,        // 50 HBD
				BondingCurveThreshold: 1_000_000_000, // 1M liquidity metric
				UpdatedAt:             time.Now(),
			}, nil
		}
		return nil, err
	}
	return &params, nil
}

func (d *dexDb) SetDexParams(params *DexParams) error {
	params.UpdatedAt = time.Now()
	opts := options.Replace().SetUpsert(true)
	_, err := d.dexParams.ReplaceOne(
		context.Background(),
		bson.M{},
		params,
		opts,
	)
	return err
}

// Cross-chain bridge actions for DEX pairs
func (d *dexDb) StorePairBridgeAction(action *PairBridgeAction) error {
	now := time.Now()
	action.UpdatedAt = now
	if action.CreatedAt.IsZero() {
		action.CreatedAt = now
	}

	opts := options.Replace().SetUpsert(true)
	_, err := d.pairBridgeActions.ReplaceOne(
		context.Background(),
		bson.M{"id": action.Id},
		action,
		opts,
	)
	return err
}

func (d *dexDb) GetPendingPairBridgeActions(chain string) ([]PairBridgeAction, error) {
	cursor, err := d.pairBridgeActions.Find(context.Background(), bson.M{
		"chain":  chain,
		"status": "pending",
	})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(context.Background())

	var actions []PairBridgeAction
	if err = cursor.All(context.Background(), &actions); err != nil {
		return nil, err
	}
	return actions, nil
}

func (d *dexDb) UpdatePairBridgeActionStatus(id string, status string) error {
	update := bson.M{
		"$set": bson.M{
			"status":     status,
			"updated_at": time.Now(),
		},
	}

	_, err := d.pairBridgeActions.UpdateOne(
		context.Background(),
		bson.M{"id": id},
		update,
	)
	return err
}
