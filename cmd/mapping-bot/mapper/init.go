package mapper

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/hasura/go-graphql-client"
	"github.com/ipfs/go-datastore"
	flatfs "github.com/ipfs/go-ds-flatfs"
)

var lastBlockKey = datastore.NewKey("lastblock")
var sentTxsKey = datastore.NewKey("senttxs")

const graphQLUrl = "https://api.vsc.eco/api/v1/graphql"

type MapperState struct {
	Mutex                sync.Mutex
	FfsDatastore         *flatfs.Datastore
	LastBlockHeight      uint32
	AwaitingSignatureTxs *AwaitingSignature
	// txs that have been posted, but haven't been seen in a block yet
	SentTxs   map[string]bool
	GqlClient *graphql.Client
}

func NewMapperState(ffs *flatfs.Datastore) (*MapperState, error) {
	unsignedTxs := &AwaitingSignature{
		Txs:    make(map[string]*SignedData),
		Hashes: make(map[string]*HashMetadata),
	}

	// heightVal, err := ffs.Get(context.TODO(), lastBlockKey)
	// if err != nil {
	// 	return nil, fmt.Errorf("failed to get last block height: %w", err)
	// }
	// heightInt, err := strconv.Atoi(string(heightVal))
	// if err != nil {
	// 	return nil, err
	// }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	height, err := ffs.Get(ctx, lastBlockKey)
	var heightInt uint64
	if err != nil {
		heightInt = 4782782
		log.Printf("error fetching last block height: %s", err.Error())
	} else {
		heightInt, err = strconv.ParseUint(string(height), 10, 32)
		if err != nil {
			return nil, err
		}
	}

	return &MapperState{
		Mutex:                sync.Mutex{},
		FfsDatastore:         ffs,
		LastBlockHeight:      uint32(heightInt),
		SentTxs:              make(map[string]bool),
		AwaitingSignatureTxs: unsignedTxs,
		GqlClient:            graphql.NewClient(graphQLUrl, nil),
	}, nil
}
