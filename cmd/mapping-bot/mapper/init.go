package mapper

import (
	"context"
	"strconv"
	"sync"

	"github.com/ipfs/go-datastore"
	flatfs "github.com/ipfs/go-ds-flatfs"
)

var lastBlockKey = datastore.NewKey("lastblock")
var observedTxsKey = datastore.NewKey("observed")
var sentTxsKey = datastore.NewKey("senttxs")

type MapperState struct {
	Mutex                sync.Mutex
	FfsDatastore         *flatfs.Datastore
	LastBlockHeight      uint32
	ObservedTxs          map[string]bool
	AwaitingSignatureTxs *AwaitingSignature
	// txs that have been posted, but haven't been seen in a block yet
	SentTxs map[string]bool
}

func NewMapperState(ffs *flatfs.Datastore) (*MapperState, error) {
	unsignedTxs := &AwaitingSignature{
		Txs:    make(map[string]*SignedData),
		Hashes: make(map[string]*HashMetadata),
	}
	ctx := context.TODO()
	heightVal, err := ffs.Get(ctx, lastBlockKey)
	if err != nil {
		return nil, err
	}
	heightInt, err := strconv.Atoi(string(heightVal))
	if err != nil {
		return nil, err
	}
	return &MapperState{
		Mutex:                sync.Mutex{},
		FfsDatastore:         ffs,
		LastBlockHeight:      uint32(heightInt),
		ObservedTxs:          make(map[string]bool),
		SentTxs:              make(map[string]bool),
		AwaitingSignatureTxs: unsignedTxs,
	}, nil
}
